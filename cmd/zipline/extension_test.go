package main

import (
	"bytes"
	"encoding/json"
	"io"
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

// formServer serves data only to a POST carrying want, with byte ranges when ranges is set. It
// counts the requests and the bodies it got.
func formServer(t *testing.T, data []byte, want string, ranges bool) (*httptest.Server, *int32, *[]string) {
	var n int32
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.Method+" "+string(b)+" "+r.Header.Get("Range"))
		mu.Unlock()
		if r.Method != "POST" || string(b) != want || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			http.Error(w, "the form is missing", 403)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="report.bin"`)
		if !ranges {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data)
			return
		}
		http.ServeContent(w, r, "report.bin", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)
	return srv, &n, &bodies
}

// A form's download (POST) is made as the browser made it, with ranges or (a server that doesn't
// do ranges on a POST) as one stream.
func TestFormDownload(t *testing.T) {
	data := bytes.Repeat([]byte("zipline form "), 300000) // 3.9 MB: more than one connection
	for _, ranges := range []bool{true, false} {
		srv, n, bodies := formServer(t, data, "q=report&year=2024", ranges)
		m := newTestManager(t)
		d := m.Add(AddReq{URL: srv.URL + "/export", Method: "POST", Body: []byte("q=report&year=2024"), Start: true,
			Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}})
		waitFor(t, m, d, StCompleted, 20*time.Second)
		fileIs(t, d, data)
		if d.FileName != "report.bin" {
			t.Errorf("name %q", d.FileName)
		}
		for _, b := range *bodies {
			if !strings.HasPrefix(b, "POST q=report&year=2024 ") {
				t.Errorf("ranges %v: the server got %q", ranges, b)
			}
		}
		if !ranges && *n != 1 {
			t.Errorf("no ranges: %d requests, want 1", *n)
		}
		if ranges && d.Resumable != true {
			t.Errorf("a server with ranges on POST should make it resumable")
		}
	}
}

// What the browser hands over when it takes a download: its request headers (minus those Zipline
// sets itself), its form, what the reply said and the window it came from. The Add dialog's probe
// then asks nothing of the server, and the early start is the only request.
func TestTakenDownloadOneRequest(t *testing.T) {
	data := bytes.Repeat([]byte("z"), 3<<20)
	srv, _, reqs := formServer(t, data, "id=7", true)
	m := newTestManager(t)
	m.settings.EarlyStart = true
	h := (&Server{m: m, token: "t"}).routes()
	res := extCall(t, h, "POST", "add", map[string]any{"url": srv.URL + "/get", "referer": srv.URL + "/page", "taken": true, "private": true,
		"method": "POST", "body": []byte("id=7"), "ctx": map[string]any{"store": "1", "private": true},
		"headers": map[string]string{"content-type": "application/x-www-form-urlencoded", "Accept-Encoding": "gzip, br", "Cookie": "s=1", "Range": "bytes=5-", "X-Requested-With": "zl"},
		"known":   map[string]any{"name": "report.bin", "size": len(data), "mime": "application/octet-stream", "finalUrl": srv.URL + "/get", "resumable": true}})
	if res["ok"] != true {
		t.Fatalf("add: %v", res)
	}
	m.mu.Lock()
	p := m.prompts[len(m.prompts)-1]
	m.mu.Unlock()
	if p.Method != "POST" || string(p.Body) != "id=7" || !p.Taken || !p.Private || p.Known == nil || p.Known.Size != int64(len(data)) {
		t.Fatalf("prompt %+v", p)
	}
	if p.Headers["Accept-Encoding"] != "" || p.Headers["Range"] != "" || p.Headers["Content-Type"] != "application/x-www-form-urlencoded" || p.Headers["Cookie"] != "s=1" || p.Headers["X-Requested-With"] != "zl" {
		t.Fatalf("headers %v", p.Headers)
	}
	// the window's Add dialog
	body, _ := json.Marshal(map[string]any{"url": p.URL, "headers": p.Headers, "early": true, "method": p.Method, "body": p.Body, "known": p.Known, "private": true})
	req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/probe", bytes.NewReader(body))
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var info map[string]any
	json.Unmarshal(rec.Body.Bytes(), &info)
	if info["name"] != "report.bin" || info["size"] != float64(len(data)) || info["resumable"] != true || info["early"] == nil {
		t.Fatalf("probe: %s", rec.Body.String())
	}
	m.mu.Lock()
	e := m.byID[info["early"].(string)]
	m.mu.Unlock()
	waitFor(t, m, e, StCompleted, 20*time.Second)
	if !e.Private || e.Method != "POST" {
		t.Fatalf("early download: private %v method %q", e.Private, e.Method)
	}
	// one request from the start of the file (the extra connections ask for their own ranges): nothing before it
	starts := 0
	for _, r := range *reqs {
		if strings.HasSuffix(r, " ") || strings.HasSuffix(r, " bytes=0-") {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("%d requests from the start of the file, want 1: %q", starts, *reqs)
	}
}

// Closing the Add dialog of a download taken from the browser hands it back, once, with what the
// extension needs to download it in the browser.
func TestGiveBack(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	window := func(path string, body any) map[string]any {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/"+path, bytes.NewReader(b))
		req.Header.Set("X-Token", "t")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	add := func() string {
		extCall(t, h, "POST", "add", map[string]any{"url": "https://files.example.com/get?id=3", "taken": true, "method": "POST", "body": []byte("id=3"),
			"cookie": "secret=1", "headers": map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, "ctx": map[string]any{"store": "firefox-container-2"}})
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.prompts[len(m.prompts)-1].ID
	}
	ping := extCall(t, h, "GET", "ping", nil)
	if ping["wait"] != false || ping["giveback"] != nil {
		t.Fatalf("nothing open yet: %v", ping)
	}
	id := add()
	if ping = extCall(t, h, "GET", "ping", nil); ping["wait"] != true {
		t.Fatalf("an Add dialog is open: the extension should keep checking: %v", ping)
	}
	// added: nothing goes back
	if r := window("browser/closed", map[string]any{"id": id, "added": true}); r["back"] != false {
		t.Fatalf("closed after adding: %v", r)
	}
	if ping = extCall(t, h, "GET", "ping", nil); ping["giveback"] != nil || ping["wait"] != false {
		t.Fatalf("%v", ping)
	}
	// dismissed: it goes back, through the waiting jobs call
	id = add()
	got := make(chan map[string]any)
	go func() { got <- extCall(t, h, "POST", "jobs", map[string]any{"wait": 10}) }()
	time.Sleep(100 * time.Millisecond)
	if r := window("browser/closed", map[string]any{"id": id, "added": false}); r["back"] != true {
		t.Fatalf("dismissed: %v", r)
	}
	var jobs map[string]any
	select {
	case jobs = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting jobs call didn't answer when the job came")
	}
	gb, _ := jobs["giveback"].([]any)
	if len(gb) != 1 {
		t.Fatalf("jobs %v", jobs)
	}
	j := gb[0].(map[string]any)
	hd, _ := j["headers"].(map[string]any)
	if j["url"] != "https://files.example.com/get?id=3" || j["method"] != "POST" || j["body"] != "aWQ9Mw==" || hd["Cookie"] != nil || hd["Content-Type"] == nil ||
		j["ctx"].(map[string]any)["store"] != "firefox-container-2" {
		t.Fatalf("give back job %v", j)
	}
	if ping = extCall(t, h, "GET", "ping", nil); ping["giveback"] != nil {
		t.Fatalf("handed out twice: %v", ping)
	}
}

// A private window's download is never saved, and leaves the list once finished (the file stays).
func TestPrivateDownload(t *testing.T) {
	old := privateLinger
	privateLinger = 200 * time.Millisecond
	defer func() { privateLinger = old }()
	data := bytes.Repeat([]byte("p"), 200000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "secret.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	m := newTestManager(t)
	pub := m.Add(AddReq{URL: srv.URL + "/public.bin"})
	d := m.Add(AddReq{URL: srv.URL + "/secret.bin", Private: true})
	m.save()
	b, _ := os.ReadFile(filepath.Join(m.dataDir, "downloads.json"))
	if strings.Contains(string(b), "secret.bin") || !strings.Contains(string(b), pub.ID) {
		t.Fatalf("the saved list: %s", b)
	}
	if v := m.Snapshot(nil); len(v.Downloads) != 2 || !v.Downloads[1].Private {
		t.Fatalf("the list should show it, marked private: %+v", v.Downloads)
	}
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 10*time.Second)
	fileIs(t, d, data)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(m.Snapshot(nil).Downloads) != 1 {
		time.Sleep(50 * time.Millisecond)
	}
	if v := m.Snapshot(nil); len(v.Downloads) != 1 || v.Downloads[0].ID != pub.ID {
		t.Fatalf("still listed: %+v", v.Downloads)
	}
	if _, err := os.Stat(d.path()); err != nil {
		t.Fatalf("the file should stay: %v", err)
	}
	m.mu.Lock()
	still := m.byID[d.ID] != nil
	m.mu.Unlock()
	if !still {
		t.Error("its Open buttons should keep working a while")
	}
}

// "Download all links" sends no cookies; pressing Download asks the browser for those of the
// chosen files' sites only, and adds them without (saying so) if it doesn't answer.
func TestPickerAsksForCookies(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	extCall(t, h, "GET", "ping", nil) // the extension is around
	res := extCall(t, h, "POST", "links", map[string]any{"page": "https://shop.example.com/files", "title": "Files", "private": true, "ctx": map[string]any{"store": "1"},
		"links": []map[string]string{{"url": "https://cdn.example.com/a.zip"}, {"url": "https://cdn.example.com/b.zip"}, {"url": "https://ads.tracker.test/c.zip"}, {"url": "https://other.example.org/d.pdf"}}})
	if res["ok"] != true {
		t.Fatalf("links: %v", res)
	}
	m.mu.Lock()
	jobID := m.prompts[len(m.prompts)-1].JobID
	m.mu.Unlock()
	add := func(urls ...string) map[string]any {
		b, _ := json.Marshal(map[string]any{"id": jobID, "urls": urls, "queueId": "main"})
		req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/grab/add", bytes.NewReader(b))
		req.Header.Set("X-Token", "t")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	// the extension: waiting for a job, answering it
	asked := make(chan []any, 1)
	go func() {
		for i := 0; i < 20; i++ {
			r := extCall(t, h, "POST", "jobs", map[string]any{"wait": 5})
			if c, ok := r["cookies"].([]any); ok && len(c) > 0 {
				job := c[0].(map[string]any)
				asked <- job["urls"].([]any)
				extCall(t, h, "POST", "cookies", map[string]any{"job": job["id"], "cookies": map[string]string{"cdn.example.com": "sid=42", "ads.tracker.test": "x=1"}})
				return
			}
		}
	}()
	out := add("https://cdn.example.com/a.zip", "https://cdn.example.com/b.zip")
	if out["added"] != float64(2) || out["note"] != nil {
		t.Fatalf("add: %v", out)
	}
	if urls := <-asked; len(urls) != 1 || urls[0] != "https://cdn.example.com/a.zip" {
		t.Fatalf("asked for %v: one address per chosen site", urls)
	}
	m.mu.Lock()
	for _, d := range m.list {
		if d.Headers["Cookie"] != "sid=42" || !d.Private {
			t.Errorf("%s: cookie %q private %v", d.URL, d.Headers["Cookie"], d.Private)
		}
	}
	m.mu.Unlock()
	// nobody answers: added without, and said so
	old := cookieWait
	cookieWait = 300 * time.Millisecond
	defer func() { cookieWait = old }()
	if out = add("https://other.example.org/d.pdf"); out["added"] != float64(1) || out["note"] != "nocookies" {
		t.Fatalf("no answer: %v", out)
	}
	m.mu.Lock()
	last := m.list[len(m.list)-1]
	m.mu.Unlock()
	if last.Headers["Cookie"] != "" || last.Note == "" {
		t.Fatalf("no answer: cookie %q note %q", last.Headers["Cookie"], last.Note)
	}
	// the picker closed: the extension can stop checking
	b, _ := json.Marshal(map[string]any{"job": jobID})
	req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/browser/closed", bytes.NewReader(b))
	req.Header.Set("X-Token", "t")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if p := extCall(t, h, "GET", "ping", nil); p["wait"] != false {
		t.Fatalf("after the picker closed: %v", p)
	}
}

// The extension's settings live in the app: its popup changes them, and every ping carries them.
func TestBrowserSettings(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	cfg := func() map[string]any {
		return extCall(t, h, "GET", "ping", nil)["config"].(map[string]any)["browser"].(map[string]any)
	}
	if b := cfg(); b["takeover"] != false || b["minKB"] != float64(256) || b["videoButton"] != true {
		t.Fatalf("defaults %v", b)
	}
	r := extCall(t, h, "POST", "settings", map[string]any{"takeover": true, "minKB": -5, "skipTypes": " .HTML  php ", "noButton": []string{" Example.com ", ""}})
	if r["ok"] != true {
		t.Fatalf("%v", r)
	}
	b := cfg()
	if b["takeover"] != true || b["minKB"] != float64(0) || b["skipTypes"] != "html php" || b["videoButton"] != true || len(b["noButton"].([]any)) != 1 || b["noButton"].([]any)[0] != "example.com" {
		t.Fatalf("after the popup: %v", b)
	}
	extCall(t, h, "POST", "settings", map[string]any{"videoButton": false})
	if b = cfg(); b["takeover"] != true || b["videoButton"] != false || len(b["noButton"].([]any)) != 1 {
		t.Fatalf("one change keeps the rest: %v", b)
	}
	// the window's theme goes along
	tb, _ := json.Marshal(map[string]string{"theme": "dark"})
	req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/theme", bytes.NewReader(tb))
	req.Header.Set("X-Token", "t")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if th := extCall(t, h, "GET", "ping", nil)["config"].(map[string]any)["theme"]; th != "dark" {
		t.Fatalf("theme %v", th)
	}
}
