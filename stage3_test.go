package main

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDataCapPausesAndResumes(t *testing.T) {
	data := make([]byte, 3<<20)
	rand.Read(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.bin", time.Now(), bytes.NewReader(data))
	}))
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Cap = DataCap{On: true, MB: 1, Hours: 1}
	m.applyLimit()
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
	time.Sleep(1500 * time.Millisecond)
	m.mu.Lock()
	v := m.capView()
	done := d.done
	m.mu.Unlock()
	if !v.Blocked || done > 2<<20 || done < 1<<20 {
		t.Fatalf("cap should stop near 1 MB: blocked=%v done=%d", v.Blocked, done)
	}
	// raising the cap lets the same download carry on and finish intact
	m.mu.Lock()
	m.settings.Cap.MB = 10
	m.applyLimit()
	m.mu.Unlock()
	waitFor(t, m, d, StCompleted, 20*time.Second)
	got, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if !bytes.Equal(got, data) {
		t.Fatal("file corrupted after cap pause")
	}
}

func TestVirusScanCustomScanner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the scanner")
	}
	dir := t.TempDir()
	scanner := filepath.Join(dir, "fake-av.sh")
	os.WriteFile(scanner, []byte("#!/bin/sh\nif grep -q EVIL \"$2\"; then echo \"$2: Test.Signature FOUND\"; exit 1; fi\nexit 0\n"), 0o755)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "bad") {
			w.Write([]byte("this file is EVIL"))
			return
		}
		w.Write([]byte("hello, all good"))
	}))
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Scan = ScanSettings{On: true, Program: scanner, Args: "--quiet %f"}
	m.mu.Unlock()
	good := m.Add(AddReq{URL: srv.URL + "/good.txt", Start: true})
	bad := m.Add(AddReq{URL: srv.URL + "/bad.txt", Start: true})
	off := m.Add(AddReq{URL: srv.URL + "/bad2.txt"})
	m.mu.Lock()
	off.Scan = "off"
	m.mu.Unlock()
	m.Start([]string{off.ID})
	for _, d := range []*Download{good, bad, off} {
		waitFor(t, m, d, StCompleted, 10*time.Second)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		g, b := good.ScanState, bad.ScanState
		m.mu.Unlock()
		if (g == "clean" || g == "failed") && (b == "threat" || b == "failed") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if good.ScanState != "clean" || bad.ScanState != "threat" || off.ScanState != "" {
		t.Fatalf("scan states: good=%q(%s) bad=%q(%s) off=%q", good.ScanState, good.ScanNote, bad.ScanState, bad.ScanNote, off.ScanState)
	}
	if !strings.Contains(bad.ScanNote, "Test.Signature FOUND") {
		t.Fatalf("threat note should include the scanner's verdict: %q", bad.ScanNote)
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Queues = append(m.settings.Queues, Queue{ID: "night", Name: "Night", MaxConcurrent: 1})
	m.normalizeSettings()
	m.mu.Unlock()
	a := m.Add(AddReq{URL: "https://example.com/a.zip", Name: "a.zip", Dir: t.TempDir(), QueueID: "night",
		Headers: map[string]string{"Referer": "https://example.com/", "Cookie": "session=secret"}})
	m.Add(AddReq{URL: "ftp://files.example.com/pub/b.iso"})
	b, name := m.Export("json", nil, false)
	if !strings.HasSuffix(name, ".json") || strings.Contains(string(b), "session=secret") || !strings.Contains(string(b), "https://example.com/") {
		t.Fatalf("export should drop cookies but keep referer: %s", b)
	}
	withSecrets, _ := m.Export("json", nil, true)
	if !strings.Contains(string(withSecrets), "session=secret") {
		t.Fatal("secrets=true should keep cookies")
	}
	txt, _ := m.Export("txt", []string{a.ID}, false)
	if strings.TrimSpace(string(txt)) != "https://example.com/a.zip" {
		t.Fatalf("txt export: %q", txt)
	}

	m2 := newTestManager(t)
	res, err := m2.Import(string(b), "", false, true)
	if err != nil || res.Added != 2 || res.Format != "zipline" {
		t.Fatalf("import: %+v %v", res, err)
	}
	m2.mu.Lock()
	var got *Download
	for _, d := range m2.list {
		if d.URL == a.URL {
			got = d
		}
	}
	hasNight := m2.queueByID("night") != nil
	m2.mu.Unlock()
	if got == nil || got.Queue != "night" || !hasNight || got.Status != StQueued || got.Headers["Referer"] == "" {
		t.Fatalf("queue/headers not restored: %+v night=%v", got, hasNight)
	}
	again, _ := m2.Import(string(b), "", false, true)
	if again.Added != 0 || again.Skipped != 2 {
		t.Fatalf("duplicates should be skipped: %+v", again)
	}
}

func TestImportEF2AndPlainLinks(t *testing.T) {
	m := newTestManager(t)
	ef2 := "<\r\nhttps://dl.example.org/tool.exe\r\nreferer: https://example.org/downloads\r\ncookie: a=1\r\n>\r\n<\r\nhttps://dl.example.org/docs.pdf\r\n>\r\n"
	res, err := m.Import(ef2, "main", false, true)
	if err != nil || res.Added != 2 || res.Format != "ef2" {
		t.Fatalf("ef2: %+v %v", res, err)
	}
	m.mu.Lock()
	first := m.list[0]
	ref, ck, q := first.Headers["Referer"], first.Headers["Cookie"], first.Queue
	m.mu.Unlock()
	if ref != "https://example.org/downloads" || ck != "a=1" || q != "main" {
		t.Fatalf("ef2 headers/queue: %q %q %q", ref, ck, q)
	}
	res, err = m.Import("Some notes\nhttps://a.example/1.zip  https://a.example/2.zip\nnot a link\nftp://b.example/3.iso\n", "", false, true)
	if err != nil || res.Added != 3 || res.Format != "links" {
		t.Fatalf("plain: %+v %v", res, err)
	}
	if _, err := m.Import("nothing useful here", "", false, true); err == nil {
		t.Fatal("expected an error for a file without links")
	}
}

func TestParseCLI(t *testing.T) {
	r, err := parseCLI([]string{"--silent", "-s", "-o", "dl", "-q", "Night", "--referer=https://x.example/", "--user", "bob", "--password", "pw", "https://x.example/a.zip"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Silent || !r.Start || r.Queue != "Night" || len(r.URLs) != 1 || !filepath.IsAbs(r.Dir) || r.Headers["Referer"] != "https://x.example/" || !strings.HasPrefix(r.Headers["Authorization"], "Basic ") {
		t.Fatalf("parsed: %+v", r)
	}
	w, err := parseCLI([]string{"/d", "https://x.example/b.iso", "/p", `C:\ISO`, "/f", "disc.iso", "/n", "/a"}, true)
	if err != nil || w.URLs[0] != "https://x.example/b.iso" || w.Name != "disc.iso" || !w.Silent || w.Queue != "main" {
		t.Fatalf("windows switches: %+v %v", w, err)
	}
	for _, bad := range [][]string{{"--bogus"}, {"notalink"}, {"-n", "x", "https://a/1", "https://a/2"}, {"-o"}} {
		if _, err := parseCLI(bad, false); err == nil {
			t.Fatalf("expected error for %v", bad)
		}
	}
	if u, _ := parseCLI([]string{"/home/me/file"}, false); u != nil && len(u.URLs) > 0 {
		t.Fatal("a Unix path must not be read as a switch")
	}
}

func TestApplyCLISilentToNewQueue(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	note, err := m.ApplyCLI(&CLIRequest{URLs: []string{"https://x.example/1.zip", "https://x.example/2.zip"}, Queue: "Weekend", Silent: true, Dir: dir})
	if err != nil || !strings.Contains(note, "added 2") {
		t.Fatalf("%q %v", note, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var qid string
	for _, q := range m.settings.Queues {
		if q.Name == "Weekend" {
			qid = q.ID
		}
	}
	if qid == "" || len(m.list) != 2 || m.list[0].Queue != qid || m.list[0].Status != StQueued || m.list[0].Dir != dir {
		t.Fatalf("silent queue add failed: q=%q list=%d", qid, len(m.list))
	}
	if len(m.prompts) != 0 {
		t.Fatal("silent adds must not prompt")
	}
}

func TestPageLinksPickerAndCookies(t *testing.T) {
	m := newTestManager(t)
	j, err := m.grab.FromPage(PageLinks{Page: "https://site.example/list", Title: "Downloads",
		Links: []PageLink{{URL: "https://site.example/a.zip", Text: "Archive"}, {URL: "https://cdn.example/b.pdf#top"}, {URL: "https://site.example/a.zip"},
			{URL: "javascript:void(0)"}, {URL: "https://site.example/about"}},
		Cookies: map[string]string{"cdn.example": "k=v"}})
	if err != nil || len(j.Files) != 3 {
		t.Fatalf("links: %d %v", len(j.Files), err)
	}
	var urls []string
	for _, f := range j.Files {
		urls = append(urls, f.URL)
	}
	n, err := m.grab.AddFiles(GrabAddReq{JobID: j.ID, URLs: urls, QueueID: "main"})
	if err != nil || n != 3 {
		t.Fatal(n, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.list {
		if strings.Contains(d.URL, "cdn.example") && d.Headers["Cookie"] != "k=v" {
			t.Fatalf("per-host cookie missing: %+v", d.Headers)
		}
		if strings.Contains(d.URL, "site.example") && d.Headers["Cookie"] != "" {
			t.Fatal("cookie leaked to the wrong host")
		}
	}
}

func TestExtCLIRejectsWebPages(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: "t"}
	h := s.routes()
	body, _ := json.Marshal(CLIRequest{URLs: []string{"https://x.example/a.zip"}, Silent: true})
	req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/ext/cli", bytes.NewReader(body))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	ext := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/ext/cli", bytes.NewReader(body))
	ext.Header.Set("Origin", "chrome-extension://abc")
	ext.Header.Set("X-Token", "t")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, ext)
	local := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/ext/cli", bytes.NewReader(body))
	local.Header.Set("X-Token", "t") // (the command line: run.json's secret)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, local)
	if rec.Code != 403 || rec2.Code != 403 || rec3.Code != 200 {
		t.Fatalf("web=%d extension=%d local=%d", rec.Code, rec2.Code, rec3.Code)
	}
}
