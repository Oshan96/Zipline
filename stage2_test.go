package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Stage 2 wiring: logins only when asked (Basic and Digest), cookies kept across redirects,
// browser-style file names, disk checks, origin marks and diagnostics.

func TestLoginOnlyWhenTheServerAsks(t *testing.T) {
	data := randData(600 << 10)
	var withAuthFirst, challenged int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "alice" || p != "s3cret" {
			if r.Header.Get("Authorization") != "" {
				atomic.AddInt32(&withAuthFirst, 1)
			}
			atomic.AddInt32(&challenged, 1)
			w.Header().Set("WWW-Authenticate", `Basic realm="files"`)
			http.Error(w, "login needed", 401)
			return
		}
		http.ServeContent(w, r, "f.bin", time.Now(), bytes.NewReader(data))
	}))
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Sites = []SiteRule{{Site: "127.0.0.1", User: "alice", Pass: "s3cret"}}
	m.applySites()
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	if b, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName)); !bytes.Equal(b, data) {
		t.Fatal("file differs")
	}
	// the first request went without the password; after the server asked, every request had it
	if c := atomic.LoadInt32(&challenged); c != 1 || atomic.LoadInt32(&withAuthFirst) != 0 {
		t.Fatalf("challenged %d times (want 1: later connections send the login straight away)", c)
	}
}

func TestDigestLogin(t *testing.T) {
	data := randData(400 << 10)
	const realm, nonce = "files@example", "dcd98b7102dd2f0e8b11d0f600bfb0c093"
	ha1 := md5hex("bob:" + realm + ":hunter2")
	var good int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		chs := parseChallenges([]string{a})
		ok := false
		if len(chs) == 1 && chs[0].Scheme == "digest" {
			p := chs[0].Params
			ha2 := md5hex(r.Method + ":" + p["uri"])
			want := md5hex(ha1 + ":" + p["nonce"] + ":" + p["nc"] + ":" + p["cnonce"] + ":" + p["qop"] + ":" + ha2)
			ok = p["username"] == "bob" && p["response"] == want && p["uri"] == r.URL.RequestURI()
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm=%q, qop="auth", nonce=%q, opaque="5ccc069c403ebaf9f0171e9517f40e41"`, realm, nonce))
			http.Error(w, "login needed", 401)
			return
		}
		atomic.AddInt32(&good, 1)
		http.ServeContent(w, r, "f.bin", time.Now(), bytes.NewReader(data))
	}))
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Sites = []SiteRule{{Site: "127.0.0.1", User: "bob", Pass: "hunter2"}}
	m.applySites()
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.URL + "/dir/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	if b, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName)); !bytes.Equal(b, data) || atomic.LoadInt32(&good) == 0 {
		t.Fatal("digest download failed")
	}
	// a wrong password is reported, not retried for ever
	m.mu.Lock()
	m.settings.Sites = []SiteRule{{Site: "127.0.0.1", User: "bob", Pass: "wrong"}}
	m.applySites()
	m.mu.Unlock()
	d2 := m.Add(AddReq{URL: srv.URL + "/dir/g.bin", Start: true})
	waitFor(t, m, d2, StError, 10*time.Second)
	m.mu.Lock()
	msg := d2.Error
	m.mu.Unlock()
	if !strings.Contains(msg, "refused the user name and password") {
		t.Fatalf("wrong password: %q", msg)
	}
}

func md5hex(s string) string { h := md5.Sum([]byte(s)); return hex.EncodeToString(h[:]) }

// A site that sets its session cookie on the way to the file (a redirect) gets it back.
func TestCookiesSetDuringRedirectsAreKept(t *testing.T) {
	data := randData(500 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/get":
			http.SetCookie(w, &http.Cookie{Name: "sess", Value: "ok42", Path: "/"})
			http.Redirect(w, r, "/files/report.pdf", http.StatusFound)
		case "/files/report.pdf":
			if c, err := r.Cookie("sess"); err != nil || c.Value != "ok42" {
				http.Error(w, "no session", 403)
				return
			}
			http.ServeContent(w, r, "report.pdf", time.Now(), bytes.NewReader(data))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/get", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	if b, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName)); !bytes.Equal(b, data) || d.FileName != "report.pdf" {
		t.Fatalf("got %q", d.FileName)
	}
}

// Names from real-world Content-Disposition headers that Go's strict parser rejects.
func TestServerFileNamesLikeABrowser(t *testing.T) {
	cases := map[string]string{
		`attachment; filename=Annual Report 2026.pdf`:                              "Annual Report 2026.pdf",
		`attachment; filename="résumé.pdf"; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf`: "résumé.pdf",
		`attachment; filename="con.txt"`:                                           "_con.txt",
		`attachment; filename="shortcut.lnk"`:                                      "shortcut.download",
	}
	for cd, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Disposition", cd)
			http.ServeContent(w, r, "", time.Now(), bytes.NewReader(randData(300<<10)))
		}))
		m := newTestManager(t)
		d := m.Add(AddReq{URL: srv.URL + "/download.php?id=7", Start: true})
		waitFor(t, m, d, StCompleted, 20*time.Second)
		m.mu.Lock()
		got := d.FileName
		m.mu.Unlock()
		srv.Close()
		if got != want {
			t.Errorf("%s → %q, want %q", cd, got, want)
		}
	}
}

// A file the disk can't hold fails at once with a plain message, instead of half-way through.
func TestNoRoomFailsBeforeDownloading(t *testing.T) {
	const huge = 1 << 60 // an exabyte
	var ranged int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			atomic.AddInt32(&ranged, 1)
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", huge-1, int64(huge)))
		w.Header().Set("Content-Length", fmt.Sprint(int64(huge)))
		w.WriteHeader(206)
		w.Write(make([]byte, 1024))
	}))
	defer srv.Close()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/big.iso", Start: true})
	waitFor(t, m, d, StError, 10*time.Second)
	m.mu.Lock()
	msg := d.Error
	m.mu.Unlock()
	if !strings.Contains(msg, "not enough space") {
		t.Fatalf("error: %q", msg)
	}
	if _, err := os.Stat(d.partPath()); err == nil {
		t.Fatal("a part file was created")
	}
}

func TestVersionDiagnosticsAndFTPSLinks(t *testing.T) {
	r, err := parseCLI([]string{"--version"}, false)
	if err != nil || !r.Version || !strings.Contains(versionText(), "Zipline "+appVersion) {
		t.Fatalf("--version: %+v %v %q", r, err, versionText())
	}
	if r, err := parseCLI([]string{"ftps://files.example.com/pub/a.iso"}, false); err != nil || len(r.URLs) != 1 {
		t.Fatalf("ftps link: %+v %v", r, err)
	}
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Proxy = ProxySettings{Mode: "manual", URL: "http://proxy.example:8080", User: "pu", Pass: "proxy-secret"}
	m.settings.Sites = []SiteRule{{Site: "files.example", User: "alice", Pass: "site-secret"}}
	m.mu.Unlock()
	h := (&Server{m: m, token: "t"}).routes()
	req := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/diagnostics", nil)
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := rec.Body.String()
	if !strings.Contains(out, "Zipline "+appVersion) || strings.Contains(out, "proxy-secret") || strings.Contains(out, "site-secret") {
		t.Fatalf("diagnostics:\n%s", out)
	}
}
