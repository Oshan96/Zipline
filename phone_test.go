package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

type phoneClient struct {
	t    *testing.T
	base string
	code string
	skew time.Duration
}

func (c *phoneClient) do(method, path string, body any, mutate func(*http.Request)) (int, string) {
	c.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	full := "/m/api/" + path
	req, _ := http.NewRequest(method, c.base+full, bytes.NewReader(b))
	ts := strconv.FormatInt(time.Now().Add(c.skew).UnixMilli(), 10)
	req.Header.Set("X-ZL-Device", deviceID(c.code))
	req.Header.Set("X-ZL-Time", ts)
	req.Header.Set("X-ZL-Sig", phoneSig(c.code, method, full, ts, b))
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(out))
}

// phoneManagerUnpaired is a manager with phone control on and a code nobody has used. The listener
// serves 127.0.0.1 as if it were a home network, so the tests can reach it.
func phoneManagerUnpaired(t *testing.T) (*Manager, *phoneClient, string) {
	m := newTestManager(t)
	loopbackLAN(m.phone)
	port := freePort(t)
	m.mu.Lock()
	m.settings.Phone.Port = port
	m.mu.Unlock()
	code, err := m.pairPhone()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.phone.stop() })
	for i := 0; i < 100; i++ {
		if _, run, _ := m.phone.status(); run {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return m, &phoneClient{t: t, base: fmt.Sprintf("http://127.0.0.1:%d", port), code: code}, code
}

// phoneManager is a manager with a phone that has been paired and allowed in the window.
func phoneManager(t *testing.T) (*Manager, *phoneClient, string) {
	m, c, code := phoneManagerUnpaired(t)
	allowPhone(t, m, c, "Test phone")
	return m, c, code
}

func TestPhonePairingAndControl(t *testing.T) {
	m, c, code := phoneManagerUnpaired(t)
	if len(code) != 16 || strings.ContainsAny(code, "ILOU") {
		t.Fatalf("code %q", code)
	}
	// the page
	resp, err := http.Get(c.base + "/m")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "connect-src 'self'") || !bytes.Contains(page, []byte("Pair with Zipline")) || bytes.Contains(page, []byte("__TOKEN__")) {
		t.Fatalf("page: %d %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	for _, p := range []string{"/api/settings", "/api/state", "/index.html", "/m/../api/settings"} {
		if r, err := http.Get(c.base + p); err == nil {
			r.Body.Close()
			if r.StatusCode != 404 && r.StatusCode != 301 {
				t.Errorf("%s answered %d on the phone port", p, r.StatusCode)
			}
		}
	}
	// pairing: the first visit only asks; the window allows
	allowPhone(t, m, c, "Pixel 8 · Chrome")
	v := m.phoneView()
	if len(v.Devices) != 1 || v.Devices[0].Pending || v.Devices[0].Name != "Pixel 8 · Chrome" {
		t.Fatalf("devices %+v", v.Devices)
	}
	// add and control a download
	data := randData(2 << 20)
	h := newFileHost(t, data, 0)
	if st, body := c.do("POST", "add", map[string]any{"url": h.URL + "/files/photos.zip", "start": false}, nil); st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("add: %d %s", st, body)
	}
	st, body := c.do("GET", "state", nil, nil)
	var state struct {
		Downloads []phoneDL `json:"downloads"`
	}
	json.Unmarshal([]byte(body), &state)
	if st != 200 || len(state.Downloads) != 1 || state.Downloads[0].Name != "photos.zip" || state.Downloads[0].Size != int64(len(data)) || state.Downloads[0].Status != StQueued {
		t.Fatalf("state: %d %s", st, body)
	}
	id := state.Downloads[0].ID
	c.do("POST", "action", map[string]any{"action": "start", "ids": []string{id}}, nil)
	waitFor(t, m, m.byID[id], StCompleted, 20*time.Second)
	fileIs(t, m.byID[id], data)
	c.do("POST", "limit", map[string]any{"on": true, "kb": 300}, nil)
	m.mu.Lock()
	lim := m.settings.SpeedLimitOn && m.settings.SpeedLimitKB == 300
	m.mu.Unlock()
	if !lim {
		t.Fatal("the speed limit should be on")
	}
	c.do("POST", "action", map[string]any{"action": "remove", "ids": []string{id}}, nil)
	if _, err := os.Stat(filepath.Join(m.byIDPath(id))); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func (m *Manager) byIDPath(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d := m.byID[id]; d != nil {
		return d.path()
	}
	return ""
}

func TestPhoneRequestsMustBeSigned(t *testing.T) {
	m, c, _ := phoneManager(t)
	if st, _ := c.do("POST", "hello", map[string]string{"name": "x"}, nil); st != 200 {
		t.Fatal("pairing failed")
	}
	// wrong key
	bad := &phoneClient{t: t, base: c.base, code: newPairCode()}
	if st, body := bad.do("GET", "state", nil, nil); st != 401 || body != "not paired" {
		t.Fatalf("unknown phone: %d %s", st, body)
	}
	// right device, tampered body
	if st, _ := c.do("POST", "add", map[string]any{"url": "http://example.com/a.zip"}, func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(`{"url":"http://evil.example/x.exe","start":true}`))
		r.ContentLength = -1
	}); st != 401 {
		t.Fatalf("a changed body must be refused: %d", st)
	}
	// replay
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sig := phoneSig(c.code, "GET", "/m/api/state", ts, nil)
	send := func() int {
		req, _ := http.NewRequest("GET", c.base+"/m/api/state", nil)
		req.Header.Set("X-ZL-Device", deviceID(c.code))
		req.Header.Set("X-ZL-Time", ts)
		req.Header.Set("X-ZL-Sig", sig)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if a, b := send(), send(); a != 200 || b != 401 {
		t.Fatalf("replay: %d then %d", a, b)
	}
	// clock far off: refused, with the server's time so the phone can adjust
	c.skew = -10 * time.Minute
	if st, body := c.do("GET", "state", nil, nil); st != 401 || body != "clock" {
		t.Fatalf("old request: %d %s", st, body)
	}
	c.skew = 0
	// guessing gets the address locked out
	for i := 0; i < 25; i++ {
		bad.do("GET", "state", nil, nil)
	}
	if st, _ := bad.do("GET", "state", nil, nil); st != 429 {
		t.Fatalf("expected a lock-out, got %d", st)
	}
	// keys never go to the window, and saving settings keeps the phones
	srv := (&Server{m: m, token: "t"}).routes()
	greq := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/settings", nil)
	greq.Header.Set("X-Token", "t")
	grec := httptest.NewRecorder()
	srv.ServeHTTP(grec, greq)
	var got map[string]any
	json.Unmarshal(grec.Body.Bytes(), &got)
	if ph, _ := got["phone"].(map[string]any); ph == nil || ph["devices"] != nil {
		t.Fatalf("settings carry phone keys: %v", got["phone"])
	}
	m.mu.Lock()
	n := len(m.settings.Phone.Devices)
	m.mu.Unlock()
	b, _ := json.Marshal(got)
	var st Settings
	json.Unmarshal(b, &st)
	apiCall(t, srv, "settings", st)
	m.mu.Lock()
	n2 := len(m.settings.Phone.Devices)
	m.mu.Unlock()
	if n2 != n {
		t.Fatalf("saving Options dropped the paired phones (%d → %d)", n, n2)
	}
	// a removed phone can't connect
	m.removePhone(deviceID(c.code))
	c2 := &phoneClient{t: t, base: c.base, code: c.code}
	time.Sleep(5 * time.Millisecond)
	// (from another address the lock-out above doesn't apply; same address here, so check the reason directly)
	m.phone.mu.Lock()
	m.phone.blocked = map[string]time.Time{}
	m.phone.mu.Unlock()
	if st, _ := c2.do("GET", "state", nil, nil); st != 401 {
		t.Fatalf("removed phone: %d", st)
	}
}

func TestPhoneCodeExpiresUnused(t *testing.T) {
	m, c, _ := phoneManagerUnpaired(t)
	m.mu.Lock()
	m.settings.Phone.Devices[0].Added = time.Now().Add(-16 * time.Minute)
	m.mu.Unlock()
	if st, _ := c.do("POST", "hello", map[string]string{"name": "late"}, nil); st != 401 {
		t.Fatalf("a code nobody used for 15 minutes should stop working: %d", st)
	}
}

func TestPhoneSettingsPersistProtected(t *testing.T) {
	m, c, code := phoneManager(t)
	c.do("POST", "hello", map[string]string{"name": "Phone"}, nil)
	m.save()
	b, _ := os.ReadFile(filepath.Join(m.dataDir, "settings.json"))
	if !bytes.Contains(b, []byte(`"devices"`)) {
		t.Fatal("phones not saved")
	}
	m2 := NewManager(m.dataDir)
	defer close(m2.quit)
	if len(m2.settings.Phone.Devices) != 1 || m2.settings.Phone.Devices[0].Key != code {
		t.Fatalf("reloaded phones: %+v", m2.settings.Phone.Devices)
	}
}

// The page computes the same signature as the app.
func TestPhonePageSignature(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	i, j := strings.Index(phoneHTML, "/*SIG*/"), strings.Index(phoneHTML, "/*/SIG*/")
	if i < 0 || j < 0 {
		t.Fatal("signature code not marked in the page")
	}
	code := "7KQ2-M9XR-4TVB-8HJD"
	body := `{"url":"https://example.com/ünïcode file.zip","start":true}`
	script := phoneHTML[i:j] + fmt.Sprintf(`
console.log(deviceId(%q));
console.log(signature(%q, "POST", "/m/api/add", "1760000000123", %q));
console.log(hex(sha256(utf8("a".repeat(1000)))));
`, code, code, body)
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v %s", err, out)
	}
	lines := strings.Fields(string(out))
	h := sha256hex([]byte(strings.Repeat("a", 1000)))
	if len(lines) != 3 || lines[0] != deviceID(code) || lines[1] != phoneSig(code, "POST", "/m/api/add", "1760000000123", []byte(body)) || lines[2] != h {
		t.Fatalf("page: %v\napp:  %s %s %s", lines, deviceID(code), phoneSig(code, "POST", "/m/api/add", "1760000000123", []byte(body)), h)
	}
}

// statusCases are downloads in many states, with the words the window and the phone both give
// them (see phoneStatus). `at` marks the fake clock the countdowns are counted from.
func statusCases(at time.Time) []struct {
	name       string
	d          DLView
	s          Snapshot
	text, tone string
} {
	ms := at.UnixMilli()
	capBlocked := Snapshot{Cap: CapView{On: true, Blocked: true}}
	capUp := Snapshot{Cap: CapView{On: true, Blocked: true}, Quota: QuotasView{Count: "up"}}
	over := Snapshot{Quota: QuotasView{Show: &QuotaView{Over: true}, Count: "both"}}
	carryOn := Snapshot{Quota: QuotasView{Show: &QuotaView{Over: true, CarryOn: true}, Count: "both"}}
	running := []QView{{ID: "main", Name: "Main queue", Running: true}}
	hours := Snapshot{Plan: PlanView{On: true}}
	metered := Snapshot{Queues: running, Plan: PlanView{Metered: true}}
	inRun := Snapshot{Queues: running}
	none := Snapshot{}
	t := func(n int) *TorrentView { return &TorrentView{Peers: n} }
	return []struct {
		name       string
		d          DLView
		s          Snapshot
		text, tone string
	}{
		{"complete", DLView{Status: StCompleted, Size: 100, Done: 100}, none, "Complete", "done"},
		{"verified", DLView{Status: StCompleted, Check: &Checksum{State: "ok"}}, none, "Complete · verified", "done"},
		{"damaged", DLView{Status: StCompleted, Check: &Checksum{State: "bad"}}, none, "Complete · damaged", "err"},
		{"scanning", DLView{Status: StCompleted, ScanState: "scanning"}, none, "Complete · scanning", "done"},
		{"threat", DLView{Status: StCompleted, ScanState: "threat"}, none, "Complete · threat found", "err"},
		{"checking", DLView{Status: StCompleted, Stage: "Checking", StagePct: 40}, none, "Complete · checking 40%", "done"},
		{"paused halfway", DLView{Status: StPaused, Size: 1000, Done: 500}, none, "50.00% · Paused", "paused"},
		{"paused", DLView{Status: StPaused}, none, "Paused", "paused"},
		{"metered pause", DLView{Status: StPaused, Metered: true}, none, "Paused · metered connection", "paused"},
		{"queued", DLView{Status: StQueued}, none, "Queued", "paused"},
		{"queued with progress", DLView{Status: StQueued, Size: 1000, Done: 250}, none, "25.0% · Queued", "paused"},
		{"waiting for a slot", DLView{Status: StQueued, Waiting: true}, none, "Waiting for a free slot", "paused"},
		{"waiting for free hours", DLView{Status: StQueued, Queue: planQueueID}, hours, "Waiting for free hours", "paused"},
		{"waiting, metered", DLView{Status: StQueued, Queue: "main"}, metered, "Waiting · metered connection", "paused"},
		{"waiting, quota", DLView{Status: StQueued, Queue: "main"}, Snapshot{Queues: running, Quota: over.Quota}, "Waiting · data quota used up", "paused"},
		{"queue not running", DLView{Status: StQueued, Queue: "main"}, Snapshot{Queues: []QView{{ID: "main"}}, Quota: over.Quota}, "Queued", "paused"},
		{"connecting", DLView{Status: StConnecting}, inRun, "Connecting…", ""},
		{"retrying soon", DLView{Status: StConnecting, RetryAt: ms + 24500}, none, "Retrying in 25\u00a0sec", ""},
		{"retrying later", DLView{Status: StConnecting, RetryAt: ms + 150000}, none, "Retrying in 3\u00a0min", ""},
		{"downloading", DLView{Status: StDownloading, Size: 10000, Done: 4520}, none, "45.20%", ""},
		{"downloading, size unknown", DLView{Status: StDownloading, Done: 1572864}, none, "1.50 MB", ""},
		{"downloading, bytes only", DLView{Status: StDownloading, Done: 700}, none, "700 B", ""},
		{"reconnecting", DLView{Status: StDownloading, Size: 100, Done: 5, Retrying: true}, none, "Reconnecting…", ""},
		{"converting", DLView{Status: StDownloading, Stage: "Joining the parts", StagePct: 35}, none, "35% · Joining the parts", ""},
		{"stream", DLView{Status: StDownloading, Kind: "hls", Parts: 8, PartsDone: 4}, none, "50.00%", ""},
		{"stream, with progress", DLView{Status: StDownloading, Kind: "hls", Parts: 8, PartsDone: 4, Prog: 0.3}, none, "30.00%", ""},
		{"recording", DLView{Status: StDownloading, Kind: "hls", Parts: 9, PartsDone: 9, Live: true, Rec: 754.4}, none, "Recording · 12:34", ""},
		{"recording, long", DLView{Status: StDownloading, Kind: "hls", Parts: 900, PartsDone: 899, Live: true, Rec: 3754}, none, "Recording · 1:02:34", ""},
		{"recording, saving", DLView{Status: StDownloading, Kind: "hls", Live: true, Rec: 60, Stage: "Converting to MP4", StagePct: 40}, none, "40% · Converting to MP4", ""},
		{"recording, data cap", DLView{Status: StDownloading, Kind: "hls", Parts: 3, PartsDone: 3, Live: true, Rec: 12}, capBlocked, "Data cap reached", "paused"},
		{"data cap", DLView{Status: StDownloading, Size: 1000, Done: 120}, capBlocked, "12.0% · Data cap reached", "paused"},
		{"data cap, uploads only", DLView{Status: StDownloading, Size: 1000, Done: 120}, capUp, "12.00%", ""},
		{"data cap, no progress yet", DLView{Status: StDownloading}, capBlocked, "Data cap reached", "paused"},
		{"data quota", DLView{Status: StDownloading, Size: 1000, Done: 120}, over, "12.0% · Data quota used up", "paused"},
		{"data quota, carrying on", DLView{Status: StDownloading, Size: 1000, Done: 120}, carryOn, "12.00%", ""},
		{"error", DLView{Status: StError, Error: "Server said no"}, none, "Error", "err"},
		{"error, trying again", DLView{Status: StError, TryAgain: ms + 200000}, none, "Error · retrying in 4\u00a0min", "err"},
		{"link expired", DLView{Status: StError, Link: "expired"}, none, "Link expired", "err"},
		{"link expired, part done", DLView{Status: StError, Link: "expired", Size: 300, Done: 100}, none, "33.3% · Link expired", "err"},
		{"fresh link", DLView{Status: StError, Link: "refreshing"}, none, "Getting a fresh link…", "paused"},
		{"torrent, file list", DLView{Status: StDownloading, Torrent: &TorrentView{NeedMeta: true}}, none, "Getting the file list…", ""},
		{"torrent, file list, part", DLView{Status: StDownloading, Torrent: &TorrentView{NeedMeta: true, MetaPct: 40}}, none, "Getting the file list… 40%", ""},
		{"torrent, no peers", DLView{Status: StDownloading, Size: 1000, Done: 100, Torrent: t(0)}, none, "10.00% · finding peers", ""},
		{"torrent, one peer", DLView{Status: StDownloading, Size: 1000, Done: 100, Torrent: t(1)}, none, "10.00% · 1 peer", ""},
		{"torrent, peers", DLView{Status: StDownloading, Size: 1000, Done: 100, Torrent: t(7)}, none, "10.00% · 7 peers", ""},
		{"torrent, cap", DLView{Status: StDownloading, Size: 1000, Done: 100, Torrent: t(7)}, capBlocked, "10.0% · Data cap reached", "paused"},
		{"torrent, checking", DLView{Status: StDownloading, Size: 1000, Done: 100, Torrent: &TorrentView{Peers: 3, Checking: true, CheckPct: 37}}, none, "Checking files · 37%", ""},
		{"torrent, checking at the cap", DLView{Status: StDownloading, Torrent: &TorrentView{Checking: true}}, capBlocked, "Checking files · 0%", "paused"},
		{"torrent, sharing", DLView{Status: StCompleted, Torrent: &TorrentView{Seeding: true}}, none, "Complete · sharing", "done"},
		{"torrent, sharing paused", DLView{Status: StCompleted, Torrent: &TorrentView{Seeding: true, Held: true}}, none, "Complete · sharing paused", "done"},
		{"torrent, files missing", DLView{Status: StCompleted, Torrent: &TorrentView{NoShare: &ShareError{Why: "missing", File: "a.bin"}}}, none, "Complete · files missing", "done"},
		{"torrent, hidden part gone", DLView{Status: StCompleted, Torrent: &TorrentView{NoShare: &ShareError{Why: "parts"}}}, none, "Complete · files missing", "done"},
		{"torrent, files changed", DLView{Status: StCompleted, Torrent: &TorrentView{NoShare: &ShareError{Why: "changed", File: "a.bin"}}}, none, "Complete · files changed", "done"},
		{"torrent, sharing off", DLView{Status: StCompleted, Torrent: &TorrentView{NoShare: &ShareError{Why: "off"}}}, none, "Complete", "done"},
		{"torrent, sharing waits for a normal connection", DLView{Status: StCompleted, Metered: true, Torrent: &TorrentView{}}, none, "Complete · sharing later", "done"},
		{"torrent, threat", DLView{Status: StCompleted, ScanState: "threat", Torrent: &TorrentView{Seeding: true}}, none, "Complete · threat found", "err"},
		{"torrent, done", DLView{Status: StCompleted, Torrent: &TorrentView{}}, none, "Complete", "done"},
	}
}

func TestPhoneStatusWords(t *testing.T) {
	at := time.UnixMilli(1760000000000)
	for _, c := range statusCases(at) {
		text, tone := phoneStatus(&c.d, &c.s, at)
		if text != c.text || tone != c.tone {
			t.Errorf("%s: %q (%s), want %q (%s)", c.name, text, tone, c.text, c.tone)
		}
	}
}

// The phone says what the window's list says: its statusText and row colours (app.js) give the
// same words for the same downloads. If this fails after a change to the window's wording,
// change phoneStatus to match.
func TestPhoneStatusMatchesWindow(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	cut := func(from, to string) string {
		i := strings.Index(appJS, from)
		j := strings.Index(appJS[max(i, 0):], to)
		if i < 0 || j < 0 {
			t.Fatalf("app.js has changed shape: no %q … %q", from, to)
		}
		return appJS[i : i+j]
	}
	tone := cut("const stCls = ", "\n") // (the colour the list gives a row's status)
	at := time.UnixMilli(1760000000000)
	type one struct {
		D DLView   `json:"d"`
		S Snapshot `json:"s"`
	}
	var in []one
	cases := statusCases(at)
	for _, c := range cases {
		c.s.Downloads = nil
		in = append(in, one{c.d, c.s})
	}
	data, _ := json.Marshal(in)
	script := "Date.now = () => " + strconv.FormatInt(at.UnixMilli(), 10) + ";\nlet S = {};\n" + i18nJS + "\n" +
		cut("const isHLS = ", "// ------------------------------------------------------------------ icons") +
		cut("const capHeld = ", "const quotaPeriod") +
		"const toneOf = d => { " + tone + " return stCls; };\n" +
		"const out = " + string(data) + ".map(c => { S = c.s; S.queues = S.queues || []; return [statusText(c.d), toneOf(c.d)]; });\n" +
		"console.log(JSON.stringify(out));\n"
	js := filepath.Join(t.TempDir(), "status.js") // (a file: the script is too long for one command-line argument)
	if err := os.WriteFile(js, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, js)
	cmd.Env = append(os.Environ(), "LC_ALL=en_US.UTF-8", "LANG=en_US.UTF-8", "LANGUAGE=en") // (the words and numbers are English)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v %s", err, out)
	}
	var got [][2]string
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node said %q (%v)", out, err)
	}
	for i, c := range cases {
		text, tone := phoneStatus(&c.d, &c.s, at)
		if got[i][0] != text || got[i][1] != tone {
			t.Errorf("%s: window says %q (%s), phone says %q (%s)", c.name, got[i][0], got[i][1], text, tone)
		}
	}
}

// The state the phone gets carries the status words, the colour and why a download failed.
func TestPhoneStateStatusWords(t *testing.T) {
	m, c, _ := phoneManager(t)
	c.do("POST", "hello", map[string]string{"name": "Phone"}, nil)
	h := newFileHost(t, randData(1<<20), 0)
	c.do("POST", "add", map[string]any{"url": h.URL + "/files/a.zip", "start": false}, nil)
	read := func() phoneDL {
		t.Helper()
		time.Sleep(3 * time.Millisecond) // (a request signed in the same millisecond as the last would be a replay)
		_, body := c.do("GET", "state", nil, nil)
		var st struct {
			Downloads []phoneDL `json:"downloads"`
		}
		if json.Unmarshal([]byte(body), &st) != nil || len(st.Downloads) != 1 {
			t.Fatalf("state: %s", body)
		}
		return st.Downloads[0]
	}
	set := func(f func(d *Download)) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, d := range m.list {
			f(d)
		}
	}
	if d := read(); d.Text != "Queued" || d.Tone != "paused" || d.Note != "" {
		t.Fatalf("queued: %+v", d)
	}
	set(func(d *Download) { d.Status, d.Error = StError, "The server said no (403)" })
	if d := read(); d.Text != "Error" || d.Tone != "err" || d.Note != "The server said no (403)" {
		t.Fatalf("failed: %+v", d)
	}
	set(func(d *Download) { d.Link = "expired" })
	if d := read(); d.Text != "Link expired" || !strings.Contains(d.Note, "open it again on the computer") {
		t.Fatalf("link expired: %+v", d)
	}
	set(func(d *Download) { d.Link = "refreshing" })
	if d := read(); d.Text != "Getting a fresh link…" || d.Tone != "paused" || d.Note != "" {
		t.Fatalf("fresh link: %+v", d)
	}
	set(func(d *Download) { d.Link, d.Error, d.Status = "", "", StCompleted })
	if d := read(); d.Text != "Complete" || d.Tone != "done" || d.Prog != 1 {
		t.Fatalf("complete: %+v", d)
	}
	set(func(d *Download) { d.Status = StPaused })
	if d := read(); d.Text != "Paused" || d.Tone != "paused" {
		t.Fatalf("paused: %+v", d)
	}
}

// The page's script is valid, and its sheets are dialogs that Escape closes.
func TestPhonePageScriptParses(t *testing.T) {
	for _, want := range []string{`role="dialog"`, `aria-modal="true"`, `"Escape"`, `["errors", "sb.errors"`} {
		if !strings.Contains(phoneHTML, want) {
			t.Errorf("the page has no %s", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	i, j := strings.Index(phoneHTML, "<script>"), strings.LastIndex(phoneHTML, "</script>")
	if i < 0 || j < i {
		t.Fatal("no script in the page")
	}
	file := filepath.Join(t.TempDir(), "m.js")
	os.WriteFile(file, []byte(phoneHTML[i+len("<script>"):j]), 0o644)
	if out, err := exec.Command(node, "--check", file).CombinedOutput(); err != nil {
		t.Fatalf("the page's script doesn't parse: %v %s", err, out)
	}
}
