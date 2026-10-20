package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The browser extension's side of the app, beyond handing over links:
//   - Downloads taken over from the browser come with the browser's own request (its headers, and a
//     form's body for a POST) and what the reply said (knownFile), so Zipline makes the same request
//     once, without asking the server first. Closing the Add dialog hands the download back: the
//     extension downloads it in the browser after all (a "give back" job).
//   - Links from "Download all links" come without cookies. When you press Download in the picker,
//     the app asks the extension for the cookies of the chosen files' sites only (a cookie job) and
//     waits up to ten seconds for them.
//   - Downloads from private windows are private: never written to the saved list, and taken off the
//     list a moment after they finish (the file stays).
//   - The extension's settings live here (Options → Browser) and go to it with each ping.
// Jobs go to the extension with its ping reply, and sooner through /api/ext/jobs, which it calls
// while a dialog that may need it is open (the reply holds until there's a job, up to 20 seconds).

// ---------------------------------------------------------------- settings

// BrowserSettings are the extension's settings, kept in the app (Options → Browser).
type BrowserSettings struct {
	Takeover    bool     `json:"takeover"`    // take over downloads the browser starts
	MinKB       int      `json:"minKB"`       // ...of at least this size
	SkipTypes   string   `json:"skipTypes"`   // file types left to the browser (extensions, space separated)
	VideoButton bool     `json:"videoButton"` // the Download button over video players
	NoButton    []string `json:"noButton"`    // sites where the button is hidden
}

func defaultBrowser() BrowserSettings {
	return BrowserSettings{MinKB: 256, SkipTypes: "html htm php asp aspx json xml js css", VideoButton: true}
}

func (b *BrowserSettings) normalize() {
	b.MinKB = min(max(b.MinKB, 0), 10<<20)
	b.SkipTypes = strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(b.SkipTypes, ".", " "))), " ")
	hosts := []string{} // (a list, never null, for the extension)
	for _, h := range b.NoButton {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" && len(hosts) < 500 {
			hosts = append(hosts, h)
		}
	}
	b.NoButton = hosts
}

// ---------------------------------------------------------------- what the browser already knows

// knownFile is what the browser's own reply said about a download it hands over, so the Add
// dialog doesn't ask the server again: the download's own request is the only one Zipline makes.
type knownFile struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"` // -1: the reply didn't say
	Mime      string `json:"mime"`
	FinalURL  string `json:"finalUrl"`
	Resumable bool   `json:"resumable"` // the reply said Accept-Ranges: bytes
	ETag      string `json:"etag"`
	LastMod   string `json:"lastMod"`
}

// result stands in for a probe of u, or is nil when the file has to be read after all (a stream,
// a torrent or a metalink is understood from what's in it).
func (k *knownFile) result(u string) *probeResult {
	if k == nil || strings.TrimSpace(k.Name) == "" || isHLSMime(k.Mime) || looksLikeTorrent(k.Mime, k.Name, k.Size) || isMetalinkMime(k.Mime, k.Name) || looksLikeHLS(u) {
		return nil
	}
	pr := &probeResult{body: http.NoBody, size: -1, name: cleanFileName(k.Name), finalURL: u, mime: k.Mime, etag: k.ETag, lastMod: k.LastMod}
	if k.Size > 0 {
		pr.size, pr.resumable = k.Size, k.Resumable
	}
	if f, err := url.Parse(k.FinalURL); err == nil && (f.Scheme == "http" || f.Scheme == "https") {
		pr.finalURL = k.FinalURL
	}
	return pr
}

// probeKnown probes d's file, unless the browser already said what it is.
func probeKnown(ctx context.Context, d *Download, k *knownFile) (*probeResult, error) {
	if pr := k.result(d.URL); pr != nil {
		return pr, nil
	}
	return probe(ctx, d, d.URL, 0, -1)
}

// requestBody is how d's request to u goes: a form's download repeats the browser's POST, to the
// address it was sent to (other addresses, and a redirect that turned it into a GET, are plain GETs).
// A server that doesn't do byte ranges on a POST answers with the whole file: it's then downloaded
// over one connection, and a pause starts it again from the beginning.
func requestBody(d *Download, u string) (string, io.Reader) {
	if d.Method == "POST" && u == d.URL {
		return "POST", bytes.NewReader(d.Body)
	}
	return "GET", nil
}

// resend is req made ready to send again (a login asked for, another name tried): a form's body is
// read afresh.
func resend(req *http.Request, ctx context.Context) *http.Request {
	r := req.Clone(ctx)
	if req.GetBody != nil {
		r.Body, _ = req.GetBody()
	}
	return r
}

// noBrowserHeader: headers of the browser's request that aren't about what to fetch, or that
// Zipline sets itself (it asks for the file uncompressed and in pieces).
var noBrowserHeader = map[string]bool{"Host": true, "Connection": true, "Keep-Alive": true, "Content-Length": true, "Transfer-Encoding": true, "Te": true,
	"Trailer": true, "Upgrade": true, "Proxy-Authorization": true, "Proxy-Connection": true, "Accept-Encoding": true, "Range": true, "If-Range": true,
	"If-None-Match": true, "If-Modified-Since": true, "If-Match": true, "If-Unmodified-Since": true, "Expect": true, "Priority": true}

// cleanBrowserHeaders keeps the browser's request headers worth repeating.
func cleanBrowserHeaders(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		k = http.CanonicalHeaderKey(strings.TrimSpace(k))
		if k == "" || noBrowserHeader[k] || strings.HasPrefix(k, ":") || strings.ContainsAny(k+v, "\r\n") || len(out) >= 40 || len(v) > 16<<10 {
			continue
		}
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------- private downloads

// privateLinger is how long a finished private download stays in the list: long enough for the
// window to say it finished; its "Open" buttons keep working a while longer.
var privateLinger = 5 * time.Second

// forgetPrivate takes a finished private download off the list in a moment (the file stays).
// Caller holds m.mu.
func (m *Manager) forgetPrivate(d *Download) {
	time.AfterFunc(privateLinger, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if d.Status != StCompleted || d.running || d.Provisional {
			return // (downloaded again meanwhile, or still in its Add dialog)
		}
		out := m.list[:0]
		for _, x := range m.list {
			if x != d {
				out = append(out, x)
			}
		}
		m.list = out
		m.dirty = true
		time.AfterFunc(10*time.Minute, func() {
			m.mu.Lock()
			if m.byID[d.ID] == d && d.Status == StCompleted && !d.running {
				delete(m.byID, d.ID)
			}
			m.mu.Unlock()
		})
	})
}

// ---------------------------------------------------------------- jobs for the extension

// takenDL is a download taken from the browser while its Add dialog is open.
type takenDL struct {
	job giveBackJob
	at  time.Time
}

// giveBackJob asks the extension to download a file in the browser after all.
type giveBackJob struct {
	ID       string            `json:"id"`
	URL      string            `json:"url"`
	Method   string            `json:"method,omitempty"`
	Body     []byte            `json:"body,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Filename string            `json:"filename,omitempty"`
	Ctx      json.RawMessage   `json:"ctx,omitempty"` // the window it came from, as the extension described it
}

// cookieAsk asks the extension for the cookies of some sites (one address each).
type cookieAsk struct {
	ID   string          `json:"id"`
	URLs []string        `json:"urls"`
	Ctx  json.RawMessage `json:"ctx,omitempty"`
}

type extJobs struct {
	mu       sync.Mutex
	taken    map[string]*takenDL  // by prompt
	pickers  map[string]time.Time // links pickers that may ask for cookies, by grab job
	giveBack []giveBackJob
	asks     []cookieAsk
	answers  map[string]chan map[string]string
	kick     chan struct{} // closed when there's something new for the extension
}

// openFor is how long the app waits for a dialog that never said it closed (a window closed meanwhile).
const openFor = 30 * time.Minute

var extJobsInit sync.Mutex

func (m *Manager) extq() *extJobs {
	extJobsInit.Lock()
	defer extJobsInit.Unlock()
	if m.ext == nil {
		m.ext = &extJobs{taken: map[string]*takenDL{}, pickers: map[string]time.Time{}, answers: map[string]chan map[string]string{}, kick: make(chan struct{})}
	}
	return m.ext
}

// wake tells a waiting /api/ext/jobs there's news. Caller holds q.mu.
func (q *extJobs) wake() {
	close(q.kick)
	q.kick = make(chan struct{})
}

// waiting: a dialog is open that may need the extension soon, or a job is ready. Caller holds q.mu.
func (q *extJobs) waiting(now time.Time) bool {
	for id, t := range q.taken {
		if now.Sub(t.at) > openFor {
			delete(q.taken, id)
		}
	}
	for id, t := range q.pickers {
		if now.Sub(t) > openFor {
			delete(q.pickers, id)
		}
	}
	return len(q.taken)+len(q.pickers)+len(q.giveBack)+len(q.asks) > 0
}

// take hands out the jobs ready for the extension (each once), and whether to keep checking.
func (q *extJobs) take() (gb []giveBackJob, asks []cookieAsk, wait bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	gb, asks = q.giveBack, q.asks
	q.giveBack, q.asks = nil, nil
	return gb, asks, q.waiting(time.Now())
}

// jobsReply adds the extension's jobs to a ping reply.
func (m *Manager) jobsReply(res map[string]any) {
	gb, asks, wait := m.extq().take()
	if len(gb) > 0 {
		res["giveback"] = gb
	}
	if len(asks) > 0 {
		res["cookies"] = asks
	}
	res["wait"] = wait
}

// noteTaken remembers a download taken from the browser while its Add dialog (prompt id) is open.
func (m *Manager) noteTaken(id string, job giveBackJob) {
	q := m.extq()
	q.mu.Lock()
	job.ID = id
	q.taken[id] = &takenDL{job: job, at: time.Now()}
	q.mu.Unlock()
}

// browserClosed: the window is done with something that came from the browser. A download's Add
// dialog closed without adding it goes back to the browser.
func (m *Manager) browserClosed(promptID, jobID string, added bool) bool {
	q := m.extq()
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.pickers, jobID)
	t := q.taken[promptID]
	if t == nil {
		return false
	}
	delete(q.taken, promptID)
	if !added {
		q.giveBack = append(q.giveBack, t.job)
		q.wake()
	}
	return true
}

func (m *Manager) pickerOpen(jobID string) {
	q := m.extq()
	q.mu.Lock()
	q.pickers[jobID] = time.Now()
	q.mu.Unlock()
}

// askCookies asks the extension for the cookies of the sites of urls, waiting up to wait for its
// answer (host → Cookie header). ok is false when it didn't answer.
func (m *Manager) askCookies(ctx context.Context, urls []string, extCtx json.RawMessage, wait time.Duration) (map[string]string, bool) {
	q := m.extq()
	id, ch := newID(), make(chan map[string]string, 1)
	q.mu.Lock()
	q.asks = append(q.asks, cookieAsk{ID: id, URLs: urls, Ctx: extCtx})
	q.answers[id] = ch
	q.wake()
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		delete(q.answers, id)
		for i, a := range q.asks { // (never handed out: nobody will answer it now)
			if a.ID == id {
				q.asks = append(q.asks[:i], q.asks[i+1:]...)
				break
			}
		}
		q.mu.Unlock()
	}()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case c := <-ch:
		return c, true
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, false
}

// ---------------------------------------------------------------- the picker's cookies

// cookieWait is how long the picker's Download waits for the browser's cookies.
var cookieWait = 10 * time.Second

// browserCookies gets the browser's cookies for the chosen files of links that came from the
// browser, for their sites only, and says "nocookies" when the extension didn't answer in time.
func (s *Server) browserCookies(ctx context.Context, req *GrabAddReq) string {
	j := s.m.grab.Job(req.JobID)
	if j == nil || !j.fromBrowser() {
		return ""
	}
	j.mu.Lock()
	byURL := map[string]bool{}
	for _, f := range j.Files {
		byURL[f.URL] = true
	}
	extCtx := j.extCtx
	j.mu.Unlock()
	var files []GrabFile
	for _, u := range req.URLs {
		if byURL[u] {
			files = append(files, GrabFile{URL: u})
		}
	}
	have := j.fileCookies(files) // ("Scan this site" sent the site's own sign-in already)
	seen, urls := map[string]bool{}, []string{}
	for _, f := range files {
		u, err := url.Parse(f.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || have[u.Hostname()] != "" || seen[u.Hostname()] || len(urls) >= 40 {
			continue
		}
		seen[u.Hostname()] = true
		urls = append(urls, f.URL)
	}
	if len(urls) == 0 {
		return ""
	}
	s.m.mu.Lock()
	active := s.m.extActive()
	s.m.mu.Unlock()
	got, ok := map[string]string(nil), false
	if active {
		got, ok = s.m.askCookies(ctx, urls, extCtx, cookieWait)
	}
	if !ok {
		req.note = "Added without the browser's sign-in (the extension didn't answer)"
		return "nocookies"
	}
	req.cookies = map[string]string{}
	for h, c := range got {
		if h = strings.ToLower(h); seen[h] && c != "" {
			req.cookies[h] = c
		}
	}
	return ""
}

// fromBrowser: the job's links came from the browser (all of them, or a page it read for a scan).
func (j *GrabJob) fromBrowser() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.FromPage {
		return true
	}
	for _, c := range j.scan.collects {
		if c.State == "done" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- handlers

func (s *Server) extRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/ext/jobs", s.ext(s.extJobs))
	mux.HandleFunc("/api/ext/cookies", s.ext(s.extCookies))
	mux.HandleFunc("/api/ext/settings", s.ext(s.extSettings))
	mux.HandleFunc("/api/browser/closed", s.auth(s.browserClosedH))
	mux.HandleFunc("/api/theme", s.auth(s.themeH))
}

// extJobs answers when there's a job for the extension, or after wait seconds (at most 20, under
// the browser's 30-second limit for an extension's request).
func (s *Server) extJobs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Wait int `json:"wait"`
	}
	if r.Method != "POST" || readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	q := s.m.extq()
	t := time.NewTimer(time.Duration(min(max(req.Wait, 0), 20)) * time.Second)
	defer t.Stop()
	for more := true; more; {
		q.mu.Lock()
		ready, open, kick := len(q.giveBack)+len(q.asks) > 0, q.waiting(time.Now()), q.kick
		q.mu.Unlock()
		if ready || !open {
			break
		}
		select {
		case <-kick:
		case <-t.C:
			more = false
		case <-r.Context().Done():
			more = false
		}
	}
	res := map[string]any{"ok": true}
	s.m.jobsReply(res)
	writeJSON(w, res)
}

// extCookies is the extension's answer to a cookie job.
func (s *Server) extCookies(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Job     string            `json:"job"`
		Cookies map[string]string `json:"cookies"`
	}
	if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	q := s.m.extq()
	q.mu.Lock()
	ch := q.answers[req.Job]
	q.mu.Unlock()
	if ch != nil {
		select {
		case ch <- req.Cookies:
		default:
		}
	}
	writeJSON(w, map[string]any{"ok": ch != nil})
}

// extSettings changes the extension's settings from its popup; the reply holds them all.
func (s *Server) extSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Takeover    *bool    `json:"takeover"`
		MinKB       *int     `json:"minKB"`
		SkipTypes   *string  `json:"skipTypes"`
		VideoButton *bool    `json:"videoButton"`
		NoButton    []string `json:"noButton"` // the whole list (absent: unchanged)
	}
	if r.Method != "POST" || readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	s.m.mu.Lock()
	b := &s.m.settings.Browser
	if req.Takeover != nil {
		b.Takeover = *req.Takeover
	}
	if req.MinKB != nil {
		b.MinKB = *req.MinKB
	}
	if req.SkipTypes != nil {
		b.SkipTypes = *req.SkipTypes
	}
	if req.VideoButton != nil {
		b.VideoButton = *req.VideoButton
	}
	if req.NoButton != nil {
		b.NoButton = req.NoButton
	}
	b.normalize()
	out := *b
	s.m.dirty = true
	s.m.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "browser": out})
}

// browserClosedH: the window closed an Add dialog for a download taken from the browser (added or
// not), or a picker of links from the browser.
func (s *Server) browserClosedH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string `json:"id"`  // the prompt
		Job   string `json:"job"` // the picker's grab job
		Added bool   `json:"added"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	back := s.m.browserClosed(req.ID, req.Job, req.Added) && !req.Added
	writeJSON(w, map[string]any{"ok": true, "back": back})
}

// themeH keeps the window's theme, which the extension's popup follows.
func (s *Server) themeH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Theme string `json:"theme"`
	}
	if readJSON(r, &req) != nil || (req.Theme != "" && req.Theme != "light" && req.Theme != "dark") {
		http.Error(w, "bad request", 400)
		return
	}
	s.m.mu.Lock()
	if s.m.settings.Theme != req.Theme {
		s.m.settings.Theme = req.Theme
		s.m.dirty = true
	}
	s.m.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}
