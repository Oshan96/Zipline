package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Phone control: Zipline can listen on the home network so a phone (any browser) can see the
// downloads, add links, pause and resume. It's off until you switch it on.
//
// Pairing: Zipline shows a code (as a QR code too). The code is the phone's secret key; it
// never travels over the network (it's in the address's #fragment, which browsers don't
// send). The phone signs each request with it (HMAC-SHA256 over the method, path, time and
// body), so someone else on the network can't control Zipline even if they see the traffic.
// Signed requests are accepted once, within 90 seconds of their time. Too many bad
// signatures from one address get it locked out for a while.

//go:embed web/m.html
var phoneHTML string

const phonePort = 17891

// appVersion is Zipline's version. The source holds the major and minor parts; a release build gets
// the full one (1.0.3) from tools/package.sh, through -ldflags "-X main.appVersion=…".
var appVersion = "1.0"

type PhoneSettings struct {
	On      bool          `json:"on"`
	Port    int           `json:"port"`
	Devices []PhoneDevice `json:"devices"`
	Allow   []PhoneNet    `json:"allow,omitempty"` // networks marked Public where the phone page is allowed anyway (phonenet.go)
}

type PhoneDevice struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Key     string    `json:"key"` // the pairing code (protected when saved on Windows)
	Added   time.Time `json:"added"`
	Seen    time.Time `json:"seen,omitempty"`
	Pending bool      `json:"pending,omitempty"` // shown on screen, not used by a phone yet
}

// pairing codes: 16 characters from an alphabet without look-alikes (80 bits)
const pairAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func newPairCode() string {
	b := make([]byte, 16)
	rand.Read(b)
	out := make([]byte, 16)
	for i, c := range b {
		out[i] = pairAlphabet[int(c)%32]
	}
	return string(out)
}

// normCode reads a typed code: case, dashes, spaces and look-alikes (O→0, I/L→1) don't matter.
func normCode(s string) string {
	s = strings.ToUpper(s)
	s = strings.NewReplacer("-", "", " ", "", "O", "0", "I", "1", "L", "1").Replace(s)
	return s
}

func deviceID(code string) string {
	h := sha256.Sum256([]byte(normCode(code)))
	return hex.EncodeToString(h[:6])
}

func fmtCode(c string) string {
	if len(c) != 16 {
		return c
	}
	return c[0:4] + "-" + c[4:8] + "-" + c[8:12] + "-" + c[12:16]
}

// phoneSig is what the phone sends in X-ZL-Sig.
func phoneSig(key, method, path, ts string, body []byte) string {
	bh := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(normCode(key)))
	fmt.Fprintf(mac, "%s\n%s\n%s\n%s", method, path, ts, hex.EncodeToString(bh[:]))
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------- the listener

type phoneServer struct {
	m         *Manager
	mu        sync.Mutex
	syncMu    sync.Mutex // one sync at a time (reading the network's labels can take a moment)
	srv       *http.Server
	port      int
	bound     string // the addresses it listens on (phonenet.go)
	err       string
	wantOn    bool // what the settings ask for (the latest wins, whatever order changes come in)
	wantPort  int
	wantAllow []PhoneNet
	stopped   bool
	env       phoneEnv
	nets      []lanNet             // the networks the computer is on, as of the last sync
	asks      map[string]*phoneAsk // phones that used a new code and wait for the window's answer (phonepair.go)
	watchOnce sync.Once
	seen      map[string]time.Time // signatures already used
	fails     map[string][]time.Time
	blocked   map[string]time.Time
}

// applyPhone starts, moves or stops the phone listener to match the settings. Caller holds m.mu.
func (m *Manager) applyPhone() {
	if m.phone == nil {
		m.phone = newPhoneServer(m)
	}
	on, port := m.settings.Phone.On, m.settings.Phone.Port
	if port <= 0 {
		port = phonePort
	}
	m.phone.mu.Lock()
	m.phone.wantOn, m.phone.wantPort, m.phone.wantAllow = on, port, append([]PhoneNet(nil), m.settings.Phone.Allow...)
	m.phone.mu.Unlock()
	m.phone.watchOnce.Do(func() { go m.phone.watch(m.quit, m.phone.env.every) })
	go m.phone.sync()
}

func (p *phoneServer) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	if p.srv != nil {
		p.srv.Close()
		p.srv = nil
	}
}

func (p *phoneServer) status() (port int, running bool, err string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.port, p.srv != nil, p.err
}

func (p *phoneServer) routes() http.Handler {
	mux := http.NewServeMux()
	page := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/m" && r.URL.Path != "/m/" {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; form-action 'none'; frame-ancestors 'none'")
		io.WriteString(w, phoneHTML)
	}
	mux.HandleFunc("/m", page)
	mux.HandleFunc("/m/", page)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/m", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("/m/api/", p.api)
	return mux
}

// authorize checks the request's signature and returns the phone it came from.
func (p *phoneServer) authorize(r *http.Request, body []byte) (*PhoneDevice, int, string) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	now := time.Now()
	p.mu.Lock()
	if until := p.blocked[ip]; now.Before(until) {
		p.mu.Unlock()
		return nil, http.StatusTooManyRequests, "too many wrong attempts — try again in a few minutes"
	}
	p.mu.Unlock()
	fail := func(code int, msg string) (*PhoneDevice, int, string) {
		p.mu.Lock()
		defer p.mu.Unlock()
		var keep []time.Time
		for _, t := range p.fails[ip] {
			if now.Sub(t) < 10*time.Minute {
				keep = append(keep, t)
			}
		}
		keep = append(keep, now)
		p.fails[ip] = keep
		if len(keep) >= 20 {
			p.blocked[ip] = now.Add(10 * time.Minute)
			delete(p.fails, ip)
		}
		return nil, code, msg
	}
	id, ts, sig := r.Header.Get("X-ZL-Device"), r.Header.Get("X-ZL-Time"), r.Header.Get("X-ZL-Sig")
	ms, err := strconv.ParseInt(ts, 10, 64)
	if id == "" || sig == "" || err != nil {
		return fail(http.StatusUnauthorized, "not paired")
	}
	if d := now.Sub(time.UnixMilli(ms)); d > 90*time.Second || d < -90*time.Second {
		return nil, http.StatusUnauthorized, "clock" // the phone's clock is off: it adjusts and retries
	}
	m := p.m
	m.mu.Lock()
	var dev *PhoneDevice
	for _, pd := range m.settings.Phone.Devices {
		if pd.ID == id && !(pd.Pending && now.Sub(pd.Added) > 15*time.Minute) { // a code nobody used goes stale
			c := pd
			dev = &c
		}
	}
	key := ""
	if dev != nil {
		key = dev.Key
	}
	m.mu.Unlock()
	if dev == nil {
		return fail(http.StatusUnauthorized, "not paired")
	}
	want := phoneSig(key, r.Method, r.URL.RequestURI(), ts, body)
	if !hmac.Equal([]byte(want), []byte(strings.ToLower(sig))) {
		return fail(http.StatusUnauthorized, "not paired")
	}
	p.mu.Lock()
	for s, t := range p.seen {
		if now.Sub(t) > 3*time.Minute {
			delete(p.seen, s)
		}
	}
	if _, used := p.seen[sig]; used {
		p.mu.Unlock()
		return nil, http.StatusUnauthorized, "replayed"
	}
	p.seen[sig] = now
	p.mu.Unlock()
	return dev, 0, ""
}

type phoneDL struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Size   int64   `json:"size"`
	Done   int64   `json:"done"`
	Prog   float64 `json:"prog"` // 0…1 (-1 = unknown)
	Speed  float64 `json:"speed"`
	Status string  `json:"status"`
	Text   string  `json:"text"`           // the status in the window's words ("Complete", "Paused"…)
	Tone   string  `json:"tone,omitempty"` // how the window colours it: "done", "err" or "paused"
	Note   string  `json:"note,omitempty"` // why it failed, when it did
	Queue  string  `json:"queue,omitempty"`
	Cat    string  `json:"cat"`
	Added  int64   `json:"added"`

	Kind    string    `json:"kind,omitempty"`    // "hls" or "torrent"
	LimitKB int       `json:"limitKB,omitempty"` // its own speed limit
	Torrent *phoneTor `json:"torrent,omitempty"`
}

func (p *phoneServer) api(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 512<<10)) // (a torrent of thousands of files sends its picks)
	dev, code, msg := p.authorize(r, body)
	if dev == nil {
		w.Header().Set("Date", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("X-ZL-Now", strconv.FormatInt(time.Now().UnixMilli(), 10))
		http.Error(w, msg, code)
		return
	}
	m := p.m
	devID := dev.ID
	m.mu.Lock()
	for i := range m.settings.Phone.Devices {
		pd := &m.settings.Phone.Devices[i]
		if pd.ID != devID {
			continue
		}
		if pd.Pending || time.Since(pd.Seen) > 10*time.Minute {
			m.dirty = true
		}
		pd.Seen = time.Now()
	}
	m.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/m/api/")
	var req phoneReq
	if len(body) > 0 && json.Unmarshal(body, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if dev.Pending && path != "hello" && path != "unpair" { // a new code opens only the question to the window (phonepair.go)
		http.Error(w, "waiting for the computer to allow this phone", http.StatusForbidden)
		return
	}
	switch path {
	case "hello":
		p.hello(w, r, dev, req.Name)
	case "probe":
		p.probe(w, r, &req)
	case "renew":
		p.renew(w, r, &req)
	case "tmeta":
		p.torrentMeta(w, r, &req)
	case "tfiles":
		p.torrentFiles(w, r, &req)
	case "tpick":
		p.torrentPick(w, r, &req)
	case "dlimit":
		p.limitOne(w, r, &req)
	case "state":
		writeJSON(w, p.state())
	case "unpair": // the phone forgets its key: the computer stops accepting it too
		m.removePhone(devID)
		m.mu.Lock()
		m.notify("phonePaired", devID, "A phone unpaired itself")
		m.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	case "add":
		p.add(w, r, &req)
	case "action":
		switch req.Action {
		case "start": // (the Free data hours queue's downloads outside free hours ask first: see gate.go)
			if err := m.StartChecked(req.IDs, req.Sure); err != nil {
				writeJSON(w, map[string]any{"error": err.Error(), "ask": "freeHours"})
				return
			}
		case "pause":
			m.Pause(req.IDs)
		case "remove":
			m.Remove(req.IDs, false)
		case "stopSharing":
			for _, id := range req.IDs {
				m.StopSharing(id)
			}
		default:
			http.Error(w, "bad request", 400)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "queue":
		switch req.Action {
		case "start":
			if err := m.StartQueueChecked(req.ID, req.Sure); err != nil {
				writeJSON(w, map[string]any{"error": err.Error(), "ask": "freeHours"})
				return
			}
		case "stop":
			m.StopQueue(req.ID)
		}
		writeJSON(w, map[string]any{"ok": true})
	case "power": // the notice's buttons: cancel or carry on now ("When all downloads finish", start-up resume)
		if req.Action == "whenDone" || m.powerControl(req.Action, "") != nil {
			http.Error(w, "bad request", 400)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "profile":
		if err := m.SetProfile(req.ID); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "limit":
		m.mu.Lock()
		m.settings.SpeedLimitOn = req.On
		if req.KB > 0 {
			m.settings.SpeedLimitKB = req.KB
		}
		m.applyLimit()
		m.dirty = true
		m.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (p *phoneServer) state() map[string]any {
	s := p.m.Snapshot(nil)
	now := time.Now()
	list := make([]phoneDL, 0, len(s.Downloads))
	for _, d := range s.Downloads {
		name := d.Name
		if name == "" { // not read from the server yet: the link's own name
			if u, err := url.Parse(d.URL); err == nil && path.Base(u.Path) != "/" && path.Base(u.Path) != "." {
				name, _ = url.PathUnescape(path.Base(u.Path))
			}
			if name == "" {
				name = d.URL
			}
		}
		v := phoneDL{ID: d.ID, Name: name, Size: d.Size, Done: d.Done, Speed: d.Speed, Status: d.Status, Queue: d.Queue, Cat: d.Category, Added: d.Added.Unix(), Prog: -1}
		switch {
		case d.Status == StCompleted:
			v.Prog = 1
		case d.Kind == "hls" && d.Prog > 0:
			v.Prog = d.Prog
		case d.Size > 0:
			v.Prog = float64(d.Done) / float64(d.Size)
		}
		v.Kind, v.LimitKB = d.Kind, d.LimitKB
		if t := d.Torrent; t != nil {
			v.Torrent = &phoneTor{Seeding: t.Seeding, Meta: !t.NeedMeta, Files: t.Count}
		}
		v.Text, v.Tone = phoneStatus(&d, &s, now)
		v.Note = phoneReason(&d)
		list = append(list, v)
	}
	var qs []map[string]any
	for _, q := range s.Queues {
		qs = append(qs, map[string]any{"id": q.ID, "name": q.Name, "running": q.Running})
	}
	return map[string]any{"downloads": list, "queues": qs, "speed": s.TotalSpeed,
		"limit": map[string]any{"on": s.SpeedLimitOn, "kb": s.SpeedLimitKB}, "now": time.Now().UnixMilli(), "power": p.m.phonePower(),
		"profile": phoneProfile(s.Profile), "quota": phoneQuota(s.Quota), "plan": phonePlan{s.Plan.On, s.Plan.In, s.Plan.From, s.Plan.To}}
}

// phoneStatus is a download's status in the window's words (statusText in app.js, which a test
// compares this with), so the phone and the window name things alike, and how the window
// colours it (rowCells): "done", "err", "paused" or "".
func phoneStatus(d *DLView, s *Snapshot, now time.Time) (text, tone string) {
	capHeld := s.Cap.Blocked && s.Quota.Count != "up"
	q := s.Quota.Show
	quotaHeld := q != nil && q.Over && !q.CarryOn && s.Quota.Count != "up" && !s.Quota.Free
	p := dlPct(d)
	// "45.2% · Paused": how far it got, when it got anywhere
	with := func(prec int, rest string) string {
		if p > 0 {
			return fmt.Sprintf("%.*f%% · %s", prec, p, rest)
		}
		return rest
	}
	wait := func(at int64) string { return fmtWait(float64(max(0, at-now.UnixMilli())) / 1000) }
	switch {
	case d.Status == StCompleted:
		tone = "done"
		if d.ScanState == "threat" || d.Check != nil && d.Check.State == "bad" {
			tone = "err"
		}
	case d.Status == StError:
		tone = "err"
		if d.Link == "refreshing" {
			tone = "paused"
		}
	case d.Status == StPaused, d.Status == StQueued, d.Status == StDownloading && (capHeld || quotaHeld):
		tone = "paused"
	}
	if t := d.Torrent; t != nil {
		switch {
		case d.Status == StDownloading && t.Checking:
			return fmt.Sprintf("Checking files · %d%%", t.CheckPct), tone
		case d.Status == StCompleted:
			if t.Seeding && d.ScanState != "threat" {
				if t.Held {
					return "Complete · sharing paused", tone
				}
				return "Complete · sharing", tone
			}
			switch {
			case t.NoShare != nil && (t.NoShare.Why == "missing" || t.NoShare.Why == "parts"):
				return "Complete · files missing", tone
			case t.NoShare != nil && t.NoShare.Why == "changed":
				return "Complete · files changed", tone
			case d.Metered: // it carries on sharing on a normal connection
				return "Complete · sharing later", tone
			}
		case d.Status == StDownloading && d.Stage == "" && !capHeld && !quotaHeld:
			if t.NeedMeta {
				if t.MetaPct > 0 {
					return fmt.Sprintf("Getting the file list… %d%%", t.MetaPct), tone
				}
				return "Getting the file list…", tone
			}
			if t.Peers > 0 {
				return fmt.Sprintf("%.2f%% · %s", p, plural(t.Peers, "peer")), tone
			}
			return fmt.Sprintf("%.2f%% · finding peers", p), tone
		}
	}
	switch d.Status {
	case StCompleted:
		switch {
		case d.Stage != "":
			return fmt.Sprintf("Complete · checking %d%%", d.StagePct), tone
		case d.Check != nil && d.Check.State == "bad":
			return "Complete · damaged", tone
		case d.ScanState == "scanning" || d.ScanState == "waiting":
			return "Complete · scanning", tone
		case d.ScanState == "threat":
			return "Complete · threat found", tone
		case d.Check != nil && d.Check.State == "ok":
			return "Complete · verified", tone
		}
		return "Complete", tone
	case StError:
		if d.TryAgain > 0 { // its queue tries it again
			return "Error · retrying in " + wait(d.TryAgain), tone
		}
		w := "Error"
		switch {
		case d.Link == "refreshing":
			w = "Getting a fresh link…"
		case d.Link != "":
			w = "Link expired"
		}
		if d.Link != "" {
			return with(1, w), tone
		}
		return w, tone
	case StConnecting:
		if d.RetryAt == 0 {
			return "Connecting…", tone
		}
		return "Retrying in " + wait(d.RetryAt), tone // every connection was lost: it tries again by itself
	case StQueued:
		inRun := false
		for _, qu := range s.Queues {
			inRun = inRun || qu.ID == d.Queue && qu.Running && d.Queue != ""
		}
		w := "Queued"
		switch {
		case d.Queue == planQueueID && s.Plan.On && !s.Plan.In:
			w = "Waiting for free hours"
		case d.Waiting:
			w = "Waiting for a free slot"
		case inRun && s.Plan.Metered:
			w = "Waiting · metered connection"
		case inRun && quotaHeld:
			w = "Waiting · data quota used up"
		}
		return with(1, w), tone
	case StPaused:
		if d.Metered {
			return with(2, "Paused · metered connection"), tone
		}
		return with(2, "Paused"), tone
	case StDownloading:
		switch {
		case d.Stage != "":
			return fmt.Sprintf("%d%% · %s", d.StagePct, d.Stage), tone
		case capHeld:
			return with(1, "Data cap reached"), tone
		case quotaHeld:
			return with(1, "Data quota used up"), tone
		case d.Retrying:
			return "Reconnecting…", tone
		case d.Live:
			return "Recording · " + clock(d.Rec), tone
		case d.Size > 0 || d.Kind == "hls" && d.Parts > 0:
			return fmt.Sprintf("%.2f%%", p), tone
		}
		return windowSize(d.Done), tone
	}
	return d.Status, tone
}

// phoneReason is why a download failed, for the line under its status. The status itself is
// the window's short word.
func phoneReason(d *DLView) string {
	switch {
	case d.Status != StError || d.Link == "refreshing":
		return ""
	case d.Link != "":
		return "The link expired: open it again on the computer"
	}
	return d.Error
}

// dlPct is how far a download is, 0…100, as the window counts it (pct in app.js).
func dlPct(d *DLView) float64 {
	switch {
	case d.Status == StCompleted:
		return 100
	case d.Live: // a recording has no end to measure against
		return 0
	case d.Kind == "hls":
		if d.Prog > 0 {
			return math.Min(100, d.Prog*100)
		}
		if d.Parts > 0 {
			return math.Min(100, float64(d.PartsDone)/float64(d.Parts)*100)
		}
	case d.Size > 0:
		return math.Min(100, float64(d.Done)/float64(d.Size)*100)
	}
	return 0
}

// fmtWait is how long until something happens: "25 sec", "3 min" (as in the window, with a non-breaking
// space, so a line doesn't break inside it).
func fmtWait(sec float64) string {
	if sec < 60 {
		return fmt.Sprintf("%d\u00a0sec", int(math.Ceil(sec)))
	}
	return fmt.Sprintf("%d\u00a0min", int(math.Ceil(sec/60)))
}

// windowSize writes a byte count the way the window does ("1.50 GB").
func windowSize(b int64) string {
	if b < 0 {
		return "Unknown"
	}
	u := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(b), 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", b)
	}
	prec := 0
	if v < 10 {
		prec = 2
	} else if v < 100 {
		prec = 1
	}
	return fmt.Sprintf("%.*f %s", prec, v, u[i])
}

// ---------------------------------------------------------------- the window's side

// PhoneView is what Options → Phone shows.
type PhoneView struct {
	On      bool           `json:"on"`
	Port    int            `json:"port"`
	Running bool           `json:"running"`
	Error   string         `json:"error,omitempty"`
	Addrs   []string       `json:"addrs"` // where the page is reachable now (phonenet.go)
	Nets    []PhoneNetView `json:"nets"`  // the networks, and which are marked Public
	Devices []phoneDevVw   `json:"devices"`
}

type phoneDevVw struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Added   time.Time   `json:"added"`
	Seen    time.Time   `json:"seen"`
	Pending bool        `json:"pending"`
	Ask     *phoneAskVw `json:"ask,omitempty"` // a phone using this code waits for the window's answer
}

func (m *Manager) phoneView() PhoneView {
	m.mu.Lock()
	ps := m.settings.Phone
	v := PhoneView{On: ps.On, Port: ps.Port, Addrs: []string{}}
	if v.Port <= 0 {
		v.Port = phonePort
	}
	for _, d := range ps.Devices {
		v.Devices = append(v.Devices, phoneDevVw{ID: d.ID, Name: d.Name, Added: d.Added, Seen: d.Seen, Pending: d.Pending})
	}
	ph := m.phone
	m.mu.Unlock()
	v.Nets = []PhoneNetView{}
	if ph != nil {
		_, v.Running, v.Error = ph.status()
		for _, a := range ph.listening() {
			v.Addrs = append(v.Addrs, a.host())
		}
		v.Nets = ph.netViews(ps.Allow)
		ph.markAsks(v.Devices)
	}
	return v
}

// pairPhone switches phone control on and makes a new pairing code.
func (m *Manager) pairPhone() (code string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// codes shown earlier and never used make way
	keep := m.settings.Phone.Devices[:0]
	for _, d := range m.settings.Phone.Devices {
		if !d.Pending {
			keep = append(keep, d)
		}
	}
	m.settings.Phone.Devices = keep
	if len(keep) >= 20 {
		return "", errors.New("remove a phone first (20 at most)")
	}
	code = newPairCode()
	m.settings.Phone.Devices = append(m.settings.Phone.Devices, PhoneDevice{ID: deviceID(code), Name: "New phone", Key: code, Added: time.Now(), Pending: true})
	if !m.settings.Phone.On {
		m.settings.Phone.On = true
	}
	m.applyPhone()
	m.dirty = true
	return code, nil
}

func (m *Manager) removePhone(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keep := m.settings.Phone.Devices[:0]
	for _, d := range m.settings.Phone.Devices {
		if d.ID != id {
			keep = append(keep, d)
		}
	}
	m.settings.Phone.Devices = keep
	m.dirty = true
}

// phoneH is /api/phone for the window: status, pairing, answering a phone that wants to connect,
// removing phones, and the networks where the page is allowed although they're marked Public.
func (s *Server) phoneH(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, s.m.phoneView())
		return
	}
	var req struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		Addr   string `json:"addr"`
		Token  string `json:"token"`
		Key    string `json:"key"`
		Allow  bool   `json:"allow"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	switch req.Action {
	case "pair":
		code, err := s.m.pairPhone()
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		s.m.phone.sync() // (so the address below is one it listens on)
		v := s.m.phoneView()
		addr := ""
		for _, a := range v.Addrs {
			if a == req.Addr {
				addr = a
			}
		}
		if addr == "" && len(v.Addrs) > 0 {
			addr = v.Addrs[0]
		}
		if addr == "" { // nothing to scan: the page is off on every network here
			s.m.removePhone(deviceID(code))
			writeJSON(w, map[string]any{"error": "the phone page isn't on any network right now", "code": phoneOffWhy(v)})
			return
		}
		link := fmt.Sprintf("http://%s:%d/m#k=%s", addr, v.Port, code)
		qr, err := encodeQR(link)
		svg := ""
		if err == nil {
			svg = qr.SVG()
		}
		writeJSON(w, map[string]any{"code": fmtCode(code), "id": deviceID(code), "url": fmt.Sprintf("http://%s:%d/m", addr, v.Port), "qr": svg})
	case "remove":
		s.m.removePhone(req.ID)
		writeJSON(w, map[string]any{"ok": true})
	case "answer", "allowNet": // only in the window: a program with the secret can't let a phone or a Public network in
		if authOf(r) != authWindow {
			http.Error(w, "only in Zipline's window", 403)
			return
		}
		var err error
		if req.Action == "answer" {
			err = s.m.answerPhone(req.Token, req.Allow)
		} else {
			err = s.m.allowPhoneNet(req.Key, req.Allow)
		}
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "bad request", 400)
	}
}

// phoneOffWhy: why there is no address to pair with, as a code the window has words for.
func phoneOffWhy(v PhoneView) string {
	for _, n := range v.Nets {
		if n.Public && !n.Allowed && n.Here {
			return "public"
		}
	}
	return "nonet"
}

func cap1(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
