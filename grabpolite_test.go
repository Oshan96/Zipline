package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The scans in these tests run against a server on this machine: short waits keep them quick.
// (The timing tests set their own.)
func init() {
	grabWaitDefault, grabWaitFloor = 5*time.Millisecond, time.Millisecond
	grabRetryBase = 30 * time.Millisecond
	grabBackoffBase = 100 * time.Millisecond
	robotsDownTTL = 5 * time.Millisecond
}

// waitGrab runs a scan to its end (up to max).
func waitGrab(t *testing.T, j *GrabJob, max time.Duration) {
	t.Helper()
	dl := time.Now().Add(max)
	for time.Now().Before(dl) {
		j.mu.Lock()
		st := j.State
		j.mu.Unlock()
		if st != "running" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the scan did not finish")
}

func scan(t *testing.T, m *Manager, o GrabOptions, max time.Duration) *GrabJob {
	t.Helper()
	j, err := m.grab.Start(o, "")
	if err != nil {
		t.Fatal(err)
	}
	waitGrab(t, j, max)
	return j
}

// recSite is a server that notes when each request arrived, and how many were open at once.
type recSite struct {
	*httptest.Server
	mu         sync.Mutex
	reqs       []rec
	open, most int
	hold       time.Duration // how long every answer takes
	robots     string        // what /robots.txt says (and robotsCode, if set, the error it gives instead)
	robotsCode int
	pre        func(http.ResponseWriter, *http.Request) bool // looks at a request first; true: dealt with
}

type rec struct {
	at                 time.Time
	method, path, host string
	rng                string
}

func newRecSite(t *testing.T, h http.HandlerFunc) *recSite {
	s := &recSite{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, rec{time.Now(), r.Method, r.URL.RequestURI(), r.Host, r.Header.Get("Range")})
		s.open++
		s.most = max(s.most, s.open)
		hold, pre := s.hold, s.pre
		s.mu.Unlock()
		defer func() { s.mu.Lock(); s.open--; s.mu.Unlock() }()
		if hold > 0 {
			time.Sleep(hold)
		}
		if pre != nil && pre(w, r) {
			return
		}
		if r.URL.Path == "/robots.txt" && s.robots != "" {
			if s.robotsCode != 0 {
				http.Error(w, "x", s.robotsCode)
				return
			}
			fmt.Fprint(w, s.robots)
			return
		}
		h(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *recSite) setPre(f func(http.ResponseWriter, *http.Request) bool) {
	s.mu.Lock()
	s.pre = f
	s.mu.Unlock()
}

func (s *recSite) count(prefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reqs {
		if strings.HasPrefix(r.path, prefix) {
			n++
		}
	}
	return n
}

func (s *recSite) times() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []time.Time
	for _, r := range s.reqs {
		out = append(out, r.at)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Before(out[b]) })
	return out
}

func htmlPage(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<!doctype html><html><body>"+body+"</body></html>")
}

// pagesSite: / links to n pages, each with a file.
func pagesSite(t *testing.T, n int) *recSite {
	return newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			var b strings.Builder
			for i := 1; i <= n; i++ {
				fmt.Fprintf(&b, `<a href="/p%d.html">p%d</a> `, i, i)
			}
			htmlPage(w, b.String())
		case strings.HasPrefix(r.URL.Path, "/p") && strings.HasSuffix(r.URL.Path, ".html"):
			htmlPage(w, fmt.Sprintf(`<a href="/files%s.pdf">f</a>`, strings.TrimSuffix(r.URL.Path[2:], ".html")))
		default:
			http.NotFound(w, r)
		}
	})
}

// The gap between requests to one server is the larger of the site's Crawl-delay and the user's
// wait (and Crawl-delay is a minimum: jitter only adds to it).
func TestCrawlDelayAndWaitSpaceRequests(t *testing.T) {
	cases := []struct {
		name     string
		robots   string
		wait     int // ms
		minGap   time.Duration
		maxGapOK time.Duration
	}{
		{"Crawl-delay beats a short wait", "User-agent: *\nCrawl-delay: 0.3\n", 20, 295 * time.Millisecond, 0},
		{"a long wait beats Crawl-delay", "User-agent: *\nCrawl-delay: 0.05\n", 400, 310 * time.Millisecond, 0}, // (400 ms, 20% less at the most)
		{"Crawl-delay for Zipline only", "User-agent: other\nCrawl-delay: 5\n\nUser-agent: Zipline\nCrawl-delay: 0.25\n", 20, 245 * time.Millisecond, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			site := pagesSite(t, 4)
			site.robots = c.robots
			m := newTestManager(t)
			j := scan(t, m, GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Robots: true, Wait: c.wait}, 30*time.Second)
			if got := strings.Join(names(j), ","); got != "files1.pdf,files2.pdf,files3.pdf,files4.pdf" {
				t.Fatalf("found %s (state %s %s)", got, j.State, j.Error)
			}
			ts := site.times()
			if len(ts) < 6 { // robots.txt, the start page and 4 pages
				t.Fatalf("only %d requests", len(ts))
			}
			// (measured where they arrive: a busy machine can hold one back a little, which
			// brings the next one closer, so each gap gets some slack and their average none)
			for i := 1; i < len(ts); i++ {
				if gap := ts[i].Sub(ts[i-1]); gap < c.minGap/3 {
					t.Errorf("requests %d and %d were %v apart, want at least %v", i-1, i, gap, c.minGap)
				}
			}
			if avg := ts[len(ts)-1].Sub(ts[0]) / time.Duration(len(ts)-1); avg < c.minGap {
				t.Errorf("requests were %v apart on average, want at least %v", avg, c.minGap)
			}
		})
	}
}

// However many workers there are, a server has at most 2 requests on their way at a time.
func TestAtMostTwoRequestsPerServer(t *testing.T) {
	site := pagesSite(t, 12)
	site.hold = 120 * time.Millisecond
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Wait: 1}, 30*time.Second)
	if len(names(j)) != 12 {
		t.Fatalf("found %v", names(j))
	}
	site.mu.Lock()
	most := site.most
	site.mu.Unlock()
	if most != 2 {
		t.Fatalf("up to %d requests at once, want 2 (the limit, reached)", most)
	}
}

// A 429 or 503 holds the whole server back for its Retry-After, then the page is tried again.
func TestRetryAfterHoldsServerBack(t *testing.T) {
	var a429 atomic.Int32
	var at429 atomic.Int64
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			htmlPage(w, `<a href="/a.html">a</a> <a href="/b.html">b</a> <a href="/c.html">c</a>`)
		case "/a.html":
			if a429.Add(1) == 1 {
				at429.Store(time.Now().UnixNano())
				w.Header().Set("Retry-After", "1")
				http.Error(w, "slow down", 429)
				return
			}
			htmlPage(w, `<a href="/files/a.pdf">a</a>`)
		case "/b.html":
			htmlPage(w, `<a href="/files/b.pdf">b</a>`)
		case "/c.html":
			htmlPage(w, `<a href="/files/c.pdf">c</a>`)
		default:
			http.NotFound(w, r)
		}
	})
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Wait: 1}, 30*time.Second)
	if got := strings.Join(names(j), ","); got != "a.pdf,b.pdf,c.pdf" {
		t.Fatalf("found %s (state %s %s)", got, j.State, j.Error)
	}
	if j.Failed != 0 {
		t.Fatalf("a 429 that clears is not a failure: %d", j.Failed)
	}
	then := time.Unix(0, at429.Load())
	site.mu.Lock()
	defer site.mu.Unlock()
	for _, r := range site.reqs {
		if d := r.at.Sub(then); d > 50*time.Millisecond && d < 950*time.Millisecond {
			t.Errorf("%s asked %v after a 429 with Retry-After: 1", r.path, d)
		}
	}
	if site.reqs[len(site.reqs)-1].at.Sub(then) < time.Second {
		t.Errorf("the retry came too soon")
	}
}

func TestServiceUnavailableBacksOffWithoutRetryAfter(t *testing.T) {
	var n atomic.Int32
	var first atomic.Int64
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if n.Add(1) <= 2 {
			first.CompareAndSwap(0, time.Now().UnixNano())
			http.Error(w, "busy", 503)
			return
		}
		htmlPage(w, `<a href="/files/x.pdf">x</a>`)
	})
	m := newTestManager(t)
	start := time.Now()
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Types: []string{"Documents"}, Wait: 1}, 30*time.Second)
	if got := strings.Join(names(j), ","); got != "x.pdf" {
		t.Fatalf("found %s (%s %s)", got, j.State, j.Error)
	}
	// 100 ms after the first 503, 200 ms after the second (it doubles): at least 300 ms in all
	if d := time.Since(start); d < 280*time.Millisecond {
		t.Fatalf("the retries came after %v", d)
	}
}

func TestRetryAfterIsCapped(t *testing.T) {
	p := newHostPace()
	h := http.Header{}
	h.Set("Retry-After", "99999")
	if d := p.slowDown("h", h); d != grabBackoffMax {
		t.Fatalf("got %v, want the %v cap", d, grabBackoffMax)
	}
	p = newHostPace()
	h.Set("Retry-After", time.Now().Add(30*time.Second).UTC().Format(http.TimeFormat)) // a date works too
	if d := p.slowDown("h", h); d < 25*time.Second || d > 31*time.Second {
		t.Fatalf("got %v for a date 30 s away", d)
	}
	p = newHostPace()
	var last time.Duration
	for i := 0; i < 3; i++ { // without a Retry-After each one waits twice as long
		d := p.slowDown("h", http.Header{})
		if i > 0 && d != 2*last {
			t.Fatalf("%v then %v", last, d)
		}
		last = d
	}
}

// While one server is being left alone the others carry on, and a server is never given more than two tickets.
func TestOtherServersCarryOn(t *testing.T) {
	p := newHostPace()
	p.slowDown("a", http.Header{})
	if tk, wake := p.try("a"); tk != nil || wake.IsZero() {
		t.Fatal("a server that asked for quiet should be left alone")
	}
	tk, _ := p.try("b")
	if tk == nil {
		t.Fatal("another server should be fine")
	}
	tk2, _ := p.try("b")
	tk3, wake := p.try("b")
	if tk2 == nil || tk3 != nil || !wake.IsZero() {
		t.Fatalf("two tickets per server: %v %v", tk2, tk3)
	}
	tk.done()
	tk.done() // (twice is harmless)
	if tk4, _ := p.try("b"); tk4 == nil {
		t.Fatal("a ticket given back is free again")
	}
	// and the crawler takes the first item that can go, not the first in the queue
	site := pagesSite(t, 1)
	m := newTestManager(t)
	j := &GrabJob{ID: "x", Opts: GrabOptions{URL: site.URL + "/", Wait: 1}, State: "running", seen: map[string]bool{}}
	cr := m.grab.newCrawler(j)
	m.grab.pace.slowDown("slowhost.test", http.Header{})
	cr.queue = []crawlItem{{URL: "http://slowhost.test/one"}, {URL: "http://slowhost.test/two"}, {URL: "http://fast.test/three"}}
	i, tk5, _ := cr.pick()
	if tk5 == nil || i != 2 {
		t.Fatalf("picked %d", i)
	}
	tk5.done()
}

func TestGapKeepsCrawlDelayAsAMinimum(t *testing.T) {
	for r := 0.0; r <= 1; r += 0.25 {
		if g := gapFor(100*time.Millisecond, 500*time.Millisecond, r); g < 500*time.Millisecond || g > 600*time.Millisecond {
			t.Errorf("crawl-delay 500 ms: %v", g)
		}
		if g := gapFor(500*time.Millisecond, 100*time.Millisecond, r); g < 400*time.Millisecond || g > 600*time.Millisecond {
			t.Errorf("wait 500 ms: %v", g)
		}
	}
}

// Pages that can't be read are tried again, then counted and listed with the reason.
func TestFailedPagesAreCountedAndListed(t *testing.T) {
	var boom, flaky atomic.Int32
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			htmlPage(w, `<a href="/ok.html">ok</a> <a href="/missing.html">m</a> <a href="/boom.html">b</a> <a href="/flaky.html">f</a> <a href="/locked.html">l</a>`)
		case "/ok.html":
			htmlPage(w, `<a href="/files/ok.pdf">ok</a>`)
		case "/boom.html":
			boom.Add(1)
			http.Error(w, "broken", 500)
		case "/flaky.html":
			if flaky.Add(1) == 1 {
				http.Error(w, "broken", 502)
				return
			}
			htmlPage(w, `<a href="/files/flaky.pdf">f</a>`)
		case "/locked.html":
			http.Error(w, "sign in", 403)
		default:
			http.NotFound(w, r)
		}
	})
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Wait: 1}, 30*time.Second)
	if got := strings.Join(names(j), ","); got != "flaky.pdf,ok.pdf" {
		t.Fatalf("found %s", got)
	}
	if boom.Load() != int32(grabTries) {
		t.Fatalf("a failing page is tried %d times, was %d", grabTries, boom.Load())
	}
	if n := site.count("/missing.html"); n != 1 {
		t.Fatalf("a missing page isn't tried again: %d", n)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Failed != 3 || len(j.Failures) != 3 {
		t.Fatalf("failed %d: %+v", j.Failed, j.Failures)
	}
	by := map[string]GrabFailure{}
	for _, f := range j.Failures {
		by[strings.TrimPrefix(f.URL, site.URL)] = f
	}
	if f := by["/boom.html"]; f.Kind != "status" || f.Status != 500 {
		t.Errorf("boom: %+v", f)
	}
	if f := by["/missing.html"]; f.Status != 404 {
		t.Errorf("missing: %+v", f)
	}
	if f := by["/locked.html"]; f.Status != 403 {
		t.Errorf("locked: %+v", f)
	}
	if j.Pages != 6 { // the start page and five pages: all dealt with
		t.Errorf("pages %d", j.Pages)
	}
}

func TestNetworkFailureIsListed(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: deadURL + "/", Types: []string{"Documents"}, Wait: 1}, 30*time.Second)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Failed != 1 || len(j.Failures) != 1 || j.Failures[0].Kind != "net" || j.Failures[0].Detail == "" {
		t.Fatalf("failed %d: %+v", j.Failed, j.Failures)
	}
}

// A robots.txt the server can't answer means nothing may be fetched; when it can again, the page is.
func TestRobotsServerErrorDisallowsAll(t *testing.T) {
	site := pagesSite(t, 2)
	site.robots, site.robotsCode = "x", 503
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Robots: true, Wait: 1}, 30*time.Second)
	if n := site.count("/p") + site.count("/files") + site.count("/?"); n != 0 || len(j.Files) != 0 {
		t.Fatalf("pages were fetched (%d) though robots.txt couldn't be read", n)
	}
	if n := site.count("/robots.txt"); n < grabTries {
		t.Fatalf("robots.txt was asked for %d times, want a retry each time", n)
	}
	j.mu.Lock()
	if j.Failed != 1 || len(j.Failures) != 1 || j.Failures[0].Kind != "robots" || j.Failures[0].Detail == "" {
		t.Errorf("the start page should be listed as unreadable because of robots.txt: %d %+v", j.Failed, j.Failures)
	}
	j.mu.Unlock()
	site.mu.Lock()
	for _, r := range site.reqs {
		if r.path != "/robots.txt" {
			t.Fatalf("fetched %s", r.path)
		}
	}
	site.mu.Unlock()

	// the same site once its robots.txt can be served
	var asks atomic.Int32
	site2 := pagesSite(t, 2)
	site2.setPre(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/robots.txt" && asks.Add(1) <= 2 {
			http.Error(w, "down", 503)
			return true
		}
		return false
	})
	j = scan(t, m, GrabOptions{URL: site2.URL + "/", Depth: 1, Types: []string{"Documents"}, Robots: true, Wait: 1}, 30*time.Second)
	if got := strings.Join(names(j), ","); got != "files1.pdf,files2.pdf" || j.Failed != 0 {
		t.Fatalf("found %s, %d failed (%s)", got, j.Failed, j.Error)
	}
}

// A missing robots.txt (4xx) allows everything; Robots off doesn't ask at all.
func TestRobotsMissingAllowsAll(t *testing.T) {
	site := pagesSite(t, 2)
	site.robots, site.robotsCode = "x", 404
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Robots: true, Wait: 1}, 30*time.Second)
	if len(names(j)) != 2 {
		t.Fatalf("found %v", names(j))
	}
	site3 := pagesSite(t, 1)
	site3.robots = "User-agent: *\nDisallow: /\n"
	j = scan(t, m, GrabOptions{URL: site3.URL + "/", Depth: 1, Types: []string{"Documents"}, Robots: false, Wait: 1}, 30*time.Second)
	if site3.count("/robots.txt") != 0 || len(names(j)) != 1 {
		t.Fatalf("robots off: %d asks, found %v", site3.count("/robots.txt"), names(j))
	}
}

// The wildcard rules of robots.txt hold in a scan, and robots.txt is read once for a site.
func TestRobotsWildcardsInAScan(t *testing.T) {
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			htmlPage(w, `<a href="/a.html">a</a> <a href="/private/b.html">b</a> <a href="/c.html?print=1">c</a> <a href="/d.html">d</a> <a href="/private/open/e.html">e</a>`)
		default:
			htmlPage(w, fmt.Sprintf(`<a href="/files%s.pdf">f</a>`, strings.TrimSuffix(r.URL.Path, ".html")))
		}
	})
	site.robots = "User-agent: *\nDisallow: /private/\nAllow: /private/open/\nDisallow: /*?print=\nDisallow: /d.htm$\n"
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Robots: true, Wait: 1}, 30*time.Second)
	if got := strings.Join(names(j), ","); got != "a.pdf,d.pdf,e.pdf" {
		t.Fatalf("found %s", got)
	}
	if site.count("/robots.txt") != 1 {
		t.Fatalf("robots.txt read %d times", site.count("/robots.txt"))
	}
	if j.Skipped != 2 { // /private/b.html and /c.html?print=1
		t.Fatalf("skipped %d", j.Skipped)
	}
}
