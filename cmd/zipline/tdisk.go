package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Pieces are put together in memory as their blocks arrive, checked against their SHA-1 there and
// written whole, at the right offsets across file boundaries, by a write queue that every torrent
// shares (libtorrent's way): a few large writes instead of one per 16 KiB block, and nothing read
// back from disk except to check files or to share them. The memory for pieces being put
// together or waiting to be written is bounded across torrents (pieceMemory): when it's used up,
// peers finish the pieces already started instead of starting new ones.

const pieceMemory = 96 << 20

func pieceMemoryLimit() int64 { return pieceMemory }

var (
	memUsed  atomic.Int64 // piece memory reserved, all torrents together
	writeQ   = make(chan writeJob, 8192)
	writerGo sync.Once
	bufPools sync.Map // piece size → *sync.Pool of piece buffers
)

type writeJob struct {
	t     *torrent
	st    *tstorage
	piece int
	ps    *pieceState
}

// memReserve claims memory for a piece (false when it's used up; the first piece always fits).
func memReserve(n int64) bool {
	for {
		u := memUsed.Load()
		if u > 0 && u+n > pieceMemory {
			return false
		}
		if memUsed.CompareAndSwap(u, u+n) {
			return true
		}
	}
}

func pieceBuf(n int64) []byte {
	pl, ok := bufPools.Load(n)
	if !ok {
		pl, _ = bufPools.LoadOrStore(n, &sync.Pool{New: func() any { return make([]byte, n) }})
	}
	return pl.(*sync.Pool).Get().([]byte)
}

func freePieceBuf(b []byte) {
	if pl, ok := bufPools.Load(int64(len(b))); ok {
		pl.(*sync.Pool).Put(b) // (reused by the next piece of the same size)
	}
}

// newPiece starts a piece (nil when there's no piece memory left). Caller holds t.mu.
func (t *torrent) newPiece(i int) *pieceState {
	size := t.mi.PieceSize(i)
	if t.ctx.Err() != nil || !memReserve(size) {
		return nil
	}
	n := int((size + blockSize - 1) / blockSize)
	ps := &pieceState{blocks: make([]byte, n), asked: make([]int8, n), from: map[*tpeer]bool{}, free: n, mem: size, last: time.Now()}
	t.pieces[i] = ps
	t.memHeld += size
	return ps
}

// releasePiece gives a piece's memory back. Caller holds t.mu.
func (t *torrent) releasePiece(ps *pieceState) {
	if ps.buf != nil {
		freePieceBuf(ps.buf)
		ps.buf = nil
	}
	if ps.mem > 0 {
		memUsed.Add(-ps.mem)
		t.memHeld -= ps.mem
		ps.mem = 0
	}
}

// dropPiece forgets a piece in progress: what it got so far no longer counts. Caller holds t.mu.
func (t *torrent) dropPiece(i int) {
	ps := t.pieces[i]
	if ps == nil {
		return
	}
	delete(t.pieces, i)
	atomic.AddInt64(&t.d.done, -ps.counted)
	ps.counted = 0
	for _, p := range t.peers {
		for b := range ps.blocks {
			delete(p.pending, blockKey{i, b * blockSize})
		}
	}
	t.releasePiece(ps)
}

// dropPartials forgets every piece still being put together (the whole ones are checked and
// written as usual). Caller holds t.mu.
func (t *torrent) dropPartials(unwantedOnly bool) {
	for i, ps := range t.pieces {
		if !ps.checking && (!unwantedOnly || !t.wanted[i]) {
			t.dropPiece(i)
		}
	}
}

// partialDone is how much of the pieces in progress has arrived (counted in the download's
// progress until they're checked). Caller holds t.mu.
func (t *torrent) partialDone() (n int64) {
	for _, ps := range t.pieces {
		n += ps.counted
	}
	return n
}

// tidyPieces drops pieces nobody has sent anything for in a while (the peers that had them left),
// so their memory goes to pieces that can be finished. Caller holds t.mu.
func (t *torrent) tidyPieces(now time.Time) {
	if memUsed.Load() < pieceMemory*3/4 {
		return
	}
	for i, ps := range t.pieces {
		if !ps.checking && !ps.web && now.Sub(ps.last) > time.Minute && ps.free == len(ps.blocks)-ps.got {
			t.dropPiece(i) // (nothing of it is asked for)
		}
	}
}

// ---------------------------------------------------------------- blocks arriving

// gotBlock puts a block a peer sent into its piece and checks the piece once it's whole.
func (t *torrent) gotBlock(p *tpeer, piece, begin int, data []byte) {
	t.mu.Lock()
	k := blockKey{piece, begin}
	if _, asked := p.pending[k]; asked {
		delete(p.pending, k)
	}
	p.lastUseful = time.Now()
	ps := t.pieces[piece]
	b := begin / blockSize
	if t.mi == nil || ps == nil || ps.checking || ps.web || begin%blockSize != 0 || b >= len(ps.blocks) || ps.blocks[b] == 2 ||
		int64(len(data)) != min(int64(blockSize), t.mi.PieceSize(piece)-int64(begin)) {
		t.fillRequests(p) // (a copy that came second in the endgame, or one we no longer want: counted once)
		t.mu.Unlock()
		return
	}
	if ps.buf == nil {
		ps.buf = pieceBuf(t.mi.PieceSize(piece))
	}
	copy(ps.buf[begin:], data)
	dup := ps.asked[b] > 1 // (the endgame asked someone else too)
	if ps.blocks[b] == 0 {
		ps.free--
	}
	ps.blocks[b] = 2
	ps.got++
	ps.counted += int64(len(data))
	ps.from[p] = true
	ps.last = time.Now()
	whole := ps.got == len(ps.blocks)
	if whole {
		ps.checking = true
		t.writing++
	}
	// endgame: others asked for the same block don't need to send it
	for _, o := range t.peers {
		if dup && o != p {
			if _, ok := o.pending[k]; ok {
				delete(o.pending, k)
				c := make([]byte, 13)
				c[0] = msgCancel
				binary.BigEndian.PutUint32(c[1:], uint32(piece))
				binary.BigEndian.PutUint32(c[5:], uint32(begin))
				binary.BigEndian.PutUint32(c[9:], uint32(len(data)))
				o.sendLocked(c)
			}
		}
	}
	p.gotFrom()
	t.fillRequests(p)
	t.mu.Unlock()

	atomic.AddInt64(&t.d.done, int64(len(data)))
	atomic.AddInt64(&t.downloaded, int64(len(data)))
	t.downRate.add(len(data))
	if whole {
		t.checkPiece(piece)
	}
}

// checkPiece checks a whole piece in memory against its SHA-1 and hands it to the write queue;
// a damaged one is thrown away and its senders blamed. Pieces that aren't whole are left alone.
func (t *torrent) checkPiece(i int) {
	t.mu.Lock()
	ps := t.pieces[i]
	if ps == nil || !ps.checking || ps.claimed || ps.buf == nil || t.mi == nil {
		t.mu.Unlock()
		return
	}
	ps.claimed = true
	mi, st := t.mi, t.st
	t.mu.Unlock()
	h := sha1.Sum(ps.buf)
	if bytes.Equal(h[:], mi.PieceHash(i)) {
		t.queueWrite(writeJob{t: t, st: st, piece: i, ps: ps})
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writing--
	if t.pieces[i] == ps {
		delete(t.pieces, i)
	}
	atomic.AddInt64(&t.d.done, -ps.counted)
	t.releasePiece(ps)
	for p := range ps.from { // someone sent bad data
		p.badPieces++
		if p.badPieces >= 2 || len(ps.from) == 1 && p.badPieces >= 1 && mi.PieceLen >= 1<<20 {
			t.ban(p.addr)
			go p.close()
		}
	}
	for _, p := range t.peers {
		t.fillRequests(p)
	}
}

// webPieceDone takes a piece a web seed fetched whole (its bytes already counted) the way a
// piece from peers goes: false when it's damaged.
func (t *torrent) webPieceDone(i int, ps *pieceState, buf []byte, mi *Metainfo) bool {
	size := int64(len(buf))
	if h := sha1.Sum(buf); !bytes.Equal(h[:], mi.PieceHash(i)) {
		atomic.AddInt64(&t.d.done, -size)
		return false
	}
	t.mu.Lock()
	if t.pieces[i] != ps || t.have.has(i) { // forgotten meanwhile (a check of the files)
		t.mu.Unlock()
		atomic.AddInt64(&t.d.done, -size)
		return true
	}
	ps.buf, ps.counted, ps.checking, ps.claimed = buf, size, true, true
	t.writing++
	st := t.st
	t.mu.Unlock()
	t.queueWrite(writeJob{t: t, st: st, piece: i, ps: ps})
	return true
}

// ---------------------------------------------------------------- the write queue

func (t *torrent) queueWrite(j writeJob) {
	writerGo.Do(func() {
		for k := 0; k < 2; k++ {
			go writer()
		}
	})
	t.mu.Lock()
	if t.ctx.Err() != nil { // stopped: what isn't queued yet is dropped
		t.writing--
		if t.pieces[j.piece] == j.ps {
			delete(t.pieces, j.piece)
		}
		atomic.AddInt64(&t.d.done, -j.ps.counted)
		t.releasePiece(j.ps)
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	writeQ <- j
}

func writer() {
	for j := range writeQ {
		off := int64(j.piece) * j.st.mi.PieceLen
		err := j.st.WriteAt(j.ps.buf, off)
		j.t.pieceWritten(j, err)
	}
}

// pieceWritten: a checked piece is on disk, so it's had (and shared) from now on.
func (t *torrent) pieceWritten(j writeJob, err error) {
	t.mu.Lock()
	i, ps := j.piece, j.ps
	t.writing--
	if t.pieces[i] == ps {
		delete(t.pieces, i)
	}
	t.releasePiece(ps)
	t.markWritten(i)
	if err != nil {
		atomic.AddInt64(&t.d.done, -ps.counted)
		t.mu.Unlock()
		t.fail(fmt.Errorf("couldn't write to disk: %w", err))
		return
	}
	defer t.mu.Unlock()
	if t.have.has(i) { // (a check of the files found it meanwhile)
		atomic.AddInt64(&t.d.done, -ps.counted)
		return
	}
	t.have.set(i)
	t.wrote = true
	atomic.AddInt64(&t.d.done, t.wantBytes[i]-ps.counted) // (it may have stopped being wanted meanwhile)
	have := make([]byte, 5)
	have[0] = msgHave
	binary.BigEndian.PutUint32(have[1:], uint32(i))
	for _, p := range t.peers {
		if !p.have.has(i) {
			p.sendLocked(have)
		}
		t.updateInterest(p)
	}
	t.checkComplete()
}

// drainWrites waits until this torrent's checked pieces are all on disk.
func (t *torrent) drainWrites() {
	for dl := time.Now().Add(2 * time.Minute); time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
		t.mu.Lock()
		n := t.writing
		t.mu.Unlock()
		if n <= 0 {
			return
		}
	}
}

// freePieces gives back the memory of a stopped torrent's pieces. Caller holds t.mu.
func (t *torrent) freePieces() {
	for i, ps := range t.pieces {
		if !ps.checking {
			delete(t.pieces, i)
			atomic.AddInt64(&t.d.done, -ps.counted)
			t.releasePiece(ps)
		}
	}
}
