package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// svcFake stands in for a cloud service: route answers its API and pages, and files are served
// (with byte ranges) at their paths.
type svcFake struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	route func(w http.ResponseWriter, r *http.Request) bool
	seen  []*http.Request
}

func newSvcFake(t *testing.T) *svcFake {
	f := &svcFake{files: map[string][]byte{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(context.Background()))
		route, data, ok := f.route, f.files[r.URL.Path], f.files[r.URL.Path] != nil
		f.mu.Unlock()
		if route != nil && route(w, r) {
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *svcFake) file(path string, data []byte) {
	f.mu.Lock()
	f.files[path] = data
	f.mu.Unlock()
}

// requests lists what was asked of the fake whose path starts with prefix.
func (f *svcFake) requests(prefix string) []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*http.Request
	for _, r := range f.seen {
		if strings.HasPrefix(r.URL.Path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// swap sets a package variable for one test.
func swap[T any](t *testing.T, p *T, v T) {
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

func writeJSONTest(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func TestMoreCloudLinksRecognised(t *testing.T) {
	cases := map[string]string{
		"https://drive.google.com/drive/folders/1AbCdEfGhIjKlMnOp?usp=sharing":           "Google Drive",
		"https://drive.google.com/drive/u/0/folders/1AbCdEfGhIjKlMnOp":                   "Google Drive",
		"https://drive.google.com/file/d/1AbCdEfGhIjKlMnOp/view?resourcekey=0-abc":       "Google Drive",
		"https://1drv.ms/f/c/b6a769b937e9eff3/EgAbCdEf":                                  "OneDrive",
		"https://1drv.ms/u/c/b6a769b937e9eff3/EVtvodt7gEhK":                              "OneDrive",
		"https://onedrive.live.com/?cid=B6A769B937E9EFF3&id=B6A769B937E9EFF3%21123":      "OneDrive",
		"https://onedrive.live.com/?id=B6A769B937E9EFF3%21123&authkey=%21AbC":            "OneDrive",
		"https://onedrive.live.com/":                                                     "",
		"https://onedrive.cloud.microsoft/?id=B6A769B937E9EFF3%21123&cid=B6A769B937E9EF": "OneDrive",
		"https://onedrive.cloud.microsoft/":                                              "",
		"https://contoso-my.sharepoint.com/:f:/g/personal/ann/EaBcD?e=abc":               "OneDrive",
		"https://mega.nz/file/AbCd1234#xyz":                                              "MEGA",
		"https://mega.nz/#!AbCd1234!xyz":                                                 "MEGA",
		"https://mega.nz/folder/AbCd1234#xyz":                                            "MEGA",
		"https://mega.nz/pro":                                                            "",
		"https://disk.yandex.ru/d/AbCdEf123":                                             "Yandex Disk",
		"https://disk.yandex.com/d/AbCdEf123/Photos/a.jpg":                               "Yandex Disk",
		"https://yadi.sk/d/AbCdEf123":                                                    "Yandex Disk",
		"https://disk.360.yandex.ru/i/AbCdEf123":                                         "Yandex Disk",
		"https://disk.yandex.ru/client/disk":                                             "",
		"https://u.pcloud.link/publink/show?code=XZabc":                                  "pCloud",
		"https://e.pcloud.link/publink/show?code=XZabc":                                  "pCloud",
		"https://my.pcloud.com/#page=publink&code=XZabc":                                 "pCloud",
		"https://www.pcloud.com/":                                                        "",
		"https://www.mediafire.com/file/ab12cd34ef/report.pdf/file":                      "MediaFire",
		"https://www.mediafire.com/download/ab12cd34ef":                                  "MediaFire",
		"https://www.mediafire.com/?ab12cd34ef":                                          "MediaFire",
		"https://www.mediafire.com/folder/ab12cd34ef/Stuff":                              "MediaFire",
		"https://www.mediafire.com/upgrade/":                                             "",
		"https://we.tl/t-AbCdEf123":                                                      "WeTransfer",
		"https://wetransfer.com/downloads/0123abcd/4567ef":                               "WeTransfer",
		"https://wetransfer.com/downloads/0123abcd/rcpt99/4567ef":                        "WeTransfer",
		"https://acme.wetransfer.com/downloads/0123abcd/4567ef":                          "WeTransfer",
		"https://wetransfer.com/pricing":                                                 "",
	}
	for u, want := range cases {
		if got := cloudService(u); got != want {
			t.Errorf("%s: %q, want %q", u, got, want)
		}
	}
	// a clipboard copy of any of them asks to download it
	m := newTestManager(t)
	if !m.offerClipboard("https://mega.nz/file/AbCd1234#xyz") || !m.offerClipboard("https://disk.yandex.ru/d/AbCdEf123") {
		t.Error("cloud links should be offered from the clipboard")
	}
}

// ---- Google Drive

func TestGoogleDriveResourceKeyAndAnyForm(t *testing.T) {
	data := randData(700 << 10)
	f := newSvcFake(t)
	swap(t, &gdriveBase, f.URL)
	swap(t, &gdriveKeyHosts, []string{"google.com", "googleusercontent.com", "127.0.0.1"})
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query()
		if r.URL.Path != "/download" {
			return false
		}
		if q.Get("resourcekey") != "0-KeyFromLink" || r.Header.Get("X-Goog-Drive-Resource-Keys") != "1KeyedFileId0000/0-KeyFromLink" {
			w.WriteHeader(404)
			fmt.Fprint(w, `<html><body><p class="errorMessage">Sorry, the file you have requested does not exist.</p></body></html>`)
			return true
		}
		if q.Get("at") != "AKey:123" { // the warning page, with a form that isn't the usual one
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<html><title>Google Drive - Virus scan warning</title><body><form action="%s/download" method="get"><input type="hidden" name="id" value="1KeyedFileId0000"><input type="hidden" name="export" value="download"><input type="hidden" name="confirm" value="t"><input type="hidden" name="uuid" value="u-1"><input type="hidden" name="at" value="AKey:123"><input type="submit" value="Download anyway"></form></body></html>`, f.URL)
			return true
		}
		w.Header().Set("Content-Disposition", `attachment; filename="old share.bin"`)
		http.ServeContent(w, r, "", time.Unix(1700000000, 0), bytes.NewReader(data))
		return true
	}
	m := newTestManager(t)
	link := "https://drive.google.com/file/d/1KeyedFileId0000/view?usp=sharing&resourcekey=0-KeyFromLink"
	d := m.Add(AddReq{URL: link, Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	if d.FileName != "old share.bin" {
		t.Fatalf("name %q", d.FileName)
	}
	// without the key Google says the file doesn't exist, in its words
	_, err := resolveGDrive(context.Background(), &Download{}, "https://drive.google.com/file/d/1KeyedFileId0000/view")
	if err == nil || !strings.Contains(err.Error(), "Google Drive: Sorry, the file you have requested does not exist.") {
		t.Fatalf("no key: %v", err)
	}
}

func TestGoogleDriveSaysGooglesWords(t *testing.T) {
	f := newSvcFake(t)
	swap(t, &gdriveBase, f.URL)
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Query().Get("id") {
		case "quota000000":
			fmt.Fprint(w, `<html><body><p class="uc-error-caption">Sorry, you can't view or download this file at this time.</p><p class="uc-error-subcaption">Too many users have viewed or downloaded this file recently. Please try accessing the file again later. If the file you are trying to access is particularly large or is shared with many people, it may take up to 24 hours to be able to view or download the file.</p></body></html>`)
		case "private0000":
			w.WriteHeader(403)
			fmt.Fprint(w, `<html><head><title>Google Drive</title></head><body><h1 class="title">You need access</h1><p>Ask for access, or switch to an account with access.</p></body></html>`)
		case "keyed000000": // an older file: its page knows the resource key
			if r.URL.Query().Get("resourcekey") == "0-FromThePage1" {
				w.Header().Set("Content-Type", "application/octet-stream")
				fmt.Fprint(w, "x")
				return true
			}
			w.WriteHeader(404)
			fmt.Fprint(w, `<html><body><a href="/open?id=keyed000000&amp;resourcekey=0-FromThePage1">open</a></body></html>`)
		default:
			return false
		}
		return true
	}
	ctx := context.Background()
	for id, want := range map[string]string{
		"quota000000": "Google Drive: Too many users have viewed or downloaded this file recently. Please try accessing the file again later.",
		"private0000": "Google Drive: You need access. The file isn't shared",
		"missing0000": "Google Drive: Sorry, the file you have requested does not exist.",
	} {
		_, err := resolveGDrive(ctx, &Download{}, "https://drive.google.com/open?id="+id)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%s: %v\nwant %q", id, err, want)
		}
	}
	if u, err := resolveGDrive(ctx, &Download{}, "https://drive.google.com/open?id=keyed000000"); err != nil || !strings.Contains(u, "resourcekey=0-FromThePage1") {
		t.Errorf("key from the page: %s %v", u, err)
	}
	if _, err := resolveCloud(ctx, &Download{}, "https://drive.google.com/drive/folders/1FolderId00000"); err == nil || !strings.Contains(err.Error(), "shared folder") {
		t.Errorf("a folder added as one download: %v", err)
	}
}

// jsEscape writes s as Drive's pages write a JavaScript string: punctuation as \xNN, other
// letters as \uNNNN (surrogate pairs for emoji).
func jsEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case strings.ContainsRune(`"[]{},:'\<>=&`, r):
			fmt.Fprintf(&b, `\x%02x`, r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		case r > 126:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// driveFake is a shared Drive folder (its page with the list for its scripts), a folder in it
// shown only in the plain embedded view, and the files' downloads.
func driveFake(t *testing.T) (*svcFake, map[string][]byte) {
	f := newSvcFake(t)
	swap(t, &gdriveBase, f.URL)
	swap(t, &gdocsBase, f.URL)
	swap(t, &gdriveWeb, f.URL)
	files := map[string][]byte{
		"1BeachJpg000000": randData(300 << 10), "1CafeTxt0000000": []byte("menu"), "1PartyPng000000": randData(5000),
		"1TodoTxt0000000": []byte("buy film"), "1PlanDoc0000000": randData(20000),
	}
	names := map[string]string{"1BeachJpg000000": "beach.jpg", "1CafeTxt0000000": "Café ☕.txt", "1PartyPng000000": "🎉.png", "1TodoTxt0000000": "todo.txt"}
	rows, _ := json.Marshal([]any{[]any{
		[]any{"1BeachJpg000000", []string{"1HolidayFolder0"}, "beach.jpg", "image/jpeg", 1, nil},
		[]any{"1CafeTxt0000000", []string{"1HolidayFolder0"}, "Café ☕.txt", "text/plain"},
		[]any{"1PartyPng000000", []string{"1HolidayFolder0"}, "🎉.png", "image/png"},
		[]any{"1NotesFolder000", []string{"1HolidayFolder0"}, "Notes", "application/vnd.google-apps.folder"},
		[]any{"1PlanDoc0000000", []string{"1HolidayFolder0"}, "Plan", "application/vnd.google-apps.document"},
		[]any{"1FormXxx0000000", []string{"1HolidayFolder0"}, "Survey", "application/vnd.google-apps.form"},
	}, nil, 1})
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/drive/folders/1HolidayFolder0":
			fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>Holiday - Google Drive</title></head><body><script nonce="n">window['_DRIVE_ivd'] = '%s';if (window['_DRIVE_ivdc']) {window['_DRIVE_ivdc']();}</script></body></html>`, jsEscape(string(rows)))
		case r.URL.Path == "/drive/folders/1NotesFolder000":
			fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Notes - Google Drive</title></head><body>(a page without the list)</body></html>`)
		case r.URL.Path == "/embeddedfolderview" && r.URL.Query().Get("id") == "1NotesFolder000":
			fmt.Fprint(w, `<!DOCTYPE html><html><head><title>Notes</title></head><body><div class="flip-entries"><a href="https://drive.google.com/file/d/1TodoTxt0000000/view?usp=drive_web" target="_blank"><div class="flip-entry"><div class="flip-entry-title">todo.txt</div></div></a></div></body></html>`)
		case r.URL.Path == "/download" && files[r.URL.Query().Get("id")] != nil:
			id := r.URL.Query().Get("id")
			w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+url.PathEscape(names[id]))
			http.ServeContent(w, r, "", time.Unix(1700000000, 0), bytes.NewReader(files[id]))
		case r.URL.Path == "/document/d/1PlanDoc0000000/export":
			w.Header().Set("Content-Disposition", `attachment; filename="Plan.docx"`)
			http.ServeContent(w, r, "", time.Unix(1700000000, 0), bytes.NewReader(files["1PlanDoc0000000"]))
		default:
			return false
		}
		return true
	}
	return f, files
}

func TestGoogleDriveFolder(t *testing.T) {
	_, files := driveFake(t)
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	link := "https://drive.google.com/drive/folders/1HolidayFolder0?usp=sharing"
	info := apiCall(t, srv, "probe", map[string]any{"url": link})
	folder, _ := info["folder"].(map[string]any)
	if info["service"] != "Google Drive" || folder == nil || folder["name"] != "Holiday" {
		t.Fatalf("probe: %v", info)
	}
	var got []string
	for _, f := range folder["files"].([]any) {
		got = append(got, f.(map[string]any)["name"].(string))
	}
	want := []string{"beach.jpg", "Café ☕.txt", "🎉.png", "Notes/todo.txt", "Plan.docx"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("files %q, want %q", got, want)
	}
	res := apiCall(t, srv, "add", map[string]any{"url": link, "meta": true, "metaFile": -1, "dir": m.settings.DownloadDir, "start": true})
	ids, _ := res["ids"].([]any)
	if len(ids) != 5 {
		t.Fatalf("add: %v", res)
	}
	for _, id := range ids {
		d := earlyDownload(m, id.(string))
		waitFor(t, m, d, StCompleted, 30*time.Second)
	}
	for rel, id := range map[string]string{"beach.jpg": "1BeachJpg000000", "Café ☕.txt": "1CafeTxt0000000", "🎉.png": "1PartyPng000000", "Notes/todo.txt": "1TodoTxt0000000", "Plan.docx": "1PlanDoc0000000"} {
		b, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, "Holiday", filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(b, files[id]) {
			t.Errorf("%s: %v", rel, err)
		}
	}
	// one file of it: straight into the folder chosen, under the name given
	res = apiCall(t, srv, "add", map[string]any{"url": link, "meta": true, "metaFile": 3, "name": "my todo.txt", "dir": m.settings.DownloadDir, "start": true})
	d := earlyDownload(m, res["id"].(string))
	waitFor(t, m, d, StCompleted, 30*time.Second)
	if b, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, "my todo.txt")); err != nil || string(b) != "buy film" {
		t.Fatalf("one file: %v %q", err, b)
	}
}

// ---- OneDrive

// onedriveFake answers OneDrive's sharing API for a file link and a folder link.
func onedriveFake(t *testing.T) (*svcFake, *int32) {
	f := newSvcFake(t)
	var tokens int32
	swap(t, &onedriveAPI, f.URL+"/v1.0")
	swap(t, &badgerURL, f.URL+"/token")
	swap(t, &oneDriveRedeem, func(context.Context, *Download, string) string { return "" })
	return f, &tokens
}

func TestOneDriveNewLinksQuickXorAndToken(t *testing.T) {
	f, tokens := onedriveFake(t)
	good, bad := randData(900<<10), randData(300<<10)
	sum := func(b []byte) string {
		h := newQuickXor()
		h.Write(b)
		return base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	links := map[string]string{ // share link → file
		"https://1drv.ms/u/c/b6a769b937e9eff3/IQBgood": "good.bin",
		"https://onedrive.cloud.microsoft/?id=B6A769B937E9EFF3%21s99&cid=B6A769B937E9EFF3&redeem=aHR0cHM6Ly8xZHJ2Lm1zL3UvYy9iYWQ": "bad.bin",
	}
	f.file("/files/good.bin", good)
	f.file("/files/bad.bin", bad)
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/token" && r.Method == "POST":
			atomic.AddInt32(tokens, 1)
			fmt.Fprint(w, `{"token":"badger-1","expiryTimeUtc":"2099-01-01T00:00:00Z"}`)
		case strings.HasPrefix(r.URL.Path, "/v1.0/shares/"):
			if r.Header.Get("Authorization") != "Badger badger-1" || r.Header.Get("Prefer") != "autoredeem" {
				writeJSONTest(w, 401, map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "Exception of type 'Microsoft.Vroom.Exceptions.UnauthenticatedVroomException' was thrown."}})
				return true
			}
			sid := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1.0/shares/"), "/driveItem")
			switch sid {
			case shareID("https://1drv.ms/u/c/b6a769b937e9eff3/IQBgood"):
				writeJSONTest(w, 200, map[string]any{"id": "G", "name": "good.bin", "size": len(good), "file": map[string]any{"hashes": map[string]string{"quickXorHash": sum(good)}}, "@content.downloadUrl": f.URL + "/files/good.bin?tempauth=1"})
			case "u!aHR0cHM6Ly8xZHJ2Lm1zL3UvYy9iYWQ": // (the link's redeem code)
				writeJSONTest(w, 200, map[string]any{"id": "B", "name": "bad.bin", "size": len(bad), "file": map[string]any{"hashes": map[string]string{"quickXorHash": sum(good)}}, "@content.downloadUrl": f.URL + "/files/bad.bin?tempauth=1"})
			default:
				writeJSONTest(w, 404, map[string]any{"error": map[string]string{"code": "itemNotFound", "message": "Item does not exist"}})
			}
		default:
			return false
		}
		return true
	}
	m := newTestManager(t)
	var ds []*Download
	for link := range links {
		ds = append(ds, m.Add(AddReq{URL: link, Start: true}))
	}
	for _, d := range ds {
		waitFor(t, m, d, StCompleted, 30*time.Second)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range ds {
		c := d.Check
		switch d.FileName {
		case "good.bin":
			if c == nil || c.Algo != "quickxor" || c.State != "ok" {
				t.Errorf("good.bin's check: %+v", c)
			}
		case "bad.bin": // OneDrive's hash is another file's: the file is flagged
			if c == nil || c.Algo != "quickxor" || c.State != "bad" {
				t.Errorf("bad.bin's check: %+v", c)
			}
		default:
			t.Errorf("name %q", d.FileName)
		}
	}
	if n := atomic.LoadInt32(tokens); n != 1 {
		t.Errorf("the anonymous token was fetched %d times (it should be kept)", n)
	}
	if _, err := resolveCloud(context.Background(), &Download{}, "https://1drv.ms/u/c/b6a769b937e9eff3/IQBgone"); err == nil || !strings.Contains(err.Error(), "OneDrive: Item does not exist") {
		t.Errorf("missing: %v", err)
	}
}

func TestOneDriveFolder(t *testing.T) {
	f, _ := onedriveFake(t)
	link := "https://1drv.ms/f/c/b6a769b937e9eff3/IgBtrip"
	sid := shareID(link)
	data := map[string][]byte{"A": randData(200 << 10), "B": []byte("bee"), "C": randData(70000)}
	item := func(id, name string) map[string]any {
		return map[string]any{"id": id, "name": name, "size": len(data[id]), "file": map[string]any{}}
	}
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		p := strings.TrimPrefix(r.URL.Path, "/v1.0/shares/"+sid)
		switch {
		case r.URL.Path == "/token":
			fmt.Fprint(w, `{"token":"t2","expiryTimeUtc":"2099-01-01T00:00:00Z"}`)
		case p == "/driveItem" && r.URL.Query().Get("$expand") == "children":
			writeJSONTest(w, 200, map[string]any{"id": "ROOT", "name": "Trip", "folder": map[string]any{"childCount": 3},
				"children":                []any{item("A", "map.pdf"), map[string]any{"id": "SUB", "name": "Day 1", "folder": map[string]any{}}},
				"children@odata.nextLink": onedriveAPI + "/shares/" + sid + "/items/ROOT/children?$skiptoken=2"})
		case p == "/driveItem":
			writeJSONTest(w, 200, map[string]any{"id": "ROOT", "name": "Trip", "folder": map[string]any{"childCount": 3}})
		case p == "/items/ROOT/children" && r.URL.Query().Get("$skiptoken") == "2":
			writeJSONTest(w, 200, map[string]any{"value": []any{item("B", "notes.txt")}})
		case p == "/items/SUB/children":
			writeJSONTest(w, 200, map[string]any{"value": []any{item("C", "photo.jpg")}})
		case strings.HasPrefix(p, "/items/"):
			id := strings.TrimPrefix(p, "/items/")
			it := item(id, map[string]string{"A": "map.pdf", "B": "notes.txt", "C": "photo.jpg"}[id])
			it["@content.downloadUrl"] = f.URL + "/files/" + id
			writeJSONTest(w, 200, it)
		case strings.HasPrefix(r.URL.Path, "/files/"):
			http.ServeContent(w, r, "", time.Unix(1700000000, 0), bytes.NewReader(data[strings.TrimPrefix(r.URL.Path, "/files/")]))
		default:
			return false
		}
		return true
	}
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, srv, "probe", map[string]any{"url": link})
	folder, _ := info["folder"].(map[string]any)
	if folder == nil || folder["name"] != "Trip" || folder["total"] != float64(len(data["A"])+len(data["B"])+len(data["C"])) || info["service"] != "OneDrive" {
		t.Fatalf("probe: %v", info)
	}
	res := apiCall(t, srv, "add", map[string]any{"url": link, "meta": true, "metaFile": -1, "dir": m.settings.DownloadDir, "start": true})
	for _, id := range res["ids"].([]any) {
		waitFor(t, m, earlyDownload(m, id.(string)), StCompleted, 30*time.Second)
	}
	for rel, id := range map[string]string{"map.pdf": "A", "notes.txt": "B", "Day 1/photo.jpg": "C"} {
		if b, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, "Trip", filepath.FromSlash(rel))); err != nil || !bytes.Equal(b, data[id]) {
			t.Errorf("%s: %v", rel, err)
		}
	}
}

// ---- Dropbox

func TestDropboxRefusalPages(t *testing.T) {
	reply := func(link string, code int, ctype, disp, body string) *http.Response {
		req, _ := http.NewRequest("GET", link, nil)
		h := http.Header{"Content-Type": {ctype}}
		if disp != "" {
			h.Set("Content-Disposition", disp)
		}
		return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}
	}
	share := "https://www.dropbox.com/scl/fi/abc/report.pdf?rlkey=x&dl=1"
	ban := `<!DOCTYPE html><html><head><title>Dropbox - 429</title></head><body><div class="err"><h1>Error (429)</h1><p>This account's links are generating too much traffic and have been temporarily disabled!</p></div></body></html>`
	for _, c := range []struct {
		resp *http.Response
		want string // "" = the file
	}{
		{reply(share, 429, "text/html", "", ban), "Dropbox: This account's links are generating too much traffic and have been temporarily disabled!"},
		{reply("https://dl.dropboxusercontent.com/s/abc/report.pdf", 429, "text/html", "", ban), "too much traffic"},
		{reply(share, 200, "text/html; charset=utf-8", "", `<html><head><title>Dropbox - Password required</title></head></html>`), "password"},
		{reply(share, 200, "text/html", "", `<html><head><title>Log in to Dropbox</title></head></html>`), "sign in"},
		{reply(share, 404, "text/html", "", `<html><head><title>Dropbox - Invalid Link - Simplify your life</title></head></html>`), "isn't valid any more (Dropbox - Invalid Link - Simplify your life)"},
		{reply(share, 200, "text/html", `attachment; filename="page.html"`, `<html>a shared web page</html>`), ""},
		{reply(share, 200, "application/pdf", `attachment; filename="report.pdf"`, `%PDF`), ""},
		{reply("https://dl.dropboxusercontent.com/s/abc/report.pdf", 200, "text/html", "", `<html>expired</html>`), ""}, // (the engine renews the address)
	} {
		err := dropboxRefusal(c.resp)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.resp.Request.URL, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s %d: %v, want %q", c.resp.Request.URL, c.resp.StatusCode, err, c.want)
		}
		if c.want == "" { // the reply is still whole
			if b, _ := io.ReadAll(c.resp.Body); len(b) == 0 {
				t.Error("the reply's body was lost")
			}
		}
	}
}

// ---- Yandex Disk

func TestYandexDiskFileAndFolder(t *testing.T) {
	f := newSvcFake(t)
	swap(t, &yandexAPI, f.URL+"/v1/disk/public/resources")
	report, a, b := randData(400<<10), randData(1000), randData(2000)
	sha := func(x []byte) string { s := sha256.Sum256(x); return hex.EncodeToString(s[:]) }
	f.file("/ydl/report.pdf", report)
	f.file("/ydl/a.jpg", a)
	f.file("/ydl/b.jpg", b)
	file := func(name, p string, x []byte) map[string]any {
		return map[string]any{"type": "file", "name": name, "path": p, "size": len(x), "sha256": sha(x)}
	}
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		q := r.URL.Query()
		key, p := q.Get("public_key"), q.Get("path")
		dl := strings.HasSuffix(r.URL.Path, "/download")
		var it map[string]any
		switch {
		case !strings.HasPrefix(r.URL.Path, "/v1/disk/public/resources"):
			return false
		case key == "https://disk.yandex.ru/d/FileKey123" && p == "":
			it = file("report.pdf", "/", report)
		case key == "https://disk.yandex.ru/d/FolderKey9" && p == "":
			it = map[string]any{"type": "dir", "name": "Photos", "path": "/", "_embedded": map[string]any{"total": 2, "items": []any{file("a.jpg", "/a.jpg", a), map[string]any{"type": "dir", "name": "2024", "path": "/2024"}}}}
		case key == "https://disk.yandex.ru/d/FolderKey9" && p == "/2024":
			it = map[string]any{"type": "dir", "name": "2024", "path": "/2024", "_embedded": map[string]any{"total": 1, "items": []any{file("b.jpg", "/2024/b.jpg", b)}}}
		case key == "https://disk.yandex.ru/d/FolderKey9" && p == "/a.jpg":
			it = file("a.jpg", "/a.jpg", a)
		case key == "https://disk.yandex.ru/d/FolderKey9" && p == "/2024/b.jpg":
			it = file("b.jpg", "/2024/b.jpg", b)
		default:
			writeJSONTest(w, 404, map[string]string{"message": "Не удалось найти запрошенный ресурс.", "description": "Resource not found.", "error": "DiskNotFoundError"})
			return true
		}
		if dl {
			it = map[string]any{"href": f.URL + "/ydl/" + it["name"].(string) + "?sig=1", "method": "GET", "templated": false}
		}
		writeJSONTest(w, 200, it)
		return true
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: "https://disk.yandex.ru/d/FileKey123", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, report)
	m.mu.Lock()
	c := d.Check
	m.mu.Unlock()
	if d.FileName != "report.pdf" || c == nil || c.Algo != "sha256" || c.State != "ok" {
		t.Fatalf("name %q, check %+v", d.FileName, c)
	}
	srv := (&Server{m: m, token: "t"}).routes()
	res := apiCall(t, srv, "add", map[string]any{"url": "https://disk.yandex.ru/d/FolderKey9", "meta": true, "metaFile": -1, "start": true})
	for _, id := range res["ids"].([]any) {
		waitFor(t, m, earlyDownload(m, id.(string)), StCompleted, 30*time.Second)
	}
	for rel, want := range map[string][]byte{"a.jpg": a, "2024/b.jpg": b} {
		if got, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, "Photos", filepath.FromSlash(rel))); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %v", rel, err)
		}
	}
	if _, err := resolveYandex(context.Background(), "https://disk.yandex.ru/d/Missing00"); err == nil || !strings.Contains(err.Error(), "Yandex Disk: Resource not found") {
		t.Errorf("missing: %v", err)
	}
}

// ---- pCloud

func TestPCloudRegionsAndFolders(t *testing.T) {
	f := newSvcFake(t)
	swap(t, &pcloudAPIs, []string{f.URL + "/us", f.URL + "/eu"})
	swap(t, &pcloudScheme, "http://")
	song, a, b := randData(500<<10), []byte("aaa"), randData(3000)
	f.file("/cBZ/song.mp3", song)
	f.file("/cBZ/a.txt", a)
	f.file("/cBZ/b.bin", b)
	host := f.Listener.Addr().String()
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		region, method, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		q := r.URL.Query()
		code := q.Get("code")
		if region != "us" && region != "eu" {
			return false
		}
		switch {
		case code == "GONE":
			writeJSONTest(w, 200, map[string]any{"result": 7002, "error": "This link is deleted by the owner."})
		case code == "EUSONG" && region == "us": // an EU link is unknown to the US API
			writeJSONTest(w, 200, map[string]any{"result": 7001, "error": "Invalid link 'code'."})
		case code == "EUSONG" && method == "showpublink":
			writeJSONTest(w, 200, map[string]any{"result": 0, "metadata": map[string]any{"name": "song.mp3", "size": len(song), "isfolder": false, "fileid": 42}})
		case code == "EUSONG" && method == "getpublinkdownload":
			writeJSONTest(w, 200, map[string]any{"result": 0, "hosts": []string{host}, "path": "/cBZ/song.mp3"})
		case code == "FOLD" && method == "showpublink":
			writeJSONTest(w, 200, map[string]any{"result": 0, "metadata": map[string]any{"name": "Shared", "isfolder": true, "contents": []any{
				map[string]any{"name": "a.txt", "size": len(a), "fileid": 1},
				map[string]any{"name": "Sub", "isfolder": true, "contents": []any{map[string]any{"name": "b.bin", "size": len(b), "fileid": 2}}}}}})
		case code == "FOLD" && method == "getpublinkdownload":
			writeJSONTest(w, 200, map[string]any{"result": 0, "hosts": []string{host}, "path": map[string]string{"1": "/cBZ/a.txt", "2": "/cBZ/b.bin"}[q.Get("fileid")]})
		default:
			writeJSONTest(w, 200, map[string]any{"result": 7001, "error": "Invalid link 'code'."})
		}
		return true
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: "https://u.pcloud.link/publink/show?code=EUSONG", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, song)
	if d.FileName != "song.mp3" {
		t.Fatalf("name %q", d.FileName)
	}
	if n := len(f.requests("/us/")); n != 2 { // asked the US API first (show, download), then the EU one
		t.Errorf("US API asked %d times", n)
	}
	srv := (&Server{m: m, token: "t"}).routes()
	res := apiCall(t, srv, "add", map[string]any{"url": "https://e.pcloud.link/publink/show?code=FOLD", "meta": true, "metaFile": -1, "start": true})
	for _, id := range res["ids"].([]any) {
		waitFor(t, m, earlyDownload(m, id.(string)), StCompleted, 30*time.Second)
	}
	for rel, want := range map[string][]byte{"a.txt": a, "Sub/b.bin": b} {
		if got, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, "Shared", filepath.FromSlash(rel))); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %v", rel, err)
		}
	}
	if _, err := resolvePCloud(context.Background(), "https://u.pcloud.link/publink/show?code=GONE"); err == nil || err.Error() != "pCloud: This link is deleted by the owner" {
		t.Errorf("deleted: %v", err)
	}
}

// ---- MediaFire

func TestMediaFire(t *testing.T) {
	f := newSvcFake(t)
	swap(t, &mediafireWeb, f.URL)
	swap(t, &mfDirectRe, regexp.MustCompile(`^`+regexp.QuoteMeta(f.URL)+`/dl/`))
	data := randData(600 << 10)
	sum := sha256.Sum256(data)
	f.file("/dl/3gg3kq/ab12cd34ef/report.pdf", data)
	direct := f.URL + "/dl/3gg3kq/ab12cd34ef/report.pdf"
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		key := r.URL.Query().Get("quick_key")
		switch {
		case r.URL.Path == "/api/1.5/file/get_info.php" && (key == "ab12cd34ef" || key == "scr4mbled1" || key == "gone000001"):
			writeJSONTest(w, 200, map[string]any{"response": map[string]any{"action": "file/get_info", "result": "Success",
				"file_info": map[string]any{"quickkey": key, "filename": "report.pdf", "size": fmt.Sprint(len(data)), "hash": hex.EncodeToString(sum[:]), "password_protected": "no", "privacy": "public"}}})
		case r.URL.Path == "/api/1.5/file/get_info.php" && key == "locked0001":
			writeJSONTest(w, 200, map[string]any{"response": map[string]any{"result": "Success", "file_info": map[string]any{"filename": "x.zip", "password_protected": "yes"}}})
		case r.URL.Path == "/api/1.5/file/get_info.php":
			writeJSONTest(w, 200, map[string]any{"response": map[string]any{"action": "file/get_info", "message": "Unknown or Invalid QuickKey", "error": 110, "result": "Error"}})
		case r.URL.Path == "/file/ab12cd34ef":
			fmt.Fprintf(w, `<html><body><div class="download_link"><a class="input popsok" aria-label="Download file" href="%s" id="downloadButton" rel="nofollow">Download (600KB)</a></div></body></html>`, direct)
		case r.URL.Path == "/file/scr4mbled1":
			fmt.Fprintf(w, `<html><body><a class="input popsok" aria-label="Download file" href="javascript:void(0)" data-scrambled-url="%s" id="downloadButton">Download</a></body></html>`, base64.StdEncoding.EncodeToString([]byte(direct)))
		case r.URL.Path == "/file/gone000001":
			http.Redirect(w, r, "/error.php?errno=320&origin=download", 302)
		case r.URL.Path == "/error.php":
			fmt.Fprint(w, `<html><body><div class="errorView"><h3>Invalid or Deleted File.</h3></div></body></html>`)
		default:
			return false
		}
		return true
	}
	m := newTestManager(t)
	for _, link := range []string{"https://www.mediafire.com/file/ab12cd34ef/report.pdf/file", "https://www.mediafire.com/?scr4mbled1"} {
		d := m.Add(AddReq{URL: link, Start: true, Dir: filepath.Join(m.settings.DownloadDir, fmt.Sprint(len(link)))})
		waitFor(t, m, d, StCompleted, 30*time.Second)
		fileIs(t, d, data)
		m.mu.Lock()
		c := d.Check
		m.mu.Unlock()
		if d.FileName != "report.pdf" || c == nil || c.State != "ok" {
			t.Errorf("%s: name %q, check %+v", link, d.FileName, c)
		}
	}
	ctx := context.Background()
	for link, want := range map[string]string{
		"https://www.mediafire.com/file/gone000001/x": "MediaFire: Invalid or Deleted File (error 320)",
		"https://www.mediafire.com/file/missing001/x": "MediaFire: Unknown or Invalid QuickKey",
		"https://www.mediafire.com/file/locked0001/x": "password",
		"https://www.mediafire.com/folder/abcdef1234": "folder links",
	} {
		if _, err := resolveCloud(ctx, &Download{}, link); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", link, err, want)
		}
	}
}

// ---- WeTransfer

func TestWeTransfer(t *testing.T) {
	f := newSvcFake(t)
	swap(t, &wetransferWeb, f.URL)
	data := randData(250 << 10)
	f.file("/wtdl/holiday.zip", data)
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/":
			http.SetCookie(w, &http.Cookie{Name: "_wt_session", Value: "s1"})
			fmt.Fprint(w, `<html><head><meta name="csrf-token" content="tok123"></head></html>`)
		case r.URL.Path == "/api/v4/transfers/0123abcd/prepare-download":
			writeJSONTest(w, 200, map[string]any{"state": "downloadable", "files": []any{map[string]any{"name": "holiday.zip", "size": len(data)}}})
		case r.URL.Path == "/api/v4/transfers/0123abcd/download":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			c, _ := r.Cookie("_wt_session")
			if r.Header.Get("X-CSRF-Token") != "tok123" || c == nil || r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				writeJSONTest(w, 422, map[string]any{"message": "Invalid CSRF token"})
				return true
			}
			if body["security_hash"] != "4567ef" || body["intent"] != "entire_transfer" {
				writeJSONTest(w, 400, map[string]any{"message": "bad request"})
				return true
			}
			writeJSONTest(w, 200, map[string]any{"direct_link": f.URL + "/wtdl/holiday.zip?sig=x"})
		case r.URL.Path == "/api/v4/transfers/dead0000/prepare-download":
			writeJSONTest(w, 200, map[string]any{"state": "expired"})
		default:
			return false
		}
		return true
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: "https://wetransfer.com/downloads/0123abcd/4567ef", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
	if d.FileName != "holiday.zip" {
		t.Fatalf("name %q", d.FileName)
	}
	if _, err := resolveWeTransfer(context.Background(), "https://wetransfer.com/downloads/dead0000/99aa"); err == nil || !strings.Contains(err.Error(), "WeTransfer: this transfer has expired") {
		t.Errorf("expired: %v", err)
	}
}

// The Add dialog shows where a file comes from and its real name and size, for every host.
func TestCloudProbeShowsService(t *testing.T) {
	f := newSvcFake(t)
	swap(t, &yandexAPI, f.URL+"/y")
	f.file("/ydl/notes.txt", []byte("hello"))
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/download") {
			writeJSONTest(w, 200, map[string]any{"href": f.URL + "/ydl/notes.txt"})
			return true
		}
		if r.URL.Path == "/y" {
			writeJSONTest(w, 200, map[string]any{"type": "file", "name": "notes.txt", "size": 5})
			return true
		}
		return false
	}
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, srv, "probe", map[string]any{"url": "https://yadi.sk/d/AbCdEf123"})
	keys := []string{}
	for k := range info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if info["service"] != "Yandex Disk" || info["name"] != "notes.txt" || info["size"] != float64(5) {
		t.Fatalf("probe: %v (%v)", info, keys)
	}
}
