package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// testSite: index → a.html → deep/c.html, a robots-blocked page, and a file on another host.
func testSite(t *testing.T) (*httptest.Server, func(string)) {
	var mu sync.Mutex
	extra := ""
	var srv *httptest.Server
	mux := http.NewServeMux()
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!doctype html><html><body>"+body+"</body></html>")
		}
	}
	other := strings.Replace(func() string { return "" }(), "", "", 0)
	_ = other
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		x := extra
		mu.Unlock()
		cdn := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
		page(`<a href="/a.html">A</a> <a href='docs/b.html'>B</a> <a href="/files/report.pdf">Report</a>
			<img src="/img/photo.jpg" srcset="/img/small.webp 1x, /img/big.webp 2x">
			<a href="/private/secret.html">secret</a> <a href="mailto:x@y.z">mail</a> <a href="javascript:void(0)">js</a>
			<a href="#top">top</a> <a href="`+cdn+`/cdn/video.mp4">cdn video</a> <a href="`+cdn+`/elsewhere.html">other site page</a>
			<div style="background:url('/img/bg.png')"></div>`+x)(w, r)
	})
	mux.HandleFunc("/a.html", page(`<a href="/files/archive.zip?v=2">zip</a> <a href="deep/c.html">deeper</a> <a href="/a.html">self</a>`))
	mux.HandleFunc("/deep/c.html", page(`<a href="/files/deep.pdf">deep pdf</a>`))
	mux.HandleFunc("/docs/b.html", page(`<a href="/docs/manual.pdf">manual</a> <a href="/a.html">up</a>`))
	mux.HandleFunc("/private/secret.html", page(`<a href="/files/secret.pdf">secret pdf</a>`))
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "User-agent: SomeBot\nDisallow: /\n\nUser-agent: *\nDisallow: /private/\n")
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func(s string) { mu.Lock(); extra = s; mu.Unlock() }
}

func runGrab(t *testing.T, m *Manager, o GrabOptions) *GrabJob {
	t.Helper()
	j, err := m.grab.Start(o, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		j.mu.Lock()
		st := j.State
		j.mu.Unlock()
		if st != "running" {
			return j
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("grab did not finish")
	return nil
}

func names(j *GrabJob) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var n []string
	for _, f := range j.Files {
		n = append(n, f.Name)
	}
	sort.Strings(n)
	return n
}

func TestGrabberDepthScopeRobots(t *testing.T) {
	srv, _ := testSite(t)
	m := newTestManager(t)
	types := []string{"Documents", "Compressed", "Images", "Video"}

	j := runGrab(t, m, GrabOptions{URL: srv.URL + "/", Depth: 1, Types: types, CrossSite: true, Robots: true})
	got := strings.Join(names(j), ",")
	want := "archive.zip,bg.png,big.webp,manual.pdf,photo.jpg,report.pdf,small.webp,video.mp4"
	if got != want {
		t.Fatalf("depth 1:\n got  %s\n want %s", got, want)
	}
	if j.Skipped == 0 {
		t.Fatal("robots.txt should have blocked /private/")
	}

	j = runGrab(t, m, GrabOptions{URL: srv.URL + "/", Depth: 2, Types: []string{"Documents"}, CrossSite: false, Robots: true})
	if got := strings.Join(names(j), ","); got != "deep.pdf,manual.pdf,report.pdf" {
		t.Fatalf("depth 2 docs: %s", got)
	}

	j = runGrab(t, m, GrabOptions{URL: srv.URL + "/", Depth: 1, Types: []string{"Documents"}, Robots: false})
	if !strings.Contains(strings.Join(names(j), ","), "secret.pdf") {
		t.Fatal("with robots off the private page should be crawled")
	}

	j = runGrab(t, m, GrabOptions{URL: srv.URL + "/", Depth: 1, Types: []string{"Video"}, CrossSite: false, Robots: true})
	if len(names(j)) != 0 {
		t.Fatalf("files from other sites must be skipped when not allowed: %v", names(j))
	}

	j = runGrab(t, m, GrabOptions{URL: srv.URL + "/docs/b.html", Depth: 3, Scope: "folder", Types: []string{"Documents", "Compressed"}, Robots: true})
	if got := strings.Join(names(j), ","); got != "manual.pdf" {
		t.Fatalf("folder scope: %s", got)
	}
}

func TestGrabberAddKeepsStructureAndReferer(t *testing.T) {
	srv, _ := testSite(t)
	m := newTestManager(t)
	j := runGrab(t, m, GrabOptions{URL: srv.URL + "/", Depth: 1, Types: []string{"Compressed"}, Robots: true})
	var urls []string
	for _, f := range j.Files {
		urls = append(urls, f.URL)
	}
	dir := t.TempDir()
	n, err := m.grab.AddFiles(GrabAddReq{JobID: j.ID, URLs: urls, Dir: dir, Keep: true, QueueID: "main"})
	if err != nil || n != 1 {
		t.Fatalf("add: %d %v", n, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.list[len(m.list)-1]
	if d.Queue != "main" || d.Headers["Referer"] != srv.URL+"/a.html" {
		t.Fatalf("queue/referer wrong: %q %q", d.Queue, d.Headers["Referer"])
	}
	if want := filepath.Join(dir, "127.0.0.1", "files"); d.Dir != want {
		t.Fatalf("structure: got %s want %s", d.Dir, want)
	}
}

func TestGrabberProjectQueuesOnlyNewFiles(t *testing.T) {
	srv, setExtra := testSite(t)
	m := newTestManager(t)
	o := GrabOptions{URL: srv.URL + "/", Depth: 0, Types: []string{"Documents"}, Robots: true}
	j := runGrab(t, m, o)
	p, err := m.grab.SaveProject(GrabProject{Name: "Reports", Opts: o, QueueID: "main", Dir: t.TempDir(), AutoMins: 60}, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	setExtra(`<a href="/files/new-report.pdf">new</a>`)
	before := len(m.list)
	j2, _ := m.grab.RunProject(p.ID, true)
	for i := 0; i < 200; i++ {
		j2.mu.Lock()
		st := j2.State
		j2.mu.Unlock()
		if st != "running" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	// the run is "done" before its new files are queued, the project saved and the notice sent: wait for
	// the notice (a slow disk on CI took over 100 ms to save)
	var added []*Download
	var text string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(25 * time.Millisecond) {
		m.mu.Lock()
		added = m.list[before:]
		for _, n := range m.notices {
			if n.Kind == "grabDone" {
				text = n.Text
			}
		}
		m.mu.Unlock()
		if text != "" || time.Now().After(deadline) {
			break
		}
	}
	if len(added) != 1 || added[0].FileName != "new-report.pdf" {
		t.Fatalf("expected only the new file to be queued, got %d", len(added))
	}
	if text != iso("Reports")+": 1 new file queued" {
		t.Fatalf("notice: %q", text)
	}
}

func TestGrabberFTPFolder(t *testing.T) {
	srv := newTestFTP(t, []byte("x"), 0, false)
	m := newTestManager(t)
	j := runGrab(t, m, GrabOptions{URL: srv.url("/pub/"), Depth: 1, Types: []string{"Compressed", "Programs"}})
	if got := strings.Join(names(j), ","); got != "deep.zip,file.bin" {
		t.Fatalf("ftp folder: %q (state %s %s)", got, j.State, j.Error)
	}
}

func TestExtractLinksAndRobots(t *testing.T) {
	r := parseRobots("User-agent: *\nDisallow: /tmp/\nAllow: /tmp/ok/\n# c\nUser-agent: X\nDisallow: /\n", robotsToken).rules
	if len(r) != 2 {
		t.Fatalf("robots rules: %#v", r)
	}
	if regDomain("a.b.example.co.uk") != "example.co.uk" || regDomain("cdn.example.com") != "example.com" {
		t.Fatal("regDomain")
	}
}
