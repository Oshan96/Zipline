package main

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"net/netip"
	"sort"
	"time"
)

// The fast extension (BEP 6): Have All / Have None instead of a bitfield, Reject Request so a
// choke no longer throws away what we asked for (the peer says which requests it won't serve),
// Allowed Fast (a few pieces a peer may fetch while choked, which gets newcomers going) and
// Suggest Piece. Both sides set bit 0x04 of the handshake's last reserved byte.

const (
	msgSuggest     = 0x0D
	msgHaveAll     = 0x0E
	msgHaveNone    = 0x0F
	msgReject      = 0x10
	msgAllowedFast = 0x11
	allowedFastK   = 10 // the allowed-fast set we give each peer
	maxAllowedIn   = 64 // …and how much of theirs we keep
)

// peerX is a peer's state for the fast extension, peer exchange (pex.go) and the DHT port
// message (dhtnode.go). Guarded by t.mu.
type peerX struct {
	fast       bool         // both sides speak the fast extension
	dht        bool         // they run a DHT node (handshake bit 0x01)
	allowedIn  map[int]bool // pieces they let us ask for while they choke us
	allowedOut map[int]bool // pieces we let them ask for while we choke them
	lport      int          // their listening port, from the extension handshake ("p")
	encPref    bool         // they prefer encrypted connections ("e")
	pexSent    map[netip.AddrPort]bool
	pexNext    time.Time
	haveAll    bool // they sent Have All (seen in tests)
	rejected   int  // requests they rejected
	suggested  int  // pieces they suggested
	fastAsked  int  // blocks asked for while choked
}

// xs is the peer's extension state (made on first use for a peer that didn't come through
// startPeer). Caller holds t.mu.
func (p *tpeer) xs() *peerX {
	if p.x == nil {
		p.x = newPeerX([68]byte{})
	}
	return p.x
}

func newPeerX(h [68]byte) *peerX {
	return &peerX{fast: h[27]&0x04 != 0, dht: h[27]&0x01 != 0, allowedIn: map[int]bool{}, allowedOut: map[int]bool{}, pexSent: map[netip.AddrPort]bool{}}
}

// allowedFastSet is BEP 6's canonical allowed-fast set: k pieces picked from SHA-1 chains of
// the peer's address (IPv4 /24, IPv6 /48) and the info hash, so reconnecting gives the same set.
func allowedFastSet(ip netip.Addr, ih [20]byte, pieces, k int) []int {
	k = min(k, pieces)
	var x []byte
	if ip = ip.Unmap(); ip.Is4() {
		a := ip.As4()
		x = append(x, a[0], a[1], a[2], 0)
	} else {
		a := ip.As16()
		x = append(append(x, a[:6]...), make([]byte, 10)...)
	}
	x = append(x, ih[:]...)
	var set []int
	seen := map[int]bool{}
	for len(set) < k {
		h := sha1.Sum(x)
		x = h[:]
		for i := 0; i < 5 && len(set) < k; i++ {
			idx := int(binary.BigEndian.Uint32(x[i*4:]) % uint32(pieces))
			if !seen[idx] {
				seen[idx] = true
				set = append(set, idx)
			}
		}
	}
	return set
}

func pieceMsg(id byte, piece int) []byte {
	return binary.BigEndian.AppendUint32([]byte{id}, uint32(piece))
}

func blockMsg(id byte, k blockKey, length int) []byte {
	b := binary.BigEndian.AppendUint32([]byte{id}, uint32(k.piece))
	b = binary.BigEndian.AppendUint32(b, uint32(k.begin))
	return binary.BigEndian.AppendUint32(b, uint32(length))
}

// sendHaveFast sends Have All or Have None in place of the bitfield when both sides speak the
// fast extension (which needs one of the three). It returns false when a bitfield should go.
// Caller holds t.mu.
func (t *torrent) sendHaveFast(p *tpeer) bool {
	if !p.xs().fast {
		return false
	}
	switch n := t.haveCount(); {
	case t.mi == nil || n == 0:
		p.sendLocked([]byte{msgHaveNone})
	case n == t.mi.NumPieces():
		p.sendLocked([]byte{msgHaveAll})
	default:
		return false
	}
	return true
}

// greetPeer finishes a new connection: our allowed-fast set, and our DHT port (BEP 5) when
// they run a DHT node too. Caller holds t.mu.
func (t *torrent) greetPeer(p *tpeer) {
	x := p.xs()
	if x.fast && t.mi != nil && t.uploadAllowed() {
		for _, i := range allowedFastSet(p.addr.Addr(), t.ih, t.mi.NumPieces(), allowedFastK) {
			x.allowedOut[i] = true
			if t.have.has(i) {
				p.sendLocked(pieceMsg(msgAllowedFast, i))
			}
		}
	}
	if x.dht && (t.mi == nil || !t.mi.Private) {
		if port := t.m.dhtPort(); port > 0 {
			p.sendLocked(binary.BigEndian.AppendUint16([]byte{msgPort}, uint16(port)))
		}
	}
}

// fastServes: a request from a peer we choke may be served (it's in their allowed-fast set).
// Caller holds t.mu.
func (t *torrent) fastServes(p *tpeer, piece int) bool {
	x := p.xs()
	return x.fast && x.allowedOut[piece] && t.uploadAllowed()
}

// reject tells a peer we won't serve a request (fast extension only). Caller holds t.mu.
func (t *torrent) reject(p *tpeer, k blockKey, length uint32) {
	if p.xs().fast {
		p.sendLocked(blockMsg(msgReject, k, int(length)))
	}
}

// keepOnChoke is what's left of a peer's requests when we choke it: with the fast extension,
// the allowed-fast ones stay and the rest get a Reject. Caller holds t.mu.
func (t *torrent) keepOnChoke(p *tpeer) []blockKey {
	if !p.xs().fast {
		return nil
	}
	var keep []blockKey
	for _, k := range p.served {
		if t.fastServes(p, k.piece) {
			keep = append(keep, k)
		} else if t.mi != nil {
			p.sendLocked(blockMsg(msgReject, k, int(min(int64(blockSize), t.mi.PieceSize(k.piece)-int64(k.begin)))))
		}
	}
	return keep
}

// handleMore handles the messages handle() leaves: the fast extension's and the DHT port.
func (t *torrent) handleMore(p *tpeer, id byte, pl []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == msgPort {
		if len(pl) == 2 && (t.mi == nil || !t.mi.Private) {
			go t.m.dhtContact(netip.AddrPortFrom(p.addr.Addr(), binary.BigEndian.Uint16(pl)))
		}
		return nil
	}
	if id < msgSuggest || id > msgAllowedFast {
		return nil // unknown: ignored
	}
	x := p.xs()
	if !x.fast {
		return errors.New("fast extension message without the fast extension")
	}
	switch id {
	case msgHaveAll, msgHaveNone:
		if len(pl) != 0 {
			return errors.New("bad have all/none")
		}
		all := id == msgHaveAll
		x.haveAll = all
		if t.mi == nil { // before the file list is known (setMeta fills it in)
			p.haveAll, p.have = all, nil
			return nil
		}
		n := t.mi.NumPieces()
		if len(p.have) == len(t.have) {
			for i := 0; i < n; i++ {
				if p.have.has(i) {
					t.avail[i]--
				}
			}
		}
		p.have, p.haveCount = newBitfield(n), 0
		if all {
			for i := 0; i < n; i++ {
				p.have.set(i)
				t.avail[i]++
			}
			p.haveCount = n
		}
		p.seed = all
		t.updateInterest(p)
	case msgReject:
		if len(pl) != 12 {
			return errors.New("bad reject")
		}
		k := blockKey{int(binary.BigEndian.Uint32(pl)), int(binary.BigEndian.Uint32(pl[4:]))}
		if _, ok := p.pending[k]; ok { // (a reject for something we cancelled is old news)
			delete(p.pending, k)
			t.unask(k)
			x.rejected++
			if p.peerChoking {
				delete(x.allowedIn, k.piece) // it doesn't honour its allowed-fast set after all
			}
		}
		// asked again on the next tick, from whoever has it (not straight back to this peer)
	case msgAllowedFast:
		if len(pl) != 4 {
			return errors.New("bad allowed fast")
		}
		i := int(binary.BigEndian.Uint32(pl))
		if (t.mi == nil || i < t.mi.NumPieces()) && len(x.allowedIn) < maxAllowedIn {
			x.allowedIn[i] = true
			t.askAllowedFast(p)
		}
	case msgSuggest:
		if len(pl) != 4 {
			return errors.New("bad suggest")
		}
		x.suggested++ // (the piece picker goes by rarity; a suggestion doesn't change that)
	}
	return nil
}

// askAllowedFast keeps asking a peer that chokes us for blocks of the pieces it allows while
// choked. Caller holds t.mu.
func (t *torrent) askAllowedFast(p *tpeer) {
	x := p.xs()
	if t.mi == nil || !p.peerChoking || !p.amInterested || p.closed || t.complete || t.checking || len(x.allowedIn) == 0 {
		return
	}
	var pieces []int
	for i := range x.allowedIn {
		if i < len(t.wanted) && t.wanted[i] && !t.have.has(i) && p.have.has(i) {
			pieces = append(pieces, i)
		}
	}
	sort.Ints(pieces)
	limit := min(p.maxPending, p.reqq)
	for _, i := range pieces {
		ps := t.pieces[i]
		if ps == nil {
			if ps = t.newPiece(i); ps == nil { // piece memory is used up (tdisk.go)
				return
			}
		}
		if ps.checking || ps.web {
			continue
		}
		for b, st := range ps.blocks {
			if len(p.pending) >= limit {
				return
			}
			if st != 0 {
				continue
			}
			k := blockKey{i, b * blockSize}
			ps.markAsked(b)
			p.pending[k] = time.Now()
			x.fastAsked++
			p.sendLocked(blockMsg(msgRequest, k, int(min(int64(blockSize), t.mi.PieceSize(i)-int64(k.begin)))))
		}
	}
}

// extLoop runs the extensions' timers: peer exchange once a minute per peer, and keeping
// choked fast-extension peers busy with their allowed-fast pieces.
func (t *torrent) extLoop() {
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case now := <-tk.C:
			t.mu.Lock()
			for _, p := range t.peers {
				t.askAllowedFast(p)
			}
			t.sendPex(now)
			t.mu.Unlock()
		}
	}
}

// peerMarks are the window's short markers for a peer: encrypted, fast extension, uTP.
// Caller holds t.mu.
func (t *torrent) peerMarks(p *tpeer) []string {
	var marks []string
	if rc4, obf := connEncryption(p.conn); rc4 {
		marks = append(marks, "enc")
	} else if obf {
		marks = append(marks, "obf")
	}
	if peerUsesUTP(p) {
		marks = append(marks, "utp")
	}
	if p.xs().fast {
		marks = append(marks, "fast")
	}
	return marks
}
