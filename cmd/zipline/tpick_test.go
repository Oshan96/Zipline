package main

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zipline/internal/bencode"
)

// pickTorrent is a torrent engine with no network: three files of 10, 10 and 10 pieces (16 KiB
// pieces: one block each, so a request is a piece). data are its bytes.
func pickTorrent(t *testing.T, files ...int) (*torrent, []byte) {
	if len(files) == 0 {
		files = []int{10, 10, 10}
	}
	const pl = blockSize
	var list []any
	total := 0
	for i, n := range files {
		list = append(list, map[string]any{"length": int64(n * pl), "path": []any{fmt.Sprintf("f%d.bin", i)}})
		total += n * pl
	}
	data := randData(total)
	var hashes []byte
	for off := 0; off < total; off += pl {
		h := sha1.Sum(data[off : off+pl])
		hashes = append(hashes, h[:]...)
	}
	mi, err := parseInfo(bencode.Encode(map[string]any{"name": "pick", "piece length": int64(pl), "pieces": string(hashes), "files": list}))
	if err != nil {
		t.Fatal(err)
	}
	m := torrentManager(t)
	d := &Download{ID: "pick", Kind: "torrent", Dir: t.TempDir(), Torrent: &TorrentState{InfoHash: mi.HashHex()}}
	tr := m.newTorrent(d, mi.InfoHash, nil)
	t.Cleanup(func() {
		tr.mu.Lock()
		tr.freePieces()
		for _, p := range tr.peers {
			tr.dropPeer(p)
		}
		tr.mu.Unlock()
		tr.cancel()
		tr.st.Close()
	})
	tr.mu.Lock()
	tr.setMeta(mi, nil, nil)
	tr.mu.Unlock()
	return tr, data
}

// fakePeer is a connected peer that has the given pieces (all of them when none are given); what
// we send it piles up unsent. It's choking us, so only the test asks it for blocks (next).
func fakePeer(tr *torrent, port int, pieces ...int) *tpeer {
	p := &tpeer{t: tr, addr: netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), uint16(port)), pending: map[blockKey]time.Time{},
		maxPending: 250, reqq: 250, ext: map[string]int{}, metaAsked: -1, amInterested: true, peerChoking: true}
	p.wcond = sync.NewCond(&p.wmu)
	n := tr.mi.NumPieces()
	p.have = newBitfield(n)
	if len(pieces) == 0 {
		for i := 0; i < n; i++ {
			pieces = append(pieces, i)
		}
	}
	for _, i := range pieces {
		p.have.set(i)
		p.haveCount++
		tr.avail[i]++
	}
	peerConns.Add(1) // (dropPeer gives it back)
	tr.peers[p.addr] = p
	return p
}

// next asks the picker for a peer's next block and marks it asked, as fillRequests does.
func next(tr *torrent, p *tpeer) (int, bool) {
	k, ok := tr.nextBlock(p)
	if ok {
		tr.pieces[k.piece].markAsked(k.begin / blockSize)
		p.pending[k] = time.Now()
	}
	return k.piece, ok
}

func TestPickerFilePriorities(t *testing.T) {
	tr, _ := pickTorrent(t)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.setPick([]int8{-1, 1, 0}, nil, nil) // f0 low, f1 high, f2 normal
	p := fakePeer(tr, 1)
	var got []int
	for {
		i, ok := next(tr, p)
		if !ok {
			break
		}
		got = append(got, i)
	}
	if len(got) != 30 {
		t.Fatalf("asked for %d pieces, want 30", len(got))
	}
	for k, i := range got {
		want := []int{1, 2, 0}[k/10] // high, then normal, then low
		if i/10 != want {
			t.Fatalf("pick %d was piece %d (file %d), want file %d: %v", k, i, i/10, want, got)
		}
	}
	// Skip: a skipped file's pieces aren't asked for
	tr2, _ := pickTorrent(t)
	tr2.mu.Lock()
	defer tr2.mu.Unlock()
	tr2.applySkip([]bool{false, true, false})
	p2 := fakePeer(tr2, 2)
	for {
		i, ok := next(tr2, p2)
		if !ok {
			break
		}
		if i/10 == 1 {
			t.Fatalf("piece %d of a skipped file was asked for", i)
		}
	}
}

func TestPickerInOrder(t *testing.T) {
	tr, _ := pickTorrent(t, 40)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	on := true
	tr.setPick(nil, &on, nil)
	p := fakePeer(tr, 1)
	tr.avail[2]-- // rarer: the first of the next few to go
	var got []int
	for len(got) < 12 {
		i, _ := next(tr, p)
		got = append(got, i)
	}
	if got[0] != 2 {
		t.Fatalf("the rarest of the next few pieces should go first: %v", got)
	}
	for k, i := range got[1:] {
		if i > k+seqSpread { // never far ahead of the earliest missing piece
			t.Fatalf("pick %d was piece %d: %v", k+1, i, got)
		}
	}
	// pieces this peer has, still in order
	q := fakePeer(tr, 2, 30, 20, 25)
	if i, _ := next(tr, q); i != 20 {
		t.Fatalf("a peer with only later pieces: got %d, want 20", i)
	}
}

func TestPickerFirstAndLastPieces(t *testing.T) {
	tr, _ := pickTorrent(t, 150, 20, 20) // f0: 2 pieces at each end (1% of 150 pieces, rounded up), f1, f2: 1
	tr.mu.Lock()
	defer tr.mu.Unlock()
	on := true
	tr.setPick(nil, nil, &on)
	p := fakePeer(tr, 1)
	want := map[int]bool{0: true, 1: true, 148: true, 149: true, 150: true, 169: true, 170: true, 189: true}
	for k, n := 0, len(want); k < n; k++ {
		i, _ := next(tr, p)
		if !want[i] {
			t.Fatalf("pick %d was piece %d, want one of each file's first and last pieces", k, i)
		}
		delete(want, i)
	}
	// with "Download in order" as well: the ends first, then from the start
	tr.setPick(nil, &on, nil)
	if i, _ := next(tr, p); i != 2 {
		t.Fatalf("after the ends, in order: got %d, want 2", i)
	}
}

func TestPickerStrictEndgame(t *testing.T) {
	tr, data := pickTorrent(t, 4)
	tr.mu.Lock()
	a := fakePeer(tr, 1)
	var started []int
	for k := 0; k < 2; k++ {
		i, _ := next(tr, a) // a is asked for two of the four pieces
		started = append(started, i)
	}
	b := fakePeer(tr, 2, started...) // b has just those two
	// pieces nobody has started are left (b doesn't have them): not the endgame, b gets nothing
	if i, ok := next(tr, b); ok {
		tr.mu.Unlock()
		t.Fatalf("b was asked for piece %d, which a is sending, while others were left untouched", i)
	}
	for k := 0; k < 4; k++ {
		next(tr, a)
	}
	// now every piece is asked for: b may ask for what it has, once more
	i, ok := next(tr, b)
	if !ok || i != started[0] && i != started[1] {
		tr.mu.Unlock()
		t.Fatalf("endgame: b got %d %v, has %v", i, ok, started)
	}
	if _, ok := next(tr, b); !ok {
		tr.mu.Unlock()
		t.Fatal("endgame: b should get the other piece it has too")
	}
	if _, ok := next(tr, b); ok {
		tr.mu.Unlock()
		t.Fatal("endgame: nothing more for b (each block twice at most)")
	}
	tr.mu.Unlock()
	// a sends piece i first: b is told it needn't (CANCEL), and its copy isn't counted again
	before := atomic.LoadInt64(&tr.d.done)
	tr.gotBlock(a, i, 0, data[i*blockSize:(i+1)*blockSize])
	b.wmu.Lock()
	cancelled := false
	for _, m := range b.outq {
		if len(m) == 17 && m[4] == msgCancel && int(binary.BigEndian.Uint32(m[5:])) == i {
			cancelled = true
		}
	}
	b.wmu.Unlock()
	if !cancelled {
		t.Fatal("no CANCEL for the endgame copy")
	}
	tr.gotBlock(b, i, 0, data[i*blockSize:(i+1)*blockSize])
	if got := atomic.LoadInt64(&tr.d.done) - before; got != blockSize {
		t.Fatalf("progress grew by %d, want one block (%d)", got, blockSize)
	}
	waitHave(t, tr, i)
	if got := atomic.LoadInt64(&tr.downloaded); got != blockSize {
		t.Fatalf("downloaded %d, want %d (the second copy doesn't count)", got, blockSize)
	}
}

func waitHave(t *testing.T, tr *torrent, i int) {
	t.Helper()
	for dl := time.Now().Add(5 * time.Second); time.Now().Before(dl); time.Sleep(5 * time.Millisecond) {
		tr.mu.Lock()
		ok := tr.have.has(i)
		tr.mu.Unlock()
		if ok {
			return
		}
	}
	t.Fatalf("piece %d never got written", i)
}

// When the piece memory is used up, peers finish the pieces already started and start no more.
func TestPieceMemoryIsBounded(t *testing.T) {
	tr, data := pickTorrent(t, 6)
	tr.mu.Lock()
	p := fakePeer(tr, 1)
	first, _ := next(tr, p)
	hold := pieceMemory - memUsed.Load() // everything else is taken
	memUsed.Add(hold)
	defer memUsed.Add(-hold)
	if _, ok := next(tr, p); ok {
		tr.mu.Unlock()
		t.Fatal("a new piece was started with no piece memory left")
	}
	tr.mu.Unlock()
	tr.gotBlock(p, first, 0, data[first*blockSize:(first+1)*blockSize])
	waitHave(t, tr, first)
	if memUsed.Load() > pieceMemory {
		t.Fatalf("piece memory %d over the limit", memUsed.Load())
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.memHeld != 0 {
		t.Fatalf("a written piece still holds %d bytes", tr.memHeld)
	}
	if _, ok := next(tr, p); !ok { // its memory came back: the next piece can start
		t.Fatal("no new piece after the first one's memory came back")
	}
}

// A damaged piece is thrown away (its bytes stop counting) and fetched again.
func TestDamagedPieceIsFetchedAgain(t *testing.T) {
	tr, data := pickTorrent(t, 3)
	tr.mu.Lock()
	p := fakePeer(tr, 1)
	i, _ := next(tr, p)
	tr.mu.Unlock()
	junk := randData(blockSize)
	tr.gotBlock(p, i, 0, junk)
	for dl := time.Now().Add(5 * time.Second); atomic.LoadInt64(&tr.d.done) != 0 && time.Now().Before(dl); {
		time.Sleep(5 * time.Millisecond)
	}
	tr.mu.Lock()
	if atomic.LoadInt64(&tr.d.done) != 0 || tr.have.has(i) || tr.pieces[i] != nil || p.badPieces != 1 {
		tr.mu.Unlock()
		t.Fatalf("after a damaged piece: done %d, had %v, in progress %v, bad %d", atomic.LoadInt64(&tr.d.done), tr.have.has(i), tr.pieces[i] != nil, p.badPieces)
	}
	j, ok := next(tr, p)
	tr.mu.Unlock()
	if !ok {
		t.Fatal("nothing asked for after a damaged piece")
	}
	tr.gotBlock(p, j, 0, data[j*blockSize:(j+1)*blockSize])
	waitHave(t, tr, j)
}

func TestConnectionCap(t *testing.T) {
	base := peerConns.Load()
	n := 0
	defer func() { peerConns.Add(int32(-n)) }()
	for takeConn() {
		n++
		if n > 1000 {
			t.Fatal("no cap")
		}
	}
	if int(base)+n != maxConns || connRoom() != 0 {
		t.Fatalf("took %d (with %d already), want %d in all", n, base, maxConns)
	}
}

func TestRequestQueueFollowsRate(t *testing.T) {
	tr, _ := pickTorrent(t, 2)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	p := fakePeer(tr, 1)
	p.maxPending, p.peerChoking = 4, false
	for k := 0; k < 20; k++ { // slow start: a block more for each block
		p.gotFrom()
	}
	if p.maxPending != 24 {
		t.Fatalf("slow start: %d", p.maxPending)
	}
	p.down.rate = 1 << 20 // 1 MiB/s, climbing
	tr.sizeQueue(p)
	p.down.rate = 1 << 20 // no longer climbing: about 3 s of it from now on
	tr.sizeQueue(p)
	if !p.q.steady || p.maxPending != 3<<20/blockSize {
		t.Fatalf("steady %v, queue %d, want %d", p.q.steady, p.maxPending, 3<<20/blockSize)
	}
	p.down.rate = 100 << 20
	tr.sizeQueue(p)
	if p.maxPending != 250 {
		t.Fatalf("fast peer: %d, want 250", p.maxPending)
	}
	p.down.rate = 0
	tr.sizeQueue(p)
	if p.maxPending != 2 {
		t.Fatalf("idle peer: %d, want 2", p.maxPending)
	}
}
