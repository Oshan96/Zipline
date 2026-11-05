package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---- Pixeldrain

func TestPixeldrain(t *testing.T) {
	f := newSvcFake(t)
	swap(t, &pixeldrainAPI, f.URL)
	song, a, b, c := randData(400<<10), randData(2000), []byte("bbb"), []byte("c")
	f.file("/api/file/Song1", song)
	f.file("/api/file/A1", a)
	f.file("/api/filesystem/Bkt/Music/b.txt", b)
	f.file("/api/filesystem/Bkt/Music/Sub/c.txt", c)
	dir := func(p string) map[string]any {
		return map[string]any{"type": "dir", "path": p, "name": filepath.Base(p)}
	}
	file := func(p string, data []byte) map[string]any {
		return map[string]any{"type": "file", "path": p, "name": filepath.Base(p), "file_size": len(data), "sha256_sum": sha256Hex(data)}
	}
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		stat := r.URL.Query().Has("stat")
		switch r.URL.Path {
		case "/api/file/Song1/info":
			writeJSONTest(w, 200, map[string]any{"id": "Song1", "name": "song.mp3", "size": len(song), "hash_sha256": sha256Hex(song)})
		case "/api/file/A1/info":
			writeJSONTest(w, 200, map[string]any{"id": "A1", "name": "a.bin", "size": len(a), "hash_sha256": sha256Hex(a)})
		case "/api/file/Busy1/info":
			writeJSONTest(w, 200, map[string]any{"id": "Busy1", "name": "big.iso", "size": 1, "availability": "file_rate_limited_captcha_required",
				"availability_message": "This file is using too much bandwidth. Please complete a captcha."})
		case "/api/file/Gone1/info":
			writeJSONTest(w, 404, map[string]any{"success": false, "value": "not_found", "message": "The entity you requested could not be found"})
		case "/api/list/List1":
			writeJSONTest(w, 200, map[string]any{"title": "Mix", "files": []any{
				map[string]any{"id": "Song1", "name": "song.mp3", "size": len(song)}, map[string]any{"id": "A1", "name": "a.bin", "size": len(a)}}})
		case "/api/filesystem/Bkt/Music":
			if !stat {
				return false
			}
			writeJSONTest(w, 200, map[string]any{"path": []any{dir("/Bkt"), dir("/Bkt/Music")}, "base_index": 1,
				"children": []any{file("/Bkt/Music/b.txt", b), dir("/Bkt/Music/Sub"), map[string]any{"type": "file", "path": "/Bkt/Music/.search_index.gz", "name": ".search_index.gz"}}})
		case "/api/filesystem/Bkt/Music/Sub":
			if !stat {
				return false
			}
			writeJSONTest(w, 200, map[string]any{"path": []any{dir("/Bkt"), dir("/Bkt/Music"), dir("/Bkt/Music/Sub")}, "base_index": 2,
				"children": []any{file("/Bkt/Music/Sub/c.txt", c)}})
		default:
			// a file of the filesystem: ?stat describes it, ?attach is the file itself
			data, ok := map[string][]byte{"/Bkt/Music/b.txt": b, "/Bkt/Music/Sub/c.txt": c}[strings.TrimPrefix(r.URL.Path, "/api/filesystem")]
			if !stat || !ok {
				return false
			}
			p := strings.TrimPrefix(r.URL.Path, "/api/filesystem")
			writeJSONTest(w, 200, map[string]any{"path": []any{dir("/Bkt"), dir(filepath.ToSlash(filepath.Dir(p))), file(p, data)}, "base_index": 2})
		}
		return true
	}
	m := newTestManager(t)
	d := m.Add(AddReq{URL: "https://pixeldrain.com/u/Song1", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, song)
	m.mu.Lock()
	ck := d.Check
	m.mu.Unlock()
	if d.FileName != "song.mp3" || ck == nil || ck.Algo != "sha256" || ck.State != "ok" {
		t.Fatalf("name %q, check %+v", d.FileName, ck)
	}
	one := m.Add(AddReq{URL: "https://pixeldrain.com/d/Bkt/Music/b.txt", Start: true}) // a file of the filesystem
	waitFor(t, m, one, StCompleted, 30*time.Second)
	fileIs(t, one, b)

	srv := (&Server{m: m, token: "t"}).routes()
	for link, want := range map[string]map[string][]byte{
		"https://pixeldrain.com/l/List1":      {"Mix/song.mp3": song, "Mix/a.bin": a},
		"https://pixeldrain.com/d/Bkt/Music/": {"Music/b.txt": b, "Music/Sub/c.txt": c},
	} {
		res := apiCall(t, srv, "add", map[string]any{"url": link, "meta": true, "metaFile": -1, "start": true})
		ids, _ := res["ids"].([]any)
		if len(ids) != len(want) {
			t.Fatalf("%s: added %v", link, res)
		}
		for _, id := range ids {
			waitFor(t, m, earlyDownload(m, id.(string)), StCompleted, 30*time.Second)
		}
		for rel, data := range want {
			if got, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, filepath.FromSlash(rel))); err != nil || !bytes.Equal(got, data) {
				t.Errorf("%s %s: %v", link, rel, err)
			}
		}
	}

	_, err := resolvePixeldrain(context.Background(), "https://pixeldrain.com/u/Busy1")
	if err == nil || !strings.Contains(err.Error(), "too much bandwidth") || !strings.Contains(err.Error(), "browser") {
		t.Errorf("a file Pixeldrain holds back: %v", err)
	}
	var perm permanentErr
	if _, err := resolvePixeldrain(context.Background(), "https://pixeldrain.com/u/Gone1"); !errors.As(err, &perm) || !strings.Contains(err.Error(), "isn't there") {
		t.Errorf("a deleted file: %v", err)
	}
	if _, err := resolvePixeldrain(context.Background(), "https://pixeldrain.com/l/List1"); !errors.Is(err, errCloudFolder) {
		t.Errorf("a whole list as one download: %v", err)
	}
}

// ---- Catbox

func TestCatboxAlbum(t *testing.T) {
	f := newSvcFake(t)
	swap(t, &catboxWeb, f.URL)
	swap(t, &catboxFiles, []string{f.URL + "/files"})
	x, y := randData(3000), []byte("png!")
	f.file("/files/ab1.jpg", x)
	f.file("/files/cd2.png", y)
	f.route = func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/c/Alb1":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><body><h1>Holiday &amp; more</h1><p>Created May 1 2024</p>
				<a href="` + f.URL + `/files/ab1.jpg"><img src="` + f.URL + `/files/ab1.jpg"></a>` + f.URL + `/files/ab1.jpg
				<a href="` + f.URL + `/files/cd2.png">` + f.URL + `/files/cd2.png</a></body></html>`))
		case "/c/Empty":
			w.Write([]byte(`<html><h1>Nothing</h1></html>`))
		default:
			return false
		}
		return true
	}
	cf, err := catboxFolder(context.Background(), "https://catbox.moe/c/Alb1")
	if err != nil || cf.Name != "Holiday & more" || len(cf.Files) != 2 || cf.Files[0].Path != "ab1.jpg" || cf.Files[1].URL != f.URL+"/files/cd2.png" {
		t.Fatalf("album: %+v %v", cf, err)
	}
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	res := apiCall(t, srv, "add", map[string]any{"url": "https://catbox.moe/c/Alb1", "meta": true, "metaFile": -1, "start": true})
	ids, _ := res["ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("added %v", res)
	}
	for _, id := range ids {
		waitFor(t, m, earlyDownload(m, id.(string)), StCompleted, 30*time.Second)
	}
	for rel, data := range map[string][]byte{"ab1.jpg": x, "cd2.png": y} {
		if got, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, "Holiday & more", rel)); err != nil || !bytes.Equal(got, data) {
			t.Errorf("%s: %v", rel, err)
		}
	}
	if _, err := catboxFolder(context.Background(), "https://catbox.moe/c/Empty"); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Errorf("an empty album: %v", err)
	}
	var perm permanentErr
	if _, err := catboxFolder(context.Background(), "https://catbox.moe/c/Missing"); !errors.As(err, &perm) {
		t.Errorf("no such album: %v", err)
	}
	if _, err := resolveCloud(context.Background(), nil, "https://catbox.moe/c/Alb1"); !errors.Is(err, errCloudFolder) {
		t.Errorf("an album as one download: %v", err)
	}
}
