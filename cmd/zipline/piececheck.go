package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Piece checks. A metalink can give a checksum for every piece of the file (SHA-1, SHA-256…).
// Like aria2, Zipline checks each piece as soon as all its bytes are written, not only once the
// whole file is done, and needs no checksum of the whole file for it. A piece is read back from
// the part file rather than hashed as it streams in: connections split their ranges anywhere, so
// a piece is often written by two connections (even two mirrors), and bytes just written are
// still in the system's cache, so reading them back costs little. The same reading serves the
// pieces a restart finds complete but never checked.
//
// A damaged piece is marked as not downloaded and fetched again from another address than the
// one that sent it. Each address's damaged pieces are counted, and one that sent maxBadPieces is
// dropped ("sent damaged data"). When several addresses sent parts of a damaged piece, a copy is
// kept until the piece arrives right: the parts that differ show who sent the damage.

const (
	maxBadPieces = 3        // damaged pieces an address may send before it's dropped
	keepBadMax   = 16 << 20 // a damaged piece several addresses sent is kept up to this size…
	keepBadTotal = 64 << 20 // …and this much for all of them
)

var (
	errBadData = errors.New("the server sent damaged data (pieces that don't match the metalink's checksums)")
	errDropped = errors.New("the server was dropped: it sent damaged data")
)

type pieceChecker struct {
	d    *Download
	m    *Manager
	ss   *sourceSet
	f    *os.File
	ph   *PieceHashes
	size int64
	name string // (for the log)
	done chan struct{}

	mu      sync.Mutex
	cond    *sync.Cond
	todo    []int // complete pieces waiting for their check
	pending int   // pieces queued or being checked
	quit    bool
	written []int64                  // per piece: bytes written since it was last marked not downloaded
	from    [][]span                 // per piece: which address sent which bytes
	avoid   map[int]map[*source]bool // pieces that arrived damaged: the addresses that sent them
	kept    map[int]*keptPiece       // damaged pieces several addresses sent
	keptN   int64
	tries   map[int]int // damaged arrivals per piece
	dropped map[*source]bool
	found   int // damaged pieces found this run
}

type keptPiece struct {
	data  []byte
	spans []span
}

// piecesFit: the piece checksums cover a file of this size.
func piecesFit(ph *PieceHashes, size int64) bool {
	return ph != nil && ph.Length > 0 && size > 0 && int64(len(ph.Hashes)) == (size+ph.Length-1)/ph.Length && newHash(ph.Algo) != nil
}

// newPieceCheck starts checking d's pieces as they complete (nil when it has no piece checksums).
// Pieces a restart finds complete but unchecked are checked first. Call it once d's segments are
// set; caller holds no locks.
func (m *Manager) newPieceCheck(d *Download, f *os.File, ss *sourceSet) *pieceChecker {
	m.mu.Lock()
	ph, size, name, kind := d.Pieces, d.Size, d.FileName, d.Kind
	m.mu.Unlock()
	if !piecesFit(ph, size) || kind != "" {
		return nil
	}
	n := len(ph.Hashes)
	pc := &pieceChecker{d: d, m: m, ss: ss, f: f, ph: ph, size: size, name: name, done: make(chan struct{}),
		written: make([]int64, n), from: make([][]span, n), avoid: map[int]map[*source]bool{}, kept: map[int]*keptPiece{},
		tries: map[int]int{}, dropped: map[*source]bool{}}
	pc.cond = sync.NewCond(&pc.mu)
	for i := range pc.written {
		pc.written[i] = pc.length(i)
	}
	d.mu.Lock()
	for _, s := range d.Segments { // what's still to download isn't written
		for a, b := s.Pos, s.End+1; a < b; {
			i := int(a / ph.Length)
			if i >= n {
				break
			}
			e := min(b, pc.end(i))
			pc.written[i] -= e - a
			a = e
		}
	}
	if len(d.PieceOK) != len(newBitfield(n)) {
		d.PieceOK = newBitfield(n)
	}
	for i := range pc.written {
		switch {
		case pc.written[i] < pc.length(i):
			d.PieceOK.unset(i)
		case !d.PieceOK.has(i): // complete before a restart, never checked
			pc.todo = append(pc.todo, i)
		}
	}
	pc.pending = len(pc.todo)
	d.pc = pc
	d.mu.Unlock()
	go pc.loop()
	return pc
}

func (pc *pieceChecker) end(i int) int64    { return min(int64(i+1)*pc.ph.Length, pc.size) }
func (pc *pieceChecker) length(i int) int64 { return pc.end(i) - int64(i)*pc.ph.Length }

// wrote notes the bytes [a, b) a connection's reply (body) brought and queues the pieces now
// complete for their check. False when its address has been dropped meanwhile: the bytes aren't
// kept, and the connection should end.
func (pc *pieceChecker) wrote(body io.ReadCloser, a, b int64) bool {
	if pc == nil || b <= a {
		return true
	}
	var s *source
	if sb, ok := body.(*srcBody); ok {
		s = sb.s
	}
	pc.mu.Lock()
	if s != nil && pc.dropped[s] {
		pc.mu.Unlock()
		pc.d.mu.Lock()
		pc.d.unmark(a, b)
		pc.d.mu.Unlock()
		atomic.AddInt64(&pc.d.done, -(b - a))
		return false
	}
	defer pc.mu.Unlock()
	for i := int(a / pc.ph.Length); i < len(pc.written) && int64(i)*pc.ph.Length < b; i++ {
		lo, hi := max(a, int64(i)*pc.ph.Length), min(b, pc.end(i))
		before := pc.written[i]
		pc.written[i] += hi - lo
		if s != nil {
			pc.from[i] = addSpan(pc.from[i], s, lo, hi)
		}
		if before < pc.length(i) && pc.written[i] >= pc.length(i) {
			pc.todo = append(pc.todo, i)
			pc.pending++
			pc.cond.Broadcast()
		}
	}
	return true
}

// addSpan records that s sent [a, b), joining it to the span before when s sent that too.
func addSpan(l []span, s *source, a, b int64) []span {
	if n := len(l); n > 0 && l[n-1].s == s && l[n-1].b == a {
		l[n-1].b = b
		return l
	}
	return append(l, span{s, a, b})
}

// avoiding lists the addresses that sent the piece holding pos damaged: a connection starting
// there goes elsewhere if it can.
func (pc *pieceChecker) avoiding(pos int64) map[*source]bool {
	out := map[*source]bool{}
	if pc == nil {
		return out
	}
	pc.mu.Lock()
	for s := range pc.avoid[int(pos/pc.ph.Length)] {
		out[s] = true
	}
	pc.mu.Unlock()
	return out
}

// loop checks the complete pieces one after another until stop.
func (pc *pieceChecker) loop() {
	defer close(pc.done)
	for {
		pc.mu.Lock()
		for len(pc.todo) == 0 && !pc.quit {
			pc.cond.Wait()
		}
		if pc.quit {
			pc.mu.Unlock()
			return
		}
		i := pc.todo[0]
		pc.todo = pc.todo[1:]
		pc.mu.Unlock()
		pc.check(i)
		pc.mu.Lock()
		pc.pending--
		pc.cond.Broadcast()
		pc.mu.Unlock()
	}
}

func (pc *pieceChecker) check(i int) {
	good, err := pieceMatches(pc.f, pc.ph, i, int64(i)*pc.ph.Length, pc.end(i))
	switch {
	case err != nil: // left unchecked: it's checked again when the file is finished
		logf("%s: couldn't read piece %d back: %v", pc.name, i, err)
	case good:
		pc.passed(i)
	default:
		pc.failed(i)
	}
}

// pieceMatches reads piece i ([a, b)) and compares it with its checksum.
func pieceMatches(r io.ReaderAt, ph *PieceHashes, i int, a, b int64) (bool, error) {
	h := newHash(ph.Algo)
	if _, err := io.Copy(h, io.NewSectionReader(r, a, b-a)); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == strings.ToLower(ph.Hashes[i]), nil
}

func (pc *pieceChecker) passed(i int) {
	pc.d.mu.Lock()
	pc.d.PieceOK.set(i)
	pc.d.mu.Unlock()
	pc.mu.Lock()
	delete(pc.avoid, i)
	pc.from[i] = nil
	k := pc.kept[i]
	if k != nil {
		delete(pc.kept, i)
		pc.keptN -= int64(len(k.data))
	}
	pc.mu.Unlock()
	if k != nil {
		pc.blame(i, k)
	}
}

// blame compares a damaged piece several addresses sent with the right one: those whose part
// differs sent the damage.
func (pc *pieceChecker) blame(i int, k *keptPiece) {
	a := int64(i) * pc.ph.Length
	right := make([]byte, len(k.data))
	if _, err := pc.f.ReadAt(right, a); err != nil {
		return
	}
	hit := map[*source]bool{}
	for _, sp := range k.spans {
		if !hit[sp.s] && !bytes.Equal(k.data[sp.a-a:sp.b-a], right[sp.a-a:sp.b-a]) {
			hit[sp.s] = true
			pc.strike(sp.s)
		}
	}
}

// failed marks a damaged piece as not downloaded, to be fetched again from someone else, and
// counts it against the address that sent it.
func (pc *pieceChecker) failed(i int) {
	d := pc.d
	a, b := int64(i)*pc.ph.Length, pc.end(i)
	pc.mu.Lock()
	spans := pc.from[i]
	pc.tries[i]++
	tries := pc.tries[i]
	room := b-a <= keepBadMax && pc.keptN+(b-a) <= keepBadTotal
	pc.mu.Unlock()
	var senders []*source
	for _, sp := range spans {
		if !containsSource(senders, sp.s) {
			senders = append(senders, sp.s)
		}
	}
	var kept *keptPiece
	if len(senders) > 1 && tries < maxBadPieces && room { // who sent the damage shows once it arrives right
		if buf := make([]byte, b-a); pieceRead(pc.f, buf, a) {
			kept = &keptPiece{buf, spans}
		}
	}
	d.mu.Lock()
	d.unmark(a, b)
	d.PieceOK.unset(i)
	pc.mu.Lock()
	pc.written[i], pc.from[i] = 0, nil
	if pc.avoid[i] == nil {
		pc.avoid[i] = map[*source]bool{}
	}
	for _, s := range senders {
		pc.avoid[i][s] = true
	}
	if old := pc.kept[i]; old != nil {
		pc.keptN -= int64(len(old.data))
		delete(pc.kept, i)
	}
	if kept != nil {
		pc.kept[i] = kept
		pc.keptN += b - a
	}
	pc.found++
	found := pc.found
	pc.mu.Unlock()
	d.mu.Unlock()
	atomic.AddInt64(&d.done, -(b - a))
	var who []string
	for _, s := range senders {
		who = append(who, stripURLPass(s.url))
	}
	logf("%s: piece %d arrived damaged (from %s): fetching it again", pc.name, i, strings.Join(who, ", "))
	// One address sent it: that one's to blame. A piece that keeps arriving damaged counts against
	// all who sent it, when there's no copy to tell them apart.
	if len(senders) == 1 || kept == nil && tries >= maxBadPieces {
		for _, s := range senders {
			pc.strike(s)
		}
	}
	pc.m.mu.Lock()
	d.Note = fmt.Sprintf("%d damaged piece%s downloaded again", found, map[bool]string{true: "", false: "s"}[found == 1])
	pc.m.dirty = true
	pc.m.mu.Unlock()
}

func pieceRead(f *os.File, buf []byte, at int64) bool {
	_, err := f.ReadAt(buf, at)
	return err == nil
}

func containsSource(l []*source, s *source) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// strike counts a damaged piece against an address and drops it at maxBadPieces. The last one
// left is dropped too: the download then stops with errBadData rather than fetching damage forever.
func (pc *pieceChecker) strike(s *source) {
	ss := pc.ss
	ss.mu.Lock()
	s.bad++
	drop := s.bad >= maxBadPieces && s.dead == ""
	if drop {
		s.dead, s.deadErr = "sent damaged data", errBadData
	}
	ss.mu.Unlock()
	if drop {
		pc.mu.Lock()
		pc.dropped[s] = true
		pc.mu.Unlock()
		logf("%s: dropped %s: it sent %d damaged pieces", pc.name, stripURLPass(s.url), maxBadPieces)
		pc.forget(s)
	}
}

// forget marks what a dropped address wrote of the pieces not complete yet as not downloaded:
// finished by others, those pieces would most likely only fail their check.
func (pc *pieceChecker) forget(s *source) {
	d := pc.d
	var lost int64
	d.mu.Lock()
	pc.mu.Lock()
	for i, spans := range pc.from {
		if pc.written[i] >= pc.length(i) || !slices.ContainsFunc(spans, func(sp span) bool { return sp.s == s }) {
			continue // (a complete piece is checked anyway)
		}
		var keep []span
		for _, sp := range spans {
			if sp.s != s {
				keep = append(keep, sp)
				continue
			}
			d.unmark(sp.a, sp.b)
			pc.written[i] -= sp.b - sp.a
			lost += sp.b - sp.a
		}
		pc.from[i] = keep
	}
	pc.mu.Unlock()
	d.mu.Unlock()
	atomic.AddInt64(&d.done, -lost)
}

// settle waits, once the connections have finished, for the pieces still being checked. When one
// of them arrived damaged it returns the range to fetch again (taken), else nil. Nothing is waited
// for when the connections stopped with work left (they failed, or the download was stopped).
func (pc *pieceChecker) settle(ctx context.Context) *Segment {
	if pc == nil || ctx.Err() != nil {
		return nil
	}
	d := pc.d
	d.mu.Lock()
	left := d.unfinished() || d.fatalErr != nil
	d.mu.Unlock()
	if left {
		return nil
	}
	stop := context.AfterFunc(ctx, func() {
		pc.mu.Lock()
		pc.cond.Broadcast()
		pc.mu.Unlock()
	})
	defer stop()
	pc.mu.Lock()
	for pc.pending > 0 && ctx.Err() == nil {
		pc.cond.Wait()
	}
	pc.mu.Unlock()
	if ctx.Err() != nil || !pc.ss.anyUsable() {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.nextSegment()
}

// stop ends the checks (those still queued are done after a restart, or when the file is
// finished) and waits for the one under way, so the part file can be closed.
func (pc *pieceChecker) stop() {
	if pc == nil {
		return
	}
	pc.mu.Lock()
	pc.quit = true
	pc.cond.Broadcast()
	pc.mu.Unlock()
	<-pc.done
	pc.d.mu.Lock()
	if pc.d.pc == pc {
		pc.d.pc = nil
	}
	pc.d.mu.Unlock()
}

// unmark marks [a, b) as not downloaded: a segment of its own, carved out of those that hold it.
// The part after it stays with its segment, and so with the connection that may still be writing
// there. Caller holds d.mu.
func (d *Download) unmark(a, b int64) {
	var keep []*Segment
	for _, s := range d.Segments {
		if s.End < a || s.Start >= b {
			keep = append(keep, s)
			continue
		}
		if s.Start < a {
			keep = append(keep, &Segment{Start: s.Start, End: a - 1, Pos: min(s.Pos, a)})
		}
		if s.End >= b {
			s.Start, s.Pos = b, max(s.Pos, b)
			keep = append(keep, s)
		}
	}
	d.Segments = append(keep, &Segment{Start: a, End: b - 1, Pos: a})
}

// pieceCounts is how many pieces passed their check, and how many arrived damaged this run (the
// progress window). Caller holds m.mu.
func (d *Download) pieceCounts() (ok, bad int) {
	d.mu.Lock()
	for _, x := range d.PieceOK {
		ok += bits.OnesCount8(x)
	}
	pc := d.pc
	d.mu.Unlock()
	if pc != nil {
		pc.mu.Lock()
		bad = pc.found
		pc.mu.Unlock()
	}
	return ok, bad
}

// checkPieces checks the pieces of a finished part file that weren't checked while it downloaded
// (a restart found them complete, or the checksums came late, from a metalink the server pointed
// to, or the server can't send ranges). Damaged ones are marked to be fetched again: errRepair.
// From a server that can't send just those, the download fails instead of passing as complete.
// Caller holds no locks.
func (m *Manager) checkPieces(ctx context.Context, d *Download) error {
	m.mu.Lock()
	ph, size, ranges := d.Pieces, d.Size, d.Resumable
	ok := piecesFit(ph, size) && d.Kind == ""
	whole := d.Check != nil && d.Check.Want != ""
	m.mu.Unlock()
	if !ok {
		return nil
	}
	n := len(ph.Hashes)
	d.mu.Lock()
	if len(d.PieceOK) != len(newBitfield(n)) || !ranges { // (a file sent whole again has nothing checked)
		d.PieceOK = newBitfield(n)
	}
	var todo []int
	for i := 0; i < n; i++ {
		if !d.PieceOK.has(i) {
			todo = append(todo, i)
		}
	}
	d.mu.Unlock()
	if len(todo) == 0 || whole && ranges && len(todo) == n {
		return nil // (none checked yet, as when the checksums came late: the whole file's check finds the damaged ones)
	}
	f, err := os.Open(d.partPath())
	if err != nil {
		return err
	}
	defer f.Close()
	m.mu.Lock()
	d.stage, d.stagePct = "Checking pieces", 0
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		d.stage, d.stagePct = "", 0
		m.mu.Unlock()
	}()
	bad, last := 0, time.Now()
	for k, i := range todo {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		a, b := int64(i)*ph.Length, min(int64(i+1)*ph.Length, size)
		good, err := pieceMatches(f, ph, i, a, b)
		if err != nil {
			return err
		}
		d.mu.Lock()
		switch {
		case good:
			d.PieceOK.set(i)
		case ranges:
			d.unmark(a, b)
		}
		d.mu.Unlock()
		if !good {
			bad++
		}
		if !good && ranges {
			atomic.AddInt64(&d.done, -(b - a))
		}
		if time.Since(last) > 150*time.Millisecond {
			last = time.Now()
			m.mu.Lock()
			d.stagePct = (k + 1) * 100 / len(todo)
			m.mu.Unlock()
		}
	}
	if bad == 0 {
		return nil
	}
	if !ranges {
		what := fmt.Sprintf("%d pieces of the file don't", bad)
		if bad == 1 {
			what = "a piece of the file doesn't"
		}
		return fmt.Errorf("%s match the metalink's checksums, and the server can't send parts of the file again — use Redownload", what)
	}
	m.mu.Lock()
	logf("%s: %d of the pieces not checked while downloading are damaged: fetching them again", d.FileName, bad)
	d.Note = fmt.Sprintf("%d damaged piece%s downloaded again", bad, map[bool]string{true: "", false: "s"}[bad == 1])
	m.dirty = true
	m.mu.Unlock()
	return errRepair
}
