package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------- tiny FTP server

type testFTP struct {
	ln          net.Listener
	data        []byte
	rate        int // bytes/sec per data connection, 0 = unlimited
	noRest      bool
	logins      int32
	retrs       int32
	lastUser    atomic.Value
	foreignData int32 // data connections from another address than their control connection
	mu          sync.Mutex
	sources     []string
}

func newTestFTP(t *testing.T, data []byte, rate int, noRest bool) *testFTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testFTP{ln: ln, data: data, rate: rate, noRest: noRest}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *testFTP) url(p string) string { return "ftp://" + s.ln.Addr().String() + p }

func (s *testFTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(f string, a ...any) { fmt.Fprintf(c, f+"\r\n", a...) }
	say("220-Welcome\r\n220 test server")
	var pasv net.Listener
	var rest int64
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(cmd) {
		case "USER":
			s.lastUser.Store(arg)
			say("331 password please")
		case "PASS":
			atomic.AddInt32(&s.logins, 1)
			say("230 logged in")
		case "TYPE":
			say("200 binary")
		case "SIZE":
			if arg == "/pub/file.bin" {
				say("213 %d", len(s.data))
			} else {
				say("550 no such file")
			}
		case "MDTM":
			say("213 20260101120000")
		case "EPSV":
			say("500 not supported") // force the PASV path
		case "PASV":
			pasv, _ = net.Listen("tcp", "127.0.0.1:0")
			p := pasv.Addr().(*net.TCPAddr).Port
			say("227 Entering Passive Mode (10,9,9,9,%d,%d)", p/256, p%256) // bogus IP on purpose
		case "REST":
			if s.noRest {
				say("502 REST not implemented")
				continue
			}
			rest, _ = strconv.ParseInt(arg, 10, 64)
			say("350 restarting at %d", rest)
		case "RETR":
			if arg != "/pub/file.bin" || pasv == nil {
				say("550 no such file")
				continue
			}
			atomic.AddInt32(&s.retrs, 1)
			say("150 opening data connection")
			dc, err := pasv.Accept()
			pasv.Close()
			pasv = nil
			if err != nil {
				return
			}
			ch, _, _ := net.SplitHostPort(c.RemoteAddr().String())
			dh, _, _ := net.SplitHostPort(dc.RemoteAddr().String())
			if ch != dh {
				atomic.AddInt32(&s.foreignData, 1) // a strict server would refuse this
			}
			s.mu.Lock()
			s.sources = append(s.sources, ch)
			s.mu.Unlock()
			src := s.data[rest:]
			rest = 0
			start, sent := time.Now(), 0
			for len(src) > 0 {
				n := 32 * 1024
				if n > len(src) {
					n = len(src)
				}
				if _, err := dc.Write(src[:n]); err != nil {
					break
				}
				src, sent = src[n:], sent+n
				if s.rate > 0 {
					if want := time.Duration(float64(sent) / float64(s.rate) * float64(time.Second)); time.Since(start) < want {
						time.Sleep(want - time.Since(start))
					}
				}
			}
			dc.Close()
			say("226 done")
		case "MLSD":
			listing := map[string]string{
				"/pub":      "type=cdir; .\r\ntype=file;size=12; file.bin\r\ntype=dir; sub\r\ntype=file;size=3; notes.txt\r\n",
				"/pub/":     "type=cdir; .\r\ntype=file;size=12; file.bin\r\ntype=dir; sub\r\ntype=file;size=3; notes.txt\r\n",
				"/pub/sub/": "type=file;size=9; deep.zip\r\n",
			}[arg]
			if pasv == nil {
				say("425 use PASV first")
				continue
			}
			say("150 listing")
			dc, err := pasv.Accept()
			pasv.Close()
			pasv = nil
			if err != nil {
				return
			}
			io.WriteString(dc, listing)
			dc.Close()
			say("226 done")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 command not implemented")
		}
	}
}

func newTestManager(t *testing.T) *Manager {
	// not t.TempDir: the manager saves its list in the background, which can race its
	// strict cleanup. Stop the downloads, then remove the folder with a few retries.
	dir, err := os.MkdirTemp("", "zltest")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(dir)
	m.settings.DownloadDir = dir
	<-m.meterReady() // the first metered check is done: a test's own m.metered stays put
	t.Cleanup(func() {
		m.mu.Lock()
		var ids []string
		for _, d := range m.list {
			ids = append(ids, d.ID)
		}
		m.mu.Unlock()
		m.Pause(ids)
		for i := 0; i < 40; i++ {
			os.RemoveAll(dir)
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return m
}

func fileSum(t *testing.T, d *Download) [32]byte {
	b, err := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestFTPMultiConnectionAndResume(t *testing.T) {
	data := make([]byte, 12<<20)
	rand.Read(data)
	srv := newTestFTP(t, data, 2<<20, false) // 2 MB/s per connection
	m := newTestManager(t)
	m.settings.Conns = 8
	t0 := time.Now()
	d := m.Add(AddReq{URL: srv.url("/pub/file.bin"), Start: true})
	time.Sleep(300 * time.Millisecond)
	m.Pause([]string{d.ID})
	waitFor(t, m, d, StPaused, 10*time.Second)
	for {
		m.mu.Lock()
		r := d.running
		m.mu.Unlock()
		if !r {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&d.done); got <= 0 || got >= int64(len(data)) {
		t.Fatalf("unexpected pause point %d", got)
	}
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	el := time.Since(t0)
	if fileSum(t, d) != sha256.Sum256(data) {
		t.Fatal("FTP checksum mismatch")
	}
	t.Logf("FTP 12MB @2MB/s per conn: %v (%.1f MB/s), logins=%d retrs=%d", el, 12/el.Seconds(), srv.logins, srv.retrs)
	if el > 3*time.Second { // single connection would need 6 s
		t.Fatalf("FTP too slow: %v", el)
	}
	if !d.Resumable || d.LastMod == "" {
		t.Fatalf("expected resumable with MDTM, got resumable=%v lastMod=%q", d.Resumable, d.LastMod)
	}
}

func TestFTPWithoutRest(t *testing.T) {
	data := make([]byte, 2<<20)
	rand.Read(data)
	srv := newTestFTP(t, data, 0, true)
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.url("/pub/file.bin"), Start: true})
	waitFor(t, m, d, StCompleted, 15*time.Second)
	if fileSum(t, d) != sha256.Sum256(data) || d.Resumable {
		t.Fatal("single-stream FTP failed")
	}
}

func TestFTPMissingFile(t *testing.T) {
	srv := newTestFTP(t, []byte("x"), 0, false)
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.url("/pub/nope.bin"), Start: true})
	waitFor(t, m, d, StError, 15*time.Second)
	if !strings.Contains(d.Error, "not found") {
		t.Fatalf("unexpected error %q", d.Error)
	}
}

// ---------------------------------------------------------------- proxy

func TestManualProxyWithAuth(t *testing.T) {
	data := make([]byte, 3<<20)
	rand.Read(data)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.bin", time.Now(), bytes.NewReader(data))
	}))
	defer origin.Close()
	var hits int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bob:s3cret"))
		if r.Header.Get("Proxy-Authorization") != want {
			w.Header().Set("Proxy-Authenticate", `Basic realm="p"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		atomic.AddInt32(&hits, 1)
		// forward files.test → origin
		u := *r.URL
		u.Host = strings.TrimPrefix(origin.URL, "http://")
		req, _ := http.NewRequest(r.Method, u.String(), nil)
		req.Header = r.Header.Clone()
		req.Header.Del("Proxy-Authorization")
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	m := newTestManager(t)
	m.settings.Conns = 4
	defer setProxySettings(ProxySettings{Mode: "system"})

	// wrong password → clear error
	setProxySettings(ProxySettings{Mode: "manual", URL: proxy.URL, User: "bob", Pass: "nope"})
	bad := m.Add(AddReq{URL: "http://files.test/f.bin", Start: true})
	waitFor(t, m, bad, StError, 15*time.Second)
	if !strings.Contains(bad.Error, "proxy") {
		t.Fatalf("expected proxy auth error, got %q", bad.Error)
	}

	setProxySettings(ProxySettings{Mode: "manual", URL: strings.TrimPrefix(proxy.URL, "http://"), User: "bob", Pass: "s3cret"})
	d := m.Add(AddReq{URL: "http://files.test/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	if fileSum(t, d) != sha256.Sum256(data) {
		t.Fatal("checksum via proxy mismatch")
	}
	if atomic.LoadInt32(&hits) < 2 {
		t.Fatalf("proxy not used for all connections (hits=%d)", hits)
	}
	// bypass list sends files.test direct (which can't resolve) → proves bypass is honoured
	pu, _ := proxyForWith(ProxySettings{Mode: "manual", URL: proxy.URL, Bypass: "*.test;<local>"}, &url.URL{Scheme: "http", Host: "files.test"})
	if pu != nil {
		t.Fatal("bypass list ignored")
	}
}

func TestPickProxyFormats(t *testing.T) {
	cases := []struct{ list, scheme, want string }{
		{"proxy.corp:8080", "https", "proxy.corp:8080"},
		{"http=a:1;https=b:2", "https", "b:2"},
		{"http=a:1;https=b:2", "http", "a:1"},
		{"PROXY p.corp:3128; DIRECT", "https", "p.corp:3128"},
		{"socks=s:1080", "https", "socks5://s:1080"},
		{"DIRECT", "http", ""},
	}
	for _, c := range cases {
		if got := pickProxy(c.list, c.scheme); got != c.want {
			t.Errorf("pickProxy(%q,%q)=%q want %q", c.list, c.scheme, got, c.want)
		}
	}
	if !bypassed("intranet", "<local>") || bypassed("example.com", "<local>") || !bypassed("a.corp.com", "*.corp.com") || !bypassed("10.1.2.3", "10.*") {
		t.Error("bypass matching wrong")
	}
}

// ---------------------------------------------------------------- queues, limiter, sync

func slowServer(data []byte, rate int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(&slowWriter{w: w, rate: rate, start: time.Now()}, r, "f.bin", time.Now(), bytes.NewReader(data))
	}))
}

func TestMultipleQueuesRespectTheirLimits(t *testing.T) {
	data := make([]byte, 1<<20)
	rand.Read(data)
	srv := slowServer(data, 1<<20)
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Conns = 1
	m.settings.Queues = append(m.settings.Queues, Queue{ID: "night", Name: "Night", MaxConcurrent: 2})
	m.settings.Queues[0].MaxConcurrent = 1
	m.mu.Unlock()
	var all []*Download
	for i := 0; i < 3; i++ {
		all = append(all, m.Add(AddReq{URL: fmt.Sprintf("%s/a%d.bin", srv.URL, i), QueueID: "main"}))
		all = append(all, m.Add(AddReq{URL: fmt.Sprintf("%s/b%d.bin", srv.URL, i), QueueID: "night"}))
	}
	m.StartQueue("main", false)
	m.StartQueue("night", false)
	maxA, maxB := 0, 0
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		a, b, done := 0, 0, 0
		m.mu.Lock()
		for _, d := range all {
			if d.running && d.Queue == "main" {
				a++
			}
			if d.running && d.Queue == "night" {
				b++
			}
			if d.Status == StCompleted {
				done++
			}
		}
		m.mu.Unlock()
		if a > maxA {
			maxA = a
		}
		if b > maxB {
			maxB = b
		}
		if done == len(all) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range all {
		if d.Status != StCompleted {
			t.Fatalf("%s not completed: %s %s", d.FileName, d.Status, d.Error)
		}
		if d.Queue != "" {
			t.Fatalf("finished download should leave a normal queue")
		}
	}
	if maxA != 1 || maxB != 2 {
		t.Fatalf("concurrency: main max %d (want 1), night max %d (want 2)", maxA, maxB)
	}
	if m.qRun["main"] || m.qRun["night"] {
		t.Fatal("queues should stop when empty")
	}
}

func TestPerDownloadSpeedLimit(t *testing.T) {
	data := make([]byte, 1<<20)
	rand.Read(data)
	srv := slowServer(data, 0)
	defer srv.Close()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/f.bin"})
	m.mu.Lock()
	d.LimitKB = 400
	m.mu.Unlock()
	t0 := time.Now()
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	el := time.Since(t0)
	if el < 2*time.Second {
		t.Fatalf("limit not applied: 1MB at 400KB/s took %v", el)
	}
	t.Logf("1MB at 400 KB/s took %v", el)
}

func TestSyncQueueUpdatesChangedFiles(t *testing.T) {
	var mu sync.Mutex
	content := []byte("version one")
	etag := `"v1"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		c, e := content, etag
		mu.Unlock()
		w.Header().Set("ETag", e)
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(c))
	}))
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Queues = append(m.settings.Queues, Queue{ID: "sync", Name: "Mirror", MaxConcurrent: 2, Sync: true})
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.URL + "/notes.txt", QueueID: "sync"})
	m.StartQueue("sync", false)
	waitFor(t, m, d, StCompleted, 10*time.Second)
	waitQueueStopped(t, m, "sync")
	if d.Queue != "sync" {
		t.Fatal("finished file should stay in a sync queue")
	}

	// unchanged → stays
	m.StartQueue("sync", false)
	waitQueueStopped(t, m, "sync")
	if d.SyncNote != "Up to date" {
		t.Fatalf("expected up to date, got %q", d.SyncNote)
	}

	// changed on server → re-downloaded into the same file
	mu.Lock()
	content, etag = []byte("version two, longer"), `"v2"`
	mu.Unlock()
	m.StartQueue("sync", false)
	waitQueueStopped(t, m, "sync")
	waitFor(t, m, d, StCompleted, 10*time.Second)
	b, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if string(b) != "version two, longer" || d.FileName != "notes.txt" {
		t.Fatalf("sync update failed: name=%s content=%q", d.FileName, b)
	}
	if entries, _ := os.ReadDir(d.Dir); len(entries) != 1 {
		t.Fatalf("expected the old file to be replaced, found %d files", len(entries))
	}
}

func waitQueueStopped(t *testing.T, m *Manager, id string) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 300; i++ {
		m.mu.Lock()
		r := m.qRun[id] || m.qSyncing[id]
		m.mu.Unlock()
		if !r {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("queue did not finish")
}
