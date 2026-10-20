package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Checking files: reading pieces back from disk and keeping the ones that match their SHA-1.
// Progress is saved with each file's size and modified time (Transmission's way), so a start
// only reads back the files that changed since (a crash, another program writing to them). A
// torrent added over files already in its folder checks them and keeps the good pieces, "Check
// files" and Redownload check everything, and Options can have a torrent check again when it
// finishes. Nothing is asked of peers while a check runs.

type fileStamp struct {
	Size int64 `json:"s"`
	Mod  int64 `json:"m"` // Unix nanoseconds; -1: a check of the file hasn't finished
}

var stampUnchecked = fileStamp{Mod: -1}

// stamp is a file's size and modified time ({} when it isn't there). The bytes of a file you
// didn't choose live in the hidden parts file, so that one counts for it.
func (s *tstorage) stamp(i int) fileStamp {
	s.mu.Lock()
	defer s.mu.Unlock()
	path, f := s.filePath(i), s.open[i]
	if s.skip[i] {
		path, f = s.parts, s.pf
	}
	var st os.FileInfo
	var err error
	if f != nil {
		st, err = f.Stat() // (an open handle sees its own writes everywhere)
	} else {
		st, err = os.Stat(path)
	}
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{Size: st.Size(), Mod: st.ModTime().UnixNano()}
}

func (s *tstorage) skipped() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.skip...)
}

// stamps are the files' stamps to save with the progress; files whose check hasn't finished are
// saved as such, so the next start checks them. Caller holds no locks.
// Only files written since the last time are looked at again (a torrent can have thousands),
// except when it stops: then all of them, as closing a file may set its time.
func (t *torrent) stamps(unchecked []bool) []fileStamp {
	t.mu.Lock()
	st, mi := t.st, t.mi
	if st == nil || mi == nil {
		t.mu.Unlock()
		return nil
	}
	n := len(mi.Files)
	if len(t.stampAt) != n || t.ctx.Err() != nil {
		t.stampAt, t.stampStale = make([]fileStamp, n), make([]bool, n)
		for i := range t.stampStale {
			t.stampStale[i] = true
		}
	}
	stale := t.stampStale
	t.stampStale = make([]bool, n)
	out := append([]fileStamp(nil), t.stampAt...)
	t.mu.Unlock()
	for i, f := range mi.Files {
		if stale[i] && !f.Pad && f.Length > 0 {
			out[i] = st.stamp(i)
		}
	}
	t.mu.Lock()
	if len(t.stampAt) == n {
		copy(t.stampAt, out)
	}
	t.mu.Unlock()
	for i := range out {
		if i < len(unchecked) && unchecked[i] {
			out[i] = stampUnchecked
		}
	}
	return out
}

// markWritten notes the files a piece just written went into (their stamps are looked at again).
// The hidden parts file holds bytes of every file you didn't choose. Caller holds t.mu.
func (t *torrent) markWritten(piece int) {
	if len(t.stampStale) != len(t.mi.Files) {
		return
	}
	fs := t.mi.Files
	off := int64(piece) * t.mi.PieceLen
	end := off + t.mi.PieceSize(piece)
	parts := false
	for i := sort.Search(len(fs), func(k int) bool { return fs[k].Offset+fs[k].Length > off }); i < len(fs) && fs[i].Offset < end; i++ {
		t.stampStale[i] = true
		parts = parts || i < len(t.skip) && t.skip[i]
	}
	for i := 0; parts && i < len(fs); i++ {
		if i < len(t.skip) && t.skip[i] {
			t.stampStale[i] = true
		}
	}
}

// uncheckedNow copies which files a check hasn't finished with. Caller holds t.mu.
func (t *torrent) uncheckedNow() []bool { return append([]bool(nil), t.unchecked...) }

func (t *torrent) checkPct() int {
	if t.checkTotal <= 0 {
		return 0
	}
	return int(min(t.checkDone.Load()*100/t.checkTotal, 99))
}

// ---------------------------------------------------------------- when to check

// checkOnStart reads back what can't be trusted before the download goes on: every piece when
// asked to ("Check files", Redownload), the pieces we had of files that changed since the progress
// was saved, and for a torrent added over files already in its folder, those files' pieces.
func (t *torrent) checkOnStart(ctx context.Context, saved []fileStamp, all bool) error {
	t.mu.Lock()
	if t.mi == nil {
		t.mu.Unlock()
		return nil
	}
	mi, st := t.mi, t.st
	fresh := len(saved) != len(mi.Files) && t.haveCount() == 0
	files := make([]bool, len(mi.Files))
	pick := make([]bool, mi.NumPieces())
	for i, f := range mi.Files {
		if f.Pad || f.Length == 0 {
			continue
		}
		whole, had := all, false // check every piece of it, or the ones we had
		switch {
		case all:
		case len(saved) == len(mi.Files):
			if saved[i] == stampUnchecked {
				whole = true
			} else {
				had = st.stamp(i) != saved[i]
			}
		case fresh:
			whole = st.stamp(i).Size > 0
		}
		if !whole && !had {
			continue
		}
		files[i] = true
		for p := int(f.Offset / mi.PieceLen); p <= int((f.Offset+f.Length-1)/mi.PieceLen); p++ {
			if whole && t.wanted[p] || t.have.has(p) {
				pick[p] = true
			}
		}
	}
	var list []int
	for p, x := range pick {
		if x {
			list = append(list, p)
		}
	}
	if len(list) == 0 || !t.beginCheck(list, files) {
		t.mu.Unlock()
		if all {
			t.m.recheckDone(t.d)
		}
		return nil
	}
	t.mu.Unlock()
	tdbg("%s: checking %d pieces before downloading", t.d.FileName, len(list))
	if !t.runCheck(ctx, list, files, all) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		t.mu.Lock()
		err := t.err
		t.mu.Unlock()
		if err != nil {
			return err
		}
		return errors.New("the torrent stopped")
	}
	return nil
}

// checkArrived: a magnet link's file list is in, and its files may be in the folder already
// (added again, or fetched some other way): check those first. Caller holds t.mu.
func (t *torrent) checkArrived() {
	if strings.HasPrefix(t.d.ID, "meta:") || t.haveCount() > 0 {
		return
	}
	mi := t.mi
	files := make([]bool, len(mi.Files))
	var list []int
	for i, f := range mi.Files {
		if f.Pad || f.Length == 0 || t.st.stamp(i).Size == 0 {
			continue
		}
		files[i] = true
		for p := int(f.Offset / mi.PieceLen); p <= int((f.Offset+f.Length-1)/mi.PieceLen); p++ {
			if t.wanted[p] && (len(list) == 0 || list[len(list)-1] < p) {
				list = append(list, p)
			}
		}
	}
	if len(list) > 0 && t.beginCheck(list, files) {
		go t.runCheck(t.ctx, list, files, false)
	}
}

// checkAll checks every piece of a running torrent ("Check files").
func (t *torrent) checkAll() {
	for {
		t.mu.Lock()
		if t.ctx.Err() != nil || t.mi == nil {
			t.mu.Unlock()
			return
		}
		if !t.checking {
			break
		}
		t.mu.Unlock()
		time.Sleep(200 * time.Millisecond) // one check at a time
	}
	list, files := t.everything()
	ok := t.beginCheck(list, files)
	t.mu.Unlock()
	if ok {
		t.runCheck(t.ctx, list, files, true)
	}
}

// everything lists every piece we want or have and every file. Caller holds t.mu.
func (t *torrent) everything() ([]int, []bool) {
	var list []int
	for p, w := range t.wanted {
		if w || t.have.has(p) {
			list = append(list, p)
		}
	}
	files := make([]bool, len(t.mi.Files))
	for i, f := range t.mi.Files {
		files[i] = !f.Pad && f.Length > 0
	}
	return list, files
}

// checkWhenDone checks every piece once a download is complete, when Options asks for it. True
// when that found pieces to download again, or the download was stopped meanwhile.
func (t *torrent) checkWhenDone(ctx context.Context) bool {
	for { // a check already under way (Check files) ends first: it may find pieces missing
		t.mu.Lock()
		busy := t.checking
		t.mu.Unlock()
		if !busy {
			break
		}
		if !sleepCtx(ctx, 100*time.Millisecond) {
			return true
		}
	}
	t.m.mu.Lock()
	on := t.m.settings.Torrent.CheckDone
	t.m.mu.Unlock()
	t.mu.Lock()
	if !t.complete { // (it did)
		t.mu.Unlock()
		return true
	}
	if !on || !t.wrote || t.mi == nil { // (nothing new on disk since the last full check)
		t.mu.Unlock()
		return false
	}
	list, files := t.everything()
	if !t.beginCheck(list, files) {
		t.mu.Unlock()
		return false
	}
	t.mu.Unlock()
	if !t.runCheck(ctx, list, files, true) {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.complete
}

// CheckTorrents checks torrents' files ("Check files"; Redownload does the same for torrents).
// A running download checks straight away; any other is started and checks first.
func (m *Manager) CheckTorrents(ids []string) {
	m.mu.Lock()
	var now []*torrent
	for _, id := range ids {
		if d := m.byID[id]; d != nil {
			if t := m.checkTorrentLocked(d); t != nil {
				now = append(now, t)
			}
		}
	}
	m.mu.Unlock()
	for _, t := range now {
		go t.checkAll()
	}
}

// checkTorrentLocked asks for a torrent's files to be checked; it returns the running engine
// that should check now (nil when it starts instead). Caller holds m.mu.
func (m *Manager) checkTorrentLocked(d *Download) *torrent {
	ts := d.Torrent
	if ts == nil || !ts.HasMeta {
		return nil
	}
	ts.Recheck = true // (until it has been done, even across a restart)
	m.dirty = true
	if t := m.torrents[d.ID]; t != nil && d.running {
		return t
	}
	if d.Status == StCompleted {
		d.Status = StPaused
	}
	d.Error = ""
	m.startByHand(d)
	return nil
}

// recheckDone: a full check is over.
func (m *Manager) recheckDone(d *Download) {
	m.mu.Lock()
	if d.Torrent != nil && d.Torrent.Recheck {
		d.Torrent.Recheck = false
		m.dirty = true
	}
	m.mu.Unlock()
}

// ---------------------------------------------------------------- checking

// beginCheck stops asking for blocks and forgets the pieces being put together (their blocks
// would land in pieces being checked). False when a check is already running. Caller holds t.mu.
func (t *torrent) beginCheck(list []int, files []bool) bool {
	if t.checking {
		return false
	}
	t.checking = true
	t.checkTotal = int64(len(list))
	t.checkDone.Store(0)
	if len(t.unchecked) != len(files) {
		t.unchecked = make([]bool, len(files))
	}
	for i, f := range files {
		t.unchecked[i] = t.unchecked[i] || f
	}
	t.dropPartials(false)
	return true
}

// runCheck reads the pieces back and keeps those that match; the rest are downloaded (again).
// all: a check of everything (it clears "Check files"). False when stopped before the end.
func (t *torrent) runCheck(ctx context.Context, list []int, files []bool, all bool) bool {
	t.drainWrites() // (pieces checked in memory still on their way to disk)
	t.publish()
	t.mu.Lock()
	mi, st := t.mi, t.st
	t.mu.Unlock()
	stop := make(chan struct{})
	go func() { // the window follows the check (the engine's own second may not be ticking yet)
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				t.publish()
			}
		}
	}()
	ok, finished := readPieces(ctx, t.ctx, mi, st, list, &t.checkDone)
	close(stop)
	t.mu.Lock()
	t.checking = false
	if !finished {
		t.mu.Unlock()
		t.publish()
		return false
	}
	var gained []int
	for k, i := range list {
		switch {
		case ok[k] && !t.have.has(i):
			t.have.set(i)
			gained = append(gained, i)
		case !ok[k] && t.have.has(i):
			t.have.unset(i)
		}
	}
	for i, f := range files {
		if f && i < len(t.unchecked) {
			t.unchecked[i] = false
		}
	}
	if all {
		t.wrote = false
	}
	var done int64
	for p, w := range t.wanted {
		if w && t.have.has(p) {
			done += t.wantBytes[p]
		}
	}
	atomic.StoreInt64(&t.d.done, done+t.partialDone())
	if t.complete && t.wantedLeft() > 0 { // pieces went missing: download them again
		t.complete = false
		t.finished = make(chan struct{})
	}
	t.checkComplete()
	for _, i := range gained {
		have := make([]byte, 5)
		have[0] = msgHave
		binary.BigEndian.PutUint32(have[1:], uint32(i))
		for _, p := range t.peers {
			if !p.have.has(i) {
				p.sendLocked(have)
			}
		}
	}
	for _, p := range t.peers {
		t.updateInterest(p)
	}
	t.mu.Unlock()
	tdbg("%s: checked %d pieces, %d good", t.d.FileName, len(list), countTrue(ok))
	if all {
		t.m.recheckDone(t.d)
	}
	t.persist()
	t.publish()
	return true
}

// finishedCh is the channel closed when every wanted piece is in (a check can make a new one).
func (t *torrent) finishedCh() chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished
}

// publish updates what the window shows.
func (t *torrent) publish() {
	v := t.view(time.Now().Unix() <= t.wantPeers.Load())
	t.live.Store(&v)
}

func countTrue(l []bool) (n int) {
	for _, x := range l {
		if x {
			n++
		}
	}
	return n
}

// readPieces reads pieces from disk and compares their SHA-1, a few at a time.
func readPieces(ctx, tctx context.Context, mi *Metainfo, st *tstorage, list []int, done *atomic.Int64) (ok []bool, finished bool) {
	ok = make([]bool, len(list))
	rd := &checkReader{s: st, skip: st.skipped(), open: map[int]*os.File{}, bad: map[int]error{}}
	defer rd.close()
	var next atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < min(max(runtime.NumCPU(), 1), 4, len(list)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, mi.PieceLen)
			for ctx.Err() == nil && tctx.Err() == nil {
				k := int(next.Add(1) - 1)
				if k >= len(list) {
					return
				}
				i := list[k]
				b := buf[:mi.PieceSize(i)]
				if rd.read(b, int64(i)*mi.PieceLen) == nil {
					h := sha1.Sum(b)
					ok[k] = bytes.Equal(h[:], mi.PieceHash(i))
				}
				done.Add(1)
			}
		}()
	}
	wg.Wait()
	return ok, ctx.Err() == nil && tctx.Err() == nil
}

// checkReader reads a torrent's bytes for a check: files are opened read-only, and a file that
// isn't there fails its pieces instead of being created as it would be for downloading.
type checkReader struct {
	s    *tstorage
	skip []bool
	mu   sync.Mutex
	open map[int]*os.File // (-1: the parts file)
	bad  map[int]error
}

func (r *checkReader) file(i int) (*os.File, error) {
	path := r.s.filePath(i)
	if r.skip[i] {
		i, path = -1, r.s.parts
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if f := r.open[i]; f != nil {
		return f, nil
	}
	if err := r.bad[i]; err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		r.bad[i] = err
		return nil, err
	}
	if len(r.open) >= 128 { // many files: the rest are opened for each read
		return f, errOneShot
	}
	r.open[i] = f
	return f, nil
}

var errOneShot = errors.New("close after reading")

func (r *checkReader) read(p []byte, off int64) error {
	return r.s.span(off, len(p), func(i int, fo int64, a, b int) error {
		if r.s.mi.Files[i].Pad {
			clear(p[a:b])
			return nil
		}
		f, err := r.file(i)
		if err == errOneShot {
			defer f.Close()
		} else if err != nil {
			return err
		}
		at := fo
		if r.skip[i] {
			at = r.s.mi.Files[i].Offset + fo
		}
		n, err := f.ReadAt(p[a:b], at)
		if n == b-a {
			return nil
		}
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF // (shorter than it should be)
		}
		return err
	})
}

func (r *checkReader) close() {
	for _, f := range r.open {
		f.Close()
	}
}
