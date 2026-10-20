package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudLinksRecognised(t *testing.T) {
	cases := map[string]string{
		"https://drive.google.com/file/d/1AbCdEfGhIjKlMnOp/view?usp=sharing":       "Google Drive",
		"https://drive.google.com/file/u/1/d/1AbCdEfGhIjKlMnOp/view":               "Google Drive",
		"https://drive.google.com/open?id=1AbCdEfGhIjKlMnOp":                       "Google Drive",
		"https://drive.google.com/uc?export=download&id=1AbCdEfGhIjKlMnOp":         "Google Drive",
		"https://docs.google.com/document/d/1AbCdEfGhIjKlMnOpQr/edit":              "Google Drive",
		"https://drive.google.com/drive/folders/1AbCdEfGhIjKlMnOp":                 "Google Drive", // (its files are listed: cloudfolder.go)
		"https://www.dropbox.com/scl/fi/abc123/report.pdf?rlkey=xyz&dl=0":          "Dropbox",
		"https://www.dropbox.com/s/abc123/photo.jpg?dl=0":                          "Dropbox",
		"https://www.dropbox.com/home":                                             "",
		"https://1drv.ms/u/s!AkfaAbCdEf":                                           "OneDrive",
		"https://contoso-my.sharepoint.com/:x:/g/personal/ann/EaBcD?e=abc":         "OneDrive",
		"https://contoso.sharepoint.com/sites/team/Shared%20Documents/report.docx": "",
		"https://example.com/file.zip":                                             "",
	}
	for u, want := range cases {
		if got := cloudService(u); got != want {
			t.Errorf("%s: %q, want %q", u, got, want)
		}
	}
	if got := dropboxDirect("https://www.dropbox.com/scl/fi/abc/r.pdf?rlkey=xyz&dl=0"); !strings.Contains(got, "dl=1") || !strings.Contains(got, "rlkey=xyz") {
		t.Errorf("dropbox: %s", got)
	}
	// the example from Microsoft's own forum
	if got := shareID("https://1drv.ms/t/c/b6a769b937e9eff3/EVtvodt7gEhKleFWNTy55vgBTFOziVeKp3kMvqG1wrrROA"); got != "u!aHR0cHM6Ly8xZHJ2Lm1zL3QvYy9iNmE3NjliOTM3ZTllZmYzL0VWdHZvZHQ3Z0VoS2xlRldOVHk1NXZnQlRGT3ppVmVLcDNrTXZxRzF3cnJST0E" {
		t.Errorf("share id %s", got)
	}
	sp, _ := resolveCloud(context.Background(), &Download{}, "https://contoso-my.sharepoint.com/:u:/g/personal/ann/EaBcD?e=abc")
	if !strings.Contains(sp, "download=1") || !strings.Contains(sp, "e=abc") {
		t.Errorf("sharepoint: %s", sp)
	}
	gd, _ := resolveCloud(context.Background(), &Download{}, "https://docs.google.com/spreadsheets/d/1AbCdEfGhIjKlMnOpQr/edit#gid=0")
	if gd != gdocsBase+"/spreadsheets/d/1AbCdEfGhIjKlMnOpQr/export?format=xlsx" {
		t.Errorf("sheets: %s", gd)
	}
}

// fakeDrive behaves like Google Drive's download service: small files straight away, big
// ones behind the "can't scan for viruses" page, whose code works for a limited number of
// requests (like the real one, which doesn't last).
func fakeDrive(t *testing.T, data []byte, big bool) (*fileHost, *int32) {
	h := newFileHost(t, data, 400<<10)
	h.cut = 512 << 10
	var pages, uuidN int32
	valid := map[string]int32{}
	h.route = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/download" {
			http.NotFound(w, r)
			return true
		}
		q := r.URL.Query()
		if strings.HasPrefix(q.Get("id"), "quota") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<html><body><p class="uc-error-caption">Sorry, you can't view or download this file at this time.</p><p class="uc-error-subcaption">Too many users have viewed or downloaded this file recently. Please try accessing the file again later.</p></body></html>`)
			return true
		}
		if q.Get("id") != "1BigFileIdForTests" {
			http.NotFound(w, r)
			return true
		}
		if big {
			h.mu.Lock()
			left, ok := valid[q.Get("uuid")]
			if ok && left > 0 {
				valid[q.Get("uuid")] = left - 1
			}
			h.mu.Unlock()
			if q.Get("confirm") != "t" || !ok || left <= 0 {
				atomic.AddInt32(&pages, 1)
				n := atomic.AddInt32(&uuidN, 1)
				id := fmt.Sprintf("uuid-%d", n)
				h.mu.Lock()
				valid[id] = 6
				h.mu.Unlock()
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>Google Drive - Virus scan warning</title></head><body>
<div class="uc-main"><p class="uc-warning-caption">Google Drive can't scan this file for viruses.</p>
<p class="uc-warning-subcaption"><span class="uc-name-size"><a href="/open?id=1BigFileIdForTests">disk.img</a> (6M)</span> is too large for Google to scan for viruses. Would you still like to download this file?</p>
<form id="download-form" action="%s/download" method="get"><input type="submit" id="uc-download-link" class="goog-inline-block jfk-button jfk-button-action" value="Download anyway"/><input type="hidden" name="id" value="1BigFileIdForTests"><input type="hidden" name="export" value="download"><input type="hidden" name="confirm" value="t"><input type="hidden" name="uuid" value="%s"></form></div></body></html>`, h.URL, id)
				return true
			}
		}
		w.Header().Set("Content-Disposition", `attachment; filename="disk image.img"; filename*=UTF-8''disk%20image.img`)
		return false
	}
	return h, &pages
}

func TestGoogleDriveSmallFile(t *testing.T) {
	data := randData(1 << 20)
	h, _ := fakeDrive(t, data, false)
	old := gdriveBase
	gdriveBase = h.URL
	defer func() { gdriveBase = old }()
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, srv, "probe", map[string]any{"url": "https://drive.google.com/file/d/1BigFileIdForTests/view?usp=sharing"})
	if info["name"] != "disk image.img" || info["service"] != "Google Drive" || info["size"] != float64(len(data)) {
		t.Fatalf("probe: %v", info)
	}
	d := m.Add(AddReq{URL: "https://drive.google.com/file/d/1BigFileIdForTests/view?usp=sharing", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
}

func TestGoogleDriveBigFileWarning(t *testing.T) {
	data := randData(6 << 20)
	h, pages := fakeDrive(t, data, true)
	old := gdriveBase
	gdriveBase = h.URL
	defer func() { gdriveBase = old }()
	m := newTestManager(t)
	m.settings.Conns = 4
	d := m.Add(AddReq{URL: "https://drive.google.com/file/d/1BigFileIdForTests/view", Start: true})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	fileIs(t, d, data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.FileName != "disk image.img" || !strings.HasPrefix(d.URL, "https://drive.google.com/") {
		t.Fatalf("name %q, url %q (the download should keep the share link)", d.FileName, d.URL)
	}
	t.Logf("warning pages passed: %d", atomic.LoadInt32(pages))
	if atomic.LoadInt32(pages) < 2 {
		t.Fatal("the short-lived download address should have been renewed through the share link")
	}
}

func TestGoogleDriveExplainsProblems(t *testing.T) {
	h, _ := fakeDrive(t, randData(1000), false)
	old := gdriveBase
	gdriveBase = h.URL
	defer func() { gdriveBase = old }()
	_, err := resolveCloud(context.Background(), &Download{}, "https://drive.google.com/uc?export=download&id=quota0000000")
	if err == nil {
		t.Fatal("expected an error")
	}
	_, err = resolveGDrive(context.Background(), &Download{}, "https://drive.google.com/open?id=quota00000")
	if err == nil || !strings.Contains(err.Error(), "Google Drive: Too many users have viewed or downloaded this file recently.") {
		t.Fatalf("quota: %v", err)
	}
}

func TestOneDriveShareLink(t *testing.T) {
	data := randData(2 << 20)
	h := newFileHost(t, data, 0)
	share := "https://1drv.ms/u/c/abc123/EfGhIjKl"
	h.route = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/token" && r.Method == "POST":
			fmt.Fprint(w, `{"token":"badger-token","expiryTimeUtc":"2030-01-01T00:00:00Z"}`)
			return true
		case strings.HasPrefix(r.URL.Path, "/v1.0/shares/") && strings.HasSuffix(r.URL.Path, "/driveItem"):
			if r.Header.Get("Authorization") != "Badger badger-token" || strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.0/shares/"), "/driveItem") != shareID(share) {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":{"code":"unauthenticated"}}`)
				return true
			}
			fmt.Fprintf(w, `{"name":"holiday.mov","size":%d,"@content.downloadUrl":"%s/files/holiday.mov?tempauth=xyz"}`, len(data), h.URL)
			return true
		case r.URL.Path == "/files/holiday.mov":
			if r.URL.Query().Get("tempauth") != "xyz" {
				http.Error(w, "unauthorized", 401)
				return true
			}
			w.Header().Set("Content-Disposition", `attachment; filename="holiday.mov"`)
		}
		return false
	}
	oldAPI, oldBadger, oldRedeem := onedriveAPI, badgerURL, oneDriveRedeem
	onedriveAPI, badgerURL = h.URL+"/v1.0", h.URL+"/token"
	oneDriveRedeem = func(context.Context, *Download, string) string { return "" }
	defer func() { onedriveAPI, badgerURL, oneDriveRedeem = oldAPI, oldBadger, oldRedeem }()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: share, Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	if d.FileName != "holiday.mov" {
		t.Fatalf("name %q", d.FileName)
	}
	_ = url.Parse
}
