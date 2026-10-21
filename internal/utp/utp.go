// Package utp is uTP, BitTorrent's transport over UDP that makes way for other traffic, and the
// mux that shares the torrent port's UDP socket between uTP and the DHT.
package utp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// uTP (BEP 29): BitTorrent over UDP with LEDBAT congestion control (RFC 6817). It measures how
// long its packets queue on the way and backs off as soon as the line starts to fill, so torrents
// make way for browsing and calls on the same connection. It shares the UDP port of the TCP
// listener (udpmux.go), and each connection is an ordinary net.Conn for the peer code.

const (
	utpData  = 0
	utpFin   = 1
	utpState = 2
	utpReset = 3
	utpSyn   = 4

	utpHeaderLen = 20
	utpPayload   = 1400                   // bytes per packet: fits a 1500-byte link with IPv6 and PPPoE headers
	utpTarget    = 100 * time.Millisecond // the queueing delay LEDBAT aims for
	utpGain      = 3000                   // most the window grows per round trip (bytes, as libtorrent)
	utpMinWindow = utpPayload
	utpMaxWindow = 4 << 20
	utpRecvWnd   = 1 << 20   // how much they may send us before we read it
	utpSendBuf   = 256 << 10 // written but not sent yet: Write waits beyond this
	utpMinRTO    = 500 * time.Millisecond
	utpMaxRTO    = 30 * time.Second
	utpResends   = 7    // a packet still unacknowledged after this many tries ends the connection
	utpSynTries  = 3    // connecting gives up after this many SYNs (about 7 seconds)
	utpAhead     = 1000 // packets kept beyond a gap (more is dropped and sent again)
	utpKeepAlive = 29 * time.Second
	utpLinger    = 30 * time.Second // a closed connection waits this long at most for its FIN to be acknowledged
)

var (
	errUTPReset   = errors.New("the peer reset the uTP connection")
	errUTPTimeout = &utpTimeoutErr{}
)

type utpTimeoutErr struct{}

func (*utpTimeoutErr) Error() string   { return "uTP connection timed out" }
func (*utpTimeoutErr) Timeout() bool   { return true }
func (*utpTimeoutErr) Temporary() bool { return true }

var utpEpoch = time.Now()

// utpNow is the microsecond clock packets carry (only differences matter, so it wraps freely).
func utpNow() uint32 { return uint32(time.Since(utpEpoch) / time.Microsecond) }

// seqLess compares sequence numbers that wrap at 65536.
func seqLess(a, b uint16) bool { return int16(a-b) < 0 }

type utpHdr struct {
	typ    byte
	conn   uint16
	ts     uint32
	tsDiff uint32
	wnd    uint32
	seq    uint16
	ack    uint16
	sack   []byte // selective ack bitmask: bit i is packet ack+2+i
}

func (h utpHdr) appendTo(b []byte) []byte {
	ext := byte(0)
	if len(h.sack) > 0 {
		ext = 1
	}
	b = append(b, h.typ<<4|1, ext)
	b = binary.BigEndian.AppendUint16(b, h.conn)
	b = binary.BigEndian.AppendUint32(b, h.ts)
	b = binary.BigEndian.AppendUint32(b, h.tsDiff)
	b = binary.BigEndian.AppendUint32(b, h.wnd)
	b = binary.BigEndian.AppendUint16(b, h.seq)
	b = binary.BigEndian.AppendUint16(b, h.ack)
	if ext == 1 {
		b = append(b, 0, byte(len(h.sack)))
		b = append(b, h.sack...)
	}
	return b
}

// looksUTP tells uTP packets (version 1, a known type) from the DHT's and trackers' on a shared port.
func looksUTP(b []byte) bool { return len(b) >= utpHeaderLen && b[0]&0x0f == 1 && b[0]>>4 <= utpSyn }

// parseUTP reads a packet's header and extensions (unknown ones are skipped).
func parseUTP(b []byte) (h utpHdr, payload []byte, ok bool) {
	if !looksUTP(b) {
		return h, nil, false
	}
	h.typ = b[0] >> 4
	ext := b[1]
	h.conn = binary.BigEndian.Uint16(b[2:])
	h.ts = binary.BigEndian.Uint32(b[4:])
	h.tsDiff = binary.BigEndian.Uint32(b[8:])
	h.wnd = binary.BigEndian.Uint32(b[12:])
	h.seq = binary.BigEndian.Uint16(b[16:])
	h.ack = binary.BigEndian.Uint16(b[18:])
	rest := b[utpHeaderLen:]
	for ext != 0 {
		if len(rest) < 2 || len(rest) < 2+int(rest[1]) {
			return h, nil, false
		}
		n := int(rest[1])
		if ext == 1 && n > 0 {
			h.sack = rest[2 : 2+n]
		}
		ext, rest = rest[0], rest[2+n:]
	}
	return h, rest, true
}

type utpKey struct {
	addr netip.AddrPort
	id   uint16
}

// Socket is the uTP side of the shared UDP port: its connections, and the ones waiting to be accepted.
type Socket struct {
	send  func(b []byte, to netip.AddrPort) error // writes one datagram
	laddr net.Addr

	mu      sync.Mutex
	conns   map[utpKey]*utpConn
	acceptq chan *utpConn
	done    chan struct{}
	once    sync.Once

	Dialed, Accepted atomic.Int64 // connections made each way (for the window and tests)
}

func newUTPSocket(send func([]byte, netip.AddrPort) error, laddr net.Addr) *Socket {
	s := &Socket{send: send, laddr: laddr, conns: map[utpKey]*utpConn{}, acceptq: make(chan *utpConn, 64), done: make(chan struct{})}
	go s.timers()
	return s
}

// Accept, Close and Addr make the socket a net.Listener for incoming peers.
func (s *Socket) Accept() (net.Conn, error) {
	select {
	case c := <-s.acceptq:
		return c, nil
	case <-s.done:
		return nil, net.ErrClosed
	}
}

func (s *Socket) Addr() net.Addr { return s.laddr }

func (s *Socket) Close() error {
	s.once.Do(func() {
		close(s.done)
		s.mu.Lock()
		cs := make([]*utpConn, 0, len(s.conns))
		for _, c := range s.conns {
			cs = append(cs, c)
		}
		s.mu.Unlock()
		for _, c := range cs {
			c.mu.Lock()
			if c.state != utpGone {
				c.emit(utpReset, c.seq, nil)
			}
			c.failLocked(net.ErrClosed)
			c.mu.Unlock()
		}
	})
	return nil
}

func (s *Socket) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *Socket) remove(c *utpConn) {
	s.mu.Lock()
	if s.conns[utpKey{c.raddr, c.recvID}] == c {
		delete(s.conns, utpKey{c.raddr, c.recvID})
	}
	s.mu.Unlock()
}

// timers resends what wasn't acknowledged, keeps quiet connections alive and lets closed ones go.
func (s *Socket) timers() {
	tk := time.NewTicker(20 * time.Millisecond)
	defer tk.Stop()
	for {
		select {
		case <-s.done:
			return
		case now := <-tk.C:
			s.mu.Lock()
			cs := make([]*utpConn, 0, len(s.conns))
			for _, c := range s.conns {
				cs = append(cs, c)
			}
			s.mu.Unlock()
			for _, c := range cs { // (never take a connection's lock while holding s.mu)
				c.mu.Lock()
				c.tickLocked(now)
				c.mu.Unlock()
			}
		}
	}
}

// handle takes one packet from the UDP port. It never blocks: the port's reader calls it.
func (s *Socket) handle(b []byte, from netip.AddrPort) {
	h, payload, ok := parseUTP(b)
	if !ok || s.closed() {
		return
	}
	from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
	var c *utpConn
	s.mu.Lock()
	switch h.typ {
	case utpSyn:
		if c = s.conns[utpKey{from, h.conn + 1}]; c == nil {
			c = s.newConn(from)
			c.recvID, c.sendID = h.conn+1, h.conn
			c.state = utpConnected
			c.seq = randUint16()
			c.ack = h.seq
			c.peerWnd = int(h.wnd)
			c.lastRecv = time.Now()
			select {
			case s.acceptq <- c:
				s.conns[utpKey{from, c.recvID}] = c
				s.Accepted.Add(1)
			default:
				s.mu.Unlock()
				return // nobody is taking connections fast enough
			}
		}
		s.mu.Unlock()
		c.mu.Lock()
		c.replyMicro = utpNow() - h.ts
		c.sendStateLocked() // accepts it (again, if our answer was lost)
		c.mu.Unlock()
		return
	case utpReset: // (it may carry either of the connection's two ids)
		for _, id := range []uint16{h.conn, h.conn + 1, h.conn - 1} {
			if x := s.conns[utpKey{from, id}]; x != nil && (x.recvID == h.conn || x.sendID == h.conn) {
				c = x
				break
			}
		}
	default:
		c = s.conns[utpKey{from, h.conn}]
	}
	s.mu.Unlock()
	if c == nil {
		if h.typ != utpReset { // not ours (any more): tell them, as libtorrent does
			r := utpHdr{typ: utpReset, conn: h.conn, ts: utpNow(), seq: randUint16(), ack: h.seq}
			s.send(r.appendTo(nil), from)
		}
		return
	}
	c.mu.Lock()
	c.recvLocked(h, payload)
	c.mu.Unlock()
}

func (s *Socket) newConn(to netip.AddrPort) *utpConn {
	c := &utpConn{s: s, raddr: to, cwnd: 4 * utpPayload, ssthresh: utpMaxWindow, slow: true, peerWnd: utpRecvWnd,
		rto: time.Second, ooo: map[uint16]utpSeg{}, lastSend: time.Now()}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// DialContext opens a uTP connection to a peer.
func (s *Socket) DialContext(ctx context.Context, to netip.AddrPort) (net.Conn, error) {
	if s.closed() {
		return nil, net.ErrClosed
	}
	to = netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	c := s.newConn(to)
	s.mu.Lock()
	for {
		id := randUint16()
		if s.conns[utpKey{to, id}] == nil && s.conns[utpKey{to, id + 1}] == nil {
			c.recvID, c.sendID = id, id+1
			s.conns[utpKey{to, id}] = c
			break
		}
	}
	s.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = utpSynSent
	c.seq = randUint16()
	syn := &utpPkt{seq: c.seq, typ: utpSyn, sentAt: time.Now()}
	c.seq++
	c.flight = append(c.flight, syn)
	c.emit(utpSyn, syn.seq, nil)
	stop := context.AfterFunc(ctx, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer stop()
	for c.state == utpSynSent && ctx.Err() == nil {
		c.cond.Wait()
	}
	switch {
	case c.state == utpConnected:
		s.Dialed.Add(1)
		return c, nil
	case c.err != nil:
		return nil, c.err
	}
	c.failLocked(ctx.Err())
	return nil, ctx.Err()
}

func randUint16() uint16 {
	var b [2]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint16(b[:])
}

const (
	utpSynSent = iota + 1
	utpConnected
	utpGone // reset, timed out or finished: out of the socket's table
)

type utpPkt struct {
	seq     uint16
	typ     byte
	payload []byte
	sentAt  time.Time
	resends int
	lost    bool // waits to be sent again (not counted in flight)
	sacked  bool // they have it, out of order
	fast    bool // already sent again after duplicate acks
}

type utpSeg struct {
	data []byte
	fin  bool
}

// utpConn is one uTP connection. Everything below mu is guarded by it.
type utpConn struct {
	s              *Socket
	raddr          netip.AddrPort
	recvID, sendID uint16

	mu     sync.Mutex
	cond   *sync.Cond // any change: data, room, state, a deadline
	state  int
	err    error
	closed bool // Close was called: what's written still goes, then our FIN
	seq    uint16
	ack    uint16 // the last packet we have everything up to

	// sending
	sbuf      []byte
	flight    []*utpPkt // sent and not yet acknowledged, oldest first
	inflight  int       // payload bytes of those, lost ones left out
	cwnd      float64   // congestion window (bytes)
	ssthresh  float64
	slow      bool // slow start: the window doubles each round trip until the delay rises
	lossSeq   uint16
	peerWnd   int
	lastAck   uint16
	dupAcks   int
	rtt       time.Duration
	rttVar    time.Duration
	ackedSent time.Time // when the newest packet they've acknowledged was sent
	ackAt     time.Time // when they last acknowledged something
	probed    bool      // a quiet spell's probe went out
	rto       time.Duration
	finSent   bool
	lastSend  time.Time
	closeAt   time.Time

	// delay: the smallest one-way delay seen lately is the line with empty queues
	replyMicro uint32 // how long their last packet took to arrive (sent back in ours)
	delayMin   [3]uint32
	delayAt    time.Time
	delays     int
	ourDelay   time.Duration

	// receiving
	rbuf     bytes.Buffer
	ooo      map[uint16]utpSeg // arrived after a gap
	oooBytes int
	gotFin   bool
	needAck  bool
	wndShut  bool // our window was nearly closed: say so when it opens
	lastRecv time.Time

	rdl, wdl       time.Time
	rtimer, wtimer *time.Timer
}

// rwndLocked is how much more we can take in.
func (c *utpConn) rwndLocked() int {
	return max(utpRecvWnd-c.rbuf.Len()-c.oooBytes, 0)
}

// emit sends one packet, with our acknowledgement and window. Caller holds c.mu.
func (c *utpConn) emit(typ byte, seq uint16, payload []byte) {
	h := utpHdr{typ: typ, conn: c.sendID, ts: utpNow(), tsDiff: c.replyMicro, seq: seq, ack: c.ack}
	if typ == utpSyn {
		h.conn = c.recvID
	} else {
		h.sack = c.sackLocked()
	}
	w := c.rwndLocked()
	if w < 2*utpPayload {
		c.wndShut = true
	}
	h.wnd = uint32(w)
	b := h.appendTo(make([]byte, 0, utpHeaderLen+len(payload)+40))
	c.s.send(append(b, payload...), c.raddr)
	c.lastSend = time.Now()
	c.needAck = false
}

func (c *utpConn) sendStateLocked() { c.emit(utpState, c.seq, nil) }

// sackLocked describes the packets we have beyond the gap (nil when there's none).
func (c *utpConn) sackLocked() []byte {
	if len(c.ooo) == 0 {
		return nil
	}
	var bits [32]byte
	top := -1
	for s := range c.ooo {
		if off := int(uint16(s - c.ack - 2)); off < len(bits)*8 {
			bits[off/8] |= 1 << (off % 8)
			top = max(top, off)
		}
	}
	if top < 0 {
		return nil
	}
	return append([]byte(nil), bits[:(top/32+1)*4]...)
}

// recvLocked takes a packet for this connection.
func (c *utpConn) recvLocked(h utpHdr, payload []byte) {
	if c.state == utpGone {
		return
	}
	now := time.Now()
	c.lastRecv = now
	if h.typ == utpReset {
		c.failLocked(errUTPReset)
		return
	}
	c.replyMicro = utpNow() - h.ts
	if c.state == utpSynSent {
		if h.typ == utpSyn {
			return
		}
		c.ack = h.seq - 1 // their first packet's number is the next one they'll send
		c.state = utpConnected
	}
	c.peerWnd = int(h.wnd)
	acked := c.ackLocked(h, now)
	if h.tsDiff != 0 {
		c.delayLocked(h.tsDiff, now)
	}
	if acked > 0 {
		c.growLocked(acked)
	}
	if h.typ == utpData || h.typ == utpFin {
		c.dataLocked(h, payload)
	}
	c.flushLocked() // (the data packets carry the acknowledgement)
	if c.needAck {
		c.sendStateLocked()
	}
	c.cond.Broadcast()
}

// ackLocked processes their acknowledgement (cumulative and selective) and returns the payload
// bytes it newly confirms.
func (c *utpConn) ackLocked(h utpHdr, now time.Time) (acked int) {
	if len(c.flight) == 0 {
		return 0
	}
	first := c.flight[0].seq
	if seqLess(h.ack, first-1) || seqLess(c.seq-1, h.ack) {
		return 0 // old, or for packets we never sent
	}
	confirm := func(p *utpPkt) {
		if !p.lost {
			c.inflight -= len(p.payload)
		}
		acked += len(p.payload)
		if p.resends == 0 {
			c.rttLocked(now.Sub(p.sentAt))
		}
		if p.sentAt.After(c.ackedSent) {
			c.ackedSent = p.sentAt
		}
		c.ackAt, c.probed = now, false
	}
	n := int(uint16(h.ack - first + 1))
	if n > 0 {
		for _, p := range c.flight[:n] {
			if !p.sacked {
				confirm(p)
			}
		}
		c.flight = append(c.flight[:0], c.flight[n:]...)
		c.dupAcks = 0
		c.rto = max(c.rtt+4*c.rttVar, utpMinRTO) // the back-off ends
	} else if h.typ == utpState && h.ack == c.lastAck {
		if c.dupAcks++; c.dupAcks == 3 { // the packet after it went missing
			c.skippedLocked(c.flight[0])
		}
	}
	c.lastAck = h.ack
	if len(h.sack) == 0 {
		if n > 0 {
			c.lossesLocked()
		}
		return acked
	}
	for _, p := range c.flight {
		off := int(uint16(p.seq - h.ack - 2))
		if !p.sacked && off < len(h.sack)*8 && h.sack[off/8]&(1<<(off%8)) != 0 {
			p.sacked = true
			confirm(p)
		}
	}
	c.lossesLocked()
	return acked
}

// lossesLocked finds the packets that are lost: one with three or more later ones received
// (BEP 29), or one sent well before a packet that has arrived (RACK, RFC 8985). Either way not
// when it was sent again after those: then the copy is still on its way.
func (c *utpConn) lossesLocked() {
	reorder := max(c.rtt/4, time.Millisecond)
	later := 0
	for i := len(c.flight) - 1; i >= 0; i-- {
		if p := c.flight[i]; p.sacked {
			later++
		} else if later >= 3 && p.sentAt.Before(c.ackedSent) || p.sentAt.Add(reorder).Before(c.ackedSent) {
			c.lostLocked(p)
		}
	}
}

// skippedLocked sends the first packet again after three duplicate acks, once: the acks that
// follow still show the gap until the copy arrives.
func (c *utpConn) skippedLocked(p *utpPkt) {
	if !p.fast {
		p.fast = true
		c.lostLocked(p)
	}
}

// lostLocked marks a packet for sending again, halving the window once per round of losses.
func (c *utpConn) lostLocked(p *utpPkt) {
	if p.lost || p.sacked {
		return
	}
	p.lost = true
	c.inflight -= len(p.payload)
	if !seqLess(p.seq, c.lossSeq) {
		c.cwnd = max(c.cwnd/2, utpMinWindow)
		c.ssthresh, c.slow = c.cwnd, false
		c.lossSeq = c.seq
	}
}

func (c *utpConn) rttLocked(d time.Duration) {
	if c.rtt == 0 {
		c.rtt, c.rttVar = d, d/2
	} else {
		delta := c.rtt - d
		if delta < 0 {
			delta = -delta
		}
		c.rttVar += (delta - c.rttVar) / 4
		c.rtt += (d - c.rtt) / 8
	}
	c.rto = max(c.rtt+4*c.rttVar, utpMinRTO)
}

// delayLocked takes the one-way delay of our packets as they measured it: what's above the
// smallest seen in the last two minutes or so is queueing.
func (c *utpConn) delayLocked(d uint32, now time.Time) {
	if c.delays == 0 || now.Sub(c.delayAt) > time.Minute {
		copy(c.delayMin[1:], c.delayMin[:2])
		c.delayMin[0], c.delayAt = d, now
		c.delays = min(c.delays+1, len(c.delayMin))
	} else if int32(d-c.delayMin[0]) < 0 {
		c.delayMin[0] = d
	}
	base := c.delayMin[0]
	for _, m := range c.delayMin[1:c.delays] {
		if int32(m-base) < 0 {
			base = m
		}
	}
	c.ourDelay = time.Duration(d-base) * time.Microsecond
}

// growLocked is LEDBAT: the window grows while the delay is under the target and shrinks above it.
func (c *utpConn) growLocked(acked int) {
	if float64(c.inflight+acked) < c.cwnd-2*utpPayload {
		return // we aren't using the window we have
	}
	if c.slow {
		if c.ourDelay > utpTarget/2 {
			c.slow, c.ssthresh = false, c.cwnd
		} else if c.cwnd += float64(acked); c.cwnd >= c.ssthresh {
			c.slow = false
		}
	}
	if !c.slow {
		off := min(max(float64(utpTarget-c.ourDelay)/float64(utpTarget), -1), 1)
		c.cwnd += utpGain * off * float64(acked) / c.cwnd
	}
	c.cwnd = min(max(c.cwnd, utpMinWindow), utpMaxWindow)
}

// dataLocked takes a data packet (or their FIN): in order it goes to the reader, after a gap it waits.
func (c *utpConn) dataLocked(h utpHdr, payload []byte) {
	c.needAck = true
	if c.gotFin {
		return
	}
	d := int16(h.seq - c.ack - 1)
	switch {
	case d < 0: // we have it (our acknowledgement was lost)
	case d == 0 && c.rbuf.Len() < 2*utpRecvWnd: // (beyond that they ignore our window: dropped)
		c.deliverLocked(payload, h.typ == utpFin)
		c.ack++
		for !c.gotFin {
			s, ok := c.ooo[c.ack+1]
			if !ok {
				break
			}
			delete(c.ooo, c.ack+1)
			c.oooBytes -= len(s.data)
			c.deliverLocked(s.data, s.fin)
			c.ack++
		}
		if c.gotFin {
			c.ooo, c.oooBytes = map[uint16]utpSeg{}, 0
		}
	case int(d) < utpAhead && c.rwndLocked() >= len(payload):
		if _, dup := c.ooo[h.seq]; !dup {
			c.ooo[h.seq] = utpSeg{append([]byte(nil), payload...), h.typ == utpFin}
			c.oooBytes += len(payload)
		}
	}
}

func (c *utpConn) deliverLocked(b []byte, fin bool) {
	if !c.closed { // (after Close what still arrives is acknowledged and dropped)
		c.rbuf.Write(b)
	}
	if fin {
		c.gotFin = true
	}
}

// flushLocked sends what the window allows: lost packets first, then new data, then our FIN.
func (c *utpConn) flushLocked() {
	if c.state != utpConnected {
		return
	}
	now := time.Now()
	if len(c.flight) == 0 {
		c.ackAt = now // (the quiet-spell clock starts with the first packet out)
	}
	win := min(int(c.cwnd), c.peerWnd)
	for _, p := range c.flight {
		if !p.lost {
			continue
		}
		if c.inflight > 0 && c.inflight+len(p.payload) > win {
			return
		}
		p.lost, p.sacked = false, false
		p.resends++
		p.sentAt = now
		c.inflight += len(p.payload)
		c.emit(p.typ, p.seq, p.payload)
	}
	for len(c.sbuf) > 0 && len(c.flight) < utpAhead {
		n := min(len(c.sbuf), utpPayload)
		// one packet may always go (it probes a closed window); a part-filled one waits while
		// others are out, so small writes share packets (Nagle, as libtorrent)
		if c.inflight > 0 && (c.inflight+n > win || n < utpPayload) {
			break
		}
		p := &utpPkt{seq: c.seq, typ: utpData, payload: append([]byte(nil), c.sbuf[:n]...), sentAt: now}
		c.seq++
		if c.sbuf = c.sbuf[n:]; len(c.sbuf) == 0 {
			c.sbuf = nil
		}
		c.flight = append(c.flight, p)
		c.inflight += n
		c.emit(utpData, p.seq, p.payload)
	}
	if c.closed && len(c.sbuf) == 0 && !c.finSent {
		p := &utpPkt{seq: c.seq, typ: utpFin, sentAt: now}
		c.seq++
		c.flight = append(c.flight, p)
		c.finSent = true
		c.emit(utpFin, p.seq, nil)
	}
	c.cond.Broadcast()
}

// tickLocked runs every few milliseconds: the retransmission timer, keep-alives, lingering.
func (c *utpConn) tickLocked(now time.Time) {
	if c.state == utpGone {
		c.s.remove(c)
		return
	}
	var oldest *utpPkt
	for _, p := range c.flight {
		if !p.lost && !p.sacked {
			oldest = p
			break
		}
	}
	// a quiet spell with packets out: send the oldest again as a probe. Its acknowledgement
	// shows what else was lost, which is then sent again without waiting for the timeout
	// that would shrink the window to a single packet (the tail loss probe of RFC 8985)
	if oldest != nil && c.state == utpConnected && !c.probed && c.rtt > 0 && now.Sub(c.ackAt) > max(2*c.rtt, 20*time.Millisecond) && now.Sub(oldest.sentAt) < c.rto {
		c.probed = true
		oldest.resends++
		oldest.sentAt = now
		c.emit(oldest.typ, oldest.seq, oldest.payload)
	}
	if oldest != nil && now.Sub(oldest.sentAt) > c.rto {
		probe := c.peerWnd < utpPayload && len(c.flight) == 1 // their window is shut: keep asking
		if c.state == utpSynSent && oldest.resends+1 >= utpSynTries || !probe && oldest.resends >= utpResends {
			c.failLocked(errUTPTimeout)
			return
		}
		// a timeout: everything out is taken as lost, and it starts again from one packet
		for _, p := range c.flight {
			if !p.lost && !p.sacked {
				p.lost = true
				c.inflight -= len(p.payload)
			}
		}
		c.ssthresh = max(c.cwnd/2, 2*utpMinWindow)
		c.cwnd, c.slow, c.lossSeq = utpMinWindow, true, c.seq
		c.rto = min(c.rto*2, utpMaxRTO)
		if c.state == utpSynSent {
			oldest.lost, oldest.sentAt = false, now
			oldest.resends++
			c.emit(utpSyn, oldest.seq, nil)
		} else {
			c.flushLocked()
		}
	}
	if c.state == utpConnected && now.Sub(c.lastSend) > utpKeepAlive {
		c.sendStateLocked() // NAT routers forget quiet UDP flows after half a minute or so
	}
	if c.closed && (c.finSent && len(c.flight) == 0 || now.Sub(c.closeAt) > utpLinger) {
		c.failLocked(net.ErrClosed)
	}
}

// failLocked ends the connection: it leaves the socket's table and every wait ends with err.
func (c *utpConn) failLocked(err error) {
	if c.state == utpGone {
		return
	}
	if c.err == nil {
		c.err = err
	}
	c.state = utpGone
	c.flight, c.sbuf = nil, nil
	c.s.remove(c)
	c.cond.Broadcast()
}

func (c *utpConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		switch {
		case c.closed:
			return 0, net.ErrClosed
		case c.rbuf.Len() > 0:
			n, _ := c.rbuf.Read(b)
			if c.wndShut && c.state == utpConnected && c.rwndLocked() >= 8*utpPayload {
				c.wndShut = false
				c.sendStateLocked() // there's room again
			}
			return n, nil
		case c.gotFin:
			return 0, io.EOF
		case c.err != nil:
			return 0, c.err
		case !c.rdl.IsZero() && !time.Now().Before(c.rdl):
			return 0, os.ErrDeadlineExceeded
		}
		c.cond.Wait()
	}
}

func (c *utpConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for len(b) > 0 {
		switch {
		case c.closed:
			return n, net.ErrClosed
		case c.err != nil:
			return n, c.err
		case !c.wdl.IsZero() && !time.Now().Before(c.wdl):
			return n, os.ErrDeadlineExceeded
		}
		room := utpSendBuf - len(c.sbuf)
		if room <= 0 {
			c.cond.Wait()
			continue
		}
		k := min(room, len(b))
		c.sbuf = append(c.sbuf, b[:k]...)
		b, n = b[k:], n+k
		c.flushLocked()
	}
	return n, nil
}

// Close sends what's still buffered, then a FIN; the connection lingers until that's acknowledged.
func (c *utpConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed, c.closeAt = true, time.Now()
	c.rbuf.Reset()
	c.ooo, c.oooBytes = map[uint16]utpSeg{}, 0
	for _, t := range []*time.Timer{c.rtimer, c.wtimer} {
		if t != nil {
			t.Stop()
		}
	}
	if c.state == utpConnected && c.err == nil {
		c.flushLocked()
	} else {
		c.failLocked(net.ErrClosed)
	}
	c.cond.Broadcast()
	return nil
}

func (c *utpConn) LocalAddr() net.Addr  { return c.s.laddr }
func (c *utpConn) RemoteAddr() net.Addr { return net.UDPAddrFromAddrPort(c.raddr) }

func (c *utpConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *utpConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rdl = t
	c.armLocked(&c.rtimer, t)
	return nil
}

func (c *utpConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wdl = t
	c.armLocked(&c.wtimer, t)
	return nil
}

// armLocked wakes the waiting reader or writer when its deadline comes.
func (c *utpConn) armLocked(tp **time.Timer, t time.Time) {
	if *tp != nil {
		(*tp).Stop()
		*tp = nil
	}
	c.cond.Broadcast()
	if t.IsZero() || c.closed {
		return
	}
	*tp = time.AfterFunc(time.Until(t), func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
}

// IsConn says whether a peer connection runs over uTP (looking through wrappers that expose
// the connection underneath, as tls.Conn does).
func IsConn(c net.Conn) bool {
	for range 4 {
		switch x := c.(type) {
		case *utpConn:
			return true
		case interface{ NetConn() net.Conn }:
			c = x.NetConn()
		default:
			return false
		}
	}
	return false
}
