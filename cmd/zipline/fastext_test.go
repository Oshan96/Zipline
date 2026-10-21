package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"zipline/internal/bencode"
)

// BEP 6's own examples of the allowed-fast set.
func TestAllowedFastSetVectors(t *testing.T) {
	ih, _ := hex.DecodeString("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	ip := netip.MustParseAddr("80.4.4.200")
	if got := fmt.Sprint(allowedFastSet(ip, [20]byte(ih), 1313, 7)); got != "[1059 431 808 1217 287 376 1188]" {
		t.Errorf("k=7: %s", got)
	}
	if got := fmt.Sprint(allowedFastSet(ip, [20]byte(ih), 1313, 9)); got != "[1059 431 808 1217 287 376 1188 353 508]" {
		t.Errorf("k=9: %s", got)
	}
	if got := fmt.Sprint(allowedFastSet(netip.MustParseAddr("80.4.4.1"), [20]byte(ih), 1313, 7)); got != "[1059 431 808 1217 287 376 1188]" {
		t.Errorf("the same /24 gets the same set: %s", got)
	}
	if got := allowedFastSet(ip, [20]byte(ih), 3, 10); len(got) != 3 {
		t.Errorf("a small torrent: every piece, once: %v", got)
	}
	if got := allowedFastSet(netip.MustParseAddr("2001:db8::1"), [20]byte(ih), 1313, 10); len(got) != 10 {
		t.Errorf("IPv6: %v", got)
	}
}

// memTorrent is a single-file torrent made in memory.
type memTorrent struct {
	data []byte
	mi   *Metainfo
	raw  []byte
}

func newMemTorrent(t *testing.T, size, plen int) *memTorrent {
	data := randData(size)
	var hashes []byte
	for off := 0; off < size; off += plen {
		h := sha1.Sum(data[off:min(off+plen, size)])
		hashes = append(hashes, h[:]...)
	}
	info := bencode.Encode(map[string]any{"name": "fast.bin", "piece length": int64(plen), "pieces": string(hashes), "length": int64(size)})
	raw := bencode.Encode(map[string]any{"info": bencode.Raw(info)})
	mi, err := parseTorrent(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &memTorrent{data: data, mi: mi, raw: raw}
}

func (mt *memTorrent) block(k blockKey, n int) []byte {
	off := k.piece*int(mt.mi.PieceLen) + k.begin
	return mt.data[off : off+n]
}

// wirePeer is the far end of a connection, driven by the test.
type wirePeer struct {
	t *testing.T
	c net.Conn
}

func (w *wirePeer) send(msg []byte) {
	w.c.Write(binary.BigEndian.AppendUint32(nil, uint32(len(msg))))
	w.c.Write(msg)
}

func (w *wirePeer) read() (byte, []byte) {
	w.t.Helper()
	for {
		var lb [4]byte
		w.c.SetReadDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.ReadFull(w.c, lb[:]); err != nil {
			w.t.Fatalf("reading from Zipline: %v", err)
		}
		n := binary.BigEndian.Uint32(lb[:])
		if n == 0 {
			continue
		}
		m := make([]byte, n)
		if _, err := io.ReadFull(w.c, m); err != nil {
			w.t.Fatalf("reading from Zipline: %v", err)
		}
		if m[0] == msgExtended {
			continue // (the extension handshake: not what these tests look at)
		}
		return m[0], m[1:]
	}
}

func readBlockKey(pl []byte) (blockKey, int) {
	return blockKey{int(binary.BigEndian.Uint32(pl)), int(binary.BigEndian.Uint32(pl[4:]))}, int(binary.BigEndian.Uint32(pl[8:]))
}

// listenWire accepts one connection, swaps plain handshakes (with reserved as our last
// reserved byte) and hands it to the test.
func listenWire(t *testing.T, ih [20]byte, reserved byte) (netip.AddrPort, chan *wirePeer, chan [68]byte) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch, hs := make(chan *wirePeer, 1), make(chan [68]byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { c.Close() })
		var h [68]byte
		if _, err := io.ReadFull(c, h[:]); err != nil {
			return
		}
		hs <- h
		var id [20]byte
		copy(id[:], "-XX0001-fastfastfast")
		mine := handshakeBytes(ih, id)
		mine[27] = reserved
		c.Write(mine)
		ch <- &wirePeer{t: t, c: c}
	}()
	ap, _ := netip.ParseAddrPort(ln.Addr().String())
	return ap, ch, hs
}

func (tr *torrent) peerAt(t *testing.T, ok func(p *tpeer) bool) *tpeer {
	t.Helper()
	for i := 0; i < 500; i++ {
		tr.mu.Lock()
		for _, p := range tr.peers {
			if ok(p) {
				tr.mu.Unlock()
				return p
			}
		}
		tr.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no such peer")
	return nil
}

// With the fast extension a choke keeps our requests: the peer rejects some, only those are
// asked again, and pieces it allows while choked are fetched anyway.
func TestFastExtensionChokeAndReject(t *testing.T) {
	mt := newMemTorrent(t, 20*32<<10, 32<<10) // 20 pieces of two blocks
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.Enc = "plain"
	m.settings.Torrent.SeedRatio = 0
	m.rememberTorrent(mt.mi, mt.raw)
	m.mu.Unlock()
	addr, wires, hs := listenWire(t, mt.mi.InfoHash, 0x04)
	d, err := m.AddTorrent(TorrentAdd{Hash: mt.mi.HashHex(), Dir: filepath.Join(m.settings.DownloadDir, "got"), Start: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := runningTorrent(t, m, d.ID)
	tr.addCands([]netip.AddrPort{addr}, "test")
	h := <-hs
	if h[27]&0x05 != 0x05 {
		t.Fatalf("Zipline should offer the fast extension and the DHT: %x", h[20:28])
	}
	w := <-wires
	if id, _ := w.read(); id != msgHaveNone {
		t.Fatalf("with nothing yet and the fast extension, Have None comes first (got %d)", id)
	}
	w.send([]byte{msgHaveAll})
	if id, _ := w.read(); id != msgInterested {
		t.Fatalf("Zipline should be interested in a seed (got %d)", id)
	}
	w.send([]byte{msgUnchoke})
	type req struct {
		k blockKey
		n int
	}
	var asked []req
	for len(asked) < 4 { // (a new peer's queue starts at 4 requests: tpick.go)
		if id, pl := w.read(); id == msgRequest {
			k, n := readBlockKey(pl)
			asked = append(asked, req{k, n})
		}
	}
	w.send([]byte{msgChoke})
	for _, r := range asked[:3] {
		w.send(blockMsg(msgReject, r.k, r.n))
	}
	p := tr.peerAt(t, func(p *tpeer) bool { return p.x.fast })
	for i := 0; i < 200; i++ {
		tr.mu.Lock()
		n := p.x.rejected
		tr.mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tr.mu.Lock()
	if p.x.rejected != 3 || !p.peerChoking {
		t.Fatalf("rejected %d, choking %v", p.x.rejected, p.peerChoking)
	}
	for i, r := range asked {
		_, pending := p.pending[r.k]
		state := tr.pieces[r.k.piece].blocks[r.k.begin/blockSize]
		if i < 3 && (pending || state != 0) || i >= 3 && (!pending || state != 1) {
			t.Errorf("request %d (%v): pending %v, block state %d", i, r.k, pending, state)
		}
	}
	before := map[blockKey]int{} // asked before the choke (some still on their way to us)
	for k := range p.pending {
		before[k] = int(min(int64(blockSize), mt.mi.PieceSize(k.piece)-int64(k.begin)))
	}
	allowedPiece := -1
	for i := mt.mi.NumPieces() - 1; i >= 0 && allowedPiece < 0; i-- {
		if tr.pieces[i] == nil {
			allowedPiece = i
		}
	}
	tr.mu.Unlock()
	// a piece allowed while choked
	w.send(pieceMsg(msgAllowedFast, allowedPiece))
	var fast []req
	for len(fast) < 2 {
		id, pl := w.read()
		if id != msgRequest {
			continue
		}
		k, n := readBlockKey(pl)
		if _, old := before[k]; old {
			continue
		}
		if k.piece != allowedPiece {
			t.Fatalf("while choked, only the allowed-fast piece may be asked for (got %v)", k)
		}
		fast = append(fast, req{k, n})
	}
	for _, r := range fast {
		w.send(append(blockMsg(msgPiece, r.k, 0)[:9], mt.block(r.k, r.n)...))
	}
	for i := 0; i < 300; i++ {
		tr.mu.Lock()
		got := tr.have.has(allowedPiece)
		tr.mu.Unlock()
		if got {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tr.mu.Lock()
	if !tr.have.has(allowedPiece) {
		t.Fatal("the allowed-fast piece should be in")
	}
	tr.mu.Unlock()
	// unchoked again: the requests still pending are served, and the rest
	go func() {
		w.send([]byte{msgUnchoke})
		for k, n := range before {
			w.send(append(blockMsg(msgPiece, k, 0)[:9], mt.block(k, n)...))
		}
		for {
			var lb [4]byte
			if _, err := io.ReadFull(w.c, lb[:]); err != nil {
				return
			}
			msg := make([]byte, binary.BigEndian.Uint32(lb[:]))
			if _, err := io.ReadFull(w.c, msg); err != nil {
				return
			}
			if len(msg) == 13 && msg[0] == msgRequest {
				k, n := readBlockKey(msg[1:])
				w.send(append(blockMsg(msgPiece, k, 0)[:9], mt.block(k, n)...))
			}
		}
	}()
	waitFor(t, m, d, StCompleted, 30*time.Second)
	got, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, "got", "fast.bin"))
	if err != nil || !bytes.Equal(got, mt.data) {
		t.Fatalf("the file: %v", err)
	}
}

// Zipline sharing to a fast-extension peer: Have All, its allowed-fast set served while
// choked, Reject for anything else, and for requests a choke or a cancel drops.
func TestFastExtensionServing(t *testing.T) {
	mt := newMemTorrent(t, 30*16<<10, 16<<10) // 30 one-block pieces
	m := torrentManager(t)
	dir := filepath.Join(m.settings.DownloadDir, "got")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "fast.bin"), mt.data, 0o644); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.settings.Torrent.Enc = "plain"
	m.rememberTorrent(mt.mi, mt.raw)
	m.mu.Unlock()
	d, err := m.AddTorrent(TorrentAdd{Hash: mt.mi.HashHex(), Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	all := newBitfield(mt.mi.NumPieces())
	for i := 0; i < mt.mi.NumPieces(); i++ {
		all.set(i)
	}
	d.Torrent.Have = all
	m.mu.Unlock()
	m.Start([]string{d.ID})
	tr := runningTorrent(t, m, d.ID)
	addr, wires, _ := listenWire(t, mt.mi.InfoHash, 0x04)
	for i := 0; i < 300; i++ { // sharing now
		tr.mu.Lock()
		seeding := tr.seeding
		tr.mu.Unlock()
		if seeding {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	tr.addCands([]netip.AddrPort{addr}, "test")
	w := <-wires
	if id, _ := w.read(); id != msgHaveAll {
		t.Fatalf("a seed with the fast extension says Have All (got %d)", id)
	}
	want := allowedFastSet(netip.MustParseAddr("127.0.0.1"), mt.mi.InfoHash, mt.mi.NumPieces(), allowedFastK)
	allowed := map[int]bool{}
	for len(allowed) < len(want) {
		id, pl := w.read()
		if id != msgAllowedFast {
			t.Fatalf("expected the allowed-fast set, got message %d", id)
		}
		allowed[int(binary.BigEndian.Uint32(pl))] = true
	}
	for _, i := range want {
		if !allowed[i] {
			t.Fatalf("allowed fast %v, want %v", allowed, want)
		}
	}
	other := 0
	for allowed[other] {
		other++
	}
	// choked: a piece outside the set is rejected, one in it is sent
	w.send(blockMsg(msgRequest, blockKey{other, 0}, blockSize))
	w.send(blockMsg(msgRequest, blockKey{want[0], 0}, blockSize))
	gotReject, gotPiece := false, false
	for !gotReject || !gotPiece {
		id, pl := w.read()
		switch id {
		case msgReject:
			if k, _ := readBlockKey(pl); k.piece != other {
				t.Fatalf("rejected %v", k)
			}
			gotReject = true
		case msgPiece:
			if k := (blockKey{int(binary.BigEndian.Uint32(pl)), int(binary.BigEndian.Uint32(pl[4:]))}); k.piece != want[0] || !bytes.Equal(pl[8:], mt.block(k, blockSize)) {
				t.Fatalf("sent %v", k)
			}
			gotPiece = true
		case msgUnchoke:
			t.Fatal("not interested yet: no reason to unchoke")
		}
	}
	// interested: unchoked, then a choke rejects what's still queued (but not allowed-fast requests)
	w.send([]byte{msgInterested})
	for id, _ := w.read(); id != msgUnchoke; id, _ = w.read() {
	}
	p := tr.peerAt(t, func(p *tpeer) bool { return p.x.fast })
	tr.mu.Lock()
	tr.choke(p)
	tr.mu.Unlock()
	if id, _ := w.read(); id != msgChoke {
		t.Fatalf("expected the choke, got %d", id)
	}
	// a cancel of a queued request gets a Reject too (pieces after a choke: none but allowed fast)
	tr.mu.Lock()
	p.served = append(p.served, blockKey{want[1], 0})
	tr.mu.Unlock()
	w.send(blockMsg(msgCancel, blockKey{want[1], 0}, blockSize))
	for {
		id, pl := w.read()
		if id == msgPiece {
			if k, _ := readBlockKey(append(pl[:8:8], 0, 0, 0, 0)); k.piece == want[1] {
				break // (sent before the cancel arrived: as good as a reject)
			}
		}
		if id == msgReject {
			if k, _ := readBlockKey(pl); k.piece == want[1] {
				break
			}
		}
	}
}
