package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"zipline/internal/bencode"
)

// webSeedTorrent makes a two-file torrent that only a web server has (BEP 19), so the window's
// APIs can be tested without other BitTorrent programs.
func webSeedTorrent(t *testing.T) (tor []byte, a, b []byte) {
	dir := t.TempDir()
	web := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(web.Close)
	a, b = randData(300<<10), randData(100<<10)
	os.MkdirAll(filepath.Join(dir, "Pack"), 0o755)
	os.WriteFile(filepath.Join(dir, "Pack", "a.bin"), a, 0o644)
	os.WriteFile(filepath.Join(dir, "Pack", "b.txt"), b, 0o644)
	all := append(append([]byte{}, a...), b...)
	const pl = 32 << 10
	var hashes []byte
	for off := 0; off < len(all); off += pl {
		h := sha1.Sum(all[off:min(off+pl, len(all))])
		hashes = append(hashes, h[:]...)
	}
	info := map[string]any{"name": "Pack", "piece length": int64(pl), "pieces": string(hashes), "files": []any{
		map[string]any{"length": int64(len(a)), "path": []any{"a.bin"}},
		map[string]any{"length": int64(len(b)), "path": []any{"b.txt"}},
	}}
	return bencode.Encode(map[string]any{"info": info, "url-list": web.URL + "/"}), a, b
}

func TestTorrentWindowAPIs(t *testing.T) {
	tor, a, b := webSeedTorrent(t)
	m := torrentManager(t)
	h := (&Server{m: m, token: "t"}).routes()

	// open the .torrent file the way the Add dialog does, then look at it
	open := apiCall(t, h, "torrent/open", map[string]any{"data": base64.StdEncoding.EncodeToString(tor)})
	ref, _ := open["url"].(string)
	if !strings.HasPrefix(ref, "torrent:") || open["fileCount"] != float64(2) {
		t.Fatalf("open: %v", open)
	}
	info := apiCall(t, h, "probe", map[string]any{"url": ref})
	if info["kind"] != "torrent" || len(info["files"].([]any)) != 2 {
		t.Fatalf("probe: %v", info)
	}
	dir := filepath.Join(m.settings.DownloadDir, "got")
	add := apiCall(t, h, "torrent/add", map[string]any{"magnet": ref, "skip": []bool{false, true}, "dir": dir, "start": true})
	id, _ := add["id"].(string)
	if id == "" {
		t.Fatalf("add: %v", add)
	}
	m.mu.Lock()
	d := m.byID[id]
	m.mu.Unlock()
	waitFor(t, m, d, StCompleted, 60*time.Second)
	if got, _ := os.ReadFile(filepath.Join(dir, "Pack", "a.bin")); !bytes.Equal(got, a) {
		t.Fatal("a.bin differs")
	}
	if _, err := os.Stat(filepath.Join(dir, "Pack", "b.txt")); err == nil {
		t.Fatal("b.txt was skipped but got written")
	}

	// the progress window's details: files, magnet link, piece map
	det := apiCall(t, h, "torrent/detail", map[string]any{"id": id})
	fs, _ := det["files"].([]any)
	if len(fs) != 2 || fs[0].(map[string]any)["done"] != float64(1) || fs[1].(map[string]any)["skip"] != true {
		t.Fatalf("detail files: %v", det["files"])
	}
	if mg, _ := det["magnet"].(string); !strings.HasPrefix(mg, "magnet:?xt=urn:btih:") || !strings.Contains(mg, "dn=Pack") {
		t.Fatalf("magnet: %v", det["magnet"])
	}
	pm, _ := base64.StdEncoding.DecodeString(det["map"].(string))
	if len(pm) != 13 || pm[0] != 100 || pm[12] != 255 { // pieces 10–12 are only b.txt
		t.Fatalf("piece map: %v", pm)
	}
	if r := apiCall(t, h, "torrent/detail", map[string]any{"id": "nope"}); r["error"] == nil {
		t.Fatal("detail of an unknown download")
	}

	// Properties can't rename a torrent or change its address
	m.mu.Lock()
	url0 := d.URL
	m.mu.Unlock()
	apiCall(t, h, "props", map[string]any{"id": id, "name": "evil.txt", "url": "http://example.com/x", "desc": "holiday"})
	m.mu.Lock()
	name, url, desc := d.FileName, d.URL, d.Desc
	m.mu.Unlock()
	if name != "Pack" || url != url0 || desc != "holiday" {
		t.Fatalf("props: %q %q %q", name, url, desc)
	}

	// choosing the other file too downloads just that one
	if r := apiCall(t, h, "torrent/files", map[string]any{"id": id, "skip": []bool{false, false}}); r["ok"] != true {
		t.Fatalf("files: %v", r)
	}
	time.Sleep(200 * time.Millisecond)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	if got, _ := os.ReadFile(filepath.Join(dir, "Pack", "b.txt")); !bytes.Equal(got, b) {
		t.Fatal("b.txt differs")
	}

	// the list shows the magnet link (for Copy magnet link)
	var tv *TorrentView
	for _, v := range m.Snapshot(nil).Downloads {
		if v.ID == id {
			tv = v.Torrent
		}
	}
	if tv == nil || !strings.HasPrefix(tv.Magnet, "magnet:") || tv.PeerList != nil {
		t.Fatalf("list view: %+v", tv)
	}

	// the phone page says it's sharing, as the window does (once the engine's next second has gone by)
	var text string
	for i := 0; i < 30 && text != "Complete · sharing"; i++ {
		time.Sleep(100 * time.Millisecond)
		for _, pd := range (&phoneServer{m: m}).state()["downloads"].([]phoneDL) {
			if pd.ID == id {
				text = pd.Text
			}
		}
	}
	if text != "Complete · sharing" {
		t.Fatalf("phone status: %q", text)
	}

	// Stop sharing; the piece map then comes from the saved state
	apiCall(t, h, "action", map[string]any{"action": "stopSharing", "ids": []string{id}})
	for i := 0; i < 100; i++ {
		m.mu.Lock()
		running := m.torrents[id] != nil
		m.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	det = apiCall(t, h, "torrent/detail", map[string]any{"id": id})
	pm, _ = base64.StdEncoding.DecodeString(det["map"].(string))
	if det["seeding"] == true || len(pm) != 13 || bytes.Count(pm, []byte{100}) != 13 {
		t.Fatalf("after sharing: seeding=%v map=%v", det["seeding"], pm)
	}
}

func TestTorrentSettingsAndPort(t *testing.T) {
	m := torrentManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	m.ensureTorrentListener()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/settings", nil)
	req.Header.Set("X-Token", "t")
	h.ServeHTTP(rec, req)
	var st map[string]any
	json.Unmarshal(rec.Body.Bytes(), &st)
	setTorrent := func(tv map[string]any) map[string]any {
		st["torrent"] = tv
		b, _ := json.Marshal(st)
		var body map[string]any
		json.Unmarshal(b, &body)
		return apiCall(t, h, "settings", body)["torrent"].(map[string]any)
	}
	got := setTorrent(map[string]any{"port": 80, "maxPeers": 5000, "upKB": -3, "seedRatio": -1, "seedMins": 30})
	if got["maxPeers"] != float64(100) || got["upKB"] != float64(0) || got["seedRatio"] != float64(0) {
		t.Fatalf("not tidied: %v", got)
	}
	if p := got["port"].(float64); p == 80 {
		t.Fatal("port 80 kept")
	}
	// a new port takes effect straight away
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	free := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	setTorrent(map[string]any{"port": free, "maxPeers": 60, "seedRatio": 1, "seedMins": 120})
	m.mu.Lock()
	now := m.tport
	m.mu.Unlock()
	if now != free {
		t.Fatalf("listening on %d, want %d", now, free)
	}
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(free), time.Second)
	if err != nil {
		t.Fatal("new port isn't listening:", err)
	}
	c.Close()
}

// The extension hands magnet links over; the app opens the Add dialog for them.
func TestExtensionMagnetLinks(t *testing.T) {
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	send := func(path string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/ext/"+path, bytes.NewReader(b))
		req.Header.Set("Origin", "chrome-extension://abc")
		req.Header.Set("X-Token", "t")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	mg := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Holiday"
	if rec := send("add", map[string]any{"url": mg, "referer": "https://example.com/"}); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("add magnet: %d %s", rec.Code, rec.Body.String())
	}
	m.mu.Lock()
	n := len(m.prompts)
	got := ""
	if n > 0 {
		got = m.prompts[0].URL
	}
	m.mu.Unlock()
	if n != 1 || got != mg {
		t.Fatalf("prompts: %d %q", n, got)
	}
	if rec := send("add", map[string]any{"url": "magnet:?xt=urn:sha1:nope"}); !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("bad magnet accepted: %s", rec.Body.String())
	}
	// the ping tells the extension whether to take magnet links and .torrent files
	m.mu.Lock()
	m.settings.Torrent.NoBrowser = true
	m.mu.Unlock()
	req := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/ext/ping", nil)
	req.Header.Set("Origin", "chrome-extension://abc")
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"torrents":false`) {
		t.Fatalf("ping: %s", rec.Body.String())
	}
}

// A file manager may hand over a .torrent file as a file:// address.
func TestCLITorrentFileURL(t *testing.T) {
	link, want := "file:///home/me/Downloads/Holiday%20Pack.torrent", "/home/me/Downloads/Holiday Pack.torrent"
	if runtime.GOOS == "windows" {
		link, want = "file:///C:/Users/me/Downloads/Holiday%20Pack.torrent", `C:\Users\me\Downloads\Holiday Pack.torrent`
	}
	r, err := parseCLI([]string{link}, runtime.GOOS == "windows")
	if err != nil || len(r.URLs) != 1 || r.URLs[0] != "torrentfile:"+want {
		t.Fatalf("%v %v", r, err)
	}
}
