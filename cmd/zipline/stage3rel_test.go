package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------- test servers

// fileHost serves one file at any path (with ranges, at rate bytes/s per connection), counting
// what it sends per path. route can take over requests first.
type fileHost struct {
	*httptest.Server
	data   []byte
	rate   int
	mu     sync.Mutex
	sent   map[string]int64
	hits   map[string]int
	active int32
	maxAct int32 // more connections than this get 403 (0 = no limit)
	cut    int64 // each reply stops after this many bytes, like a flaky connection (0 = never)
	route  func(w http.ResponseWriter, r *http.Request) bool
	mangle func(b []byte, off int64) // changes what's sent (a mirror with damaged data)
}

func newFileHost(t *testing.T, data []byte, rate int) *fileHost {
	h := &fileHost{data: data, rate: rate, sent: map[string]int64{}, hits: map[string]int{}}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)
	return h
}

func (h *fileHost) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.hits[r.URL.Path]++
	route := h.route
	h.mu.Unlock()
	if route != nil && route(w, r) {
		return
	}
	n := atomic.AddInt32(&h.active, 1)
	defer atomic.AddInt32(&h.active, -1)
	if h.maxAct > 0 && n > h.maxAct {
		http.Error(w, "too many connections", 403)
		return
	}
	data := h.data
	if h.mangle != nil {
		data = append([]byte(nil), h.data...)
		h.mangle(data, 0)
	}
	cw := &countWriter{slowWriter: slowWriter{w: w, rate: h.rate, start: time.Now()}, h: h, path: r.URL.Path}
	http.ServeContent(cw, r, "file.bin", time.Unix(1700000000, 0), bytes.NewReader(data))
}

func (h *fileHost) bytesSent(path string) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if path == "" {
		var t int64
		for _, v := range h.sent {
			t += v
		}
		return t
	}
	return h.sent[path]
}

func (h *fileHost) hitCount(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits[path]
}

type countWriter struct {
	slowWriter
	h    *fileHost
	path string
	n    int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.h.cut > 0 && c.n+int64(len(p)) > c.h.cut {
		p = p[:max(c.h.cut-c.n, 0)]
		if len(p) == 0 {
			return 0, errors.New("connection dropped")
		}
	}
	n, err := c.slowWriter.Write(p)
	c.n += int64(n)
	c.h.mu.Lock()
	c.h.sent[c.path] += int64(n)
	c.h.mu.Unlock()
	return n, err
}

func randData(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func fileIs(t *testing.T, d *Download, want []byte) {
	t.Helper()
	got, err := os.ReadFile(d.path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("downloaded file differs (%d bytes, want %d)", len(got), len(want))
	}
}

func extCall(t *testing.T, h http.Handler, method, path string, body any) map[string]any {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:17890/api/ext/"+path, rd)
	req.Header.Set("Origin", "chrome-extension://abc")
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 {
		t.Fatalf("ext/%s: %d %s", path, rec.Code, rec.Body.String())
	}
	return out
}

func waitLink(t *testing.T, m *Manager, d *Download, want string, max time.Duration) {
	t.Helper()
	dl := time.Now().Add(max)
	for time.Now().Before(dl) {
		m.mu.Lock()
		l, run := d.Link, d.running
		m.mu.Unlock()
		if l == want && !run {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t.Fatalf("link state %q (status %s: %s), want %q", d.Link, d.Status, d.Error, want)
}

// ---------------------------------------------------------------- mirrors

func TestMirrorsShareTheWork(t *testing.T) {
	// (big enough that pieces are left when the mirror has been checked and joins)
	data := randData(24 << 20)
	a := newFileHost(t, data, 700<<10)
	b := newFileHost(t, data, 700<<10)
	other := newFileHost(t, randData(5<<20), 0) // a "mirror" with another file
	m := newTestManager(t)
	d := m.Add(AddReq{URL: a.URL + "/pub/app.zip", Mirrors: []string{b.URL + "/mirror/app.zip", other.URL + "/app.zip", "notaurl"}, Start: true})
	m.mu.Lock()
	if len(d.Mirrors) != 2 {
		t.Fatalf("mirrors kept: %v", d.Mirrors)
	}
	m.mu.Unlock()
	sawSources := false
	for i := 0; i < 400; i++ {
		v := m.Snapshot(map[string]bool{d.ID: true})
		if len(v.Downloads) == 1 && len(v.Downloads[0].Sources) == 3 {
			for _, s := range v.Downloads[0].Sources {
				if s.URL == other.URL+"/app.zip" && s.Dead == "" {
					t.Fatalf("a mirror with a different file should be left out: %+v", s)
				}
			}
			sawSources = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	sa, sb := a.bytesSent(""), b.bytesSent("")
	t.Logf("main sent %d KB, mirror %d KB, other %d B; sources shown: %v", sa>>10, sb>>10, other.bytesSent(""), sawSources)
	if sa < 1<<20 || sb < 1<<20 {
		t.Fatalf("both servers should share the work: %d / %d bytes", sa, sb)
	}
	if other.bytesSent("") > 64<<10 {
		t.Fatalf("the wrong mirror sent %d bytes", other.bytesSent(""))
	}
}

func TestMirrorStandsInForBrokenMainLink(t *testing.T) {
	data := randData(3 << 20)
	mirror := newFileHost(t, data, 0)
	main := newFileHost(t, data, 0)
	main.route = func(w http.ResponseWriter, r *http.Request) bool {
		http.Error(w, "down for maintenance", 503)
		return true
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: main.URL + "/f.bin", Name: "f.bin", Mirrors: []string{mirror.URL + "/f.bin"}, Start: true})
	waitFor(t, m, d, StCompleted, 40*time.Second)
	fileIs(t, d, data)
}

func TestServerConnectionLimitIsLearned(t *testing.T) {
	data := randData(4 << 20)
	h := newFileHost(t, data, 400<<10)
	h.maxAct = 3 // 403 for a fourth connection
	m := newTestManager(t)
	m.settings.Conns = 8
	d := m.Add(AddReq{URL: h.URL + "/limited.bin", Start: true})
	waitFor(t, m, d, StCompleted, 40*time.Second)
	fileIs(t, d, data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.Link != "" {
		t.Fatalf("a busy server isn't an expired link: %q", d.Link)
	}
}

func TestCookiesStayWithTheirSite(t *testing.T) {
	data := randData(2 << 20)
	var leaked int32
	main := newFileHost(t, data, 300<<10)
	mirror := newFileHost(t, data, 300<<10)
	mirror.route = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Cookie") != "" {
			atomic.AddInt32(&leaked, 1)
		}
		return false
	}
	// two hosts that aren't the same site: 127.0.0.1 and localhost
	mirrorURL := strings.Replace(mirror.URL, "127.0.0.1", "localhost", 1) + "/f.bin"
	m := newTestManager(t)
	d := m.Add(AddReq{URL: main.URL + "/f.bin", Mirrors: []string{mirrorURL}, Headers: map[string]string{"Cookie": "session=secret"}, Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	if leaked > 0 {
		t.Fatalf("the sign-in cookie went to another site %d times", leaked)
	}
	if !sameSite("dl.example.com", "cdn.example.com") || sameSite("example.co.uk", "other.co.uk") || !sameSite("a.example.co.uk", "example.co.uk") {
		t.Fatal("sameSite")
	}
}

// ---------------------------------------------------------------- expired links

func TestSignedRedirectIsRenewed(t *testing.T) {
	data := randData(5 << 20)
	h := newFileHost(t, data, 500<<10)
	h.cut = 400 << 10 // connections drop now and then, so new requests are made after the address expired
	var issued int32
	valid := sync.Map{}
	h.route = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/download/setup.exe": // the stable link hands out signed addresses good for 1.2 s
			n := atomic.AddInt32(&issued, 1)
			tok := strconv.Itoa(int(n))
			valid.Store(tok, time.Now().Add(1200*time.Millisecond))
			exp := time.Now().Add(1200 * time.Millisecond).Unix()
			http.Redirect(w, r, fmt.Sprintf("/cdn/setup.exe?token=%s&Expires=%d", tok, exp), http.StatusFound)
			return true
		case "/cdn/setup.exe":
			v, ok := valid.Load(r.URL.Query().Get("token"))
			if !ok || time.Now().After(v.(time.Time)) {
				http.Error(w, "Request has expired", 403)
				return true
			}
		}
		return false
	}
	m := newTestManager(t)
	m.settings.Conns = 4
	d := m.Add(AddReq{URL: h.URL + "/download/setup.exe", Start: true})
	waitFor(t, m, d, StCompleted, 40*time.Second)
	fileIs(t, d, data)
	if atomic.LoadInt32(&issued) < 2 {
		t.Fatalf("the signed address should have been renewed (issued %d)", issued)
	}
	t.Logf("signed addresses issued: %d", issued)
}

// expiringHost serves a file under /dl/<token>/<name>; tokens can be revoked.
func expiringHost(t *testing.T, data []byte, rate int) (*fileHost, func(tok string), func(tok string)) {
	h := newFileHost(t, data, rate)
	var mu sync.Mutex
	ok := map[string]bool{}
	h.route = func(w http.ResponseWriter, r *http.Request) bool {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 4 || parts[1] != "dl" {
			return false
		}
		mu.Lock()
		good := ok[parts[2]]
		mu.Unlock()
		if !good {
			http.Error(w, "link expired", 403)
			return true
		}
		return false
	}
	allow := func(tok string) { mu.Lock(); ok[tok] = true; mu.Unlock() }
	revoke := func(tok string) { mu.Lock(); delete(ok, tok); mu.Unlock() }
	return h, allow, revoke
}

func TestExpiredLinkRenewedFromThePage(t *testing.T) {
	data := randData(6 << 20)
	h, allow, revoke := expiringHost(t, data, 300<<10)
	h.cut = 512 << 10
	allow("tok1")
	m := newTestManager(t)
	m.settings.Conns = 4
	srv := (&Server{m: m, token: "t"}).routes()
	extCall(t, srv, "GET", "ping", nil) // the browser extension is around
	page := h.URL + "/files/page.html"
	d := m.Add(AddReq{URL: h.URL + "/dl/tok1/dataset.tar", Headers: map[string]string{"Referer": page}, Start: true})
	waitDone(t, m, d, 1<<20)
	revoke("tok1") // the link runs out part-way
	waitLink(t, m, d, "refreshing", 20*time.Second)
	m.mu.Lock()
	part := atomic.LoadInt64(&d.done)
	m.mu.Unlock()

	ping := extCall(t, srv, "GET", "ping", nil)
	jobs, _ := ping["refresh"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["page"] != page {
		t.Fatalf("the extension should be asked to open the page: %v", ping["refresh"])
	}
	allow("tok2")
	before := h.bytesSent("/dl/tok2/dataset.tar")
	r := extCall(t, srv, "POST", "refresh", map[string]any{"id": d.ID, "links": []map[string]string{
		{"url": h.URL + "/dl/tok2/readme.txt-but-other-size"}, // wrong size: a different file
		{"url": h.URL + "/dl/tok2/dataset.tar", "referer": page},
	}})
	if r["matched"] != true {
		t.Fatalf("refresh: %v", r)
	}
	waitFor(t, m, d, StCompleted, 40*time.Second)
	fileIs(t, d, data)
	fresh := h.bytesSent("/dl/tok2/dataset.tar") - before
	t.Logf("had %d KB when the link expired; the fresh link sent %d KB of %d KB", part>>10, fresh>>10, len(data)>>10)
	if fresh >= int64(len(data)) {
		t.Fatal("it should carry on, not start again")
	}
	if !strings.Contains(m.byID[d.ID].URL, "tok2") {
		t.Fatal("the download should keep the fresh link")
	}
}

func TestExpiredLinkPickedUpWhenClickedAgain(t *testing.T) {
	data := randData(4 << 20)
	h, allow, revoke := expiringHost(t, data, 300<<10)
	h.cut = 512 << 10
	allow("a")
	m := newTestManager(t)
	m.settings.Conns = 3
	srv := (&Server{m: m, token: "t"}).routes()
	d := m.Add(AddReq{URL: h.URL + "/dl/a/video.mp4", Headers: map[string]string{"Referer": h.URL + "/watch"}, Start: true})
	waitDone(t, m, d, 512<<10)
	revoke("a")
	waitLink(t, m, d, "expired", 20*time.Second) // no extension: it waits for the link
	m.mu.Lock()
	if !strings.Contains(d.Error, "click the download link again") {
		t.Fatalf("error should say what to do: %q", d.Error)
	}
	m.mu.Unlock()
	// the Add dialog recognises the file
	allow("b")
	info := apiCall(t, srv, "probe", map[string]any{"url": h.URL + "/dl/b/video.mp4", "early": true})
	if mt, _ := info["matches"].(map[string]any); mt == nil || mt["id"] != d.ID || info["early"] != nil {
		t.Fatalf("the Add dialog should offer to carry on the stopped download: %v", info)
	}
	// clicking the link on the page (the extension sends it) carries the download on
	extCall(t, srv, "POST", "add", map[string]any{"url": h.URL + "/dl/b/video.mp4", "referer": h.URL + "/watch"})
	for i := 0; i < 300; i++ { // the app checks the link in the background
		m.mu.Lock()
		l := d.Link
		m.mu.Unlock()
		if l == "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitFor(t, m, d, StCompleted, 40*time.Second)
	fileIs(t, d, data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.prompts) != 0 {
		t.Fatal("no Add dialog should open for a link that carried a download on")
	}
	if len(m.list) != 1 {
		t.Fatalf("it should not add a second download: %d", len(m.list))
	}
}

func TestRenewChecksItsTheSameFile(t *testing.T) {
	data := randData(1 << 20)
	h, allow, _ := expiringHost(t, data, 0)
	other := newFileHost(t, randData(900<<10), 0)
	allow("x")
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	d := m.Add(AddReq{URL: h.URL + "/dl/old/report.pdf", Name: "report.pdf", Size: 1 << 20})
	m.mu.Lock()
	d.Status, d.Resumable = StError, true
	m.mu.Unlock()
	if r := apiCall(t, srv, "renew", map[string]any{"id": d.ID, "url": other.URL + "/report.pdf"}); r["error"] == nil {
		t.Fatalf("a different file must be refused: %v", r)
	}
	if r := apiCall(t, srv, "renew", map[string]any{"id": d.ID, "url": h.URL + "/dl/x/report.pdf"}); r["ok"] != true {
		t.Fatalf("renew: %v", r)
	}
	waitFor(t, m, d, StCompleted, 20*time.Second)
	fileIs(t, d, data)
}

func TestLinkExpiry(t *testing.T) {
	now := time.Now().UTC()
	amz := fmt.Sprintf("https://b.s3.amazonaws.com/f?X-Amz-Date=%s&X-Amz-Expires=600&X-Amz-Signature=abc", now.Format("20060102T150405Z"))
	cases := map[string]time.Time{
		amz: now.Add(10 * time.Minute).Truncate(time.Second),
		"https://d.cloudfront.net/f?Expires=1900000000&Signature=x":            time.Unix(1900000000, 0),
		"https://acct.blob.core.windows.net/c/f?se=2027-01-02T03:04:05Z&sig=x": time.Date(2027, 1, 2, 3, 4, 5, 0, time.UTC),
		"https://example.com/f?e=12":                                           {},
		"https://example.com/plain.zip":                                        {},
		"https://example.com/f?expires=abc":                                    {},
	}
	for u, want := range cases {
		if got := linkExpiry(u); !got.Equal(want) {
			t.Errorf("%s: %v, want %v", u, got, want)
		}
	}
}

// ---------------------------------------------------------------- checksums

func sha256hex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func TestParseChecksum(t *testing.T) {
	h := sha256hex([]byte("x"))
	md := "9dd4e461268c8034f5c8564e155c67a6"
	b64 := base64.StdEncoding.EncodeToString(func() []byte { s := sha256.Sum256([]byte("x")); return s[:] }())
	cases := []struct{ in, algo, want string }{
		{h, "sha256", h},
		{strings.ToUpper(h), "sha256", h},
		{"sha256:" + h, "sha256", h},
		{"SHA-256 " + h, "sha256", h},
		{md + "  *setup.exe", "md5", md},
		{"SHA256 (setup.exe) = " + h, "sha256", h},
		{"aaaa" + h[4:] + "  other.iso\n" + h + "  setup.exe\n", "sha256", h},
		{"sha-256=" + b64, "sha256", h},
	}
	for _, c := range cases {
		got, err := parseChecksum(c.in, "setup (1).exe")
		if err != nil || got.Algo != c.algo || got.Want != c.want {
			t.Errorf("%q: %+v %v", c.in, got, err)
		}
	}
	for _, bad := range []string{"hello", "abc123", h + "  a.iso\n" + h[:63] + "f  b.iso"} {
		if _, err := parseChecksum(bad, "setup.exe"); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestChecksumYouGiveIsChecked(t *testing.T) {
	data := randData(3 << 20)
	h := newFileHost(t, data, 0)
	m := newTestManager(t)
	good := m.Add(AddReq{URL: h.URL + "/good.iso", Checksum: "sha256:" + sha256hex(data), Start: true})
	bad := m.Add(AddReq{URL: h.URL + "/bad.iso", Checksum: sha256hex([]byte("something else")), Start: true})
	waitFor(t, m, good, StCompleted, 20*time.Second)
	waitFor(t, m, bad, StCompleted, 20*time.Second)
	m.mu.Lock()
	defer m.mu.Unlock()
	if good.Check.State != "ok" {
		t.Fatalf("good: %+v", good.Check)
	}
	if bad.Check.State != "bad" || bad.Check.Got != sha256hex(data) {
		t.Fatalf("bad: %+v", bad.Check)
	}
	found := false
	for _, n := range m.notices {
		found = found || n.Kind == "checkBad" && n.ID == bad.ID
	}
	if !found {
		t.Fatal("a damaged file should be reported")
	}
	if strings.HasSuffix(bad.FileName, partSuffix) {
		t.Fatal("a file that doesn't match still gets its name (to look at it)")
	}
}

func TestChecksumFoundNextToTheFile(t *testing.T) {
	old := sumMinSize
	sumMinSize = 1 << 20
	defer func() { sumMinSize = old }()
	data := randData(2 << 20)
	h := newFileHost(t, data, 0)
	h.route = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/releases/v2/SHA256SUMS":
			fmt.Fprintf(w, "%s  tool-v2-linux.tar.gz\n%s *tool-v2.iso\n", sha256hex([]byte("a")), sha256hex(data))
			return true
		case "/releases/v2/tool-v2.iso.sha256", "/releases/v2/tool-v2.iso.sha256sum", "/releases/v2/tool-v2.iso.sha512",
			"/releases/v2/tool-v2.iso.sha512sum", "/releases/v2/tool-v2.iso.sha1", "/releases/v2/tool-v2.iso.md5":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>Not here</html>") // a site that answers everything with a page
			return true
		}
		if strings.HasPrefix(r.URL.Path, "/releases/v2/") && !strings.HasSuffix(r.URL.Path, ".iso") {
			http.NotFound(w, r)
			return true
		}
		return false
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: h.URL + "/releases/v2/tool-v2.iso", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.Check == nil || d.Check.State != "ok" || d.Check.From != "SHA256SUMS" {
		t.Fatalf("checksum: %+v", d.Check)
	}
}

func TestChecksumFromServerHeaders(t *testing.T) {
	data := randData(1 << 20)
	sum := sha256.Sum256(data)
	h := newFileHost(t, data, 0)
	h.route = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Digest", "MD5=AAAAAAAAAAAAAAAAAAAAAA==, SHA-256="+base64.StdEncoding.EncodeToString(sum[:]))
		return false
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: h.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.Check == nil || d.Check.Algo != "sha256" || d.Check.State != "ok" || d.Check.From != "server" {
		t.Fatalf("checksum: %+v", d.Check)
	}
}

func TestChecksumOfAFinishedFile(t *testing.T) {
	data := randData(1 << 20)
	h := newFileHost(t, data, 0)
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	d := m.Add(AddReq{URL: h.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	apiCall(t, srv, "checksum", map[string]any{"id": d.ID, "calc": true})
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		got := d.Check.Got
		m.mu.Unlock()
		if got != "" {
			if got != sha256hex(data) {
				t.Fatalf("calculated %s", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no checksum calculated")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r := apiCall(t, srv, "checksum", map[string]any{"id": d.ID, "want": "md5:00000000000000000000000000000000"})
	if r["checking"] != true {
		t.Fatalf("%v", r)
	}
	for i := 0; i < 250; i++ {
		m.mu.Lock()
		st := d.Check.State
		m.mu.Unlock()
		if st == "bad" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("a wrong checksum should be found out")
}

// ---------------------------------------------------------------- metalink

func metalinkDoc(name string, data []byte, piece int, urls ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<metalink xmlns="urn:ietf:params:xml:ns:metalink">
  <file name="%s">
    <size>%d</size>
    <hash type="sha-256">%s</hash>
    <pieces length="%d" type="sha-1">`, name, len(data), sha256hex(data), piece)
	for off := 0; off < len(data); off += piece {
		s := sha1.Sum(data[off:min(off+piece, len(data))])
		fmt.Fprintf(&b, "<hash>%x</hash>", s)
	}
	b.WriteString("</pieces>\n")
	for i, u := range urls {
		fmt.Fprintf(&b, "    <url priority=\"%d\">%s</url>\n", i+1, u)
	}
	b.WriteString(`  </file>
</metalink>`)
	return b.String()
}

func TestParseMetalink(t *testing.T) {
	data := randData(300 << 10)
	files, err := parseMetalink(strings.NewReader(metalinkDoc("../evil/name.iso", data, 128<<10, "http://a/x", "ftp://b/x", "magnet:?xt=1")))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	f := files[0]
	if f.Name != "name.iso" || f.Size != int64(len(data)) || len(f.URLs) != 2 || f.Hash.Want != sha256hex(data) || f.Pieces == nil || len(f.Pieces.Hashes) != 3 {
		t.Fatalf("%+v", f)
	}
	v3 := `<metalink version="3.0" xmlns="http://www.metalinker.org/"><files><file name="old.zip"><size>10</size>
<verification><hash type="md5">9dd4e461268c8034f5c8564e155c67a6</hash></verification>
<resources><url type="http" preference="10">http://slow/old.zip</url><url type="http" preference="100">http://fast/old.zip</url></resources></file></files></metalink>`
	files, err = parseMetalink(strings.NewReader(v3))
	if err != nil || len(files) != 1 || files[0].URLs[0].URL != "http://fast/old.zip" || files[0].Hash.Algo != "md5" {
		t.Fatalf("v3: %+v %v", files, err)
	}
	if _, err := parseMetalink(strings.NewReader("<html><body>hi</body></html>")); err == nil {
		t.Fatal("not a metalink")
	}
}

func TestMetalinkRepairsDamagedPieces(t *testing.T) {
	data := randData(4 << 20)
	good := newFileHost(t, data, 600<<10)
	broken := newFileHost(t, data, 600<<10)
	broken.mangle = func(b []byte, _ int64) { // this mirror has a damaged copy: a few bytes here and there
		for _, at := range []int{300 << 10, 1<<20 + 1000, 2<<20 + 77, 3<<20 + 500<<10} {
			for i := at; i < at+2000; i++ {
				b[i] ^= 0xff
			}
		}
	}
	ml := newFileHost(t, nil, 0)
	doc := metalinkDoc("disk.img", data, 256<<10, broken.URL+"/disk.img", good.URL+"/disk.img")
	ml.route = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "application/metalink4+xml")
		fmt.Fprint(w, doc)
		return true
	}
	m := newTestManager(t)
	m.settings.Conns = 6
	srv := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, srv, "probe", map[string]any{"url": ml.URL + "/disk.img.meta4"})
	mlv, _ := info["metalink"].(map[string]any)
	if mlv == nil || info["name"] != "disk.img" {
		t.Fatalf("probe: %v", info)
	}
	r := apiCall(t, srv, "add", map[string]any{"url": ml.URL + "/disk.img.meta4", "meta": true, "metaFile": 0, "start": true})
	d := m.byID[r["id"].(string)]
	waitFor(t, m, d, StCompleted, 60*time.Second)
	fileIs(t, d, data)
	m.mu.Lock()
	defer m.mu.Unlock()
	t.Logf("note: %q; good sent %d KB, broken sent %d KB", d.Note, good.bytesSent("")>>10, broken.bytesSent("")>>10)
	if d.Check.State != "ok" {
		t.Fatalf("check: %+v", d.Check)
	}
	if broken.bytesSent("") > 0 && good.bytesSent("") > 0 && broken.bytesSent("") >= int64(len(data)) {
		t.Fatal("the damaged mirror shouldn't be trusted with the whole file")
	}
}

func TestMirrorsAnnouncedByTheServer(t *testing.T) {
	// (16 pieces: a download starts with 8 connections and adds more after a second; the mirror,
	// checked meanwhile, gets the pieces left for them)
	data := randData(24 << 20)
	sum := sha256.Sum256(data)
	mirror := newFileHost(t, data, 500<<10)
	main := newFileHost(t, data, 500<<10)
	main.route = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Add("Link", fmt.Sprintf(`<%s/pub/big.bin>; rel=duplicate; pri=1; geo=de, <%s/big.bin.meta4>; rel=describedby; type="application/metalink4+xml"`, mirror.URL, main.URL))
		w.Header().Set("Digest", "SHA-256="+base64.StdEncoding.EncodeToString(sum[:]))
		return false
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: main.URL + "/big.bin", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(d.Mirrors) != 1 || mirror.bytesSent("") < 256<<10 {
		t.Fatalf("the announced mirror should help: %v, sent %d", d.Mirrors, mirror.bytesSent(""))
	}
	if d.Check == nil || d.Check.State != "ok" {
		t.Fatalf("check %+v", d.Check)
	}
}

func TestPublishedChecksumSkippedForSmallFiles(t *testing.T) {
	data := randData(256 << 10)
	h := newFileHost(t, data, 0)
	m := newTestManager(t)
	d := m.Add(AddReq{URL: h.URL + "/dir/small.zip", Start: true})
	waitFor(t, m, d, StCompleted, 10*time.Second)
	if n := h.hitCount("/dir/SHA256SUMS"); n != 0 {
		t.Fatalf("small files shouldn't send the server looking for checksum files (%d)", n)
	}
	_ = filepath.Join
}

// A line that drops every connection after a while still finishes: drops after real
// progress don't count against the retry limit.
func TestFlakyConnectionsKeepGoing(t *testing.T) {
	data := randData(6 << 20)
	h := newFileHost(t, data, 0)
	h.cut = 128 << 10
	m := newTestManager(t)
	m.settings.Conns = 2
	d := m.Add(AddReq{URL: h.URL + "/big.bin", Start: true})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	fileIs(t, d, data)
}

// A checksum given while the early start had already finished is still checked.
func TestEarlyStartChecksumChecked(t *testing.T) {
	data := randData(1 << 20)
	h := newFileHost(t, data, 0)
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, srv, "probe", map[string]any{"url": h.URL + "/f.bin", "early": true})
	id, _ := info["early"].(string)
	if id == "" {
		t.Fatalf("no early start: %v", info)
	}
	d := earlyDownload(m, id)
	waitFor(t, m, d, StCompleted, 20*time.Second)
	apiCall(t, srv, "early", map[string]any{"action": "commit", "id": id, "start": true, "checksum": "sha256:" + sha256hex(data)})
	for i := 0; i < 200; i++ {
		m.mu.Lock()
		st := d.Check.State
		m.mu.Unlock()
		if st == "ok" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the file should be checked: %+v", d.Check)
}
