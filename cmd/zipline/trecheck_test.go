package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"zipline/internal/bencode"
)

// webPack is a two-file torrent (a.bin 300 KiB, b.txt 100 KiB, 32 KiB pieces: pieces 0–9 hold
// a.bin, 9–12 b.txt) that only a web server has; served counts the bytes it sent, and slow (when
// set) holds back the end of b.txt (piece 12) for a few seconds.
type webPack struct {
	tor    []byte
	a, b   []byte
	served atomic.Int64
	slow   atomic.Bool
}

func newWebPack(t *testing.T) *webPack {
	w := &webPack{}
	var tor []byte
	tor, w.a, w.b = webSeedTorrent(t) // (for its contents; it serves them from its own server)
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "Pack"), 0o755)
	os.WriteFile(filepath.Join(dir, "Pack", "a.bin"), w.a, 0o644)
	os.WriteFile(filepath.Join(dir, "Pack", "b.txt"), w.b, 0o644)
	files := http.FileServer(http.Dir(dir))
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.slow.Load() && strings.HasSuffix(r.URL.Path, "b.txt") && strings.HasPrefix(r.Header.Get("Range"), "bytes=86016-") {
			time.Sleep(3 * time.Second)
		}
		files.ServeHTTP(countingWriter{rw, &w.served}, r)
	}))
	t.Cleanup(srv.Close)
	v, _ := bencode.Decode(tor)
	top := v.(map[string]any)
	top["url-list"] = srv.URL + "/"
	w.tor = bencode.Encode(top)
	return w
}

type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n.Add(int64(n))
	return n, err
}

func (w *webPack) dir(m *Manager) string { return filepath.Join(m.settings.DownloadDir, "got", "Pack") }

func (w *webPack) check(t *testing.T, m *Manager) {
	t.Helper()
	if got, _ := os.ReadFile(filepath.Join(w.dir(m), "a.bin")); !bytes.Equal(got, w.a) {
		t.Fatal("a.bin differs")
	}
	if got, _ := os.ReadFile(filepath.Join(w.dir(m), "b.txt")); !bytes.Equal(got, w.b) {
		t.Fatal("b.txt differs")
	}
}

// damage writes junk into a file (the size stays; the modified time moves on).
func damage(t *testing.T, path string, at int64) {
	t.Helper()
	time.Sleep(20 * time.Millisecond) // (a modified time that differs, even on coarse clocks)
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("this is not what the torrent holds"), at)
	f.Close()
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(path, later, later)
}

// G5: a torrent added over files already in its folder checks them and keeps the good pieces.
func TestTorrentAddedOverExistingFiles(t *testing.T) {
	w := newWebPack(t)
	m := torrentManager(t)
	os.MkdirAll(w.dir(m), 0o755)
	os.WriteFile(filepath.Join(w.dir(m), "a.bin"), w.a, 0o644)
	os.WriteFile(filepath.Join(w.dir(m), "b.txt"), append(append([]byte{}, w.b[:50<<10]...), make([]byte, 50<<10+77)...), 0o644) // half there, and too long
	d, _ := addTorrentBytes(t, m, w.tor, nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	w.check(t, m)
	// a.bin and the start of b.txt were good: only pieces 10–12 (where b.txt goes wrong, and on) are fetched
	if got := w.served.Load(); got > 80<<10 {
		t.Fatalf("the web server sent %d KiB; the files already there should have been kept", got>>10)
	}
	if st, _ := os.Stat(filepath.Join(w.dir(m), "b.txt")); st.Size() != int64(len(w.b)) {
		t.Fatalf("b.txt is %d bytes, want %d", st.Size(), len(w.b))
	}
}

// G5: progress is saved with each file's size and modified time; a start checks again the
// pieces of files that changed since (a crash, another program), and only those.
func TestTorrentChecksChangedFilesOnStart(t *testing.T) {
	w := newWebPack(t)
	m := torrentManager(t)
	d, _ := addTorrentBytes(t, m, w.tor, nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	m.StopSharing(d.ID)
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	stamps := append([]fileStamp(nil), d.Torrent.Stamps...)
	m.mu.Unlock()
	if len(stamps) != 2 || stamps[0].Size != int64(len(w.a)) || stamps[1].Size != int64(len(w.b)) || stamps[0].Mod <= 0 {
		t.Fatalf("stamps saved: %+v", stamps)
	}
	damage(t, filepath.Join(w.dir(m), "a.bin"), 3*32<<10+100) // piece 3
	served := w.served.Load()
	m.mu.Lock()
	d.Status = StPaused // (as after a crash: the saved progress says it's all there)
	m.startByHand(d)
	m.mu.Unlock()
	waitFor(t, m, d, StCompleted, 60*time.Second)
	w.check(t, m)
	if got := w.served.Load() - served; got != 32<<10 {
		t.Fatalf("fetched %d bytes again, want just piece 3 (%d)", got, 32<<10)
	}
	tr := runningTorrent(t, m, d.ID)
	tr.mu.Lock()
	checked := tr.checkTotal
	tr.mu.Unlock()
	if checked != 10 { // pieces 0–9: a.bin's (b.txt didn't change)
		t.Fatalf("checked %d pieces, want a.bin's 10", checked)
	}
}

// G5: "Check files" on a finished torrent reads everything back and fetches what's damaged;
// Redownload does the same for a torrent instead of starting over.
func TestTorrentCheckFilesAndRedownload(t *testing.T) {
	w := newWebPack(t)
	m := torrentManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	d, _ := addTorrentBytes(t, m, w.tor, nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	// damaged without a trace: same size, same modified time
	path := filepath.Join(w.dir(m), "b.txt")
	st, _ := os.Stat(path)
	damage(t, path, 10)
	os.Chtimes(path, st.ModTime(), st.ModTime())
	served := w.served.Load()
	if r := apiCall(t, h, "action", map[string]any{"action": "recheck", "ids": []string{d.ID}}); r["ok"] != true {
		t.Fatalf("recheck: %v", r)
	}
	time.Sleep(200 * time.Millisecond)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	w.check(t, m)
	if got := w.served.Load() - served; got != 32<<10 { // piece 9 (the end of a.bin, the start of b.txt)
		t.Fatalf("fetched %d bytes again, want one piece", got)
	}
	m.mu.Lock()
	again := d.Torrent.Recheck
	m.mu.Unlock()
	if again {
		t.Fatal("the check is done: it shouldn't run again next time")
	}
	// Redownload: b.txt is gone, a.bin stays
	os.Remove(path)
	served = w.served.Load()
	m.Redownload([]string{d.ID})
	time.Sleep(200 * time.Millisecond)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	w.check(t, m)
	if got := w.served.Load() - served; got > 4*32<<10 { // pieces 9–12
		t.Fatalf("Redownload fetched %d KiB; a.bin should have been kept", got>>10)
	}
}

// G5: with "Check the files again when finished", a piece damaged on disk after it was written
// is found at the end and fetched again.
func TestTorrentCheckWhenFinished(t *testing.T) {
	w := newWebPack(t)
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.CheckDone = true
	m.mu.Unlock()
	w.slow.Store(true) // the last piece comes last
	d, _ := addTorrentBytes(t, m, w.tor, nil)
	tr := runningTorrent(t, m, d.ID)
	for i := 0; i < 9; i++ {
		waitHave(t, tr, i)
	}
	tr.mu.Lock()
	left := !tr.have.has(12)
	tr.mu.Unlock()
	if !left {
		t.Fatal("the last piece should still be on its way")
	}
	damage(t, filepath.Join(w.dir(m), "a.bin"), 100) // piece 0, after it was checked and written
	served := w.served.Load()
	waitFor(t, m, d, StCompleted, 60*time.Second)
	w.check(t, m)
	if w.served.Load()-served < 32<<10 {
		t.Fatal("the damaged piece wasn't fetched again")
	}
}

// G5: a magnet link whose files are already in the folder checks them once the file list
// arrives, and downloads only what's missing.
func TestMagnetOverExistingFiles(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seed(t, py)
	m := torrentManager(t)
	root := filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack")
	for p, b := range sw.files {
		if !strings.Contains(p, "two") { // all but one file are there already
			os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
			os.WriteFile(filepath.Join(root, filepath.FromSlash(p)), b, 0o644)
		}
	}
	ih, _ := hex.DecodeString(sw.hash)
	d, err := m.AddTorrent(TorrentAdd{Magnet: magnetLink([20]byte(ih), "", []string{sw.tracker.httpURL()}), Dir: filepath.Join(m.settings.DownloadDir, "got"), Start: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, d, StCompleted, 90*time.Second)
	checkFiles(t, root, sw.files)
	tr := runningTorrent(t, m, d.ID)
	got := atomic.LoadInt64(&tr.downloaded)
	two := int64(len(sw.files["video/clip two.mp4"]))
	if got > two+2*65536 { // clip two and the pieces it shares with its neighbours
		t.Fatalf("downloaded %d bytes; only clip two (%d) was missing", got, two)
	}
}

// File priorities and the picking options are saved, shown in the file list, and survive a
// restart; old saves without them still load.
func TestTorrentPickOptionsSaved(t *testing.T) {
	w := newWebPack(t)
	m := torrentManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	mi, err := parseTorrent(w.tor)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.rememberTorrent(mi, w.tor)
	m.mu.Unlock()
	add := apiCall(t, h, "torrent/add", map[string]any{"magnet": torrentRef(mi.InfoHash), "skip": []bool{false, false}, "prio": []int{1, -1},
		"seq": true, "dir": filepath.Join(m.settings.DownloadDir, "got"), "start": false})
	id, _ := add["id"].(string)
	if id == "" {
		t.Fatalf("add: %v", add)
	}
	det := apiCall(t, h, "torrent/detail", map[string]any{"id": id})
	fs, _ := det["files"].([]any)
	if det["seq"] != true || det["ends"] != nil || len(fs) != 2 || fs[0].(map[string]any)["prio"] != float64(1) || fs[1].(map[string]any)["prio"] != float64(-1) {
		t.Fatalf("detail: seq %v ends %v files %v", det["seq"], det["ends"], fs)
	}
	if r := apiCall(t, h, "torrent/files", map[string]any{"id": id, "ends": true, "seq": false}); r["ok"] != true {
		t.Fatalf("options: %v", r)
	}
	if r := apiCall(t, h, "torrent/files", map[string]any{"id": id, "prio": []int{0, 0}}); r["ok"] != true {
		t.Fatalf("priorities: %v", r)
	}
	m.mu.Lock()
	ts := *m.byID[id].Torrent
	m.mu.Unlock()
	if ts.Seq || !ts.Ends || ts.Prio != nil {
		t.Fatalf("saved: seq %v ends %v prio %v", ts.Seq, ts.Ends, ts.Prio)
	}
	// running: applied to the engine at once
	m.mu.Lock()
	m.startByHand(m.byID[id])
	m.mu.Unlock()
	tr := runningTorrent(t, m, id)
	apiCall(t, h, "torrent/files", map[string]any{"id": id, "prio": []int{-1, 1}})
	tr.mu.Lock()
	lowA, highB := tr.prio[0] == prioEnds && tr.prio[5] == prioLow, tr.prio[11] == prioHigh
	tr.mu.Unlock()
	if !lowA || !highB {
		t.Fatal("the running torrent didn't pick up the new priorities")
	}
	var old TorrentState // a save from before: only Skip
	if err := json.Unmarshal([]byte(`{"infoHash":"`+mi.HashHex()+`","skip":[false,true],"have":"gA=="}`), &old); err != nil || len(old.Skip) != 2 || old.Prio != nil || old.Stamps != nil {
		t.Fatalf("old save: %+v %v", old, err)
	}
}
