package main

import (
	"errors"
	"math"
	mrand "math/rand/v2"
	"sync/atomic"
)

// Piece picking: which block to ask a peer for next. Pieces already started come first (so
// they finish and free their memory), then a new piece: the most important files' pieces first
// (file priorities, and each file's first and last pieces when asked), then either in order
// ("Download in order", for watching while it downloads) or rarest first. When every block
// left has been asked for, the endgame asks a second peer for the slow ones and cancels the
// spare request as soon as one copy arrives (libtorrent's strict endgame).

// Piece priorities (tx.prio): 0 = not wanted.
const (
	prioLow    = 1
	prioNormal = 2
	prioHigh   = 3
	prioEnds   = 4 // a file's first and last pieces, with "First and last pieces first"

	seqSpread = 4   // downloading in order: the rarest of the next few pieces, so one slow peer doesn't hold up the rest
	maxConns  = 250 // connections to peers, all torrents together (each has at most Options' "peers per torrent")
)

// tx is a torrent's piece picking, piece memory and checking state (tpick.go, tdisk.go,
// trecheck.go). Guarded by t.mu unless noted.
type tx struct {
	prio    []int8 // per piece (see prioLow…)
	fprio   []int8 // per file: -1 low, 0 normal, 1 high (TorrentState.Prio)
	skip    []bool // per file, as last applied
	seq     bool   // download in order
	ends    bool   // each file's first and last pieces first
	seqFrom int    // downloading in order: no piece before this one is still missing

	memHeld int64 // piece memory this torrent holds (tdisk.go)
	writing int   // whole pieces being checked or waiting to be written
	wrote   bool  // pieces were written since the last check of everything

	checking   bool         // reading pieces back from disk (trecheck.go)
	checkDone  atomic.Int64 // pieces read back so far
	checkTotal int64
	unchecked  []bool      // per file: a check of its pieces hasn't finished (saved as such)
	stampAt    []fileStamp // per file: size and modified time, as last looked at
	stampStale []bool      // per file: written since
}

// peerQ sizes a peer's request queue (sizeQueue).
type peerQ struct {
	steady   bool // past slow start: the queue follows the measured rate
	lastRate float64
	rbuf     []byte // reused for its messages (read loop only)
}

// peerConns counts connections to peers across every torrent (at most maxConns).
var peerConns atomic.Int32

// takeConn claims a connection slot (false when they're all taken).
func takeConn() bool {
	if peerConns.Add(1) > maxConns {
		peerConns.Add(-1)
		return false
	}
	return true
}

// connRoom is how many more connections all torrents together may open.
func connRoom() int { return maxConns - int(peerConns.Load()) }

// msgBuf is where the read loop puts a message of n bytes: a block (the most common message by
// far) reuses the peer's buffer, as nothing keeps a message once it has been handled.
func (p *tpeer) msgBuf(n int) []byte {
	if n > blockSize+16 {
		return make([]byte, n)
	}
	if p.q.rbuf == nil {
		p.q.rbuf = make([]byte, blockSize+16)
	}
	return p.q.rbuf[:n]
}

// sizeQueue keeps about three seconds of a peer's download rate asked for (libtorrent's way): at
// least 2 blocks, at most 250. A new connection starts with a few and gets one more for each block
// it sends (slow start) until its rate stops climbing. Caller holds t.mu.
func (t *torrent) sizeQueue(p *tpeer) {
	r := p.down.rate
	want := int(min(max(r*3/blockSize, 2), 250))
	if p.peerChoking { // when it unchokes us again, start small and grow again
		p.q.steady, p.q.lastRate = false, 0
	}
	if !p.q.steady {
		if r > 0 && r < p.q.lastRate*1.1 {
			p.q.steady = true
		}
		p.q.lastRate = r
		want = max(want, p.maxPending)
	}
	p.maxPending = want
}

// gotFrom grows a peer's queue during slow start. Caller holds t.mu.
func (p *tpeer) gotFrom() {
	if !p.q.steady && p.maxPending < 250 {
		p.maxPending++
	}
}

// ---------------------------------------------------------------- priorities

// filePrio is a file's priority as saved (-1 low, 0 normal, 1 high). Caller holds t.mu.
func (t *torrent) filePrio(i int) int8 {
	if i < len(t.fprio) && len(t.fprio) == len(t.mi.Files) {
		return min(max(t.fprio[i], -1), 1)
	}
	return 0
}

// updatePrio works out each piece's priority from the files it's in: a piece shared by two files
// takes the higher one. Caller holds t.mu.
func (t *torrent) updatePrio(skip []bool) {
	mi := t.mi
	t.skip = skip
	prio := make([]int8, mi.NumPieces())
	for i, f := range mi.Files {
		if f.Pad || f.Length == 0 || i < len(skip) && skip[i] {
			continue
		}
		first, last := int(f.Offset/mi.PieceLen), int((f.Offset+f.Length-1)/mi.PieceLen)
		lvl := prioNormal + t.filePrio(i)
		for p := first; p <= last; p++ {
			prio[p] = max(prio[p], lvl)
		}
		if t.ends { // 1% of the file at each end, at least a piece (what a player reads first: headers, the index)
			k := max(1, int((f.Length/100+mi.PieceLen-1)/mi.PieceLen))
			for j := 0; j < k && first+j <= last; j++ {
				prio[first+j], prio[last-j] = prioEnds, prioEnds
			}
		}
	}
	t.prio = prio
	t.seqFrom = 0
}

// setPick changes the file priorities and picking options; nil leaves one as it is. Caller holds t.mu.
func (t *torrent) setPick(fprio []int8, seq, ends *bool) {
	if fprio != nil {
		t.fprio = append([]int8(nil), fprio...)
	}
	if seq != nil {
		t.seq = *seq
	}
	if ends != nil {
		t.ends = *ends
	}
	if t.mi != nil && t.skip != nil {
		t.updatePrio(t.skip)
		for _, p := range t.peers {
			t.fillRequests(p)
		}
	}
}

// ---------------------------------------------------------------- choosing blocks

// markAsked notes a request for block b of a piece. Caller holds t.mu.
func (ps *pieceState) markAsked(b int) {
	if ps.blocks[b] == 0 {
		ps.free--
	}
	ps.blocks[b] = 1
	ps.asked[b]++
}

// firstFree is the first block nobody has been asked for (-1: none).
func (ps *pieceState) firstFree() int {
	for b := ps.hint; b < len(ps.blocks); b++ {
		if ps.blocks[b] == 0 {
			ps.hint = b
			return b
		}
	}
	ps.hint = len(ps.blocks)
	return -1
}

// startedBefore says whether started piece i should be worked on before piece j. Caller holds t.mu.
func (t *torrent) startedBefore(i int, a *pieceState, j int, b *pieceState) bool {
	if t.prio[i] != t.prio[j] {
		return t.prio[i] > t.prio[j]
	}
	if t.seq || a.got == b.got {
		return i < j
	}
	return a.got > b.got
}

// nextBlock chooses the next block to ask a peer for. Caller holds t.mu.
func (t *torrent) nextBlock(p *tpeer) (blockKey, bool) {
	// 1. pieces already started: the most important first, then the earliest (in order) or the fullest
	best, bi := (*pieceState)(nil), -1
	anyFree := false // a started piece still has blocks nobody was asked for
	for i, ps := range t.pieces {
		if ps.checking || ps.web || ps.free == 0 || t.prio[i] == 0 {
			continue
		}
		anyFree = true
		if p.have.has(i) && (best == nil || t.startedBefore(i, ps, bi, best)) {
			best, bi = ps, i
		}
	}
	if best != nil {
		if b := best.firstFree(); b >= 0 {
			return blockKey{bi, b * blockSize}, true
		}
	}
	// 2. a new piece this peer has
	if i, untouched := t.pickNew(p); i >= 0 {
		if ps := t.newPiece(i); ps != nil {
			return blockKey{i, 0}, true
		}
		return blockKey{}, false // no piece memory left: finish what's started first
	} else if untouched || anyFree {
		return blockKey{}, false // others have what's left: not the endgame yet
	}
	// 3. the endgame: every block left is asked for; ask this peer too (once more per block),
	// the pieces closest to done first
	var eg *pieceState
	ei := -1
	for i, ps := range t.pieces {
		if ps.checking || ps.web || t.prio[i] == 0 || !p.have.has(i) {
			continue
		}
		if eg == nil || t.prio[i] > t.prio[ei] || t.prio[i] == t.prio[ei] && ps.got > eg.got {
			found := false
			for b, st := range ps.blocks {
				if _, mine := p.pending[blockKey{i, b * blockSize}]; st == 1 && ps.asked[b] < 2 && !mine {
					found = true
					break
				}
			}
			if found {
				eg, ei = ps, i
			}
		}
	}
	if eg != nil {
		for b, st := range eg.blocks {
			k := blockKey{ei, b * blockSize}
			if _, mine := p.pending[k]; st == 1 && eg.asked[b] < 2 && !mine {
				return k, true
			}
		}
	}
	return blockKey{}, false
}

// pickNew chooses a piece to start for a peer (-1: none). untouched says whether some wanted
// piece nobody has started is left at all (if so, it isn't the endgame). Caller holds t.mu.
func (t *torrent) pickNew(p *tpeer) (pick int, untouched bool) {
	n := t.mi.NumPieces()
	for t.seqFrom < n && (t.prio[t.seqFrom] == 0 || t.have.has(t.seqFrom)) {
		t.seqFrom++
	}
	top := int8(0) // the highest priority among pieces this peer could start
	for i := t.seqFrom; i < n; i++ {
		if t.prio[i] == 0 || t.have.has(i) || t.pieces[i] != nil {
			continue
		}
		untouched = true
		if p.have.has(i) && t.prio[i] > top {
			top = t.prio[i]
			if top == prioEnds {
				break
			}
		}
	}
	if top == 0 {
		return -1, untouched
	}
	can := func(i int) bool {
		return t.prio[i] == top && !t.have.has(i) && t.pieces[i] == nil && p.have.has(i)
	}
	if t.seq { // in order, but the rarest of the next few (one slow peer doesn't hold up the rest)
		best, seen := -1, 0
		for i := t.seqFrom; i < n && seen < seqSpread; i++ {
			if can(i) {
				seen++
				if best < 0 || t.avail[i] < t.avail[best] {
					best = i
				}
			}
		}
		return best, true
	}
	// rarest first (random among equals)
	best, bestAvail, ties := -1, math.MaxInt, 0
	start := t.seqFrom + mrand.IntN(n-t.seqFrom)
	for j := t.seqFrom; j < n; j++ {
		i := start + j - t.seqFrom
		if i >= n {
			i -= n - t.seqFrom
		}
		if !can(i) {
			continue
		}
		if a := t.avail[i]; a < bestAvail {
			best, bestAvail, ties = i, a, 1
		} else if a == bestAvail {
			ties++
			if mrand.IntN(ties) == 0 {
				best = i
			}
		}
	}
	return best, true
}

// webSeedBetter orders pieces for web seeds: the most important first, then in order or the
// most common (peers have the rare ones). Caller holds t.mu.
func (t *torrent) webSeedBetter(i, j int) bool {
	if t.prio[i] != t.prio[j] {
		return t.prio[i] > t.prio[j]
	}
	if t.seq {
		return i < j
	}
	return t.avail[i] > t.avail[j]
}

// pickState reads a torrent's saved picking options. Caller holds m.mu.
func (ts *TorrentState) pickState() (prio []int8, seq, ends bool) {
	return append([]int8(nil), ts.Prio...), ts.Seq, ts.Ends
}

// SetTorrentPick changes a torrent's file priorities (per file: -1 low, 0 normal, 1 high), "Download
// in order" or "First and last pieces first" (nil leaves one as it is); a running torrent picks
// by them at once.
func (m *Manager) SetTorrentPick(id string, prio []int8, seq, ends *bool) error {
	m.mu.Lock()
	d := m.byID[id]
	if d == nil || d.Torrent == nil {
		m.mu.Unlock()
		return errors.New("not a torrent")
	}
	ts := d.Torrent
	if prio != nil {
		ih, _ := ts.hash()
		if mi := m.loadTorrent(ih); mi == nil || len(prio) != len(mi.Files) {
			m.mu.Unlock()
			return errors.New("the file list isn't known yet")
		}
		ts.Prio = nil
		for _, p := range prio {
			if p != 0 { // (all normal: nothing to keep)
				ts.Prio = append([]int8(nil), prio...)
				break
			}
		}
	}
	if seq != nil {
		ts.Seq = *seq
	}
	if ends != nil {
		ts.Ends = *ends
	}
	m.dirty = true
	t := m.torrents[id]
	m.mu.Unlock()
	if t != nil {
		t.mu.Lock()
		t.setPick(prio, seq, ends)
		t.mu.Unlock()
	}
	return nil
}
