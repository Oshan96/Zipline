package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Moving a download to another folder (Properties and the right-click menu). Finished files are
// moved as they are. A download that's still going is paused, its part files go with it, and it
// carries on from the new folder. A torrent's whole folder moves and it keeps sharing from there.
//
// A file is renamed into place when the folders are on the same drive. Between drives the file is
// copied beside its final name, renamed into place (so a half-copied file never has the real name)
// and only then is the original deleted. Nothing is ever overwritten: a name that's taken gets
// "(1)" added, as everywhere in Zipline. A torrent's names come from the torrent, so one whose
// folder (or file) is already there isn't moved.

// moveRename moves one file or folder to a name on the same drive. (A test swaps it, to move "between drives".)
var moveRename = os.Rename

// moves holds the downloads being moved now (ID -> *moveState).
var moves sync.Map

func moveActive(id string) bool {
	_, ok := moves.Load(id)
	return ok
}

// moveState is how far a move has got.
type moveState struct {
	dir   string
	total int64
	done  atomic.Int64
	mark  func(path string) // run on each finished file that was copied (Windows' "from the internet" mark doesn't copy)
}

func (st *moveState) add(n int64) {
	if st != nil {
		st.done.Add(n)
	}
}

// MoveView is a move in progress, for the list.
type MoveView struct {
	Pct int    `json:"pct"`
	Dir string `json:"dir"`
}

// moveView describes the download's move, if it has one. Safe without any lock.
func moveView(id string) *MoveView {
	v, ok := moves.Load(id)
	if !ok {
		return nil
	}
	st := v.(*moveState)
	pct := 0
	if st.total > 0 {
		pct = int(min(st.done.Load()*100/st.total, 100))
	}
	return &MoveView{Pct: pct, Dir: st.dir}
}

// moveItem is one file or folder to move.
type moveItem struct {
	src, dst string
	tree     bool // a folder
	optional bool // may be missing (a part file that was never written)
}

const moveTmp = ".zlmove" // what a file copied between drives is called until it's whole

// ---------------------------------------------------------------- moving files

// moveItems moves each item. When one fails, those already moved go back and the error is returned.
func moveItems(items []moveItem, st *moveState) error {
	for _, it := range items {
		if sz, err := sizeOf(it.src); err == nil {
			st.total += sz
		}
	}
	var done []moveItem
	for _, it := range items {
		moved, err := moveOne(it, st)
		if err == nil {
			if moved {
				done = append(done, it)
			}
			continue
		}
		var stuck []string
		for i := len(done) - 1; i >= 0; i-- { // (back where they were)
			back := moveItem{src: done[i].dst, dst: done[i].src, tree: done[i].tree}
			if _, e := moveOne(back, nil); e != nil {
				stuck = append(stuck, done[i].dst)
			}
		}
		if len(stuck) > 0 {
			err = fmt.Errorf("%w (and %s couldn't be put back; it's now in %s)", err, filepath.Base(stuck[0]), filepath.Dir(stuck[0]))
		}
		return err
	}
	return nil
}

// moveOne moves one file or folder, reporting whether there was anything to move.
func moveOne(it moveItem, st *moveState) (bool, error) {
	sz, err := sizeOf(it.src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && it.optional {
			return false, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return false, fmt.Errorf("%s isn't there any more", filepath.Base(it.src))
		}
		return false, err
	}
	if _, err := os.Lstat(it.dst); err == nil {
		return false, fmt.Errorf("%s is already there", filepath.Base(it.dst))
	}
	if err := os.MkdirAll(filepath.Dir(it.dst), 0o755); err != nil {
		return false, err
	}
	err = moveRename(it.src, it.dst)
	if err == nil {
		st.add(sz)
		return true, nil
	}
	if !crossDevice(err) {
		return false, err
	}
	// another drive: copy beside the final name, rename it into place, then delete the original
	tmp := it.dst + moveTmp
	os.RemoveAll(tmp) // (left by an earlier try)
	if it.tree {
		err = copyTree(it.src, tmp, st)
	} else {
		err = copyFile(it.src, tmp, st)
	}
	if err == nil {
		if _, e := os.Lstat(it.dst); e == nil { // (made in the meantime)
			err = fmt.Errorf("%s is already there", filepath.Base(it.dst))
		}
	}
	if err == nil {
		err = os.Rename(tmp, it.dst)
	}
	if err != nil {
		os.RemoveAll(tmp)
		return false, err
	}
	if it.tree {
		if err := os.RemoveAll(it.src); err != nil {
			logf("moved %s but couldn't delete the original: %v", it.src, err) // (the copy is whole: that's the one that counts)
		}
		return true, nil
	}
	if err := os.Remove(it.src); err != nil {
		os.Remove(it.dst)
		return false, err
	}
	return true, nil
}

func sizeOf(p string) (int64, error) {
	fi, err := os.Lstat(p)
	if err != nil {
		return 0, err
	}
	if !fi.IsDir() {
		return fi.Size(), nil
	}
	var n int64
	err = filepath.WalkDir(p, func(_ string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type().IsRegular() {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n, err
}

// copyFile copies a file under a new name (which must not exist), keeping its time.
func copyFile(src, dst string, st *moveState) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(out, countReader{in, st}, make([]byte, 1<<20))
	if err == nil {
		err = out.Sync() // (so a crash can't leave a name with nothing behind it)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	if st != nil && st.mark != nil {
		st.mark(dst)
	}
	os.Chtimes(dst, time.Now(), fi.ModTime())
	return nil
}

type countReader struct {
	io.Reader
	st *moveState
}

func (r countReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	r.st.add(int64(n))
	return n, err
}

// copyTree copies a folder under a new name.
func copyTree(src, dst string, st *moveState) error {
	err := filepath.WalkDir(src, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		to := filepath.Join(dst, rel)
		switch {
		case e.IsDir():
			return os.MkdirAll(to, 0o755)
		case e.Type().IsRegular():
			return copyFile(p, to, st)
		case e.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, to)
		}
		return nil // (sockets and the like)
	})
	if err != nil {
		os.RemoveAll(dst)
	}
	return err
}

// ---------------------------------------------------------------- moving a download

var errMoving = errors.New("it is being moved right now")

// MoveDownload moves a download's files to dir. It returns when they're there (or it failed, and
// everything is where it was).
func (m *Manager) MoveDownload(id, dir string) error {
	dir = filepath.Clean(strings.TrimSpace(dir))
	if dir == "." || !filepath.IsAbs(dir) {
		return errors.New("choose a folder")
	}
	m.mu.Lock()
	d := m.byID[id]
	switch {
	case d == nil || d.Provisional:
		m.mu.Unlock()
		return errors.New("not found")
	case sameFolder(d.Dir, dir):
		m.mu.Unlock()
		return errors.New("it is in that folder already")
	case d.stage != "" || d.ScanState == "scanning" || d.ScanState == "waiting":
		m.mu.Unlock()
		return errors.New("wait until it has been checked or converted")
	case d.Live && d.running:
		m.mu.Unlock()
		return errors.New("stop the recording first")
	}
	st := &moveState{dir: dir}
	if _, busy := moves.LoadOrStore(id, st); busy {
		m.mu.Unlock()
		return errMoving
	}
	defer moves.Delete(id)
	wasRunning, t := d.running, m.torrents[id]
	if d.Status == StCompleted {
		src, ref := d.URL, d.Headers["Referer"]
		st.mark = func(p string) { markFromInternet(p, src, ref) }
	}
	m.dirty = true
	m.mu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return errors.New("that isn't a folder")
	}

	// stop whatever is using the files
	seeding := false
	if wasRunning {
		m.Pause([]string{id})
		m.waitStopped(d)
		m.mu.Lock()
		stuck := d.running
		m.mu.Unlock()
		if stuck {
			return errors.New("couldn't pause it")
		}
	}
	if t != nil && !wasRunning {
		t.mu.Lock()
		seeding = t.seeding
		t.mu.Unlock()
		if seeding {
			t.moving.Store(true) // (its part file isn't deleted as it stops)
			t.stop("stopped")
			for i := 0; i < 500; i++ {
				t.mu.Lock()
				still := t.seeding
				t.mu.Unlock()
				if !still {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	defer func() { // carry on where it was
		moves.Delete(id)
		if wasRunning {
			m.Start([]string{id})
		}
		if seeding {
			m.reseedTorrent(d, false)
		}
		m.kick()
	}()

	m.mu.Lock()
	items, name, err := m.planMove(d, dir)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if err := moveItems(items, st); err != nil {
		return err
	}
	m.mu.Lock()
	d.Dir, d.FileName = dir, name
	m.dirty = true
	m.mu.Unlock()
	return nil
}

func sameFolder(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b) || sameFile(a, b)
}

// sameFile: two spellings of one folder (a different case on a case-blind drive, a link).
func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

// planMove lists what moves, and the name the download has in the new folder. Caller holds m.mu.
func (m *Manager) planMove(d *Download, dir string) (items []moveItem, name string, err error) {
	if d.Torrent != nil {
		return m.planTorrentMove(d, dir)
	}
	name = m.uniqueName(dir, d.FileName, nil) // (one already there gets "(1)" like any other)
	nd := &Download{Dir: dir, FileName: name, Extra: d.Extra}
	if d.Status == StCompleted {
		items = append(items, moveItem{src: d.path(), dst: nd.path()})
		if d.Kind == "hls" {
			items = append(items, m.streamSidecars(d, nd)...)
		}
		return items, name, nil
	}
	items = append(items, moveItem{src: d.partPath(), dst: nd.partPath(), optional: true},
		moveItem{src: d.audioPartPath(), dst: nd.audioPartPath(), optional: true})
	for i := range d.Extra {
		items = append(items, moveItem{src: d.extraPartPath(i), dst: nd.extraPartPath(i), optional: true})
	}
	return items, name, nil
}

// streamSidecars finds the files a finished stream left beside the video: its subtitles
// ("Movie.en.srt"), parts in another format ("Movie (part 2).mkv") and kept audio tracks
// ("Movie (audio Deutsch).m4a"). They follow the video's new name.
func (m *Manager) streamSidecars(d, nd *Download) []moveItem {
	oldBase := strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName))
	newBase := strings.TrimSuffix(nd.FileName, filepath.Ext(nd.FileName))
	entries, err := os.ReadDir(d.Dir)
	if err != nil {
		return nil
	}
	own := map[string]bool{}
	for _, o := range m.list {
		if o != d && strings.EqualFold(o.Dir, d.Dir) {
			own[strings.ToLower(o.FileName)] = true // another download's own file
		}
	}
	var out []moveItem
	for _, e := range entries {
		n := e.Name()
		rest, ok := strings.CutPrefix(n, oldBase)
		if !ok || n == d.FileName || !e.Type().IsRegular() || own[strings.ToLower(n)] {
			continue
		}
		if !(strings.HasPrefix(rest, ".") && strings.HasSuffix(strings.ToLower(rest), ".srt") ||
			strings.HasPrefix(rest, " (part ") || strings.HasPrefix(rest, " (audio ")) {
			continue
		}
		to := newBase + rest
		for i := 1; ; i++ { // (taken: the same "(1)" again)
			if _, e := os.Lstat(filepath.Join(nd.Dir, to)); e != nil {
				break
			}
			ext := filepath.Ext(rest)
			to = newBase + strings.TrimSuffix(rest, ext) + " (" + itoa(i) + ")" + ext
		}
		out = append(out, moveItem{src: filepath.Join(d.Dir, n), dst: filepath.Join(nd.Dir, to)})
	}
	return out
}

// planTorrentMove: a torrent's folder (or its one file) and its hidden part file. Caller holds m.mu.
func (m *Manager) planTorrentMove(d *Download, dir string) (items []moveItem, name string, err error) {
	ts := d.Torrent
	ih, ok := ts.hash()
	var mi *Metainfo
	if ok && ts.HasMeta {
		mi = m.loadTorrent(ih)
	}
	if mi == nil { // the file list isn't known: nothing is on disk yet
		return nil, d.FileName, nil
	}
	skip := ts.Skip
	if len(skip) != len(mi.Files) {
		skip = make([]bool, len(mi.Files))
	}
	from, to := newStorage(mi, d.Dir, skip, ts.InfoHash), newStorage(mi, dir, skip, ts.InfoHash)
	if mi.Multi {
		items = append(items, moveItem{src: from.root, dst: to.root, tree: true, optional: true})
	} else {
		items = append(items, moveItem{src: from.filePath(0), dst: to.filePath(0), optional: true})
	}
	items = append(items, moveItem{src: from.parts, dst: to.parts, optional: true})
	for _, it := range items {
		if _, e := os.Lstat(it.dst); e == nil {
			if _, e := os.Lstat(it.src); e == nil { // (nothing to put there anyway when the source isn't)
				return nil, "", fmt.Errorf("%s is already in that folder, and a torrent's files can't be renamed", filepath.Base(it.dst))
			}
		}
	}
	return items, d.FileName, nil
}

// reseedTorrent shares a finished torrent again, from wherever its files are now. It's what finish
// does after the last piece, without the "completed" announce, the notice and the virus scan. With
// fromNow the ratio counts what is uploaded from here on (Share again); without, all of it (a move
// carries on the sharing it interrupted). False when it didn't start.
func (m *Manager) reseedTorrent(d *Download, fromNow bool) bool {
	m.mu.Lock()
	ts := d.Torrent
	if ts == nil || m.torrents[d.ID] != nil {
		m.mu.Unlock()
		return false
	}
	ih, ok := ts.hash()
	var mi *Metainfo
	if ok {
		mi = m.loadTorrent(ih)
	}
	if mi == nil {
		m.mu.Unlock()
		return false
	}
	skip := append([]bool(nil), ts.Skip...)
	have := append(bitfield(nil), ts.Have...)
	trackers := append([]string(nil), ts.Trackers...)
	up, down, upKB := ts.Uploaded, ts.Downloaded, ts.UpKB
	planQ := d.Queue == planQueueID
	m.mu.Unlock()
	t := m.newTorrent(d, ih, trackers)
	t.setUpKB(upKB)
	t.planQ.Store(planQ)
	atomic.StoreInt64(&t.uploaded, up)
	atomic.StoreInt64(&t.downloaded, down)
	t.mu.Lock()
	t.setMeta(mi, skip, have)
	t.dropMissingFiles()
	t.checkComplete()
	whole := t.complete
	t.seeding, t.seedAt = whole, time.Now()
	if fromNow {
		t.shareBase = up
	}
	t.mu.Unlock()
	m.mu.Lock()
	if !whole || m.torrents[d.ID] != nil {
		m.mu.Unlock()
		t.cancel()
		return false
	}
	m.torrents[d.ID] = t
	m.mu.Unlock()
	go t.trackerLoop()
	go t.dhtLoop()
	go t.persistLoop()
	go t.extLoop()
	t.tick(0) // (so the window sees it sharing at once)
	ratio, limit := t.seedNow()
	go t.seed(ratio, limit)
	return true
}

// ---------------------------------------------------------------- the window's call

// moveH moves downloads to a folder: {ids, dir}. It answers when they're all there.
func (s *Server) moveH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
		Dir string   `json:"dir"`
	}
	if readJSON(r, &req) != nil || len(req.IDs) == 0 {
		http.Error(w, "bad request", 400)
		return
	}
	moved := 0
	var failed []string
	for _, id := range req.IDs {
		err := s.m.MoveDownload(id, req.Dir)
		if err == nil {
			moved++
			continue
		}
		s.m.mu.Lock()
		name := ""
		if d := s.m.byID[id]; d != nil && len(req.IDs) > 1 {
			name = d.FileName + ": "
		}
		s.m.mu.Unlock()
		failed = append(failed, name+err.Error())
	}
	res := map[string]any{"moved": moved}
	if len(failed) > 0 {
		res["error"] = strings.Join(failed, "; ")
	}
	writeJSON(w, res)
}
