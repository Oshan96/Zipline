package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// signIn opens a window the way Zipline does (login?c=…) and returns its session cookie.
func signIn(t *testing.T, s *Server, h http.Handler) *http.Cookie {
	t.Helper()
	u, _ := url.Parse(s.loginURL())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:17890"+u.RequestURI(), nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("login: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == s.cookieName() {
			return c
		}
	}
	t.Fatal("login set no session cookie")
	return nil
}

// winReq is a request from the window's own page: its cookie, and X-Zipline on what changes something.
func winReq(h http.Handler, method, path string, body any, c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:17890"+path, rd)
	if c != nil {
		req.AddCookie(c)
	}
	if method != "GET" {
		req.Header.Set("X-Zipline", "1")
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The page at Zipline's address never carries a token: without a session it only says how to open the
// window; with one it's the app, which signs in with its cookie.
func TestWindowPageHasNoToken(t *testing.T) {
	m := newTestManager(t)
	const secret = "0123456789abcdef-the-run-secret"
	s := &Server{m: m, token: secret}
	h := s.routes()
	rec := winReq(h, "GET", "/", nil, nil, nil)
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `t("lock.title")`) || strings.Contains(body, "/app.js") || strings.Contains(body, secret) || strings.Contains(body, "TOKEN") {
		t.Fatalf("page without a session: %d\n%s", rec.Code, body)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("the page may be framed: %v", rec.Header())
	}
	for _, want := range []string{"--accentText:", "prefers-color-scheme: dark", `data-theme="dark"`, "zl.theme"} { // styled like the app, light and dark
		if !strings.Contains(body, want) {
			t.Errorf("the locked page is missing %q", want)
		}
	}
	c := signIn(t, s, h)
	rec = winReq(h, "GET", "/", nil, c, nil)
	body = rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `<script src="/app.js">`) || !strings.Contains(body, `<script src="/extlink.js">`) || strings.Contains(body, secret) || strings.Contains(body, "TOKEN") {
		t.Fatalf("the window's page: %d", rec.Code)
	}
	for _, p := range []string{"/app.js", "/i18n.js", "/extlink.js"} {
		if rec := winReq(h, "GET", p, nil, nil, nil); rec.Code != 200 || strings.Contains(rec.Body.String(), secret) || strings.Contains(rec.Body.String(), "X-Token") {
			t.Errorf("%s: %d (or it mentions the token)", p, rec.Code)
		}
	}
	// the window's API works with the cookie alone
	if rec := winReq(h, "GET", "/api/settings", nil, c, nil); rec.Code != 200 {
		t.Fatalf("settings with the session: %d", rec.Code)
	}
	if rec := winReq(h, "GET", "/api/settings", nil, nil, nil); rec.Code != 401 {
		t.Fatalf("settings without a session: %d", rec.Code)
	}
	if rec := winReq(h, "GET", "/api/settings", nil, &http.Cookie{Name: sessionCookie, Value: strings.Repeat("ab", 32)}, nil); rec.Code != 401 {
		t.Fatalf("settings with a made-up session: %d", rec.Code)
	}
}

// A window code works once, for a minute; the cookie it gives is HttpOnly, SameSite=Strict, for the whole app.
func TestLoginCodeOnceAndExpiring(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: "t"}
	h := s.routes()
	u, _ := url.Parse(s.loginURL())
	if !strings.HasPrefix(u.String(), appURL+"login?c=") || len(u.Query().Get("c")) < 32 {
		t.Fatalf("login address %q", u)
	}
	get := func(c *http.Cookie) *httptest.ResponseRecorder { return winReq(h, "GET", u.RequestURI(), nil, c, nil) }
	rec := get(nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d", rec.Code)
	}
	var c *http.Cookie
	for _, x := range rec.Result().Cookies() {
		if x.Name == sessionCookie {
			c = x
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.MaxAge != 0 || !c.Expires.IsZero() || len(c.Value) != 64 || c.Secure {
		t.Fatalf("cookie %+v", c)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "HttpOnly") || !strings.Contains(rec.Header().Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("Set-Cookie %q", rec.Header().Get("Set-Cookie"))
	}
	rec = get(nil) // used up
	if rec.Code != 403 || len(rec.Result().Cookies()) != 0 || !strings.Contains(rec.Body.String(), `t("lock.title")`) {
		t.Fatalf("second use: %d %v", rec.Code, rec.Result().Cookies())
	}
	if rec = get(c); rec.Code != http.StatusSeeOther || len(rec.Result().Cookies()) != 0 { // a signed-in window reloading it
		t.Fatalf("reload in a signed-in window: %d", rec.Code)
	}
	if rec = winReq(h, "GET", "/login?c=nope", nil, nil, nil); rec.Code != 403 {
		t.Fatalf("made-up code: %d", rec.Code)
	}
	if rec = winReq(h, "GET", "/login", nil, nil, nil); rec.Code != 403 {
		t.Fatalf("no code: %d", rec.Code)
	}
	// older than a minute
	u2, _ := url.Parse(s.loginURL())
	sec := s.security()
	sec.mu.Lock()
	sec.now = func() time.Time { return time.Now().Add(61 * time.Second) }
	sec.mu.Unlock()
	if rec = winReq(h, "GET", u2.RequestURI(), nil, nil, nil); rec.Code != 403 {
		t.Fatalf("expired code: %d", rec.Code)
	}
	if rec = winReq(h, "POST", u.RequestURI(), nil, nil, nil); rec.Code != 405 {
		t.Fatalf("POST login: %d", rec.Code)
	}
}

// DNS rebinding (another Host), other sites (Origin, Sec-Fetch-Site) and forms without X-Zipline are refused.
func TestHostAndOriginChecks(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: "t", port: 17890}
	h := s.routes()
	c := signIn(t, s, h)
	host := func(hp string) int {
		req := httptest.NewRequest("GET", "http://127.0.0.1:17890/i18n.js", nil)
		req.Host = hp
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for hp, want := range map[string]int{"127.0.0.1:17890": 200, "localhost:17890": 200, "evil.example:17890": 403, "127.0.0.1:9999": 403,
		"evil.example": 403, "127.0.0.1": 403, "[::1]:17890": 403, "127.0.0.1.evil.example:17890": 403} {
		if got := host(hp); got != want {
			t.Errorf("Host %q: %d, want %d", hp, got, want)
		}
	}
	for _, tc := range []struct {
		name   string
		method string
		hdr    map[string]string
		want   int
	}{
		{"own origin", "POST", map[string]string{"Origin": "http://127.0.0.1:17890"}, 200},
		{"no origin", "POST", nil, 200},
		{"another site", "POST", map[string]string{"Origin": "https://evil.example"}, 403},
		{"another port", "POST", map[string]string{"Origin": "http://127.0.0.1:8080"}, 403},
		{"the phone page", "POST", map[string]string{"Origin": "http://192.168.1.5:17891"}, 403},
		{"an extension", "POST", map[string]string{"Origin": "chrome-extension://abc"}, 403},
		{"cross-site fetch", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"same-site fetch", "POST", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"form post (no X-Zipline)", "POST", map[string]string{"X-Zipline": "", "Content-Type": "application/x-www-form-urlencoded"}, 403},
		{"read from another site", "GET", map[string]string{"Origin": "https://evil.example"}, 403},
	} {
		path := "/api/prompt/dismiss"
		if tc.method == "GET" {
			path = "/api/settings"
		}
		if rec := winReq(h, tc.method, path, map[string]any{"id": "x"}, c, tc.hdr); rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
	// the phone listener has none of the window's API
	rec := httptest.NewRecorder()
	(&phoneServer{m: m}).routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/settings", nil))
	if rec.Code != 404 {
		t.Errorf("the phone port serves /api/settings: %d", rec.Code)
	}
}

// The virus-scan program runs after every download: only the window (which asks first) may change it.
func TestScannerChangeOnlyFromWindow(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: "t"}
	h := s.routes()
	secret := map[string]string{"X-Token": "t"}
	var st map[string]any
	json.Unmarshal(winReq(h, "GET", "/api/settings", nil, nil, secret).Body.Bytes(), &st)
	st["scan"] = map[string]any{"on": true, "program": "/tmp/evil.sh", "args": "%f"}
	if rec := winReq(h, "POST", "/api/settings", st, nil, secret); rec.Code != 403 {
		t.Fatalf("scanner change with the secret: %d", rec.Code)
	}
	m.mu.Lock()
	prog := m.settings.Scan.Program
	m.mu.Unlock()
	if prog != "" {
		t.Fatalf("the program was saved: %q", prog)
	}
	st["scan"] = map[string]any{"on": true}
	st["maxActive"] = 7
	if rec := winReq(h, "POST", "/api/settings", st, nil, secret); rec.Code != 200 { // other changes are fine
		t.Fatalf("settings with the secret: %d %s", rec.Code, rec.Body.String())
	}
	// a paired extension's key doesn't open the window's API at all
	if rec := winReq(h, "POST", "/api/settings", st, nil, map[string]string{"Origin": "chrome-extension://abc", "X-Zipline-Pair": "p1"}); rec.Code != 401 {
		t.Fatalf("settings from an extension: %d", rec.Code)
	}
	c := signIn(t, s, h)
	st["scan"] = map[string]any{"on": true, "program": "/usr/bin/clamscan", "args": "--no-summary %f"}
	if rec := winReq(h, "POST", "/api/settings", st, c, nil); rec.Code != 200 {
		t.Fatalf("scanner change from the window: %d %s", rec.Code, rec.Body.String())
	}
	m.mu.Lock()
	sc := m.settings.Scan
	m.mu.Unlock()
	if sc.Program != "/usr/bin/clamscan" || sc.Args != "--no-summary %f" {
		t.Fatalf("scanner %+v", sc)
	}
	st["scan"] = map[string]any{"on": true, "program": "/usr/bin/clamscan", "args": "--remove %f"} // the arguments count too
	if rec := winReq(h, "POST", "/api/settings", st, nil, secret); rec.Code != 403 {
		t.Fatalf("arguments change with the secret: %d", rec.Code)
	}
}

// --login-url's endpoint: only a program with the secret, and the address works once.
func TestLoginURLForPrograms(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: "t"}
	h := s.routes()
	if rec := winReq(h, "POST", "/api/login/new", map[string]any{}, signIn(t, s, h), nil); rec.Code != 403 {
		t.Fatalf("from the window: %d", rec.Code)
	}
	if rec := winReq(h, "POST", "/api/login/new", map[string]any{}, nil, map[string]string{"X-Token": "t", "X-Zipline-Via": "native"}); rec.Code != 403 {
		t.Fatalf("through the native host: %d", rec.Code)
	}
	rec := winReq(h, "POST", "/api/login/new", map[string]any{}, nil, map[string]string{"X-Token": "t"})
	var out struct{ URL string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	u, err := url.Parse(out.URL)
	if rec.Code != 200 || err != nil || u.Path != "/login" {
		t.Fatalf("login/new: %d %s", rec.Code, rec.Body.String())
	}
	if rec := winReq(h, "GET", u.RequestURI(), nil, nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("its address: %d", rec.Code)
	}
}

// run.json is this user's only, and findApp trusts only a program that proves it holds the secret.
func TestRunFileAndFindApp(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: strings.Repeat("5e", 32)}
	app := httptest.NewServer(s.routes())
	defer app.Close()
	dir := t.TempDir()
	if _, err := findApp(dir); err != errNotRunning {
		t.Fatalf("no run.json: %v", err)
	}
	port := app.Listener.Addr().(*net.TCPAddr).Port
	if err := writeRunFile(dir, runInfo{Port: port, PID: os.Getpid(), Version: appVersion, Secret: s.token}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(runFilePath(dir)); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("run.json: %v %v", st.Mode(), err)
	}
	a, err := findApp(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ URL string }
	if err := a.post("api/login/new", map[string]any{}, &out); err != nil || !strings.Contains(out.URL, "/login?c=") {
		t.Fatalf("login/new: %v %q", err, out.URL)
	}
	// a program that took the port answers without the proof
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Zipline-Proof", "0000")
		writeJSON(w, map[string]any{"app": "zipline", "ok": true})
	}))
	defer fake.Close()
	writeRunFile(dir, runInfo{Port: fake.Listener.Addr().(*net.TCPAddr).Port, PID: 1, Secret: s.token})
	if _, err := findApp(dir); err == nil || err == errNotRunning {
		t.Fatalf("a fake app was trusted: %v", err)
	}
	removeRunFile(dir, 2) // not this process's: stays
	if _, err := os.Stat(runFilePath(dir)); err != nil {
		t.Fatal("removed another process's run.json")
	}
	removeRunFile(dir, 1)
	if _, err := os.Stat(filepath.Join(dir, "run.json")); !os.IsNotExist(err) {
		t.Fatal("run.json stayed")
	}
}

// One Zipline per data folder: a second one is refused, another folder is fine.
func TestInstanceLock(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	l1, err := lockInstance(a)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := lockInstance(a); err != errLocked {
		l.Release()
		t.Fatalf("second lock on the same folder: %v", err)
	}
	l2, err := lockInstance(b)
	if err != nil {
		t.Fatalf("another folder: %v", err)
	}
	l2.Release()
	l1.Release()
	l3, err := lockInstance(a)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	l3.Release()
	var none *instanceLock
	none.Release() // (no lock: nothing to do)
}

// An older Zipline (no lock, no handshake) on the usual port is told apart from another program and
// from a current Zipline (which answers with its version).
func TestOlderZiplineOnPort(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"app": "zipline", "ok": true, "config": map[string]any{}})
	}))
	defer old.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>hello</html>")) }))
	defer other.Close()
	cur := httptest.NewServer((&Server{m: newTestManager(t), token: "t"}).routes())
	defer cur.Close()
	port := func(s *httptest.Server) int { return s.Listener.Addr().(*net.TCPAddr).Port }
	if !olderZiplineOn(port(old)) || olderZiplineOn(port(other)) || olderZiplineOn(port(cur)) {
		t.Fatal("older Zipline not told apart")
	}
}
