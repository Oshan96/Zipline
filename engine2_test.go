package main

// Tests for the engine work of October 2026: smarter splitting (B1), connection reuse (B2),
// adaptive connection counts (B3), per-server budgets and the "downloads at once" cap (B4),
// an honest User-Agent and bot checks (B5), crash-safe progress (C1), checked ranged replies
// (C2), retries that survive outages (C3) and making sure new sources serve the same file (C7).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// peakServer serves data at rate bytes/s per connection, counting requests, new TCP connections
// and how many replies are being sent at once.
type peakServer struct {
	*httptest.Server
	data                     []byte
	rate                     int
	reqs, conns, active, max int32
	modtime                  time.Time
	handle                   func(w http.ResponseWriter, r *http.Request) bool // takes over a request first
	uas                      sync.Map                                          // User-Agents seen
}

func newPeakServer(t *testing.T, data []byte, rate int) *peakServer {
	s := &peakServer{data: data, rate: rate, modtime: time.Unix(1700000000, 0)}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.reqs, 1)
		s.uas.Store(r.Header.Get("User-Agent"), true)
		if s.handle != nil && s.handle(w, r) {
			return
		}
		n := atomic.AddInt32(&s.active, 1)
		defer atomic.AddInt32(&s.active, -1)
		for {
			p := atomic.LoadInt32(&s.max)
			if n <= p || atomic.CompareAndSwapInt32(&s.max, p, n) {
				break
			}
		}
		http.ServeContent(&slowWriter{w: w, rate: s.rate, start: time.Now()}, r, "f.bin", s.modtime, bytes.NewReader(s.data))
	}))
	s.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			atomic.AddInt32(&s.conns, 1)
		}
	}
	s.Start()
	t.Cleanup(s.Close)
	return s
}

// setVar replaces a timing variable for one test.
func setVar[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// stopAll pauses the manager's downloads and waits for them to let go (before timing variables
// are put back).
func stopAll(t *testing.T, m *Manager) {
	t.Cleanup(func() {
		m.mu.Lock()
		var ds []*Download
		for _, d := range m.list {
			ds = append(ds, d)
		}
		m.mu.Unlock()
		for _, d := range ds {
			m.waitStopped(d)
		}
	})
}

// ---------------------------------------------------------------- B1

func TestSplitOnlyWhenItPays(t *testing.T) {
	now := time.Now()
	seg := func(start, end, pos int64, age time.Duration, rate float64) *Segment {
		return &Segment{Start: start, End: end, Pos: pos, active: true, t0: now.Add(-age), p0: pos - int64(rate*age.Seconds())}
	}
	// 1 MB left at 1 MB/s: the new half would take half a second
	d := &Download{Segments: []*Segment{seg(0, 4<<20-1, 3<<20, 2*time.Second, 1<<20)}}
	if ns := d.nextSegment(); ns != nil {
		t.Fatalf("split a segment that ends in a second: %+v", ns)
	}
	// 8 MB left at 1 MB/s: split at the midpoint
	d = &Download{Segments: []*Segment{seg(0, 10<<20-1, 2<<20, 2*time.Second, 1<<20)}}
	ns := d.nextSegment()
	if ns == nil || ns.Start != 6<<20 || ns.End != 10<<20-1 || d.Segments[0].End != 6<<20-1 {
		t.Fatalf("slow big remainder: %+v / %+v", ns, d.Segments[0])
	}
	// the longest time left is split, not the most bytes
	fast, slow := seg(0, 20<<20-1, 2<<20, 2*time.Second, 8<<20), seg(20<<20, 26<<20-1, 21<<20, 2*time.Second, 512<<10)
	d = &Download{Segments: []*Segment{fast, slow}}
	if ns := d.nextSegment(); ns == nil || ns.Start <= slow.Start || ns.End != 26<<20-1 {
		t.Fatalf("should split the slow segment: %+v", ns)
	}
	// speed not known yet (young segment): only the size counts
	d = &Download{Segments: []*Segment{seg(0, 3*minSplit-1, 0, 100*time.Millisecond, 0)}}
	if ns := d.nextSegment(); ns == nil || ns.End-ns.Start+1 < minSplit || d.Segments[0].End-d.Segments[0].Start+1 < minSplit {
		t.Fatalf("young segment: %+v", ns)
	}
	d = &Download{Segments: []*Segment{seg(0, 2*minSplit-2, 0, 0, 0)}}
	if ns := d.nextSegment(); ns != nil {
		t.Fatal("a piece smaller than minSplit was made")
	}
	// a crawling connection keeps a small slice, never less than minSplit
	crawl, ok := seg(0, 8<<20-1, 1<<20, 3*time.Second, 10<<10), seg(8<<20, 16<<20-1, 9<<20, 3*time.Second, 4<<20)
	d = &Download{Segments: []*Segment{crawl, ok}}
	if ns := d.nextSegment(); ns == nil || crawl.End-crawl.Pos+1 != minSplit {
		t.Fatalf("crawler keeps %d bytes", crawl.End-crawl.Pos+1)
	}
}

// ---------------------------------------------------------------- B2

func TestConnectionsAreReused(t *testing.T) {
	data := randData(16 << 20)
	srv := newPeakServer(t, data, 0)
	m := newTestManager(t)
	m.settings.Conns = 16 // 16 pieces of 1 MB for 8 connections to work through
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	reqs, conns := atomic.LoadInt32(&srv.reqs), atomic.LoadInt32(&srv.conns)
	t.Logf("%d requests over %d connections", reqs, conns)
	if reqs < 16 || conns > reqs*3/4 { // (8 connections at first: each should carry about two pieces)
		t.Fatalf("connections aren't reused: %d requests over %d connections", reqs, conns)
	}
}

// A reply cut short because its segment was split is read out when little is left, so the
// connection is reused.
func TestDrainLeftoverReply(t *testing.T) {
	data := randData(1 << 20)
	srv := newPeakServer(t, data, 0)
	m := newTestManager(t)
	d := &Download{URL: srv.URL + "/f.bin", Size: int64(len(data))}
	b, err := m.openRange(context.Background(), d, 0, 300<<10-1)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 100<<10)
	if _, err := io.ReadFull(b, buf); err != nil {
		t.Fatal(err)
	}
	m.drainRest(b) // 200 KB left: read it out
	b.Close()
	b2, err := m.openRange(context.Background(), d, 0, 600<<10-1)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(b2, buf)
	m.drainRest(b2) // 500 KB left: too much, closed instead
	b2.Close()
	b3, err := m.openRange(context.Background(), d, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, b3)
	b3.Close()
	if c := atomic.LoadInt32(&srv.conns); c != 2 {
		t.Fatalf("%d connections for 3 requests (want 2: the drained one reused)", c)
	}
}

// ---------------------------------------------------------------- B3

func TestMoreConnectionsWhileItPays(t *testing.T) {
	setVar(t, &ctlEvery, 300*time.Millisecond)
	data := randData(8 << 20)
	srv := newPeakServer(t, data, 512<<10) // each connection is capped
	m := newTestManager(t)
	stopAll(t, m)
	m.settings.Conns = 16
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	t.Logf("capped server: up to %d connections at once", srv.max)
	if atomic.LoadInt32(&srv.max) <= 4 {
		t.Fatalf("stayed at %d connections although each one is capped", srv.max)
	}
}

func TestFastServerKeepsFewConnections(t *testing.T) {
	data := randData(24 << 20)
	srv := newPeakServer(t, data, 0)
	m := newTestManager(t)
	m.settings.Conns = 32
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	t.Logf("fast server: up to %d connections at once, %d requests", srv.max, srv.reqs)
	// at most one doubling (8 → 16) that didn't pay, never the full 32
	if atomic.LoadInt32(&srv.max) > 16 {
		t.Fatalf("opened %d connections to a server that's fast with a few", srv.max)
	}
}

// A connection that gives up is replaced while the others keep going.
func TestFailedConnectionIsReplaced(t *testing.T) {
	setVar(t, &maxTries, 1)
	setVar(t, &ctlEvery, 200*time.Millisecond)
	setVar(t, &onlineCheck, func() bool { return true })
	data := randData(4 << 20)
	srv := newPeakServer(t, data, 512<<10) // 4 pieces of 1 MB: 2 s each
	var mu sync.Mutex
	var tries []time.Time
	srv.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.Header.Get("Range"), "bytes=1048576-") {
			return false
		}
		mu.Lock()
		tries = append(tries, time.Now())
		n := len(tries)
		mu.Unlock()
		if n == 1 { // the second piece fails: its connection gives up (and the server rests for a second)
			http.Error(w, "oops", 500)
			return true
		}
		http.ServeContent(w, r, "f.bin", srv.modtime, bytes.NewReader(data))
		return true
	}
	m := newTestManager(t)
	stopAll(t, m)
	m.settings.Conns = 4
	t0 := time.Now()
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	mu.Lock()
	defer mu.Unlock()
	// the other pieces take 2 s: a try before that is a new connection, not one that finished its piece
	if len(tries) < 2 || tries[1].Sub(t0) > 1800*time.Millisecond {
		t.Fatalf("the failed connection wasn't replaced while the others ran: %d tries, %v", len(tries), tries[len(tries)-1].Sub(t0))
	}
	t.Logf("piece 2 tried %d times, the last %v after the start", len(tries), tries[len(tries)-1].Sub(t0))
}

// A server's learned connection limit goes back up a while later (never past its site rule).
func TestLearnedLimitCreepsBack(t *testing.T) {
	setVar(t, &learnedCreep, 50*time.Millisecond)
	s := &source{learned: 2, learnedAt: time.Now(), siteMax: 3}
	if l := s.limit(time.Now()); l != 2 {
		t.Fatalf("limit %d", l)
	}
	if l := s.limit(time.Now().Add(time.Second)); l != 3 {
		t.Fatalf("after a while: %d", l)
	}
	if l := s.limit(time.Now().Add(time.Hour)); l != 3 {
		t.Fatalf("past the site rule: %d", l)
	}
	free := &source{learned: 2, learnedAt: time.Now().Add(-time.Minute)}
	if l := free.limit(time.Now()); l != 3 {
		t.Fatalf("without a site rule: %d", l)
	}
}

// ---------------------------------------------------------------- B4

func TestServerSharedFairlyBetweenDownloads(t *testing.T) {
	setVar(t, &ctlEvery, 200*time.Millisecond)
	data := randData(6 << 20)
	srv := newPeakServer(t, data, 512<<10)
	m := newTestManager(t)
	stopAll(t, m)
	m.mu.Lock()
	m.settings.Conns = 4 // the budget for this server
	m.applyLimit()
	m.mu.Unlock()
	add := func(name string) *Download {
		d := m.Add(AddReq{URL: srv.URL + "/" + name})
		m.mu.Lock()
		d.Conns = 6 // each would like more than the budget
		m.mu.Unlock()
		m.Start([]string{d.ID})
		return d
	}
	a := add("a.bin")
	waitFor(t, m, a, StDownloading, 10*time.Second)
	time.Sleep(700 * time.Millisecond)
	if n := m.hosts.held("127.0.0.1", a); n != 4 {
		t.Fatalf("alone, the first download should use the whole budget: %d", n)
	}
	atomic.StoreInt32(&srv.max, 0)
	b := add("b.bin")
	shrunk := false
	for i := 0; i < 300 && !shrunk; i++ {
		ha, hb := m.hosts.held("127.0.0.1", a), m.hosts.held("127.0.0.1", b)
		shrunk = ha <= 2 && hb >= 1
		time.Sleep(20 * time.Millisecond)
	}
	if !shrunk {
		t.Fatalf("the first download should make way: holds %d, the second %d", m.hosts.held("127.0.0.1", a), m.hosts.held("127.0.0.1", b))
	}
	da, db := atomic.LoadInt64(&a.done), atomic.LoadInt64(&b.done)
	time.Sleep(600 * time.Millisecond)
	if atomic.LoadInt64(&a.done) == da || atomic.LoadInt64(&b.done) == db {
		t.Fatal("both downloads should progress")
	}
	waitFor(t, m, a, StCompleted, 30*time.Second)
	waitFor(t, m, b, StCompleted, 30*time.Second)
	fileIs(t, a, data)
	fileIs(t, b, data)
	// a newcomer may always open its first connection; beyond that the budget holds
	if p := atomic.LoadInt32(&srv.max); p > 5 {
		t.Fatalf("%d connections at once with a budget of 4", p)
	}
}

func TestDownloadsAtOnceCap(t *testing.T) {
	data := randData(1 << 20)
	srv := newPeakServer(t, data, 1<<20)
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.MaxActive = 1
	m.mu.Unlock()
	a := m.Add(AddReq{URL: srv.URL + "/a.bin"})
	b := m.Add(AddReq{URL: srv.URL + "/b.bin"})
	c := m.Add(AddReq{URL: srv.URL + "/c.bin"})
	m.Start([]string{a.ID, b.ID, c.ID})
	snap := func(d *Download) DLView {
		for _, v := range m.Snapshot(nil).Downloads {
			if v.ID == d.ID {
				return v
			}
		}
		t.Fatal("not listed")
		return DLView{}
	}
	if v := snap(b); !v.Waiting || v.Status != StQueued || v.Queue != "" {
		t.Fatalf("second download should wait: %+v", v)
	}
	m.Pause([]string{c.ID}) // changed their mind: no longer waiting
	if v := snap(c); v.Waiting || v.Status != StPaused {
		t.Fatalf("paused while waiting: %+v", v)
	}
	waitFor(t, m, a, StCompleted, 20*time.Second)
	waitFor(t, m, b, StCompleted, 20*time.Second)
	fileIs(t, b, data)
	if v := snap(c); v.Status != StPaused {
		t.Fatalf("a paused download must not start: %s", v.Status)
	}
	// the setting round-trips through the settings API
	h := (&Server{m: m, token: "t"}).routes()
	req := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/settings", nil)
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var st Settings
	if json.Unmarshal(rec.Body.Bytes(), &st) != nil || st.MaxActive != 1 {
		t.Fatalf("settings: %.200s", rec.Body.String())
	}
	st.MaxActive = 80
	if got := apiCall(t, h, "settings", st); got["maxActive"] != float64(50) {
		t.Fatalf("saved maxActive %v (want it kept within 1..50)", got["maxActive"])
	}
	m.mu.Lock()
	m.settings.MaxActive = 0
	m.normalizeSettings()
	def := m.settings.MaxActive
	m.mu.Unlock()
	if def != 6 {
		t.Fatalf("default %d", def)
	}
}

// ---------------------------------------------------------------- B5

func TestHonestUserAgent(t *testing.T) {
	data := randData(512 << 10)
	open := newPeakServer(t, data, 0)
	m := newTestManager(t)
	d := m.Add(AddReq{URL: open.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 10*time.Second)
	if _, ok := open.uas.Load("Zipline/" + appVersion); !ok {
		t.Fatal("Zipline should say who it is")
	}
	// a download handed over by the browser keeps the browser's own User-Agent
	own := m.Add(AddReq{URL: open.URL + "/g.bin", Headers: map[string]string{"User-Agent": "TestBrowser/1"}, Start: true})
	waitFor(t, m, own, StCompleted, 10*time.Second)
	if _, ok := open.uas.Load("TestBrowser/1"); !ok {
		t.Fatal("the browser's User-Agent was replaced")
	}

	picky := newPeakServer(t, data, 0)
	var refused int32
	picky.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.Header.Get("User-Agent"), "Zipline/") {
			atomic.AddInt32(&refused, 1)
			http.Error(w, "browsers only", 403)
			return true
		}
		return false
	}
	p := m.Add(AddReq{URL: picky.URL + "/f.bin", Start: true})
	waitFor(t, m, p, StCompleted, 10*time.Second)
	fileIs(t, p, data)
	if n := atomic.LoadInt32(&refused); n != 1 {
		t.Fatalf("Zipline's name was refused %d times (want once: then the server is remembered)", n)
	}
	if _, ok := picky.uas.Load(browserUA); !ok {
		t.Fatal("no browser User-Agent sent")
	}
}

func TestBotChallengeIsReported(t *testing.T) {
	srv := newPeakServer(t, nil, 0)
	var hits int32
	srv.handle = func(w http.ResponseWriter, r *http.Request) bool {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(403)
		fmt.Fprint(w, "<html>Just a moment...</html>")
		return true
	}
	m := newTestManager(t)
	t0 := time.Now()
	d := m.Add(AddReq{URL: srv.URL + "/f.zip", Start: true})
	waitFor(t, m, d, StError, 10*time.Second)
	if time.Since(t0) > 3*time.Second {
		t.Fatalf("a bot check isn't worth retrying (took %v)", time.Since(t0))
	}
	v := m.Snapshot(nil).Downloads[0]
	if !v.Challenge || !strings.Contains(v.Error, "bot protection") {
		t.Fatalf("view %+v", v)
	}
	// Cloudflare's page without the header
	rec := &http.Response{StatusCode: 503, Header: http.Header{"Server": {"cloudflare"}, "Content-Type": {"text/html; charset=UTF-8"}}}
	if !challenged(rec) {
		t.Fatal("a Cloudflare 503 page is a challenge")
	}
	rec.Header.Set("Content-Type", "application/octet-stream")
	if challenged(rec) {
		t.Fatal("not a page: not a challenge")
	}
	// starting it again clears the mark
	m.Start([]string{d.ID})
	if m.Snapshot(nil).Downloads[0].Challenge {
		t.Fatal("restarted download still marked")
	}
}

// ---------------------------------------------------------------- C1

// keepDir is a data folder removed after the test (with retries: managers save in the background).
func keepDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "zltest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 40; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
}

func TestSavedListFormatAndUncleanExit(t *testing.T) {
	dir := keepDir(t)
	p := filepath.Join(dir, "downloads.json")
	old := `[{"id":"a","url":"http://x.test/a","status":"downloading","size":10485760,"resumable":true,
	  "segments":[{"s":0,"e":5242879,"p":3145728},{"s":5242880,"e":10485759,"p":5767168},{"s":0,"e":9,"p":10}]},
	 {"id":"b","url":"http://x.test/b","status":"paused","size":100,"segments":[{"s":0,"e":99,"p":50}]}]`
	os.WriteFile(p, []byte(old), 0o644)
	os.WriteFile(filepath.Join(dir, "running"), nil, 0o644) // the last run didn't exit cleanly
	m := NewManager(dir)
	a, b := m.byID["a"], m.byID["b"]
	if a == nil || b == nil {
		t.Fatal("old list format didn't load")
	}
	if s := a.Segments; s[0].Pos != 2<<20 || s[1].Pos != 5<<20 || s[2].Pos != 10 || a.done != 2<<20+10 || a.Status != StPaused {
		t.Fatalf("rolled back to %d, %d, %d (done %d, %s)", s[0].Pos, s[1].Pos, s[2].Pos, a.done, a.Status)
	}
	if b.Segments[0].Pos != 50 {
		t.Fatal("a paused download was rolled back")
	}
	m.save()
	raw, _ := os.ReadFile(p)
	var v struct {
		Format    int               `json:"format"`
		Downloads []json.RawMessage `json:"downloads"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Format != 2 || len(v.Downloads) != 2 {
		t.Fatalf("saved list: %.200s", raw)
	}
	m.closeClean()
	if _, err := os.Stat(filepath.Join(dir, "running")); !os.IsNotExist(err) {
		t.Fatal("a clean exit should remove the marker")
	}
	// a clean exit: progress is trusted as saved
	m.mu.Lock()
	a.Status = StDownloading
	m.mu.Unlock()
	m.save()
	m.closeClean()
	m2 := NewManager(dir)
	defer close(m2.quit)
	if a2 := m2.byID["a"]; a2 == nil || a2.Segments[0].Pos != 2<<20 {
		t.Fatal("rolled back after a clean exit")
	}
	if _, err := os.Stat(filepath.Join(dir, "running")); err != nil {
		t.Fatal("a running Zipline marks it")
	}
	// the backup in the new format still loads when the list is damaged
	writeAtomic(p, raw) // the good list becomes the backup
	os.WriteFile(p, []byte(`{"format":2,"downloads":[{"id":`), 0o644)
	list, note := loadState[dlList](p, nil, "download list")
	if len(list) != 2 || !strings.Contains(note, "backup") {
		t.Fatalf("backup: %d downloads, note %q", len(list), note)
	}
}

// Saved progress never claims more than the part file holds: the part file is synced while the
// download runs, and saves keep up.
func TestProgressSavedWhileDownloading(t *testing.T) {
	data := randData(4 << 20)
	srv := newPeakServer(t, data, 512<<10)
	m := newTestManager(t)
	stopAll(t, m)
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitDone(t, m, d, 1<<20)
	m.save()
	raw, _ := os.ReadFile(filepath.Join(m.dataDir, "downloads.json"))
	var v struct {
		Downloads []*Download `json:"downloads"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || len(v.Downloads) != 1 {
		t.Fatalf("%v %.100s", err, raw)
	}
	part, _ := os.ReadFile(d.partPath())
	var saved int64
	for _, s := range v.Downloads[0].Segments {
		saved += s.Pos - s.Start
		if !bytes.Equal(part[s.Start:s.Pos], data[s.Start:s.Pos]) {
			t.Fatalf("saved progress [%d, %d) isn't on disk", s.Start, s.Pos)
		}
	}
	if saved < 1<<20 {
		t.Fatalf("saved progress %d", saved)
	}
}

// ---------------------------------------------------------------- C2

func TestChangedFileCaughtByIfRange(t *testing.T) {
	one, two := randData(16<<20), randData(16<<20) // same size: only the version tag tells them apart
	var cur atomic.Pointer[[]byte]
	cur.Store(&one)
	var tagged, ifRange int32
	srv := newPeakServer(t, nil, 512<<10)
	srv.handle = func(w http.ResponseWriter, r *http.Request) bool {
		data, etag := *cur.Load(), `"v1"`
		if &data[0] == &two[0] {
			etag = `"v2"`
		}
		if r.Header.Get("If-Range") == `"v1"` {
			atomic.AddInt32(&tagged, 1)
		} else if r.Header.Get("If-Range") != "" {
			atomic.AddInt32(&ifRange, 1)
		}
		w.Header().Set("ETag", etag)
		http.ServeContent(&slowWriter{w: w, rate: 512 << 10, start: time.Now()}, r, "f.bin", time.Time{}, bytes.NewReader(data))
		return true
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true}) // 16 pieces for 8 connections: more requests follow
	waitDone(t, m, d, 512<<10)
	cur.Store(&two) // the file changes part-way
	waitFor(t, m, d, StError, 20*time.Second)
	if !strings.Contains(d.Error, "changed on the server") {
		t.Fatalf("error %q", d.Error)
	}
	if atomic.LoadInt32(&tagged) == 0 || atomic.LoadInt32(&ifRange) != 0 {
		t.Fatalf("pieces should be asked for with If-Range: %q (%d with it)", `"v1"`, tagged)
	}
	// resuming checks too
	m.mu.Lock()
	d.Status, d.Error = StPaused, ""
	m.mu.Unlock()
	m.Start([]string{d.ID})
	waitFor(t, m, d, StError, 20*time.Second)
	if !strings.Contains(d.Error, "changed on the server") {
		t.Fatalf("resume: %q", d.Error)
	}
}

func TestWrongPartIsRefused(t *testing.T) {
	data := randData(4 << 20)
	srv := newPeakServer(t, data, 1<<20)
	srv.handle = func(w http.ResponseWriter, r *http.Request) bool {
		rng := r.Header.Get("Range")
		if rng == "" || strings.HasPrefix(rng, "bytes=0-") {
			return false
		}
		// a broken cache: always the start of the file
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", 64<<10-1, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(64<<10))
		w.WriteHeader(206)
		w.Write(data[:64<<10])
		return true
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StError, 20*time.Second)
	if !strings.Contains(d.Error, "wrong part") {
		t.Fatalf("error %q", d.Error)
	}
	for _, c := range []struct {
		cr       string
		pos, end int64
		ok       bool
	}{
		{"bytes 100-199/1000", 100, 199, true},
		{"bytes 100-999/1000", 100, -1, true},
		{"bytes 900-999/1000", 900, 1999, true}, // the file ends first
		{"bytes 100-150/1000", 100, 199, false},
		{"bytes 0-199/1000", 100, 199, false},
		{"bytes 100-199/*", 100, 199, true},
		{"", 0, 99, false},
		{"bytes 5-3/10", 5, 9, false},
	} {
		if got := rightPart(c.cr, c.pos, c.end); got != c.ok {
			t.Errorf("%q for %d-%d: %v", c.cr, c.pos, c.end, got)
		}
	}
}

// ---------------------------------------------------------------- C3

func TestRetryDelayBounds(t *testing.T) {
	seen := map[time.Duration]bool{}
	for n := 1; n <= 12; n++ {
		base := min(time.Second<<(n-1), time.Minute)
		for i := 0; i < 50; i++ {
			d := retryDelay(n)
			if d < base*8/10 || d > base*12/10 {
				t.Fatalf("try %d: waited %v (base %v)", n, d, base)
			}
			seen[d] = true
		}
	}
	if len(seen) < 100 {
		t.Fatal("no jitter")
	}
	if d := retryDelay(1000); d > 72*time.Second {
		t.Fatalf("capped at a minute: %v", d)
	}
}

func TestNetworkChangeWakesRetries(t *testing.T) {
	go func() {
		time.Sleep(100 * time.Millisecond)
		signalNetChange() // the Wi-Fi came back
	}()
	t0 := time.Now()
	if !backoffSleep(context.Background(), 20*time.Second) || time.Since(t0) > 3*time.Second {
		t.Fatalf("a network change should cut the wait short (%v)", time.Since(t0))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if backoffSleep(ctx, time.Second) {
		t.Fatal("a stopped download doesn't wait")
	}
}

func TestRetryAfterIsHonouredPerServer(t *testing.T) {
	setVar(t, &retryBase, 10*time.Millisecond)
	data := randData(256 << 10)
	srv := newPeakServer(t, data, 0)
	var mu sync.Mutex
	var at []time.Time
	srv.handle = func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		if len(at) == 1 {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "busy", 503)
			return true
		}
		return false
	}
	m := newTestManager(t)
	stopAll(t, m)
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	mu.Lock()
	defer mu.Unlock()
	if len(at) < 2 || at[1].Sub(at[0]) < 1800*time.Millisecond {
		t.Fatalf("asked again too soon: %v", at[1].Sub(at[0]))
	}
	if retryAfter("7200") != time.Hour || retryAfter("30") != 30*time.Second {
		t.Fatal("Retry-After is capped at an hour")
	}
}

func TestMissingFileFailsFast(t *testing.T) {
	srv := newPeakServer(t, nil, 0)
	srv.handle = func(w http.ResponseWriter, r *http.Request) bool {
		http.NotFound(w, r)
		return true
	}
	m := newTestManager(t)
	t0 := time.Now()
	d := m.Add(AddReq{URL: srv.URL + "/gone.zip", Start: true})
	waitFor(t, m, d, StError, 5*time.Second)
	if time.Since(t0) > time.Second || atomic.LoadInt32(&srv.reqs) != 1 {
		t.Fatalf("a 404 took %v and %d requests", time.Since(t0), srv.reqs)
	}
}

// Every connection is lost (the server's network goes down); the download waits, shows when it
// tries again, and finishes once the server is back, without anyone pressing anything.
func TestDownloadSurvivesAnOutage(t *testing.T) {
	setVar(t, &maxTries, 2)
	setVar(t, &retryBase, 20*time.Millisecond)
	setVar(t, &wholeRetry, []time.Duration{500 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond})
	setVar(t, &onlineCheck, func() bool { return true })
	data := randData(2 << 20)
	srv := newPeakServer(t, data, 512<<10)
	var down atomic.Bool
	srv.handle = func(w http.ResponseWriter, r *http.Request) bool {
		if !down.Load() {
			return false
		}
		if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
			c.Close() // no answer at all
		}
		return true
	}
	m := newTestManager(t)
	stopAll(t, m)
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	waitDone(t, m, d, 256<<10)
	down.Store(true)
	srv.CloseClientConnections() // (replies in flight are cut too)
	var v DLView
	for i := 0; i < 500; i++ {
		v = m.Snapshot(nil).Downloads[0]
		if v.RetryAt != 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if v.RetryAt == 0 || v.Status != StConnecting || v.Error == "" {
		t.Fatalf("while waiting to try again: %+v", v)
	}
	down.Store(false)
	waitFor(t, m, d, StCompleted, 20*time.Second)
	fileIs(t, d, data)
	if v := m.Snapshot(nil).Downloads[0]; v.RetryAt != 0 || v.Error != "" {
		t.Fatalf("after: %+v", v)
	}
}

// ---------------------------------------------------------------- C7

func TestMirrorWithDifferentBytesIsLeftOut(t *testing.T) {
	data := randData(3 << 20)
	main := newFileHost(t, data, 512<<10)
	other := newFileHost(t, randData(3<<20), 0) // same size, different file
	m := newTestManager(t)
	stopAll(t, m)
	d := m.Add(AddReq{URL: main.URL + "/f.bin", Mirrors: []string{other.URL + "/f.bin"}, Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	var dead string
	for _, s := range d.srcs.view() {
		if s.Mirror {
			dead = s.Dead
		}
	}
	if dead != "serves a different file" {
		t.Fatalf("mirror: %q", dead)
	}
	if n := other.bytesSent(""); n > 64<<10 {
		t.Fatalf("the impostor sent %d bytes", n)
	}
}

func TestMirrorChecksumsSettleIt(t *testing.T) {
	data := randData(2 << 20)
	sum := sha256.Sum256(data)
	repr := func(b []byte) string { return "sha-256=:" + base64.StdEncoding.EncodeToString(b) + ":" }
	main := newFileHost(t, data, 512<<10)
	main.route = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Repr-Digest", repr(sum[:]))
		return false
	}
	liar := newFileHost(t, data, 0) // the same bytes so far, but it says it's another file
	liar.route = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Repr-Digest", repr(make([]byte, 32)))
		return false
	}
	m := newTestManager(t)
	stopAll(t, m)
	d := m.Add(AddReq{URL: main.URL + "/f.bin", Mirrors: []string{liar.URL + "/f.bin"}, Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	if n := liar.hitCount("/f.bin"); n != 1 {
		t.Fatalf("the checksums settle it without comparing pieces: %d requests", n)
	}
	if digestsAgree(map[string]string{"sha256": "a"}, map[string]string{"sha256": "a", "sha512": "b"}) != 1 ||
		digestsAgree(map[string]string{"sha256": "a"}, map[string]string{"sha512": "b"}) != 0 {
		t.Fatal("digestsAgree")
	}
}

// A fresh link from the browser must serve the bytes already downloaded, not just the same name
// and size.
func TestRenewedLinkMustMatchWhatsDownloaded(t *testing.T) {
	data := randData(1 << 20)
	good := newFileHost(t, data, 0)
	fake := newFileHost(t, randData(1<<20), 0)
	m := newTestManager(t)
	dir := t.TempDir()
	d := &Download{ID: "x", URL: good.URL + "/old/report.pdf", FileName: "report.pdf", Dir: dir, Size: 1 << 20,
		Resumable: true, Status: StError, Link: "expired", Segments: []*Segment{{Start: 0, End: 1<<20 - 1, Pos: 300 << 10}}}
	os.WriteFile(d.partPath(), append(append([]byte(nil), data[:300<<10]...), make([]byte, 1<<20-300<<10)...), 0o644)
	m.mu.Lock()
	m.list = append(m.list, d)
	m.byID[d.ID] = d
	m.mu.Unlock()
	if got := m.tryLinks(context.Background(), []linkCand{{URL: fake.URL + "/new/report.pdf"}}, "", ""); got != nil {
		t.Fatal("a link to another file with the same name and size was taken")
	}
	if got := m.tryLinks(context.Background(), []linkCand{{URL: good.URL + "/new/report.pdf"}}, "", ""); got != d {
		t.Fatal("the right link was refused")
	}
	waitFor(t, m, d, StCompleted, 20*time.Second)
	fileIs(t, d, data)
}

// More connections (doubling) while that makes it faster; held at the first increase that
// didn't pay, with a smaller try now and then.
func TestGrowthStopsWhenItDoesntPay(t *testing.T) {
	now := time.Now()
	g := newGrower(32)
	var seen []int
	for _, speed := range []float64{10, 10.5} {
		g.step(now, speed)
		seen = append(seen, g.target)
	}
	// 8 → 16 (first measurement) → only +5%: held at 16
	if fmt.Sprint(seen) != "[16 16]" || g.growing {
		t.Fatalf("targets %v growing %v", seen, g.growing)
	}
	g.step(now.Add(10*time.Second), 30)
	if g.target != 16 {
		t.Fatal("grew again before the pause was over")
	}
	g.step(now.Add(growPause+time.Second), 10.5)   // a smaller try: +4
	g.step(now.Add(growPause+2*time.Second), 10.6) // it didn't pay: held again
	if g.target != 20 || g.growing {
		t.Fatalf("after the pause: %d growing %v", g.target, g.growing)
	}
	for i := 0; i < 5; i++ {
		g.step(now.Add(time.Hour+time.Duration(i)*time.Second), float64(100*(i+1)))
	}
	if g.target != 32 {
		t.Fatalf("never above the connection setting: %d", g.target)
	}
	if newGrower(2).target != 2 || newGrower(32).target != 8 {
		t.Fatal("starts with at most 8, and at most the setting")
	}
}
