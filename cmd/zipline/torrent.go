package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zipline/internal/bencode"
)

// The BitTorrent engine: the peer wire protocol (BEP 3) with the extension protocol (BEP 10),
// metadata exchange for magnet links (BEP 9) and peer exchange (BEP 11); rarest-first piece
// picking with an endgame; every piece checked against its SHA-1 before it counts; uploading
// to the peers that upload to us (tit-for-tat), plus one rotating optimistic unchoke.

const (
	blockSize      = 16 << 10
	maxPeerMsg     = 2 << 20
	msgChoke       = 0
	msgUnchoke     = 1
	msgInterested  = 2
	msgNotInterest = 3
	msgHave        = 4
	msgBitfield    = 5
	msgRequest     = 6
	msgPiece       = 7
	msgCancel      = 8
	msgPort        = 9
	msgExtended    = 20
	extMetadata    = 1 // our ids for the extensions we speak
	extPex         = 2
	uploadSlots    = 4
)

var peerIDPrefix = "-ZL1000-"

func newPeerID() (id [20]byte) {
	copy(id[:], peerIDPrefix)
	const chars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	var b [12]byte
	rand.Read(b[:])
	for i, c := range b {
		id[8+i] = chars[int(c)%len(chars)]
	}
	return
}

type bitfield []byte

func newBitfield(n int) bitfield   { return make(bitfield, (n+7)/8) }
func (b bitfield) has(i int) bool  { return i >= 0 && i/8 < len(b) && b[i/8]&(0x80>>(i%8)) != 0 }
func (b bitfield) set(i int)       { b[i/8] |= 0x80 >> (i % 8) }
func (b bitfield) unset(i int)     { b[i/8] &^= 0x80 >> (i % 8) }
func (b bitfield) clone() bitfield { return append(bitfield(nil), b...) }

// rateMeter keeps a smoothed bytes-per-second figure (tick once a second).
type rateMeter struct {
	n    int64 // atomic: bytes since the last tick
	rate float64
	tot  int64 // atomic
}

func (r *rateMeter) add(n int) {
	atomic.AddInt64(&r.n, int64(n))
	atomic.AddInt64(&r.tot, int64(n))
}
func (r *rateMeter) tick() { r.rate = r.rate*0.6 + float64(atomic.SwapInt64(&r.n, 0))*0.4 }

type blockKey struct{ piece, begin int }

type pieceState struct {
	blocks   []byte // 0 = to fetch, 1 = asked for, 2 = got
	asked    []int8 // how many peers were asked for each block (endgame asks twice)
	got      int
	free     int             // blocks nobody has been asked for (0s in blocks)
	hint     int             // no block before this one is free
	checking bool            // whole: being checked, then written (tdisk.go)
	claimed  bool            // its check has begun
	web      bool            // a web seed fetches it whole
	from     map[*tpeer]bool // who sent its blocks (blamed if it's damaged)
	buf      []byte          // the piece, put together in memory
	mem      int64           // piece memory reserved for it
	counted  int64           // bytes of it counted in the download's progress
	last     time.Time       // a block last arrived then
}

type tpeer struct {
	t        *torrent
	conn     net.Conn
	addr     netip.AddrPort
	id       [20]byte
	client   string
	outgoing bool

	// guarded by t.mu
	have           bitfield
	haveAll        bool // has every piece (before the torrent's size is known)
	haveCount      int
	amChoking      bool
	amInterested   bool
	peerChoking    bool
	peerInterested bool
	pending        map[blockKey]time.Time
	maxPending     int
	ext            map[string]int // the peer's ids for extensions
	metaAsked      int            // metadata piece asked for (-1 none)
	metaAskedAt    time.Time
	lastUseful     time.Time
	unchokedAt     time.Time
	badPieces      int
	closed         bool
	seed           bool
	reqq           int
	served         []blockKey // their requests waiting to be sent
	q              peerQ      // how many requests to keep going (tpick.go)
	x              *peerX     // fast extension, peer exchange, DHT port (fastext.go)

	down, up rateMeter

	wmu     sync.Mutex
	wcond   *sync.Cond
	outq    [][]byte
	wclosed bool         // guarded by wmu
	nserved atomic.Int32 // len(served), readable without t.mu
}

type tcand struct {
	addr  netip.AddrPort
	fails int
	next  time.Time
	src   string
}

type torrent struct {
	m       *Manager
	d       *Download
	ih      [20]byte
	peerID  [20]byte
	port    atomic.Int32 // our listening port (changes if you pick another in Options)
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time

	mu           sync.Mutex
	mi           *Metainfo
	st           *tstorage
	have         bitfield
	wanted       []bool
	wantBytes    []int64 // per piece: bytes downloaded for it (0 = not wanted)
	pieces       map[int]*pieceState
	avail        []int
	peers        map[netip.AddrPort]*tpeer
	cands        map[netip.AddrPort]*tcand
	dialing      int
	trackers     []*trackerState
	webseeds     []*webSeed
	metaSize     int
	metaBuf      []byte
	metaGot      []bool
	optimist     *tpeer
	optimAt      time.Time
	complete     bool
	finishedHere bool // finished downloading during this run (so "completed" goes to trackers)
	seeding      bool
	seedAt       time.Time // started sharing then
	shareBase    int64     // what it had uploaded when sharing began again (0: sharing counts from the first byte)
	err          error
	metaReady    chan struct{}           // closed when the file list is known
	finished     chan struct{}           // closed when every wanted piece is in
	banned       map[netip.Addr]bool     // peers that sent damaged data (by address on the internet)
	bannedAt     map[netip.AddrPort]bool // (by address and port: home networks share addresses)

	uploaded   int64 // atomic
	downloaded int64 // atomic
	upRate     rateMeter
	downRate   rateMeter
	live       atomic.Pointer[TorrentLive] // for the window (read without t.mu)
	wantPeers  atomic.Int64                // the window wants the peer list until then (Unix seconds)
	planQ      atomic.Bool                 // from the Free data hours queue: uploads only in free hours
	upHold     atomic.Bool                 // the gate pauses its uploads (gate.go)
	wasHeld    atomic.Bool                 // …as of its last look

	tx   // piece picking, piece memory and checking files (tpick.go, tdisk.go, trecheck.go)
	tctl // its own upload limit; being moved (tcontrols.go)
}

type trackerState struct {
	url      string
	next     time.Time
	interval time.Duration
	status   string
	peers    int
	seeders  int
	leechers int
	started  bool
	fails    int
	last     time.Time     // last asked
	minGap   time.Duration // the tracker's "min interval"
	id       string        // the tracker's "tracker id"
	sentDone bool          // "completed" was sent (only once, BEP 3)
	tier     int           // which tier of BEP 12 it is in
	retryAt  time.Time     // after a failure: not asked before then
	asking   bool          // an announce is on its way
	warning  string        // the tracker's "warning message"
	detail   string        // what the tracker said, when the status puts it in other words
	code     string        // the status as a word for the window ("asking", "working", "refused"…); arg: the tracker's own words
	arg      string
}

func (t *torrent) hex() string { return fmt.Sprintf("%x", t.ih[:]) }

// ---------------------------------------------------------------- setting up

// setMeta installs the file list (from a .torrent or fetched from peers). Caller holds t.mu.
func (t *torrent) setMeta(mi *Metainfo, skip []bool, have bitfield) {
	t.mi = mi
	n := mi.NumPieces()
	if len(have) != len(newBitfield(n)) {
		have = newBitfield(n)
	}
	t.have = have
	t.avail = make([]int, n)
	t.pieces = map[int]*pieceState{}
	if skip == nil || len(skip) != len(mi.Files) {
		skip = make([]bool, len(mi.Files))
	}
	for i, f := range mi.Files {
		if f.Pad {
			skip[i] = true
		}
	}
	t.st = newStorage(mi, t.d.Dir, skip, t.hex())
	t.applySkip(skip)
	for _, p := range t.peers {
		if p.haveAll {
			p.have = newBitfield(n)
			for i := 0; i < n; i++ {
				p.have.set(i)
			}
			p.haveCount = n
		}
		if len(p.have) == len(t.have) {
			for i := 0; i < n; i++ {
				if p.have.has(i) {
					t.avail[i]++
				}
			}
		} else {
			p.have = newBitfield(n)
		}
	}
	select {
	case <-t.metaReady:
	default:
		close(t.metaReady)
	}
}

// applySkip works out which pieces are wanted from the files that are. Caller holds t.mu.
func (t *torrent) applySkip(skip []bool) {
	mi := t.mi
	n := mi.NumPieces()
	t.wanted = make([]bool, n)
	t.wantBytes = make([]int64, n)
	for i, f := range mi.Files {
		if skip[i] || f.Length == 0 {
			continue
		}
		first := int(f.Offset / mi.PieceLen)
		last := int((f.Offset + f.Length - 1) / mi.PieceLen)
		for p := first; p <= last && p < n; p++ {
			t.wanted[p] = true
		}
	}
	var size, done int64
	for p := 0; p < n; p++ {
		if t.wanted[p] {
			t.wantBytes[p] = mi.PieceSize(p)
			size += t.wantBytes[p]
			if t.have.has(p) {
				done += t.wantBytes[p]
			}
		}
	}
	if t.st != nil {
		t.st.setSkip(skip)
	}
	t.dropPartials(true) // (pieces no chosen file needs any more)
	t.updatePrio(skip)
	atomic.StoreInt64(&t.d.done, done+t.partialDone())
	t.m.mu.Lock()
	t.d.Size = size
	t.m.mu.Unlock()
}

// wantedLeft is how many wanted bytes are still missing. Caller holds t.mu.
func (t *torrent) wantedLeft() int64 {
	if t.mi == nil {
		return 1
	}
	var left int64
	for p, w := range t.wanted {
		if w && !t.have.has(p) {
			left += t.wantBytes[p]
		}
	}
	return left
}

func (t *torrent) checkComplete() {
	if t.mi == nil || t.complete {
		return
	}
	for p, w := range t.wanted {
		if w && !t.have.has(p) {
			return
		}
	}
	t.complete = true
	close(t.finished)
}

// ---------------------------------------------------------------- peers: connecting

func (t *torrent) addCands(addrs []netip.AddrPort, src string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, a := range addrs {
		a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
		if !a.IsValid() || a.Port() == 0 || a.Addr().IsUnspecified() || t.isBanned(a) || len(t.cands) > 3000 {
			continue
		}
		if a.Port() == uint16(t.lport()) && (a.Addr().IsLoopback() || isOwnAddr(a.Addr())) {
			continue // that's us
		}
		if _, ok := t.cands[a]; !ok {
			t.cands[a] = &tcand{addr: a, src: src}
		}
	}
}

var ownAddrs struct {
	sync.Once
	set map[netip.Addr]bool
}

func isOwnAddr(a netip.Addr) bool {
	ownAddrs.Do(func() {
		ownAddrs.set = map[netip.Addr]bool{}
		ifs, _ := net.InterfaceAddrs()
		for _, ia := range ifs {
			if ipn, ok := ia.(*net.IPNet); ok {
				if x, ok := netip.AddrFromSlice(ipn.IP); ok {
					ownAddrs.set[x.Unmap()] = true
				}
			}
		}
	})
	return ownAddrs.set[a]
}

func (t *torrent) maxPeers() int {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	if n := t.m.settings.Torrent.MaxPeers; n > 0 {
		return n
	}
	return 60
}

// maxPeersLocked is maxPeers for callers holding t.mu (t.mu may be held while taking m.mu,
// never the other way round).
func (t *torrent) maxPeersLocked() int { return t.maxPeers() }

// connectMore dials candidates while there's room.
func (t *torrent) connectMore() {
	limit := t.maxPeers()
	t.mu.Lock()
	now := time.Now()
	room := min(limit-len(t.peers), connRoom()) - t.dialing // (and all torrents together: maxConns)
	if room <= 0 || t.complete && !t.seeding {
		t.mu.Unlock()
		return
	}
	var pick []*tcand
	for _, c := range t.cands {
		if _, connected := t.peers[c.addr]; connected || now.Before(c.next) {
			continue
		}
		pick = append(pick, c)
	}
	sort.Slice(pick, func(i, j int) bool { return pick[i].fails < pick[j].fails })
	if n := min(room, 12-t.dialing); n < len(pick) {
		pick = pick[:max(n, 0)]
	}
	for _, c := range pick {
		c.next = now.Add(time.Duration(30*(c.fails+1)) * time.Second)
		t.dialing++
		go t.dial(c.addr)
	}
	t.mu.Unlock()
}

func (t *torrent) dial(a netip.AddrPort) {
	defer func() {
		t.mu.Lock()
		t.dialing--
		t.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(t.ctx, 8*time.Second)
	conn, err := t.dialPeer(ctx, a) // uTP or TCP, or through the proxy (tnet.go)
	cancel()
	if err != nil {
		t.candFailed(a)
		return
	}
	conn, h, err := t.encHandshake(conn, a, func() (net.Conn, error) { // encrypted first, plain again if refused (mse.go)
		ctx, cancel := context.WithTimeout(t.ctx, 8*time.Second)
		defer cancel()
		return t.dialPeer(ctx, a)
	})
	if err != nil {
		t.candFailed(a)
		return
	}
	t.startPeer(conn, a, true, h)
}

func (t *torrent) candFailed(a netip.AddrPort) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if c := t.cands[a]; c != nil {
		c.fails++
		if c.fails > 5 {
			delete(t.cands, a)
		}
	}
}

func handshakeBytes(ih, peerID [20]byte) []byte {
	b := make([]byte, 0, 68)
	b = append(b, 19)
	b = append(b, "BitTorrent protocol"...)
	b = append(b, 0, 0, 0, 0, 0, 0x10, 0, 0x05) // extension protocol; fast extension and DHT (fastext.go)
	b = append(b, ih[:]...)
	return append(b, peerID[:]...)
}

// handshake runs an outgoing connection's handshake and returns the peer's (an incoming one
// is read by the listener, which answers and then calls startPeer).
func (t *torrent) handshake(conn net.Conn) ([68]byte, error) {
	var h [68]byte
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.Write(handshakeBytes(t.ih, t.peerID)); err != nil {
		return h, err
	}
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		return h, err
	}
	if h[0] != 19 || string(h[1:20]) != "BitTorrent protocol" || !bytes.Equal(h[28:48], t.ih[:]) {
		return h, errors.New("wrong handshake")
	}
	if bytes.Equal(h[48:68], t.peerID[:]) {
		return h, errors.New("connected to ourselves")
	}
	conn.SetDeadline(time.Time{})
	return h, nil
}

// startPeer registers a connected peer and starts its reader and writer.
func (t *torrent) startPeer(conn net.Conn, a netip.AddrPort, outgoing bool, h [68]byte) {
	t.mu.Lock()
	if _, dup := t.peers[a]; dup || len(t.peers) >= t.maxPeersLocked()+5 || t.ctx.Err() != nil || !takeConn() {
		t.mu.Unlock()
		conn.Close()
		return
	}
	p := &tpeer{t: t, conn: conn, addr: a, outgoing: outgoing, amChoking: true, peerChoking: true,
		pending: map[blockKey]time.Time{}, maxPending: 4, ext: map[string]int{}, metaAsked: -1, lastUseful: time.Now(), reqq: 250}
	copy(p.id[:], h[48:68])
	p.client = clientName(p.id)
	p.wcond = sync.NewCond(&p.wmu)
	p.x = newPeerX(h)
	if t.mi != nil {
		p.have = newBitfield(t.mi.NumPieces())
	}
	t.peers[a] = p
	if c := t.cands[a]; c != nil {
		c.fails = 0
	} else {
		t.cands[a] = &tcand{addr: a, src: "incoming"}
	}
	supportsExt := h[25]&0x10 != 0
	// our extension handshake and what we have
	if supportsExt {
		m := map[string]any{"ut_metadata": extMetadata, "ut_pex": extPex}
		if t.mi != nil && t.mi.Private {
			delete(m, "ut_pex") // BEP 27: private torrents get peers only from their tracker
		}
		eh := map[string]any{"m": m, "v": "Zipline " + appVersion, "reqq": 250}
		if p := t.lport(); p > 0 {
			eh["p"] = p
		}
		if t.mi != nil {
			eh["metadata_size"] = len(t.mi.Info)
		}
		t.extHandshakeMore(eh) // (pex.go)
		p.sendLocked(extMsg(0, bencode.Encode(eh)))
	}
	if !t.sendHaveFast(p) && t.mi != nil && t.haveCount() > 0 { // (or Have All / Have None: fastext.go)
		p.sendLocked(append([]byte{msgBitfield}, t.have...))
	}
	t.greetPeer(p)
	t.mu.Unlock()
	go p.writeLoop()
	go p.readLoop()
}

func (t *torrent) haveCount() int {
	n := 0
	for _, b := range t.have {
		n += popcount(b)
	}
	return n
}

func popcount(b byte) int {
	n := 0
	for ; b != 0; b &= b - 1 {
		n++
	}
	return n
}

func extMsg(id byte, payload []byte) []byte {
	return append([]byte{msgExtended, id}, payload...)
}

// clientName reads the client from its peer id ("-qB4630-" → qBittorrent 4.6.3).
func clientName(id [20]byte) string {
	names := map[string]string{"qB": "qBittorrent", "TR": "Transmission", "UT": "µTorrent", "LT": "libtorrent", "lt": "libTorrent", "DE": "Deluge",
		"AZ": "Vuze", "BI": "BiglyBT", "BT": "BitTorrent", "ZL": "Zipline", "KT": "KTorrent", "FD": "Free Download Manager", "TX": "Tixati", "WW": "WebTorrent", "AG": "Ares", "XL": "Xunlei", "SD": "Thunder"}
	if id[0] == '-' && id[7] == '-' {
		code := string(id[1:3])
		name := names[code]
		if name == "" {
			name = code
		}
		v := string(id[3:7])
		return fmt.Sprintf("%s %s", name, strings.TrimRight(strings.Join(strings.Split(v, ""), "."), "."))
	}
	if id[0] == 'M' {
		return "BitTorrent"
	}
	return "unknown client"
}

// ---------------------------------------------------------------- peers: sending

// sendLocked queues a message (length prefix added). Caller holds t.mu (or the peer isn't shared yet).
func (p *tpeer) sendLocked(msg []byte) {
	b := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(b, uint32(len(msg)))
	copy(b[4:], msg)
	p.wmu.Lock()
	p.outq = append(p.outq, b)
	p.wmu.Unlock()
	p.wcond.Signal()
}

func (p *tpeer) writeLoop() {
	w := bufio.NewWriterSize(upCounter{p.conn, p.t.m}, 64<<10) // everything sent counts as data used
	keep := time.NewTicker(90 * time.Second)
	defer keep.Stop()
	go func() { // keep-alives
		for {
			select {
			case <-keep.C:
				p.wmu.Lock()
				if len(p.outq) == 0 {
					p.outq = append(p.outq, []byte{0, 0, 0, 0})
					p.wcond.Signal()
				}
				p.wmu.Unlock()
			case <-p.t.ctx.Done():
				p.wcond.Broadcast()
				return
			}
		}
	}()
	for {
		p.wmu.Lock() // (never take t.mu while holding wmu: senders hold t.mu when they take wmu)
		for len(p.outq) == 0 && p.nserved.Load() == 0 && !p.wclosed && p.t.ctx.Err() == nil {
			p.wcond.Wait()
		}
		q := p.outq
		p.outq = nil
		closed := p.wclosed
		p.wmu.Unlock()
		if closed || p.t.ctx.Err() != nil {
			p.close()
			return
		}
		for _, m := range q {
			p.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if _, err := w.Write(m); err != nil {
				p.close()
				return
			}
		}
		if err := w.Flush(); err != nil {
			p.close()
			return
		}
		// blocks they asked for, read from disk and sent at the upload limit (messages queued meanwhile go first)
		for p.sendBlock(w) && !p.queued() {
		}
	}
}

// queued: messages are waiting to go out.
func (p *tpeer) queued() bool {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return len(p.outq) > 0
}

// sendBlock sends one block a peer asked for (false when there's none to send).
func (p *tpeer) sendBlock(w *bufio.Writer) bool {
	t := p.t
	t.mu.Lock()
	if len(p.served) == 0 || p.closed || p.amChoking && !t.fastServes(p, p.served[0].piece) || t.st == nil {
		t.mu.Unlock()
		return false
	}
	if t.upHeld() { // uploads are paused (gate.go): this peer waits, choked, until they carry on
		t.choke(p)
		t.mu.Unlock()
		return false
	}
	k := p.served[0]
	p.served = p.served[1:]
	p.nserved.Store(int32(len(p.served)))
	st, mi := t.st, t.mi
	ok := t.have.has(k.piece)
	t.mu.Unlock()
	if !ok {
		return true
	}
	length := int(min(int64(blockSize), mi.PieceSize(k.piece)-int64(k.begin)))
	if length <= 0 {
		return true
	}
	buf := make([]byte, 13+length)
	if err := st.ReadAt(buf[13:], int64(k.piece)*mi.PieceLen+int64(k.begin)); err != nil {
		return true
	}
	t.upWait(length) // the upload limits: this torrent's own, Options, the traffic profile, polite mode
	binary.BigEndian.PutUint32(buf, uint32(9+length))
	buf[4] = msgPiece
	binary.BigEndian.PutUint32(buf[5:], uint32(k.piece))
	binary.BigEndian.PutUint32(buf[9:], uint32(k.begin))
	p.conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	if _, err := w.Write(buf); err != nil {
		p.close()
		return false
	}
	if w.Flush() != nil {
		p.close()
		return false
	}
	p.up.add(length)
	t.upRate.add(length)
	atomic.AddInt64(&t.uploaded, int64(length))
	return true
}

func (p *tpeer) close() {
	t := p.t
	t.mu.Lock()
	if p.closed {
		t.mu.Unlock()
		return
	}
	p.closed = true
	t.dropPeer(p)
	t.mu.Unlock()
	p.conn.Close()
	p.wmu.Lock()
	p.wclosed = true
	p.wmu.Unlock()
	p.wcond.Broadcast()
}

// dropPeer forgets a peer: its unanswered requests go back to the pool. Caller holds t.mu.
func (t *torrent) dropPeer(p *tpeer) {
	if t.peers[p.addr] == p {
		delete(t.peers, p.addr)
		peerConns.Add(-1)
	}
	for k := range p.pending {
		t.unask(k)
	}
	p.pending = map[blockKey]time.Time{}
	if t.mi != nil && len(p.have) == len(t.have) {
		for i := 0; i < t.mi.NumPieces(); i++ {
			if p.have.has(i) {
				t.avail[i]--
			}
		}
	}
	if t.optimist == p {
		t.optimist = nil
	}
}

func (t *torrent) unask(k blockKey) {
	ps := t.pieces[k.piece]
	if ps == nil {
		return
	}
	b := k.begin / blockSize
	if b < len(ps.asked) && ps.blocks[b] == 1 {
		if ps.asked[b]--; ps.asked[b] <= 0 {
			ps.asked[b] = 0
			ps.blocks[b] = 0
			ps.free++
			ps.hint = min(ps.hint, b)
		}
	}
}

// ---------------------------------------------------------------- peers: receiving

// pacedConn counts what comes in against the data cap and quotas. It doesn't hold anything back:
// the speed limit applies to blocks only (see readLoop), because a limit applied to every read
// made a peer's request or unchoke wait behind the reads of all the other peers.
type pacedConn struct {
	net.Conn
	t *torrent
}

func (c pacedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.t.m.countDown(int64(n))
	return n, err
}

func (p *tpeer) readLoop() {
	defer p.close()
	t := p.t
	r := bufio.NewReaderSize(pacedConn{p.conn, t}, 64<<10)
	var lenBuf [4]byte
	for {
		p.conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			tdbg("peer %s (%s, out=%v) closed: %v", p.addr, p.client, p.outgoing, err)
			return
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n == 0 {
			continue // keep-alive
		}
		if n > maxPeerMsg {
			return
		}
		if id, err := r.Peek(1); err == nil && id[0] == msgPiece { // a block waits for the speed limit; the messages around it don't
			t.m.paceBlock(t.ctx, t.d, int(n)+4)
			if t.ctx.Err() != nil {
				return
			}
			p.conn.SetReadDeadline(time.Now().Add(3 * time.Minute)) // (the data cap may have held it a long while)
		}
		msg := p.msgBuf(int(n))
		if _, err := io.ReadFull(r, msg); err != nil {
			return
		}
		p.down.add(int(n) + 4)
		if err := t.handle(p, msg); err != nil {
			tdbg("peer %s handle error: %v", p.addr, err)
			return
		}
	}
}

func (t *torrent) handle(p *tpeer, msg []byte) error {
	id, pl := msg[0], msg[1:]
	switch id {
	case msgChoke:
		t.mu.Lock()
		p.peerChoking = true
		if !p.xs().fast { // without the fast extension, a choke drops our requests (with it, they reject what they won't send)
			for k := range p.pending {
				t.unask(k)
			}
			p.pending = map[blockKey]time.Time{}
		}
		t.mu.Unlock()
	case msgUnchoke:
		t.mu.Lock()
		p.peerChoking = false
		t.fillRequests(p)
		t.mu.Unlock()
	case msgInterested, msgNotInterest:
		t.mu.Lock()
		p.peerInterested = id == msgInterested
		if p.peerInterested && p.amChoking && t.unchokedCount() < uploadSlots && t.uploadAllowed() {
			t.unchoke(p)
		}
		t.mu.Unlock()
	case msgHave:
		if len(pl) != 4 {
			return errors.New("bad have")
		}
		i := int(binary.BigEndian.Uint32(pl))
		t.mu.Lock()
		if t.mi != nil {
			if i >= t.mi.NumPieces() {
				t.mu.Unlock()
				return errors.New("bad have")
			}
			if !p.have.has(i) {
				p.have.set(i)
				p.haveCount++
				t.avail[i]++
			}
			p.seed = p.haveCount == t.mi.NumPieces()
			t.updateInterest(p)
		}
		t.mu.Unlock()
	case msgBitfield:
		t.mu.Lock()
		if t.mi == nil {
			// before the file list is known: remember whether it's a seed
			all := len(pl) > 0
			for _, b := range pl[:max(len(pl)-1, 0)] {
				if b != 0xff {
					all = false
				}
			}
			p.haveAll = all
			p.have = bitfield(append([]byte(nil), pl...))
			t.mu.Unlock()
			return nil
		}
		if len(pl) != len(t.have) {
			t.mu.Unlock()
			return errors.New("bad bitfield")
		}
		if len(p.have) == len(t.have) {
			for i := 0; i < t.mi.NumPieces(); i++ {
				if p.have.has(i) {
					t.avail[i]--
				}
			}
		}
		p.have = bitfield(append([]byte(nil), pl...))
		p.haveCount = 0
		for i := 0; i < t.mi.NumPieces(); i++ {
			if p.have.has(i) {
				p.haveCount++
				t.avail[i]++
			}
		}
		p.seed = p.haveCount == t.mi.NumPieces()
		t.updateInterest(p)
		t.mu.Unlock()
	case msgRequest, msgCancel:
		if len(pl) != 12 {
			return errors.New("bad request")
		}
		k := blockKey{int(binary.BigEndian.Uint32(pl)), int(binary.BigEndian.Uint32(pl[4:]))}
		length := binary.BigEndian.Uint32(pl[8:])
		t.mu.Lock()
		if id == msgCancel {
			for i, s := range p.served {
				if s == k {
					p.served = append(p.served[:i], p.served[i+1:]...)
					t.reject(p, k, length) // (the fast extension answers every request)
					break
				}
			}
			p.nserved.Store(int32(len(p.served)))
			t.mu.Unlock()
			return nil
		}
		ok := t.mi != nil && (!p.amChoking || t.fastServes(p, k.piece)) && k.piece < t.mi.NumPieces() && t.have.has(k.piece) && length == uint32(min(int64(blockSize), t.mi.PieceSize(k.piece)-int64(k.begin))) && k.begin%blockSize == 0 && len(p.served) < 400
		if ok {
			p.served = append(p.served, k)
			p.nserved.Store(int32(len(p.served)))
		} else {
			t.reject(p, k, length)
		}
		t.mu.Unlock()
		if ok {
			p.wmu.Lock()
			p.wcond.Signal()
			p.wmu.Unlock()
		}
	case msgPiece:
		if len(pl) < 8 {
			return errors.New("bad piece")
		}
		t.gotBlock(p, int(binary.BigEndian.Uint32(pl)), int(binary.BigEndian.Uint32(pl[4:])), pl[8:])
	case msgExtended:
		if len(pl) < 1 {
			return nil
		}
		return t.handleExt(p, pl[0], pl[1:])
	default:
		return t.handleMore(p, id, pl) // the fast extension and the DHT port (fastext.go)
	}
	return nil
}

// updateInterest tells a peer whether we want something it has. Caller holds t.mu.
func (t *torrent) updateInterest(p *tpeer) {
	want := false
	if t.mi != nil && !t.complete {
		for i := 0; i < t.mi.NumPieces(); i++ {
			if t.wanted[i] && !t.have.has(i) && p.have.has(i) {
				want = true
				break
			}
		}
	}
	if want != p.amInterested {
		p.amInterested = want
		if want {
			p.sendLocked([]byte{msgInterested})
		} else {
			p.sendLocked([]byte{msgNotInterest})
		}
	}
	if want && !p.peerChoking {
		t.fillRequests(p)
	}
}

// ---------------------------------------------------------------- the piece picker

// fillRequests keeps a peer's request pipeline full. Caller holds t.mu.
func (t *torrent) fillRequests(p *tpeer) {
	if t.mi == nil || p.peerChoking || !p.amInterested || p.closed || t.complete || t.checking {
		return
	}
	limit := min(p.maxPending, p.reqq)
	for len(p.pending) < limit {
		k, ok := t.nextBlock(p)
		if !ok {
			break
		}
		t.pieces[k.piece].markAsked(k.begin / blockSize)
		p.pending[k] = time.Now()
		length := int(min(int64(blockSize), t.mi.PieceSize(k.piece)-int64(k.begin)))
		req := make([]byte, 13)
		req[0] = msgRequest
		binary.BigEndian.PutUint32(req[1:], uint32(k.piece))
		binary.BigEndian.PutUint32(req[5:], uint32(k.begin))
		binary.BigEndian.PutUint32(req[9:], uint32(length))
		p.sendLocked(req)
	}
}

func (t *torrent) fail(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
	t.cancel()
}

// ---------------------------------------------------------------- extensions: metadata and peer exchange

func (t *torrent) handleExt(p *tpeer, id byte, pl []byte) error {
	if id == 0 { // their extension handshake
		v, err := bencode.Decode(pl)
		if err != nil {
			return nil
		}
		d, _ := v.(map[string]any)
		t.mu.Lock()
		defer t.mu.Unlock()
		for k, v := range bencode.Dict(d, "m") {
			if n, ok := v.(int64); ok {
				p.ext[k] = int(n)
			}
		}
		if n, ok := bencode.Int(d, "reqq"); ok && n > 0 {
			p.reqq = int(min(n, 500))
		}
		if v := bencode.Str(d, "v"); v != "" {
			p.client = v
		}
		t.gotExtHandshake(p, d) // (pex.go)
		if size, ok := bencode.Int(d, "metadata_size"); ok && t.mi == nil && size > 0 && size < 32<<20 && t.metaSize == 0 {
			t.metaSize = int(size)
			t.metaBuf = make([]byte, size)
			t.metaGot = make([]bool, (size+blockSize-1)/blockSize)
		}
		t.askMetadata()
		return nil
	}
	switch int(id) {
	case extMetadata:
		v, n, err := bencode.DecodePrefix(pl)
		if err != nil {
			return nil
		}
		d, _ := v.(map[string]any)
		typ, _ := bencode.Int(d, "msg_type")
		piece, _ := bencode.Int(d, "piece")
		t.mu.Lock()
		defer t.mu.Unlock()
		switch typ {
		case 0: // they ask us for a piece of the metadata
			theirs, ok := p.ext["ut_metadata"]
			if !ok {
				return nil
			}
			if t.mi == nil || piece < 0 || int(piece)*blockSize >= len(t.mi.Info) {
				p.sendLocked(extMsg(byte(theirs), bencode.Encode(map[string]any{"msg_type": 2, "piece": piece})))
				return nil
			}
			end := min(int(piece+1)*blockSize, len(t.mi.Info))
			data := t.mi.Info[int(piece)*blockSize : end]
			p.sendLocked(extMsg(byte(theirs), append(bencode.Encode(map[string]any{"msg_type": 1, "piece": piece, "total_size": len(t.mi.Info)}), data...)))
		case 1: // a piece of the metadata we asked for
			if t.mi != nil || t.metaBuf == nil || int(piece) != p.metaAsked {
				return nil
			}
			p.metaAsked = -1
			off := int(piece) * blockSize
			data := pl[n:]
			if off+len(data) > len(t.metaBuf) || piece >= int64(len(t.metaGot)) {
				return nil
			}
			copy(t.metaBuf[off:], data)
			t.metaGot[piece] = true
			done := true
			for _, g := range t.metaGot {
				done = done && g
			}
			if done {
				t.metadataArrived()
			} else {
				t.askMetadata()
			}
		case 2: // they don't have it
			p.metaAsked = -1
			delete(p.ext, "ut_metadata")
			t.askMetadata()
		}
	case extPex:
		if t.mi != nil && t.mi.Private {
			return nil // BEP 27
		}
		v, err := bencode.Decode(pl)
		if err != nil {
			return nil
		}
		d, _ := v.(map[string]any)
		var add []netip.AddrPort
		add = append(add, compactPeers([]byte(bencode.Str(d, "added")), 6)...)
		add = append(add, compactPeers([]byte(bencode.Str(d, "added6")), 18)...)
		if len(add) > 200 {
			add = add[:200]
		}
		go t.addCands(add, "peer exchange")
	}
	return nil
}

// askMetadata asks peers that have the file list for the pieces still missing. Caller holds t.mu.
func (t *torrent) askMetadata() {
	if t.mi != nil || t.metaBuf == nil {
		return
	}
	asked := map[int]bool{}
	for _, p := range t.peers {
		if p.metaAsked >= 0 {
			if time.Since(p.metaAskedAt) > 20*time.Second {
				p.metaAsked = -1 // too slow: ask someone else
			} else {
				asked[p.metaAsked] = true
			}
		}
	}
	for _, p := range t.peers {
		id, ok := p.ext["ut_metadata"]
		if !ok || p.metaAsked >= 0 || p.closed {
			continue
		}
		for i, g := range t.metaGot {
			if !g && !asked[i] {
				p.metaAsked, p.metaAskedAt = i, time.Now()
				asked[i] = true
				p.sendLocked(extMsg(byte(id), bencode.Encode(map[string]any{"msg_type": 0, "piece": i})))
				break
			}
		}
	}
}

// metadataArrived checks the file list against the torrent's hash and starts the download
// proper. Caller holds t.mu.
func (t *torrent) metadataArrived() {
	if h := sha1.Sum(t.metaBuf); !bytes.Equal(h[:], t.ih[:]) {
		t.metaSize, t.metaBuf, t.metaGot = 0, nil, nil // damaged: start over
		return
	}
	mi, err := parseInfo(t.metaBuf)
	if err != nil {
		t.err = err
		go t.cancel()
		return
	}
	t.m.mu.Lock()
	ts := t.d.Torrent
	var skip []bool
	if ts != nil && len(ts.Select) > 0 { // files chosen in a magnet link (so=)
		skip = make([]bool, len(mi.Files))
		for i := range skip {
			skip[i] = true
		}
		for _, k := range ts.Select {
			if k >= 0 && k < len(skip) {
				skip[k] = false
			}
		}
	}
	t.m.mu.Unlock()
	mi.Trackers = t.trackerTiers()
	t.setMeta(mi, skip, nil)
	t.m.torrentMetaArrived(t.d, mi, skip)
	t.checkArrived() // (its files may be in the folder already)
	for _, p := range t.peers {
		if len(p.have) != len(t.have) {
			p.have = newBitfield(mi.NumPieces())
		}
		t.updateInterest(p)
	}
}

func (t *torrent) trackerTiers() [][]string { return t.tierURLs() }

// ---------------------------------------------------------------- choking (uploading)

func (t *torrent) unchokedCount() int {
	n := 0
	for _, p := range t.peers {
		if !p.amChoking {
			n++
		}
	}
	return n
}

// uploadAllowed: uploads aren't paused (see uploadHold in gate.go). Caller holds t.mu.
func (t *torrent) uploadAllowed() bool {
	return !t.upHeld()
}

func (t *torrent) unchoke(p *tpeer) {
	p.amChoking = false
	p.unchokedAt = time.Now()
	p.sendLocked([]byte{msgUnchoke})
}

func (t *torrent) choke(p *tpeer) {
	p.amChoking = true
	p.sendLocked([]byte{msgChoke})
	p.served = t.keepOnChoke(p) // (the fast extension: allowed-fast requests stay, the rest are rejected)
	p.nserved.Store(int32(len(p.served)))
}

// rechoke gives the upload slots to the interested peers that send us the most (or, when
// we're only sharing, that take the most), plus one chosen at random every 30 seconds.
// Caller holds t.mu.
func (t *torrent) rechoke() {
	var cands []*tpeer
	for _, p := range t.peers {
		if p.peerInterested && !p.closed {
			cands = append(cands, p)
		}
	}
	allowed := t.uploadAllowed()
	sort.Slice(cands, func(i, j int) bool {
		if t.complete {
			return cands[i].up.rate > cands[j].up.rate
		}
		return cands[i].down.rate > cands[j].down.rate
	})
	keep := map[*tpeer]bool{}
	if allowed {
		for i := 0; i < len(cands) && i < uploadSlots-1; i++ {
			keep[cands[i]] = true
		}
		if t.optimist == nil || time.Since(t.optimAt) > 30*time.Second || t.optimist.closed {
			var rest []*tpeer
			for _, p := range cands {
				if !keep[p] {
					rest = append(rest, p)
				}
			}
			t.optimist = nil
			if len(rest) > 0 {
				t.optimist = rest[mrand.IntN(len(rest))]
				t.optimAt = time.Now()
			}
		}
		if t.optimist != nil {
			keep[t.optimist] = true
		}
	}
	for _, p := range t.peers {
		if keep[p] && p.amChoking {
			t.unchoke(p)
		} else if !keep[p] && !p.amChoking {
			t.choke(p)
		}
	}
}

// ---------------------------------------------------------------- the loop

// tick runs once a second: rates, slow requests, choking, connections, the file-list hunt.
func (t *torrent) tick(n int) {
	t.mu.Lock()
	now := time.Now()
	t.upRate.tick()
	t.downRate.tick()
	for _, p := range t.peers {
		p.down.tick()
		p.up.tick()
		t.sizeQueue(p) // pipeline depth follows the peer's speed (about three seconds of data)
		for k, at := range p.pending {
			if now.Sub(at) > 45*time.Second {
				delete(p.pending, k)
				t.unask(k)
			}
		}
		// a peer that has nothing for us and wants nothing from us makes room for others
		if t.mi != nil && !p.amInterested && !p.peerInterested && now.Sub(p.lastUseful) > 90*time.Second && len(t.peers) > t.maxPeersLocked()/2 {
			go p.close()
		}
		if t.complete && p.seed { // both done: nothing to trade
			go p.close()
		}
	}
	if n%10 == 0 {
		t.rechoke()
	}
	if t.mi == nil {
		t.askMetadata()
	} else {
		t.tidyPieces(now)
		for _, p := range t.peers {
			t.fillRequests(p)
		}
	}
	t.mu.Unlock()
	t.connectMore()
	v := t.view(time.Now().Unix() <= t.wantPeers.Load())
	t.live.Store(&v)
}

// TorrentPeerView is one connected peer in the progress window.
type TorrentPeerView struct {
	Addr   string  `json:"addr"`
	Client string  `json:"client"`
	Have   float64 `json:"have"` // 0…1
	Down   float64 `json:"down"`
	Up     float64 `json:"up"`
	Flags  string  `json:"flags"`
	Marks  string  `json:"marks,omitempty"` // space-separated: "enc" (RC4), "obf" (encrypted handshake only), "utp", "fast"
}

// view describes the torrent for the window. Caller holds no locks.
func (t *torrent) view(peers bool) TorrentLive {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := TorrentLive{Peers: len(t.peers), Up: t.upRate.rate, Uploaded: atomic.LoadInt64(&t.uploaded),
		Seeding: t.seeding, NeedMeta: t.mi == nil, Held: t.upHeld(), Base: t.shareBase}
	v.Checking, v.CheckPct = t.checking, t.checkPct()
	for a := range t.cands { // known but not connected
		if t.peers[a] == nil {
			v.Known++
		}
	}
	if t.metaBuf != nil && t.mi == nil {
		got := 0
		for _, g := range t.metaGot {
			if g {
				got++
			}
		}
		v.MetaPct = got * 100 / len(t.metaGot)
	}
	for _, p := range t.peers {
		if p.seed {
			v.Seeds++
		}
	}
	v.Trackers = t.trackerViews(time.Now())
	if t.mi != nil {
		n := t.mi.NumPieces()
		buckets := min(n, 300)
		v.Map = make([]byte, buckets)
		for b := 0; b < buckets; b++ {
			lo, hi := b*n/buckets, (b+1)*n/buckets
			got, want := 0, 0
			for i := lo; i < hi; i++ {
				if t.wanted[i] {
					want++
					if t.have.has(i) {
						got++
					}
				}
			}
			switch {
			case want == 0:
				v.Map[b] = 255 // not downloading this part
			default:
				v.Map[b] = byte(got * 100 / want)
			}
		}
	}
	if peers {
		for _, p := range t.peers {
			pv := TorrentPeerView{Addr: p.addr.String(), Client: p.client, Down: p.down.rate, Up: p.up.rate}
			if t.mi != nil {
				pv.Have = float64(p.haveCount) / float64(t.mi.NumPieces())
			}
			var f []string
			if !p.outgoing {
				f = append(f, "incoming")
			}
			if p.peerChoking {
				f = append(f, "choking us")
			}
			if !p.amChoking {
				f = append(f, "we upload")
			}
			pv.Flags = strings.Join(f, " · ")
			pv.Marks = strings.Join(t.peerMarks(p), " ")
			v.PeerList = append(v.PeerList, pv)
		}
		sort.Slice(v.PeerList, func(i, j int) bool { return v.PeerList[i].Down+v.PeerList[i].Up > v.PeerList[j].Down+v.PeerList[j].Up })
		if len(v.PeerList) > 60 {
			v.PeerList = v.PeerList[:60]
		}
	}
	return v
}

// TorrentLive is a running torrent's state for the window.
type TorrentLive struct {
	Peers    int               `json:"peers"`
	Seeds    int               `json:"seeds"`
	Known    int               `json:"known"`
	Up       float64           `json:"up"`
	Uploaded int64             `json:"uploaded"`
	Seeding  bool              `json:"seeding"`
	Held     bool              `json:"held"` // uploads are paused (gate.go)
	Base     int64             `json:"base,omitempty"`
	NeedMeta bool              `json:"needMeta"`
	MetaPct  int               `json:"metaPct"`
	Map      []byte            `json:"map,omitempty"`
	Trackers []TrackerView     `json:"trackers,omitempty"`
	PeerList []TorrentPeerView `json:"peerList,omitempty"`
	Checking bool              `json:"checking,omitempty"` // reading its files back (trecheck.go)
	CheckPct int               `json:"checkPct,omitempty"`
}

type TrackerView struct {
	URL      string `json:"url"`
	Status   string `json:"status"`
	Peers    int    `json:"peers"`
	Seeders  int    `json:"seeders"`
	Leechers int    `json:"leechers"`
	State    string `json:"state"` // Status as a word the window puts in its own language; Arg has the tracker's words
	Arg      string `json:"arg,omitempty"`
	Announce string `json:"announce,omitempty"` // the full address (URL is shortened)
	Tier     int    `json:"tier"`
	Warning  string `json:"warning,omitempty"` // the tracker's "warning message"
	Detail   string `json:"detail,omitempty"`  // what it said, when Status puts that in other words
	Fails    int    `json:"fails,omitempty"`   // failed this many times in a row…
	RetryIn  int    `json:"retryIn,omitempty"` // …and is left alone for this many more seconds
}

var tdebugOn = os.Getenv("ZL_TDEBUG") != ""

func tdbg(f string, a ...any) {
	if tdebugOn {
		fmt.Fprintf(os.Stderr, time.Now().Format("15:04:05.000 ")+f+"\n", a...)
	}
}

// ban shuts out a peer that sent damaged data. Caller holds t.mu.
func (t *torrent) ban(a netip.AddrPort) {
	t.bannedAt[a] = true
	if ip := a.Addr(); !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
		t.banned[ip] = true
	}
	delete(t.cands, a)
}

func (t *torrent) lport() int { return int(t.port.Load()) }

func (t *torrent) isBanned(a netip.AddrPort) bool { return t.banned[a.Addr()] || t.bannedAt[a] }
