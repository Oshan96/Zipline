package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// betweenDrives makes every rename fail the way one between two drives does, so a move has to copy.
func betweenDrives(t *testing.T) *atomic.Int32 {
	old := moveRename
	var n atomic.Int32
	moveRename = func(a, b string) error {
		n.Add(1)
		return &os.LinkError{Op: "rename", Old: a, New: b, Err: errCrossDevice}
	}
	t.Cleanup(func() { moveRename = old })
	return &n
}

func mvWrite(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mvGone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); err == nil {
		t.Errorf("%s is still there", p)
	}
}

func mvDownload(m *Manager, id, dir, name, status string) *Download {
	d := &Download{ID: id, URL: "http://example.invalid/" + name, FileName: name, Dir: dir, Status: status, Size: -1, Added: time.Now()}
	m.mu.Lock()
	m.list = append(m.list, d)
	m.byID[id] = d
	m.mu.Unlock()
	return d
}

// ---------------------------------------------------------------- the files

func TestMoveOneFileAndNeverOverwrite(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a", "f.bin"), filepath.Join(dir, "b", "f.bin")
	mvWrite(t, a, []byte("first"))
	if moved, err := moveOne(moveItem{src: a, dst: b}, nil); err != nil || !moved {
		t.Fatalf("%v %v", moved, err)
	}
	mvGone(t, a)
	if string(mustRead(t, b)) != "first" {
		t.Fatal("content")
	}
	// something is there already: refused, both left as they were
	mvWrite(t, a, []byte("second"))
	if _, err := moveOne(moveItem{src: a, dst: b}, nil); err == nil || !strings.Contains(err.Error(), "already there") {
		t.Fatalf("%v", err)
	}
	if string(mustRead(t, a)) != "second" || string(mustRead(t, b)) != "first" {
		t.Fatal("a file was overwritten")
	}
	// a part file that was never written is fine; a finished file that's missing is not
	if moved, err := moveOne(moveItem{src: filepath.Join(dir, "none"), dst: filepath.Join(dir, "x"), optional: true}, nil); err != nil || moved {
		t.Fatalf("%v %v", moved, err)
	}
	if _, err := moveOne(moveItem{src: filepath.Join(dir, "none"), dst: filepath.Join(dir, "x")}, nil); err == nil || !strings.Contains(err.Error(), "isn't there") {
		t.Fatalf("%v", err)
	}
}

func TestMoveBetweenDrivesCopiesThenDeletes(t *testing.T) {
	n := betweenDrives(t)
	dir := t.TempDir()
	data := randData(3<<20 + 123)
	src, dst := filepath.Join(dir, "a", "big.bin"), filepath.Join(dir, "b", "big.bin")
	mvWrite(t, src, data)
	when := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	os.Chtimes(src, when, when)
	marked := 0
	st := &moveState{mark: func(string) { marked++ }}
	if err := moveItems([]moveItem{{src: src, dst: dst}}, st); err != nil {
		t.Fatal(err)
	}
	if n.Load() == 0 {
		t.Fatal("the rename was never tried")
	}
	if !bytes.Equal(mustRead(t, dst), data) {
		t.Fatal("the copy differs")
	}
	mvGone(t, src)
	mvGone(t, dst+moveTmp)
	if fi, _ := os.Stat(dst); !fi.ModTime().Equal(when) {
		t.Errorf("modified %v, want %v", fi.ModTime(), when)
	}
	if st.total != int64(len(data)) || st.done.Load() != int64(len(data)) || marked != 1 {
		t.Errorf("progress %d of %d, marked %d", st.done.Load(), st.total, marked)
	}

	// a folder: copied whole, renamed into place, the original deleted
	tree := filepath.Join(dir, "a", "Pack")
	mvWrite(t, filepath.Join(tree, "x", "one.bin"), []byte("1"))
	mvWrite(t, filepath.Join(tree, "two.txt"), []byte("22"))
	os.MkdirAll(filepath.Join(tree, "empty"), 0o755)
	if err := moveItems([]moveItem{{src: tree, dst: filepath.Join(dir, "b", "Pack"), tree: true}}, &moveState{}); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, filepath.Join(dir, "b", "Pack", "x", "one.bin"))) != "1" || string(mustRead(t, filepath.Join(dir, "b", "Pack", "two.txt"))) != "22" {
		t.Fatal("tree content")
	}
	if fi, err := os.Stat(filepath.Join(dir, "b", "Pack", "empty")); err != nil || !fi.IsDir() {
		t.Fatal("an empty folder was lost")
	}
	mvGone(t, tree)
	mvGone(t, filepath.Join(dir, "b", "Pack")+moveTmp)

	// a name that appeared meanwhile is never replaced
	mvWrite(t, src, data)
	mvWrite(t, dst, []byte("mine"))
	if _, err := moveOne(moveItem{src: src, dst: dst}, nil); err == nil {
		t.Fatal("overwrote")
	}
	if string(mustRead(t, dst)) != "mine" || !bytes.Equal(mustRead(t, src), data) {
		t.Fatal("files changed")
	}
	mvGone(t, dst+moveTmp)
}

// When one of several files can't move, the ones that did go back.
func TestMoveItemsAllOrNothing(t *testing.T) {
	for _, drives := range []bool{false, true} {
		if drives {
			betweenDrives(t)
		}
		dir := t.TempDir()
		mvWrite(t, filepath.Join(dir, "a", "1"), []byte("one"))
		mvWrite(t, filepath.Join(dir, "a", "2"), []byte("two"))
		mvWrite(t, filepath.Join(dir, "b", "2"), []byte("in the way"))
		err := moveItems([]moveItem{
			{src: filepath.Join(dir, "a", "1"), dst: filepath.Join(dir, "b", "1")},
			{src: filepath.Join(dir, "a", "2"), dst: filepath.Join(dir, "b", "2")},
		}, &moveState{})
		if err == nil {
			t.Fatal("the second should fail")
		}
		if string(mustRead(t, filepath.Join(dir, "a", "1"))) != "one" || string(mustRead(t, filepath.Join(dir, "a", "2"))) != "two" || string(mustRead(t, filepath.Join(dir, "b", "2"))) != "in the way" {
			t.Fatalf("drives=%v: files are not where they were", drives)
		}
		mvGone(t, filepath.Join(dir, "b", "1"))
	}
}

func TestMoveViewShowsHowFar(t *testing.T) {
	if moveView("nobody") != nil {
		t.Fatal("not moving")
	}
	st := &moveState{dir: "/there", total: 200}
	st.done.Store(50)
	moves.Store("x", st)
	defer moves.Delete("x")
	if v := moveView("x"); v == nil || v.Pct != 25 || v.Dir != "/there" {
		t.Fatalf("%+v", v)
	}
	st.done.Store(999)
	if v := moveView("x"); v.Pct != 100 {
		t.Fatalf("%+v", v)
	}
}

// ---------------------------------------------------------------- downloads

func TestMoveCompletedDownload(t *testing.T) {
	m := newTestManager(t)
	a, b := filepath.Join(m.settings.DownloadDir, "a"), filepath.Join(m.settings.DownloadDir, "b")
	mvWrite(t, filepath.Join(a, "f.bin"), []byte("mine"))
	mvWrite(t, filepath.Join(b, "f.bin"), []byte("someone else's"))
	d := mvDownload(m, "c1", a, "f.bin", StCompleted)
	if err := m.MoveDownload(d.ID, b); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	dir, name := d.Dir, d.FileName
	m.mu.Unlock()
	if dir != b || name != "f (1).bin" {
		t.Fatalf("now %s/%s", dir, name)
	}
	if string(mustRead(t, filepath.Join(b, "f (1).bin"))) != "mine" || string(mustRead(t, filepath.Join(b, "f.bin"))) != "someone else's" {
		t.Fatal("content")
	}
	mvGone(t, filepath.Join(a, "f.bin"))
	// already there; not a folder; not found
	if err := m.MoveDownload(d.ID, b); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("%v", err)
	}
	if err := m.MoveDownload(d.ID, "relative/dir"); err == nil {
		t.Fatal("a relative folder")
	}
	mvWrite(t, filepath.Join(a, "file"), nil)
	if err := m.MoveDownload(d.ID, filepath.Join(a, "file")); err == nil {
		t.Fatal("a file is not a folder")
	}
	if err := m.MoveDownload("nope", a); err == nil {
		t.Fatal("unknown")
	}
	// the file is gone: it says so, and stays where it is
	os.Remove(filepath.Join(b, "f (1).bin"))
	if err := m.MoveDownload(d.ID, a); err == nil || !strings.Contains(err.Error(), "isn't there") {
		t.Fatalf("%v", err)
	}
	m.mu.Lock()
	dir = d.Dir
	m.mu.Unlock()
	if dir != b {
		t.Fatalf("moved to %s anyway", dir)
	}
}

// While it moves, the list says so, and nothing can start it or rename it under the move.
func TestMoveIsVisibleAndGuarded(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	a, b := filepath.Join(m.settings.DownloadDir, "a"), filepath.Join(m.settings.DownloadDir, "b")
	mvWrite(t, filepath.Join(a, "f.bin"), randData(100<<10))
	d := mvDownload(m, "c1", a, "f.bin", StCompleted)
	started, release := make(chan struct{}), make(chan struct{})
	old := moveRename
	moveRename = func(x, y string) error {
		close(started)
		<-release
		return os.Rename(x, y)
	}
	t.Cleanup(func() { moveRename = old })
	done := make(chan error, 1)
	go func() { done <- m.MoveDownload(d.ID, b) }()
	<-started
	var mv *MoveView
	for _, v := range m.Snapshot(nil).Downloads {
		if v.ID == d.ID {
			mv = v.Move
		}
	}
	if mv == nil || mv.Dir != b {
		t.Fatalf("the list doesn't show the move: %+v", mv)
	}
	if r := apiCall(t, h, "props", map[string]any{"id": d.ID, "name": "other.bin"}); r["error"] == nil {
		t.Errorf("renamed during the move: %v", r)
	}
	if r := apiCall(t, h, "move", map[string]any{"ids": []string{d.ID}, "dir": filepath.Join(a, "c")}); r["error"] == nil || r["moved"] != float64(0) {
		t.Errorf("moved twice at once: %v", r)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if moveView(d.ID) != nil {
		t.Error("still shown as moving")
	}
	for _, v := range m.Snapshot(nil).Downloads {
		if v.ID == d.ID && (v.Move != nil || v.Dir != b) {
			t.Errorf("after: %+v %s", v.Move, v.Dir)
		}
	}
}

// A download that's going: paused, its part files go along, and it carries on in the new folder.
func TestMoveDownloadInProgress(t *testing.T) {
	data := randData(1200 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(&slowWriter{w: w, rate: 400 << 10, start: time.Now()}, r, "f.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	m := newTestManager(t)
	m.settings.Conns = 2
	a, b := filepath.Join(m.settings.DownloadDir, "a"), filepath.Join(m.settings.DownloadDir, "b")
	d := m.Add(AddReq{URL: srv.URL + "/f.bin", Dir: a, Start: true})
	waitCond(t, 20*time.Second, "some of it is here", func() bool { return atomic.LoadInt64(&d.done) > 150<<10 })
	if err := m.MoveDownload(d.ID, b); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	dir := d.Dir
	m.mu.Unlock()
	if dir != b {
		t.Fatalf("now in %s", dir)
	}
	mvGone(t, filepath.Join(a, "f.bin"+partSuffix))
	waitFor(t, m, d, StCompleted, 30*time.Second)
	if !bytes.Equal(mustRead(t, filepath.Join(b, "f.bin")), data) {
		t.Fatal("the file differs")
	}
	mvGone(t, filepath.Join(a, "f.bin"))
	mvGone(t, filepath.Join(b, "f.bin"+partSuffix))
}

// Paused downloads (and a stream's other parts) move too, and need not have written anything yet.
func TestMovePausedDownloadWithPartFiles(t *testing.T) {
	m := newTestManager(t)
	a, b := filepath.Join(m.settings.DownloadDir, "a"), filepath.Join(m.settings.DownloadDir, "b")
	d := mvDownload(m, "p1", a, "v.mp4", StPaused)
	d.Extra = []*TrackPart{{Lang: "de"}, {Lang: "fr"}}
	mvWrite(t, d.partPath(), []byte("video"))
	mvWrite(t, d.audioPartPath(), []byte("audio"))
	mvWrite(t, d.extraPartPath(0), []byte("de"))
	mvWrite(t, d.extraPartPath(1), []byte("fr"))
	if err := m.MoveDownload(d.ID, b); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	dir := d.Dir
	m.mu.Unlock()
	for p, want := range map[string]string{d.partPath(): "video", d.audioPartPath(): "audio", d.extraPartPath(0): "de", d.extraPartPath(1): "fr"} {
		if !strings.HasPrefix(p, b) || string(mustRead(t, p)) != want {
			t.Errorf("%s", p)
		}
	}
	if dir != b {
		t.Fatalf("dir %s", dir)
	}
	entries, _ := os.ReadDir(a)
	if len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
	// nothing written at all
	e := mvDownload(m, "p2", a, "e.bin", StPaused)
	if err := m.MoveDownload(e.ID, b); err != nil {
		t.Fatal(err)
	}
	if e.Dir != b {
		t.Fatal("not moved")
	}
}

func TestMoveStreamKeepsItsSubtitlesAndParts(t *testing.T) {
	m := newTestManager(t)
	a, b := filepath.Join(m.settings.DownloadDir, "a"), filepath.Join(m.settings.DownloadDir, "b")
	for _, n := range []string{"Movie.mp4", "Movie.en.srt", "Movie.de.forced.srt", "Movie (part 2).mkv", "Movie (audio Deutsch).m4a", "Movie.notes.txt", "Movie 2.mp4", "Other.en.srt"} {
		mvWrite(t, filepath.Join(a, n), []byte(n))
	}
	mvWrite(t, filepath.Join(b, "Movie.mp4"), []byte("taken"))
	d := mvDownload(m, "h1", a, "Movie.mp4", StCompleted)
	d.Kind = "hls"
	mvDownload(m, "h2", a, "Movie 2.mp4", StCompleted) // another download's own file stays
	if err := m.MoveDownload(d.ID, b); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Movie (1).mp4", "Movie (1).en.srt", "Movie (1).de.forced.srt", "Movie (1) (part 2).mkv", "Movie (1) (audio Deutsch).m4a", "Movie.mp4"} {
		if _, err := os.Stat(filepath.Join(b, n)); err != nil {
			t.Errorf("%s didn't arrive", n)
		}
	}
	if string(mustRead(t, filepath.Join(b, "Movie.mp4"))) != "taken" || string(mustRead(t, filepath.Join(b, "Movie (1).en.srt"))) != "Movie.en.srt" {
		t.Error("contents")
	}
	left, _ := os.ReadDir(a)
	var names []string
	for _, e := range left {
		names = append(names, e.Name())
	}
	if strings.Join(names, "|") != "Movie 2.mp4|Movie.notes.txt|Other.en.srt" {
		t.Errorf("left behind: %v", names)
	}
}

func TestMoveAPI(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	a, b := filepath.Join(m.settings.DownloadDir, "a"), filepath.Join(m.settings.DownloadDir, "b")
	mvWrite(t, filepath.Join(a, "1.bin"), []byte("1"))
	mvWrite(t, filepath.Join(a, "2.bin"), []byte("2"))
	one, two := mvDownload(m, "m1", a, "1.bin", StCompleted), mvDownload(m, "m2", a, "2.bin", StCompleted)
	if r := apiCall(t, h, "move", map[string]any{"ids": []string{one.ID, "nope"}, "dir": "somewhere"}); r["moved"] != float64(0) || r["error"] == nil {
		t.Fatalf("relative: %v", r)
	}
	r := apiCall(t, h, "move", map[string]any{"ids": []string{one.ID, two.ID, "nope"}, "dir": b})
	if r["moved"] != float64(2) || !strings.Contains(mvStr(r["error"]), "not found") {
		t.Fatalf("two of three: %v", r)
	}
	if string(mustRead(t, filepath.Join(b, "1.bin"))) != "1" || string(mustRead(t, filepath.Join(b, "2.bin"))) != "2" {
		t.Fatal("files")
	}
	if r := apiCall(t, h, "move", map[string]any{"ids": []string{one.ID}, "dir": b}); r["moved"] != float64(0) || !strings.Contains(mvStr(r["error"]), "already") {
		t.Fatalf("same folder: %v", r)
	}
}

func mvStr(v any) string {
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------- torrents

// A finished torrent that's sharing moves with its whole folder and carries on sharing from there.
func TestMoveSharingTorrent(t *testing.T) {
	for _, drives := range []bool{false, true} {
		tor, a, b := webSeedTorrent(t)
		all := append(append([]byte{}, a...), b...)
		if drives {
			betweenDrives(t)
		}
		m := torrentManager(t)
		d, _ := addTorrentBytes(t, m, tor, []bool{false, true}) // b.txt isn't wanted: some of it is in the hidden part file
		waitFor(t, m, d, StCompleted, 60*time.Second)
		old := runningTorrent(t, m, d.ID)
		from := filepath.Join(m.settings.DownloadDir, "got")
		to := filepath.Join(m.settings.DownloadDir, "elsewhere")
		parts := filepath.Join(from, "."+"Pack"+"."+d.Torrent.InfoHash[:8]+".parts")
		if _, err := os.Stat(parts); err != nil {
			t.Fatalf("the part file should exist while sharing: %v", err)
		}
		if err := m.MoveDownload(d.ID, to); err != nil {
			t.Fatalf("drives=%v: %v", drives, err)
		}
		if !bytes.Equal(mustRead(t, filepath.Join(to, "Pack", "a.bin")), a) {
			t.Fatal("a.bin")
		}
		mvGone(t, filepath.Join(from, "Pack"))
		mvGone(t, parts)
		if _, err := os.Stat(filepath.Join(to, "."+"Pack"+"."+d.Torrent.InfoHash[:8]+".parts")); err != nil {
			t.Fatalf("the part file didn't come along: %v", err)
		}
		m.mu.Lock()
		dir, status := d.Dir, d.Status
		m.mu.Unlock()
		if dir != to || status != StCompleted {
			t.Fatalf("now %s, %s", dir, status)
		}
		tr := runningTorrent(t, m, d.ID)
		if tr == old {
			t.Fatal("still the old run")
		}
		tr.mu.Lock()
		seeding, root := tr.seeding, tr.st.root
		tr.mu.Unlock()
		if !seeding || root != filepath.Join(to, "Pack") {
			t.Fatalf("seeding %v from %s", seeding, root)
		}
		// it reads the pieces from the new place, the one that spans both files too
		buf := make([]byte, 32<<10)
		for _, off := range []int64{0, 9 * 32 << 10} {
			if err := tr.st.ReadAt(buf, off); err != nil || !bytes.Equal(buf, all[off:off+int64(len(buf))]) {
				t.Fatalf("drives=%v: reading at %d: %v", drives, off, err)
			}
		}
		// and it stops for good when it's done sharing
		tr.stop("stopped")
		waitCond(t, 5*time.Second, "it stopped", func() bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			return m.torrents[d.ID] == nil
		})
	}
}

// A torrent's names come from the torrent: one whose folder is taken isn't moved, and keeps sharing.
func TestMoveTorrentRefusesATakenName(t *testing.T) {
	tor, a, _ := webSeedTorrent(t)
	m := torrentManager(t)
	d, _ := addTorrentBytes(t, m, tor, nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	from := filepath.Join(m.settings.DownloadDir, "got")
	to := filepath.Join(m.settings.DownloadDir, "elsewhere")
	mvWrite(t, filepath.Join(to, "Pack", "mine.txt"), []byte("mine"))
	err := m.MoveDownload(d.ID, to)
	if err == nil || !strings.Contains(err.Error(), "Pack is already in that folder") {
		t.Fatalf("%v", err)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(from, "Pack", "a.bin")), a) || string(mustRead(t, filepath.Join(to, "Pack", "mine.txt"))) != "mine" {
		t.Fatal("files were touched")
	}
	m.mu.Lock()
	dir := d.Dir
	m.mu.Unlock()
	tr := runningTorrent(t, m, d.ID)
	tr.mu.Lock()
	seeding, root := tr.seeding, tr.st.root
	tr.mu.Unlock()
	if dir != from || !seeding || root != filepath.Join(from, "Pack") {
		t.Fatalf("dir %s, seeding %v from %s", dir, seeding, root)
	}
}

// A torrent that's still downloading is paused, moved and carries on from the new folder.
func TestMoveTorrentWhileDownloading(t *testing.T) {
	tor, a, b := webSeedTorrent(t)
	v, _ := bdecode(tor)
	top := v.(map[string]any)
	web := top["url-list"].(string)
	// a web server that is slow: there is time to move it half way
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, _ := http.NewRequestWithContext(r.Context(), "GET", web+strings.TrimPrefix(r.URL.Path, "/"), nil)
		if rg := r.Header.Get("Range"); rg != "" {
			req.Header.Set("Range", rg)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for _, k := range []string{"Content-Range", "Content-Length", "Accept-Ranges", "Content-Type"} {
			if x := resp.Header.Get(k); x != "" {
				w.Header().Set(k, x)
			}
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 16<<10)
		for {
			n, err := resp.Body.Read(buf)
			w.Write(buf[:n])
			time.Sleep(100 * time.Millisecond)
			if err != nil {
				return
			}
		}
	}))
	defer slow.Close()
	top["url-list"] = slow.URL + "/"
	m := torrentManager(t)
	d, _ := addTorrentBytes(t, m, bencode(top), nil)
	waitCond(t, 30*time.Second, "some of it is here", func() bool { return atomic.LoadInt64(&d.done) > 60<<10 })
	to := filepath.Join(m.settings.DownloadDir, "elsewhere")
	if err := m.MoveDownload(d.ID, to); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, d, StCompleted, 60*time.Second)
	if !bytes.Equal(mustRead(t, filepath.Join(to, "Pack", "a.bin")), a) || !bytes.Equal(mustRead(t, filepath.Join(to, "Pack", "b.txt")), b) {
		t.Fatal("the files differ")
	}
	mvGone(t, filepath.Join(m.settings.DownloadDir, "got", "Pack"))
}
