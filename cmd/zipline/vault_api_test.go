package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func vtGet(t *testing.T, h http.Handler, path string) []byte {
	t.Helper()
	req := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/"+path, nil)
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func vtSettings(t *testing.T, h http.Handler) (settingsView, string) {
	t.Helper()
	raw := vtGet(t, h, "settings")
	var v settingsView
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v, string(raw)
}

// vtPasswords reads what is stored: the proxy's password and the sites' by site name.
func vtPasswords(m *Manager) (proxy string, sites map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sites = map[string]string{}
	for _, r := range m.settings.Sites {
		sites[r.Site] = r.Pass
	}
	return m.settings.Proxy.Pass, sites
}

func TestSettingsAPINeverReturnsPasswords(t *testing.T) {
	m := newTestManager(t)
	t.Cleanup(func() { setProxySettings(ProxySettings{Mode: "system"}) })
	h := (&Server{m: m, token: "t"}).routes()
	m.mu.Lock()
	m.settings.Proxy = ProxySettings{Mode: "manual", URL: "http://pu:purl-pw@proxy.example:8080", User: "pu2", Pass: "proxy-pw"}
	m.settings.Sites = []SiteRule{{ID: "a", Site: "a.example", User: "ann", Pass: "pw-a"}, {ID: "b", Site: "b.example", User: "bob", Pass: "pw-b"}, {ID: "c", Site: "c.example", User: "cy"}}
	m.settings.Phone.Devices = []PhoneDevice{{ID: "p1", Name: "Phone", Key: "PAIRCODE12345678"}}
	m.mu.Unlock()

	v, raw := vtSettings(t, h)
	vtNoPlain(t, "GET /api/settings", raw, "proxy-pw", "pw-a", "pw-b", "purl-pw", "PAIRCODE12345678")
	if !v.Proxy.PassSet || v.Proxy.Pass != "" || v.Proxy.URL != "http://pu@proxy.example:8080" || v.Proxy.User != "pu2" {
		t.Fatalf("proxy in the view: %+v", v.Proxy)
	}
	if !v.Sites[0].PassSet || !v.Sites[1].PassSet || v.Sites[2].PassSet || v.Sites[0].Pass != "" || v.Sites[0].ID != "a" || v.Sites[0].User != "ann" {
		t.Fatalf("sites in the view: %+v", v.Sites)
	}
	if !strings.Contains(raw, `"passSet":true`) || v.Vault.How == "" {
		t.Errorf("view: %.200s", raw)
	}
	// the answer to a save never has them either
	post := func(v settingsView) string {
		b, _ := json.Marshal(v)
		req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/settings", strings.NewReader(string(b)))
		req.Header.Set("X-Token", "t")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("POST: %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	stored := func() (string, map[string]string, string) {
		p, s := vtPasswords(m)
		m.mu.Lock()
		defer m.mu.Unlock()
		return p, s, m.settings.Proxy.URL
	}

	// 1. a save that touches nothing keeps every password
	v.MaxActive = 7
	vtNoPlain(t, "POST /api/settings", post(v), "proxy-pw", "pw-a", "pw-b", "purl-pw")
	p, s, u := stored()
	if p != "proxy-pw" || s["a.example"] != "pw-a" || s["b.example"] != "pw-b" || u != "http://pu:purl-pw@proxy.example:8080" {
		t.Fatalf("an untouched save changed them: %q %v %q", p, s, u)
	}
	// 2. rows deleted and reordered keep their own passwords
	v, _ = vtSettings(t, h)
	v.Sites = []SiteRule{v.Sites[2], v.Sites[1]}
	post(v)
	if _, s, _ = stored(); len(s) != 2 || s["b.example"] != "pw-b" || s["c.example"] != "" {
		t.Fatalf("after deleting a row: %v", s)
	}
	// 3. a rule renamed in the window keeps its password too (it goes by id)
	v, _ = vtSettings(t, h)
	v.Sites[1].Site = "b2.example"
	post(v)
	if _, s, _ = stored(); s["b2.example"] != "pw-b" {
		t.Fatalf("after renaming: %v", s)
	}
	// 4. a typed password replaces, a cleared one clears
	v, _ = vtSettings(t, h)
	v.Sites[0].Pass = "pw-c-new" // c.example: typed
	v.Sites[1].PassSet = false   // b2.example: cleared
	v.Proxy.Pass = "proxy-new"
	post(v)
	p, s, u = stored()
	if s["c.example"] != "pw-c-new" || s["b2.example"] != "" || p != "proxy-new" || u != "http://pu:purl-pw@proxy.example:8080" {
		t.Fatalf("typed/cleared: %q %v %q", p, s, u)
	}
	v, _ = vtSettings(t, h)
	v.Proxy.PassSet = false // cleared
	v.Proxy.URL = "http://pu:typed@proxy.example:8080"
	post(v)
	if p, _, u = stored(); p != "" || u != "http://pu:typed@proxy.example:8080" {
		t.Fatalf("proxy cleared/typed: %q %q", p, u)
	}
	// 5. changing the proxy's address drops the old address's password
	v, _ = vtSettings(t, h)
	v.Proxy.URL = "http://pu@other.example:8080"
	post(v)
	if _, _, u = stored(); u != "http://pu@other.example:8080" {
		t.Fatalf("proxy url %q", u)
	}
	// 6. a new row gets an id of its own; a window that sends no ids keeps passwords by site and user
	v, _ = vtSettings(t, h)
	v.Sites = append(v.Sites, SiteRule{Site: "new.example", User: "n", Pass: "pw-n"})
	post(v)
	m.mu.Lock()
	ids := map[string]bool{}
	for _, r := range m.settings.Sites {
		ids[r.ID] = r.ID != ""
	}
	m.mu.Unlock()
	if len(ids) != 3 || ids[""] {
		t.Fatalf("ids: %v", ids)
	}
	v, _ = vtSettings(t, h)
	for i := range v.Sites {
		v.Sites[i].ID = ""
	}
	post(v)
	if _, s, _ = stored(); s["new.example"] != "pw-n" || s["c.example"] != "pw-c-new" {
		t.Fatalf("by site and user: %v", s)
	}
}

func TestProxyTestUsesTheSavedPassword(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Proxy = ProxySettings{Mode: "manual", URL: "http://pu:purl-pw@proxy.example:8080", User: "x", Pass: "proxy-pw"}
	m.mu.Unlock()
	p := ProxySettings{Mode: "manual", URL: "http://pu@proxy.example:8080", User: "x", PassSet: true}
	m.fillProxyPass(&p)
	if p.Pass != "proxy-pw" || p.PassSet || p.URL != "http://pu:purl-pw@proxy.example:8080" {
		t.Fatalf("%+v", p)
	}
	q := ProxySettings{Mode: "manual", URL: "http://proxy.example:8080", User: "x"} // cleared in the window
	m.fillProxyPass(&q)
	if q.Pass != "" {
		t.Fatalf("a cleared password came back: %+v", q)
	}
}

func TestSitesGetIdsOnLoad(t *testing.T) {
	vtUse(t, &vtKeys{keys: vtKey(1)})
	dir := vtDir(t)
	writeAtomic(filepath.Join(dir, "settings.json"), []byte(`{"downloadDir":"`+filepath.ToSlash(dir)+`","sites":[{"site":"a.example","user":"u","pass":"p"},{"site":"b.example"}]}`))
	m := NewManager(dir)
	if s := m.settings.Sites; len(s) != 2 || s[0].ID == "" || s[0].ID == s[1].ID {
		t.Fatalf("%+v", s)
	}
	m.save()
	if m2 := NewManager(dir); m2.settings.Sites[0].ID != m.settings.Sites[0].ID {
		t.Error("ids must survive a restart")
	}
}

func TestDownloadLinksReachTheWindowWithoutTheirPassword(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	sec := vtSetSecrets(m)
	s := m.Snapshot(nil)
	raw, _ := json.Marshal(s)
	vtNoPlain(t, "the window's state", string(raw), sec.link, sec.mirror)
	var v DLView
	for _, x := range s.Downloads {
		if x.ID == "d1" {
			v = x
		}
	}
	if v.URL != "https://bob@files.example/f.zip" || len(v.Mirrors) != 1 || v.Mirrors[0] != "ftp://bob@mirror.example/f.zip" {
		t.Fatalf("view: %q %q", v.URL, v.Mirrors)
	}
	// the Properties window sends back what it was shown: the password stays
	apiCall(t, h, "props", map[string]any{"id": "d1", "url": v.URL, "desc": "note", "mirrors": v.Mirrors})
	m.mu.Lock()
	d := m.byID["d1"]
	ok := d.URL == "https://bob:dl-pw@files.example/f.zip" && d.Mirrors[0] == "ftp://bob:mir-pw@mirror.example/f.zip" && d.Desc == "note"
	m.mu.Unlock()
	if !ok {
		t.Fatalf("props lost the link's password: %q %q", d.URL, d.Mirrors)
	}
	// a link typed there replaces it, with or without a password of its own
	apiCall(t, h, "props", map[string]any{"id": "d1", "url": "https://bob:typed@files.example/g.zip"})
	apiCall(t, h, "props", map[string]any{"id": "d1", "url": "https://files.example/h.zip"})
	m.mu.Lock()
	got := d.URL
	m.mu.Unlock()
	if got != "https://files.example/h.zip" {
		t.Fatalf("url %q", got)
	}
}

func TestExportLeavesOutSecrets(t *testing.T) {
	m := newTestManager(t)
	sec := vtSetSecrets(m)
	plain, _ := m.Export("json", nil, false)
	vtNoPlain(t, "an export without secrets", string(plain), sec.link, sec.mirror, sec.cookie, sec.auth, "cHJveHk6cHc=")
	if !strings.Contains(string(plain), "https://bob@files.example/f.zip") || !strings.Contains(string(plain), "https://files.example/page") {
		t.Errorf("export: %.400s", plain)
	}
	txt, _ := m.Export("txt", nil, false)
	if strings.Contains(string(txt), sec.link) {
		t.Errorf("links export: %s", txt)
	}
	full, _ := m.Export("json", nil, true)
	for _, s := range []string{sec.link, sec.mirror, sec.cookie, sec.auth, "cHJveHk6cHc="} {
		if !strings.Contains(string(full), s) {
			t.Errorf("an export with secrets lacks %q", s)
		}
	}
}

func TestGrabberSecretsAreSealedAndNeverSent(t *testing.T) {
	vtUse(t, &vtKeys{keys: vtKey(1)})
	dir := vtDir(t)
	m := NewManager(dir)
	h := (&Server{m: m, token: "t"}).routes()
	hd := map[string]string{"Cookie": "sid=SECRETCOOKIE", "Authorization": "Basic c2VjcmV0", "Referer": "https://x.example/r"}
	got := apiCall(t, h, "grab/project", map[string]any{"project": map[string]any{"name": "p", "opts": map[string]any{"url": "https://x.example/", "types": []string{"Images"}, "headers": hd}}})
	proj := got["project"].(map[string]any)
	rawProj, _ := json.Marshal(proj)
	vtNoPlain(t, "the saved project sent to the window", string(rawProj), "SECRETCOOKIE", "c2VjcmV0")
	if !strings.Contains(string(rawProj), "https://x.example/r") {
		t.Errorf("the Referer should stay: %s", rawProj)
	}
	projects := string(vtGet(t, h, "grab/projects"))
	vtNoPlain(t, "GET /api/grab/projects", projects, "SECRETCOOKIE", "c2VjcmV0")
	file := vtRead(t, filepath.Join(dir, "grabber.json"))
	vtNoPlain(t, "grabber.json", file, "SECRETCOOKIE", "c2VjcmV0")
	if !strings.Contains(file, sealPrefix) || !strings.Contains(file, "https://x.example/r") {
		t.Errorf("grabber.json: %s", file)
	}
	// a saved scan
	job := &GrabJob{ID: "s1", Opts: GrabOptions{URL: "https://x.example/", Headers: hd}, State: "done", Cookies: map[string]string{"x.example": "sid=SCANCOOKIE"}, seen: map[string]bool{}}
	m.grab.mu.Lock()
	m.grab.jobs["s1"] = job
	m.grab.mu.Unlock()
	m.grab.persist(job)
	scan := vtRead(t, filepath.Join(dir, "scans", "s1.json"))
	vtNoPlain(t, "the saved scan", scan, "SECRETCOOKIE", "c2VjcmV0", "SCANCOOKIE")
	status := string(vtGet(t, h, "grab/status?id=s1"))
	vtNoPlain(t, "GET /api/grab/status", status, "SECRETCOOKIE", "c2VjcmV0")
	// the next start has them all again
	m2 := NewManager(dir)
	if p := m2.grab.Projects(); len(p) != 1 {
		t.Fatalf("projects: %v", p)
	}
	m2.grab.mu.Lock()
	hp := m2.grab.projects[0].Opts.Headers
	m2.grab.mu.Unlock()
	j := m2.grab.Job("s1")
	if hp["Cookie"] != "sid=SECRETCOOKIE" || hp["Authorization"] != "Basic c2VjcmV0" || j == nil || j.Opts.Headers["Cookie"] != "sid=SECRETCOOKIE" || j.Cookies["x.example"] != "sid=SCANCOOKIE" {
		t.Fatalf("after a restart: %v / %+v", hp, j)
	}
}

func TestPasswordFromStandardInput(t *testing.T) {
	old := cliStdin
	defer func() { cliStdin = old }()
	cliStdin = strings.NewReader("s3cret pw \r\nsecond line\n")
	r, err := parseCLI([]string{"--password-stdin", "--user", "bob", "https://x.example/a.zip"}, false)
	if err != nil || r.Headers["Authorization"] != basicAuth("bob", "s3cret pw ") {
		t.Fatalf("%v %v", r, err)
	}
	cliStdin = strings.NewReader("no newline")
	if r, err = parseCLI([]string{"--user=bob", "--password-stdin", "https://x.example/a.zip"}, false); err != nil || r.Headers["Authorization"] != basicAuth("bob", "no newline") {
		t.Fatalf("%v %v", r, err)
	}
	for name, c := range map[string]struct {
		in   string
		args []string
	}{
		"no user":       {"pw\n", []string{"--password-stdin", "https://x.example/a.zip"}},
		"both flags":    {"pw\n", []string{"--user", "b", "--password", "x", "--password-stdin", "https://x.example/a.zip"}},
		"empty input":   {"", []string{"--user", "b", "--password-stdin", "https://x.example/a.zip"}},
		"empty line":    {"\n", []string{"--user", "b", "--password-stdin", "https://x.example/a.zip"}},
		"only newlines": {"\r\n", []string{"--user", "b", "--password-stdin", "https://x.example/a.zip"}},
	} {
		cliStdin = strings.NewReader(c.in)
		if _, err := parseCLI(c.args, false); err == nil || !strings.Contains(err.Error(), "password") {
			t.Errorf("%s: err %v", name, err)
		}
	}
	// --help doesn't wait for a password
	cliStdin = failingReader{}
	if r, err := parseCLI([]string{"--password-stdin", "--help"}, false); err != nil || !r.Help {
		t.Errorf("--help: %v %v", r, err)
	}
	if !strings.Contains(cliUsage, "--password-stdin") || !strings.Contains(cliUsage, "safer") {
		t.Error("--help should recommend --password-stdin")
	}
	if p, err := parseCLI([]string{"--user", "bob", "--password", "pw", "https://x.example/a.zip"}, false); err != nil || p.Headers["Authorization"] != basicAuth("bob", "pw") {
		t.Errorf("--password must keep working: %v %v", p, err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("standard input was read") }
