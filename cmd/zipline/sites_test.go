package main

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSiteMatch(t *testing.T) {
	u := func(s string) *url.URL { x, _ := url.Parse(s); return x }
	cases := []struct {
		rule, link string
		want       bool
	}{
		{"example.com", "https://example.com/a.zip", true},
		{"example.com", "https://cdn.example.com/a.zip", true},
		{"*.example.com", "https://cdn.example.com/a.zip", true},
		{"example.com", "https://notexample.com/a.zip", false},
		{"https://Example.com/", "http://example.com/x", true},
		{"example.com/private", "https://example.com/private/a.zip", true},
		{"example.com/private", "https://example.com/privateer/a.zip", false},
		{"example.com/private", "https://example.com/public/a.zip", false},
		{"example.com:8080", "http://example.com:8080/a", true},
		{"example.com:8080", "http://example.com/a", false},
		{"ftp.example.com:21", "ftp://ftp.example.com/a", true},
		{"", "https://example.com/", false},
	}
	for _, c := range cases {
		if _, got := siteMatch(c.rule, u(c.link)); got != c.want {
			t.Errorf("%q vs %q: got %v", c.rule, c.link, got)
		}
	}
	// the most specific rule wins
	rules := []SiteRule{{Site: "example.com", MaxConns: 8}, {Site: "dl.example.com", MaxConns: 2}, {Site: "dl.example.com/slow", MaxConns: 1}}
	siteRules.Store(&rules)
	defer siteRules.Store(&[]SiteRule{})
	for link, want := range map[string]int{"https://example.com/a": 8, "https://dl.example.com/a": 2, "https://dl.example.com/slow/a": 1, "https://other.org/a": 0} {
		if got := siteConns(link); got != want {
			t.Errorf("%s: %d connections, want %d", link, got, want)
		}
	}
}

// siteServer needs a login under /private/ and counts how many connections run at once.
type siteServer struct {
	*httptest.Server
	data         []byte
	active, peak int32
	unauthorized int32
}

func newSiteServer(t *testing.T, size int, perConn int) *siteServer {
	s := &siteServer{data: make([]byte, size)}
	rand.Read(s.data)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/private/") {
			if u, p, ok := r.BasicAuth(); !ok || u != "alice" || p != "s3cret" {
				atomic.AddInt32(&s.unauthorized, 1)
				w.Header().Set("WWW-Authenticate", `Basic realm="files"`)
				http.Error(w, "login needed", 401)
				return
			}
		}
		n := atomic.AddInt32(&s.active, 1)
		defer atomic.AddInt32(&s.active, -1)
		for {
			p := atomic.LoadInt32(&s.peak)
			if n <= p || atomic.CompareAndSwapInt32(&s.peak, p, n) {
				break
			}
		}
		http.ServeContent(&slowWriter{w: w, rate: perConn, start: time.Now()}, r, "f.bin", time.Now(), bytes.NewReader(s.data))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestSiteLoginsAndConnectionLimit(t *testing.T) {
	srv := newSiteServer(t, 3<<20, 2<<20)
	m := newTestManager(t)
	m.settings.Conns = 16

	// no saved login: refused
	t0 := time.Now()
	d := m.Add(AddReq{URL: srv.URL + "/private/a.bin", Start: true})
	waitFor(t, m, d, StError, 10*time.Second)
	m.mu.Lock()
	msg := d.Error
	m.mu.Unlock()
	if !strings.Contains(msg, "Options → Sites") || time.Since(t0) > 2*time.Second {
		t.Fatalf("a login error should be reported at once and say where to fix it: %q after %v", msg, time.Since(t0))
	}

	// saved login and a 2-connection limit for this site
	m.mu.Lock()
	m.settings.Sites = []SiteRule{{Site: "127.0.0.1", MaxConns: 2, User: "alice", Pass: "s3cret"}}
	m.applySites()
	m.mu.Unlock()
	atomic.StoreInt32(&srv.peak, 0)
	d2 := m.Add(AddReq{URL: srv.URL + "/private/b.bin", Start: true})
	waitFor(t, m, d2, StCompleted, 20*time.Second)
	if b, _ := os.ReadFile(filepath.Join(d2.Dir, d2.FileName)); !bytes.Equal(b, srv.data) {
		t.Fatal("downloaded file differs")
	}
	if p := atomic.LoadInt32(&srv.peak); p > 2 || p < 1 {
		t.Fatalf("site limit 2, but %d connections ran at once", p)
	}

	// without the limit it uses many connections again
	m.mu.Lock()
	m.settings.Sites = []SiteRule{{Site: "127.0.0.1", User: "alice", Pass: "s3cret"}}
	m.applySites()
	m.mu.Unlock()
	atomic.StoreInt32(&srv.peak, 0)
	d3 := m.Add(AddReq{URL: srv.URL + "/private/c.bin", Start: true})
	waitFor(t, m, d3, StCompleted, 20*time.Second)
	if p := atomic.LoadInt32(&srv.peak); p <= 2 {
		t.Fatalf("expected more than 2 connections without a limit, got %d", p)
	}
}

func TestSiteLoginFTP(t *testing.T) {
	data := make([]byte, 256<<10)
	rand.Read(data)
	srv := newTestFTP(t, data, 0, false)
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Sites = []SiteRule{{Site: "127.0.0.1", User: "bob", Pass: "pw"}}
	m.applySites()
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.url("/pub/file.bin"), Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	if u, _ := srv.lastUser.Load().(string); u != "bob" {
		t.Fatalf("FTP logged in as %q, want the saved login", u)
	}
}

func TestSavedPasswordsStayPrivate(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	m.mu.Lock()
	m.settings.Sites = []SiteRule{{Site: "files.example", User: "alice", Pass: "s3cret!"}}
	m.settings.Proxy.Pass = "proxy-pw"
	m.mu.Unlock()
	m.save()
	raw, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if runtime.GOOS == "windows" {
		if bytes.Contains(raw, []byte("s3cret!")) || !bytes.Contains(raw, []byte("enc:v1:")) { // (sealed with DPAPI: vault_windows.go)
			t.Fatal("password stored without encryption")
		}
	} else if st, _ := os.Stat(filepath.Join(dir, "settings.json")); st.Mode().Perm() != 0o600 {
		t.Fatalf("settings file mode %v, want 0600", st.Mode().Perm())
	}
	m2 := NewManager(dir)
	if m2.settings.Sites[0].Pass != "s3cret!" || m2.settings.Proxy.Pass != "proxy-pw" {
		t.Fatalf("passwords didn't survive a restart: %+v / %q", m2.settings.Sites, m2.settings.Proxy.Pass)
	}
}

// apiCall talks to the app's HTTP API the way the window does.
func apiCall(t *testing.T, h http.Handler, path string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/"+path, bytes.NewReader(b))
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	return out
}

func earlyDownload(m *Manager, id string) *Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[id]
}

func waitDone(t *testing.T, m *Manager, d *Download, min int64) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if atomic.LoadInt64(&d.done) >= min {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no progress while the dialog was open")
}

func TestEarlyStartMoveAndStart(t *testing.T) {
	srv := newSiteServer(t, 4<<20, 1<<20)
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, h, "probe", map[string]any{"url": srv.URL + "/report.pdf", "early": true})
	id, _ := info["early"].(string)
	if id == "" {
		t.Fatalf("expected an early start: %v", info)
	}
	d := earlyDownload(m, id)
	waitDone(t, m, d, 256<<10)
	snap := m.Snapshot(nil)
	if len(snap.Downloads) != 0 || len(snap.Early) != 1 {
		t.Fatalf("a download waiting in the dialog must stay out of the list: %d listed, %d early", len(snap.Downloads), len(snap.Early))
	}
	oldPart := d.partPath()
	dir2 := filepath.Join(m.settings.DownloadDir, "Elsewhere")
	r := apiCall(t, h, "early", map[string]any{"action": "commit", "id": id, "name": "Q3 report.pdf", "dir": dir2, "start": true})
	if r["id"] != id {
		t.Fatalf("commit: %v", r)
	}
	waitFor(t, m, d, StCompleted, 30*time.Second)
	got, err := os.ReadFile(filepath.Join(dir2, "Q3 report.pdf"))
	if err != nil || !bytes.Equal(got, srv.data) {
		t.Fatalf("moved download is wrong: %v", err)
	}
	if _, err := os.Stat(oldPart); err == nil {
		t.Fatal("the old partial file was left behind")
	}
	if len(m.Snapshot(nil).Downloads) != 1 {
		t.Fatal("confirmed download should be in the list")
	}
}

func TestEarlyStartLaterKeepsProgress(t *testing.T) {
	srv := newSiteServer(t, 6<<20, 1<<20)
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, h, "probe", map[string]any{"url": srv.URL + "/big.iso", "early": true})
	id := info["early"].(string)
	d := earlyDownload(m, id)
	waitDone(t, m, d, 512<<10)
	apiCall(t, h, "early", map[string]any{"action": "commit", "id": id, "start": false, "queueId": "main"})
	m.mu.Lock()
	st, q, done := d.Status, d.Queue, atomic.LoadInt64(&d.done)
	m.mu.Unlock()
	if st != StQueued || q != "main" || done < 512<<10 {
		t.Fatalf("Download Later: status %s queue %q done %d", st, q, done)
	}
	if _, err := os.Stat(d.partPath()); err != nil {
		t.Fatal("partial file should be kept for later")
	}
	m.Start([]string{id})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	if b, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName)); !bytes.Equal(b, srv.data) {
		t.Fatal("resumed download differs")
	}
}

func TestEarlyStartCancelAndAbandon(t *testing.T) {
	srv := newSiteServer(t, 8<<20, 256<<10)
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	id := apiCall(t, h, "probe", map[string]any{"url": srv.URL + "/a.bin", "early": true})["early"].(string)
	d := earlyDownload(m, id)
	waitDone(t, m, d, 64<<10)
	part := d.partPath()
	apiCall(t, h, "early", map[string]any{"action": "cancel", "id": id})
	time.Sleep(600 * time.Millisecond)
	if _, err := os.Stat(part); err == nil || earlyDownload(m, id) != nil {
		t.Fatal("Cancel should stop and delete the partial download")
	}

	// a dialog that disappears (window closed) stops checking in
	old := earlyTimeout
	earlyTimeout = 300 * time.Millisecond
	defer func() { earlyTimeout = old }()
	id2 := apiCall(t, h, "probe", map[string]any{"url": srv.URL + "/b.bin", "early": true})["early"].(string)
	part2 := earlyDownload(m, id2).partPath()
	time.Sleep(2 * time.Second)
	if earlyDownload(m, id2) != nil {
		t.Fatal("abandoned early download was kept")
	}
	if _, err := os.Stat(part2); err == nil {
		t.Fatal("abandoned early download left its file")
	}
	// the setting turns it off
	m.mu.Lock()
	m.settings.EarlyStart = false
	m.mu.Unlock()
	if e := apiCall(t, h, "probe", map[string]any{"url": srv.URL + "/c.bin", "early": true})["early"]; e != nil {
		t.Fatal("early start should be off")
	}
}

func TestEarlyStartFinishesBeforeConfirm(t *testing.T) {
	srv := newSiteServer(t, 200<<10, 0)
	m := newTestManager(t)
	var played []string
	old := soundPlayer
	soundPlayer = func(n string) bool { m.mu.Lock(); played = append(played, n); m.mu.Unlock(); return true }
	defer func() { soundPlayer = old }()
	m.mu.Lock()
	m.settings.Sounds = SoundSettings{On: true, Done: true, Failed: true, Queue: true}
	lastSound = time.Time{}
	m.mu.Unlock()
	h := (&Server{m: m, token: "t"}).routes()
	id := apiCall(t, h, "probe", map[string]any{"url": srv.URL + "/small.zip", "early": true})["early"].(string)
	d := earlyDownload(m, id)
	waitFor(t, m, d, StCompleted, 10*time.Second)
	time.Sleep(100 * time.Millisecond)
	m.mu.Lock()
	for _, n := range m.notices {
		if n.Kind == "complete" {
			t.Fatal("finished before confirming: must not announce yet")
		}
	}
	if len(played) != 0 {
		t.Fatal("no sound until it's confirmed")
	}
	m.mu.Unlock()
	apiCall(t, h, "early", map[string]any{"action": "commit", "id": id, "name": "renamed.zip", "start": true})
	if b, err := os.ReadFile(filepath.Join(d.Dir, "renamed.zip")); err != nil || !bytes.Equal(b, srv.data) {
		t.Fatalf("finished file wasn't moved to the chosen name: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for _, n := range m.notices {
		found = found || (n.Kind == "complete" && n.ID == id)
	}
	if !found || len(played) != 1 || played[0] != "done" {
		t.Fatalf("expected the completion notice and sound after confirming (sounds %v)", played)
	}
}

func TestSounds(t *testing.T) {
	for _, n := range []string{"done", "failed", "queue"} {
		b := soundWAV(n)
		if len(b) < 44+20000 || string(b[:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || string(b[36:40]) != "data" {
			t.Fatalf("%s: not a valid WAV (%d bytes)", n, len(b))
		}
	}
	if soundWAV("nope") != nil {
		t.Fatal("unknown sound")
	}
	// no system player: the window is asked to play it
	m := newTestManager(t)
	old := soundPlayer
	soundPlayer = func(string) bool { return false }
	defer func() { soundPlayer = old }()
	m.mu.Lock()
	m.settings.Sounds = SoundSettings{On: true, Failed: true}
	lastSound = time.Time{}
	m.playEvent("done") // switched off
	m.playEvent("failed")
	m.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	m.mu.Lock()
	defer m.mu.Unlock()
	var kinds []string
	for _, n := range m.notices {
		if n.Kind == "sound" {
			kinds = append(kinds, n.ID+n.Text)
		}
	}
	if len(kinds) != 1 {
		t.Fatalf("sound notices %v", kinds)
	}
}

func TestExtPingSendsBrowserConfig(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.SkipSites = []string{"bank.example", " "}
	m.settings.Keys = KeySettings{Zipline: "ctrlalt", Browser: "ctrlalt"}
	m.normalizeSettings()
	m.mu.Unlock()
	req := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/ext/ping", nil)
	req.Header.Set("Origin", "chrome-extension://abc")
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	(&Server{m: m, token: "t"}).routes().ServeHTTP(rec, req)
	var out struct {
		OK     bool `json:"ok"`
		Config struct {
			Skip []string    `json:"skipSites"`
			Keys KeySettings `json:"keys"`
		} `json:"config"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || len(out.Config.Skip) != 1 || out.Config.Keys.Zipline != "ctrlalt" || out.Config.Keys.Browser != "off" {
		t.Fatalf("ext config: %s", rec.Body.String())
	}
}
