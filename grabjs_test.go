package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const reactShell = `<!doctype html><html><head><title>App</title><link rel="stylesheet" href="/static/css/main.css"><link rel="icon" href="/favicon.ico"></head>
<body><noscript>You need to enable JavaScript to run this app.</noscript><div id="root"></div><script src="/static/js/main.4f2a.js"></script></body></html>`

func TestLooksScriptBuilt(t *testing.T) {
	text := strings.Repeat("A paragraph of real words that a visitor reads on the page itself. ", 8)
	cases := []struct {
		name, html string
		want       bool
	}{
		{"React shell", reactShell, true},
		{"Angular", `<html><body><app-root></app-root><script src="runtime.js"></script><script src="polyfills.js"></script><script src="main.js"></script></body></html>`, true},
		{"Next with an empty container", `<html><body><div id="__next"></div><script src="/_next/static/chunks/main-1.js"></script></body></html>`, true},
		{"an ordinary page", `<html><body><h1>Title</h1><p>` + text + `</p><script>var x=1</script></body></html>`, false},
		{"a tiny static page", `<html><body>Hello</body></html>`, false},
		{"a page with a big script and little text", `<html><body><p>Hi</p><script>` + strings.Repeat("a=1;", 12000) + `</script></body></html>`, false},
		{"an empty root with text rendered by the server", `<html><body><div id="root"><h1>Hi</h1><p>` + text + `</p></div><script src="main.js"></script></body></html>`, false},
	}
	for _, c := range cases {
		if got := looksScriptBuilt(c.html); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// scriptSite: a start page whose links are made by script, and the pages and files behind them.
func scriptSite(t *testing.T) *recSite {
	return newRecSite(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			htmlPage(w, reactShell)
		case "/plain.html":
			htmlPage(w, `<p>A page that is plain HTML.</p><a href="/files/plain.pdf">p</a> <a href="/x.html">x</a> <a href="/y.html">y</a>`)
		case "/hidden.html":
			htmlPage(w, `<a href="/files/b.pdf">b</a>`)
		case "/dl":
			w.Header().Set("Content-Type", "application/pdf")
			w.Header().Set("Content-Disposition", `attachment; filename="c.pdf"`)
		default:
			http.NotFound(w, r)
		}
	})
}

func waitStatus(t *testing.T, h http.Handler, id string, ok func(map[string]any) bool) map[string]any {
	t.Helper()
	var v map[string]any
	for i := 0; i < 400; i++ {
		v = apiGet(t, h, "grab/status?id="+id)
		if ok(v) {
			return v
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("the scan never got there: %v", v)
	return nil
}

func fileNames(v map[string]any) string {
	var n []string
	for _, f := range v["files"].([]any) {
		n = append(n, f.(map[string]any)["name"].(string))
	}
	return strings.Join(n, ",")
}

func stateIs(s string) func(map[string]any) bool {
	return func(v map[string]any) bool { return v["state"] == s }
}

// A page that builds its links with script is reported, and the browser's links for it join the scan.
func TestScriptBuiltPageAndTheBrowserRoundTrip(t *testing.T) {
	site := scriptSite(t)
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()

	r := apiCall(t, h, "grab/start", GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Wait: 1})
	id := r["id"].(string)
	v := waitStatus(t, h, id, stateIs("done"))
	pages, _ := v["jsPages"].([]any)
	if len(pages) != 1 || pages[0].(map[string]any)["url"] != site.URL+"/" {
		t.Fatalf("the script-built start page should be reported: %v", v["jsPages"])
	}
	if fileNames(v) != "" {
		t.Fatalf("found %s without the browser", fileNames(v))
	}

	// the extension isn't around yet
	if r = apiCall(t, h, "grab/browser", map[string]any{"id": id}); r["code"] != "noext" {
		t.Fatalf("with no extension connected: %v", r)
	}
	// it checks in, and is asked to open the page
	extCall(t, h, "GET", "ping", nil)
	if r = apiCall(t, h, "grab/browser", map[string]any{"id": id}); r["error"] != nil {
		t.Fatalf("ask: %v", r)
	}
	ping := extCall(t, h, "GET", "ping", nil)
	asks, _ := ping["grab"].([]any)
	if len(asks) != 1 {
		t.Fatalf("the ping should carry the page to open: %v", ping["grab"])
	}
	ask := asks[0].(map[string]any)
	if ask["page"] != site.URL+"/" || ask["jobId"] != id {
		t.Fatalf("ask: %v", ask)
	}
	if again, _ := extCall(t, h, "GET", "ping", nil)["grab"].([]any); len(again) != 0 {
		t.Fatalf("an ask is handed out once: %v", again)
	}
	if v = apiGet(t, h, "grab/status?id="+id); v["collects"].([]any)[0].(map[string]any)["state"] != "waiting" {
		t.Fatalf("collects: %v", v["collects"])
	}

	// the browser sends what the page showed once its script ran
	res := extCall(t, h, "POST", "grab", map[string]any{"id": ask["id"], "jobId": id, "links": []map[string]string{
		{"url": site.URL + "/files/a.pdf", "text": "A"}, {"url": site.URL + "/hidden.html"}, {"url": site.URL + "/dl?id=2"},
		{"url": site.URL + "/static/js/main.4f2a.js"},
	}, "cookies": map[string]string{}})
	if res["ok"] != true {
		t.Fatalf("grab: %v", res)
	}
	v = waitStatus(t, h, id, func(v map[string]any) bool { return v["state"] == "done" && fileNames(v) != "" })
	if got := fileNames(v); !(strings.Contains(got, "a.pdf") && strings.Contains(got, "b.pdf") && strings.Contains(got, "c.pdf")) || strings.Count(got, ",") != 2 {
		t.Fatalf("the links should join the scan like fetched ones (a file, a page one level down, a link checked): %s", got)
	}
	c := v["collects"].([]any)[0].(map[string]any)
	if c["state"] != "done" || c["links"].(float64) != 4 {
		t.Fatalf("collect: %v", c)
	}
	// a second ask the browser can't answer
	apiCall(t, h, "grab/browser", map[string]any{"id": id})
	asks, _ = extCall(t, h, "GET", "ping", nil)["grab"].([]any)
	if len(asks) != 1 {
		t.Fatalf("asked again for the page: %v", asks)
	}
	extCall(t, h, "POST", "grab", map[string]any{"id": asks[0].(map[string]any)["id"], "jobId": id, "error": "blocked"})
	v = apiGet(t, h, "grab/status?id="+id)
	if cs := v["collects"].([]any); len(cs) != 2 || cs[1].(map[string]any)["state"] != "failed" || cs[1].(map[string]any)["error"] != "blocked" {
		t.Fatalf("collects: %v", v["collects"])
	}
	// nobody asked for this one
	if res = extCall(t, h, "POST", "grab", map[string]any{"id": "nope", "jobId": id, "links": []map[string]string{{"url": site.URL + "/x"}}}); res["ok"] != false {
		t.Fatalf("an unknown ask: %v", res)
	}
	// and a plain page is not reported
	j := scan(t, m, GrabOptions{URL: site.URL + "/plain.html", Types: []string{"Documents"}, Wait: 1}, 20*time.Second)
	if len(j.scan.jsPages) != 0 {
		t.Fatalf("a plain page: %v", j.scan.jsPages)
	}
}

// A browser that doesn't answer in time is given up on.
func TestBrowserAskTimesOut(t *testing.T) {
	site := scriptSite(t)
	m := newTestManager(t)
	j := scan(t, m, GrabOptions{URL: site.URL + "/", Types: []string{"Documents"}, Wait: 1}, 20*time.Second)
	m.mu.Lock()
	m.extSeen = time.Now()
	m.mu.Unlock()
	if err := m.grab.AskBrowser(j.ID, ""); err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	j.scan.collects[0].added = time.Now().Add(-2 * time.Minute)
	j.mu.Unlock()
	if len(m.grab.takeAsks()) != 0 {
		t.Fatal("a stale ask shouldn't be handed out")
	}
	v := j.view()
	if c := v["collects"].([]GrabCollect)[0]; c.State != "failed" || c.Error != "timeout" {
		t.Fatalf("collect: %+v", c)
	}
}

// slowSite: / links to n pages, each takes a while, each with a file.
func slowSite(t *testing.T, n int, hold time.Duration) *recSite {
	s := pagesSite(t, n)
	s.hold = hold
	return s
}

func readDirNames(t *testing.T, dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// A scan that was stopped, or cut short, survives a restart and can be resumed.
func TestScanSavedAndResumedAfterRestart(t *testing.T) {
	site := slowSite(t, 8, 150*time.Millisecond)
	m := newTestManager(t)
	j, err := m.grab.Start(GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Wait: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 400; i++ { // a few pages in
		j.mu.Lock()
		p := j.Pages
		j.mu.Unlock()
		if p >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.grab.flush() // what Zipline does when it saves, so this is what a close in the middle leaves behind
	j.stop()
	waitGrab(t, j, 10*time.Second)
	j.mu.Lock()
	left, st, found := len(j.scan.left), j.State, len(j.Files)
	j.mu.Unlock()
	if st != "stopped" || left == 0 {
		t.Fatalf("stopped scan: %s, %d left", st, left)
	}
	if found == 8 {
		t.Fatal("it should not have finished")
	}
	m.save()
	file := filepath.Join(m.dataDir, "scans", j.ID+".json")
	if fi, err := os.Stat(file); err != nil {
		t.Fatalf("the scan isn't saved: %v", err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("a scan can hold a sign-in: mode %v", fi.Mode())
	}

	// Zipline is started again
	m.closeClean()
	m2 := NewManager(m.dataDir)
	defer close(m2.quit)
	var seen map[string]any
	for _, s := range m2.grab.Scans() {
		if s["id"] == j.ID {
			seen = s
		}
	}
	if seen == nil || seen["state"] != "stopped" || seen["resumable"] != true || seen["files"].(int) != found {
		t.Fatalf("after the restart: %v", m2.grab.Scans())
	}
	j2, err := m2.grab.Resume(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitGrab(t, j2, 30*time.Second)
	if got := strings.Join(names(j2), ","); got != "files1.pdf,files2.pdf,files3.pdf,files4.pdf,files5.pdf,files6.pdf,files7.pdf,files8.pdf" {
		t.Fatalf("resumed: %s (%s %s)", got, j2.State, j2.Error)
	}
	if j2.State != "done" || len(j2.scan.left) != 0 || j2.Pages != 9 {
		t.Fatalf("resumed scan: %s, %d left, %d pages", j2.State, len(j2.scan.left), j2.Pages)
	}
	// every page was read once in all (the resumed scan didn't go back to the ones done)
	for i := 1; i <= 8; i++ {
		if n := site.count(fmt.Sprintf("/p%d.html", i)); n > 2 {
			t.Errorf("page %d was read %d times", i, n)
		}
	}
	if _, err := m2.grab.Resume(j.ID); err == nil {
		t.Error("a finished scan has nothing to resume")
	}
	v := j2.view()
	if v["resumable"] != false {
		t.Errorf("view: %v", v["resumable"])
	}
}

// A scan that was running when Zipline closed comes back as interrupted, and resumes.
func TestInterruptedScanComesBack(t *testing.T) {
	site := slowSite(t, 6, 120*time.Millisecond)
	m := newTestManager(t)
	j, _ := m.grab.Start(GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, Wait: 1}, "")
	for i := 0; i < 400 && func() bool { j.mu.Lock(); defer j.mu.Unlock(); return j.Pages < 2 }(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	m.grab.flush()
	raw, _ := os.ReadFile(filepath.Join(m.dataDir, "scans", j.ID+".json"))
	var saved savedScan
	if json.Unmarshal(raw, &saved) != nil || saved.State != "running" || len(saved.Left) == 0 {
		t.Fatalf("saved while running: %.300s", raw)
	}
	// (copy what was on disk then to a folder of its own: this is the file a crash leaves)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "scans"), 0o700)
	os.WriteFile(filepath.Join(dir, "scans", j.ID+".json"), raw, 0o600)
	j.stop()
	waitGrab(t, j, 10*time.Second)

	m2 := NewManager(dir)
	defer close(m2.quit)
	j2 := m2.grab.Job(j.ID)
	if j2 == nil || j2.State != "stopped" || !j2.scan.interrupted || len(j2.scan.left) == 0 {
		t.Fatalf("after the crash: %+v", j2)
	}
	j2, err := m2.grab.Resume(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitGrab(t, j2, 30*time.Second)
	if len(names(j2)) != 6 || j2.State != "done" {
		t.Fatalf("resumed: %v %s", names(j2), j2.State)
	}
}

// Only the last 20 scans are kept; a scan from a project's timer or a page's links isn't saved.
func TestOnlyTheLastTwentyScansAreKept(t *testing.T) {
	m := newTestManager(t)
	now := time.Now()
	for i := 0; i < 25; i++ {
		j := &GrabJob{ID: fmt.Sprintf("scan%02d", i), Opts: GrabOptions{URL: "http://h/"}, State: "done", Started: now.Add(time.Duration(i) * time.Minute), seen: map[string]bool{}}
		m.grab.jobs[j.ID] = j
		m.grab.persist(j)
	}
	p := &GrabJob{ID: "timer", Opts: GrabOptions{URL: "http://h/"}, State: "done", Project: "p1", seen: map[string]bool{}}
	m.grab.persist(p)
	pg := &GrabJob{ID: "page", Opts: GrabOptions{URL: "http://h/"}, State: "done", FromPage: true, seen: map[string]bool{}}
	m.grab.persist(pg)
	if n := len(readDirNames(t, filepath.Join(m.dataDir, "scans"))); n != 25 {
		t.Fatalf("%d files", n)
	}
	m2 := NewManager(m.dataDir)
	defer close(m2.quit)
	list := m2.grab.Scans()
	if len(list) != 20 || list[0]["id"] != "scan24" || list[19]["id"] != "scan05" {
		t.Fatalf("kept %d: first %v", len(list), list[0]["id"])
	}
	if n := len(readDirNames(t, filepath.Join(m.dataDir, "scans"))); n != 20 {
		t.Fatalf("old scans' files should go: %d files", n)
	}
	m2.grab.DeleteScan("scan24")
	if _, err := os.Stat(filepath.Join(m.dataDir, "scans", "scan24.json")); !os.IsNotExist(err) || m2.grab.Job("scan24") != nil {
		t.Fatal("a deleted scan is gone")
	}
}

// The browser's sign-in goes to the site the scan started on, and to no other site.
func TestBrowserSignInStaysOnTheSite(t *testing.T) {
	var mu sync.Mutex
	got := map[string][]string{} // host → Cookie headers seen
	var other *recSite
	var otherURL string
	site := newRecSite(t, func(w http.ResponseWriter, r *http.Request) {})
	site.setPre(func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		got[r.Host] = append(got[r.Host], r.Method+" "+r.URL.Path+" "+r.Header.Get("Cookie"))
		mu.Unlock()
		switch r.URL.Path {
		case "/":
			htmlPage(w, `<a href="/members.html">m</a> <a href="/files/own.pdf">own</a> <a href="`+otherURL+`/dl.php?id=1">out</a> <a href="`+otherURL+`/files/out.pdf">out2</a>`)
		case "/members.html":
			htmlPage(w, `<a href="/files/secret.pdf">s</a>`)
		default:
			http.NotFound(w, r)
		}
		return true
	})
	other = newRecSite(t, func(w http.ResponseWriter, r *http.Request) {})
	otherURL = strings.Replace(other.URL, "127.0.0.1", "localhost", 1) // (a different site: cookies don't tell ports apart)
	other.setPre(func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		got[r.Host] = append(got[r.Host], r.Method+" "+r.URL.Path+" "+r.Header.Get("Cookie"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="elsewhere.pdf"`)
		return true
	})
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	host := strings.TrimPrefix(site.URL, "http://")
	hostname := strings.Split(host, ":")[0]

	// "Scan this site" in the browser: the page and its cookies go to the app, which opens its window on the page
	res := extCall(t, h, "POST", "scan", map[string]any{"page": site.URL + "/", "title": "Members", "cookies": map[string]string{
		hostname: "session=abc123", "evil.example": "stolen=1"}})
	if res["ok"] != true {
		t.Fatalf("scan: %v", res)
	}
	m.mu.Lock()
	var pr Prompt
	for _, p := range m.prompts {
		if p.Kind == "scan" {
			pr = p
		}
	}
	m.mu.Unlock()
	if pr.URL != site.URL+"/" || pr.JobID == "" {
		t.Fatalf("the window should be told to open the grabber: %+v", pr)
	}
	if res = extCall(t, h, "POST", "scan", map[string]any{"page": "javascript:alert(1)"}); res["ok"] != false {
		t.Fatalf("not a web page: %v", res)
	}

	r := apiCall(t, h, "grab/start", GrabOptions{URL: site.URL + "/", Depth: 1, Types: []string{"Documents"}, CrossSite: true, Wait: 1, Session: pr.JobID})
	v := waitStatus(t, h, r["id"].(string), stateIs("done"))
	if si, _ := v["signedIn"].([]any); len(si) != 1 || si[0] != hostname {
		t.Fatalf("signed in as: %v", v["signedIn"])
	}
	if want := "elsewhere.pdf,own.pdf,out.pdf,secret.pdf"; fileNames(v) != want && len(strings.Split(fileNames(v), ",")) != 4 {
		t.Fatalf("found %s", fileNames(v))
	}
	mu.Lock()
	for _, line := range got[host] {
		if !strings.HasSuffix(line, "session=abc123") {
			t.Errorf("a request to the site went without the sign-in: %q", line)
		}
	}
	if len(got[host]) < 2 {
		t.Errorf("%v", got[host])
	}
	otherHost := strings.TrimPrefix(otherURL, "http://")
	if len(got[otherHost]) == 0 {
		t.Error("the other site should have been asked about its download")
	}
	for _, line := range got[otherHost] {
		if strings.Contains(line, "abc123") || strings.Contains(line, "stolen") {
			t.Errorf("the sign-in went to another site: %q", line)
		}
	}
	mu.Unlock()

	// files added from the scan carry the sign-in only for the site's own
	var urls []string
	for _, f := range v["files"].([]any) {
		urls = append(urls, f.(map[string]any)["url"].(string))
	}
	if n := apiCall(t, h, "grab/add", GrabAddReq{JobID: r["id"].(string), URLs: urls, Dir: t.TempDir(), QueueID: "main"}); n["added"].(float64) < 3 {
		t.Fatalf("added %v", n)
	}
	m.mu.Lock()
	for _, d := range m.list {
		c := d.Headers["Cookie"]
		switch {
		case strings.HasPrefix(d.URL, site.URL) && c != "session=abc123":
			t.Errorf("%s: cookie %q", d.URL, c)
		case strings.HasPrefix(d.URL, otherURL) && c != "":
			t.Errorf("%s: cookie %q", d.URL, c)
		}
	}
	m.mu.Unlock()
	// the scan keeps the sign-in so a resumed scan is still signed in
	var saved savedScan
	m.grab.flush()
	raw, _ := os.ReadFile(filepath.Join(m.dataDir, "scans", r["id"].(string)+".json"))
	err := json.Unmarshal(raw, &saved)
	if strings.Contains(string(raw), "abc123") {
		t.Error("the sign-in should be saved protected")
	}
	m.vault.revealAll(saved.Cookies)
	if err != nil || saved.Cookies[hostname] != "session=abc123" || saved.Cookies["evil.example"] != "" {
		t.Fatalf("saved cookies: %v", saved.Cookies)
	}
}

func TestCookiesGoOnlyToTheSameSite(t *testing.T) {
	j := &GrabJob{Opts: GrabOptions{URL: "https://a.github.io/repo/"}, Cookies: map[string]string{"a.github.io": "x=1"}}
	if j.cookieFor("a.github.io") != "x=1" || j.cookieFor("b.github.io") != "" || j.cookieFor("github.io") != "" {
		t.Fatal("github.io is a public suffix: a.github.io and b.github.io are different sites")
	}
	j = &GrabJob{Opts: GrabOptions{URL: "https://www.example.co.uk/"}, Cookies: map[string]string{"www.example.co.uk": "k=v", "files.example.co.uk": "f=1"}}
	for host, want := range map[string]string{"www.example.co.uk": "k=v", "files.example.co.uk": "f=1", "cdn.example.co.uk": "k=v", "example.co.uk": "k=v", "other.co.uk": "", "example.com": ""} {
		if got := j.cookieFor(host); got != want {
			t.Errorf("%s: %q, want %q", host, got, want)
		}
	}
	// a start page that redirected to another host of the site takes in that one too
	j = &GrabJob{Opts: GrabOptions{URL: "https://example.com/", Sites: []string{"www.example.com"}}, Cookies: map[string]string{"example.com": "k=v"}}
	if j.cookieFor("www.example.com") != "k=v" {
		t.Fatal("the redirect target is the site too")
	}
	if (&GrabJob{Opts: GrabOptions{URL: "https://example.com/"}}).cookieFor("example.com") != "" {
		t.Fatal("no sign-in, no cookie")
	}
}

func TestSessionOnlySignsInTheSitesHost(t *testing.T) {
	m := newTestManager(t)
	id := m.grab.NewSession("https://example.com/", map[string]string{"example.com": "a=1", "www.example.com": "b=2", "evil.test": "c=3", "empty.example.com": ""})
	j := &GrabJob{Opts: GrabOptions{URL: "https://example.com/members"}}
	m.grab.signIn(j, id)
	if len(j.Cookies) != 2 || j.Cookies["evil.test"] != "" || strings.Join(j.SignedIn, ",") != "example.com,www.example.com" {
		t.Fatalf("%v %v", j.Cookies, j.SignedIn)
	}
	// a scan started on another site gets none of it
	j = &GrabJob{Opts: GrabOptions{URL: "https://other.org/"}}
	m.grab.signIn(j, id)
	if j.Cookies != nil || len(j.SignedIn) != 0 {
		t.Fatalf("%v %v", j.Cookies, j.SignedIn)
	}
}
