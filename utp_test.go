package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"
)

// lossyLink is a UDP socket on loopback that loses, reorders and duplicates what it sends.
type lossyLink struct {
	conn               *net.UDPConn
	drop, late, dup    float64
	mu                 sync.Mutex
	rnd                *mrand.Rand
	sent, lost, copies int
	types              [5]int
}

func (l *lossyLink) send(b []byte, to netip.AddrPort) error {
	l.mu.Lock()
	r, wait := l.rnd.Float64(), time.Duration(1+l.rnd.IntN(25))*time.Millisecond
	l.sent++
	l.types[b[0]>>4]++
	l.mu.Unlock()
	b = append([]byte(nil), b...)
	switch {
	case r < l.drop:
		l.mu.Lock()
		l.lost++
		l.mu.Unlock()
		return nil
	case r < l.drop+l.late: // overtaken by the packets after it
		time.AfterFunc(wait, func() { l.conn.WriteToUDPAddrPort(b, to) })
		return nil
	case r < l.drop+l.late+l.dup:
		l.mu.Lock()
		l.copies++
		l.mu.Unlock()
		l.conn.WriteToUDPAddrPort(b, to)
	}
	_, err := l.conn.WriteToUDPAddrPort(b, to)
	return err
}

// utpEnd is one end of a test link: a uTP socket on its own loopback UDP port.
func utpEnd(t *testing.T, drop, late, dup float64, seed uint64) (*utpSocket, *lossyLink, netip.AddrPort) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadBuffer(4 << 20)
	l := &lossyLink{conn: c, drop: drop, late: late, dup: dup, rnd: mrand.New(mrand.NewPCG(seed, 7))}
	s := newUTPSocket(l.send, c.LocalAddr())
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			s.handle(buf[:n], from)
		}
	}()
	t.Cleanup(func() { s.Close(); c.Close() })
	return s, l, c.LocalAddr().(*net.UDPAddr).AddrPort()
}

// utpConnPair connects two uTP sockets and returns both ends of the connection.
func utpConnPair(t *testing.T, drop, late, dup float64) (client, server net.Conn, cl, sl *lossyLink) {
	a, la, _ := utpEnd(t, drop, late, dup, 1)
	b, lb, addrB := utpEnd(t, drop, late, dup, 2)
	got := make(chan net.Conn, 1)
	go func() {
		c, err := b.Accept()
		if err == nil {
			got <- c
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := a.DialContext(ctx, addrB)
	if err != nil {
		t.Fatal("dial:", err)
	}
	select {
	case s := <-got:
		return c, s, la, lb
	case <-time.After(10 * time.Second):
		t.Fatal("not accepted")
	}
	return nil, nil, nil, nil
}

// exchange sends size random bytes each way at once and checks both arrive whole, then that a
// close reaches the other side as the end of the stream.
func exchange(t *testing.T, a, b net.Conn, size int, limit time.Duration) {
	t.Helper()
	ab, ba := randData(size), randData(size)
	type res struct {
		got []byte
		err error
	}
	read := func(c net.Conn, n int, out chan<- res) {
		c.SetReadDeadline(time.Now().Add(limit))
		buf := make([]byte, n)
		_, err := io.ReadFull(c, buf)
		out <- res{buf, err}
	}
	ra, rb := make(chan res, 1), make(chan res, 1)
	go read(a, size, ra)
	go read(b, size, rb)
	go func() { a.SetWriteDeadline(time.Now().Add(limit)); a.Write(ab) }()
	go func() { b.SetWriteDeadline(time.Now().Add(limit)); b.Write(ba) }()
	for _, x := range []struct {
		ch   chan res
		want []byte
		name string
	}{{rb, ab, "a→b"}, {ra, ba, "b→a"}} {
		r := <-x.ch
		if r.err != nil || !bytes.Equal(r.got, x.want) {
			t.Fatalf("%s: %v (equal %v)", x.name, r.err, bytes.Equal(r.got, x.want))
		}
	}
	a.Close()
	b.SetReadDeadline(time.Now().Add(15 * time.Second))
	if n, err := b.Read(make([]byte, 10)); err != io.EOF {
		t.Fatalf("after the other side closed: %d %v, want EOF", n, err)
	}
	b.Close()
}

func TestUTPHeader(t *testing.T) {
	h := utpHdr{typ: utpState, conn: 0xBEEF, ts: 1, tsDiff: 2, wnd: 3, seq: 65535, ack: 9, sack: []byte{5, 0, 0, 0}}
	b := h.appendTo(nil)
	if b[0] != 0x21 || b[1] != 1 || len(b) != 26 {
		t.Fatalf("% x", b)
	}
	got, payload, ok := parseUTP(append(b, "data"...))
	if !ok || got.conn != h.conn || got.seq != h.seq || got.ack != h.ack || got.wnd != 3 || !bytes.Equal(got.sack, h.sack) || string(payload) != "data" {
		t.Fatalf("%+v %q %v", got, payload, ok)
	}
	// libtorrent's close-reason extension (3) is skipped
	odd := append(utpHdr{typ: utpFin}.appendTo(nil), 0, 4, 0, 0, 0, 1)
	odd[1] = 3
	if _, p, ok := parseUTP(odd); !ok || len(p) != 0 {
		t.Fatal("unknown extension not skipped")
	}
	for _, other := range [][]byte{[]byte("d1:ad2:id20:abcdefghij0123456789e1:q4:ping1:t2:aa1:y1:qe"), make([]byte, 16), append([]byte{0x00, 0, 0, 0}, make([]byte, 20)...), append([]byte{0x51}, make([]byte, 19)...)} {
		if looksUTP(other) {
			t.Errorf("% x taken for uTP", other[:4])
		}
	}
	if !seqLess(65535, 2) || seqLess(2, 65535) {
		t.Fatal("sequence numbers wrap")
	}
}

func TestUTPTransfer(t *testing.T) {
	a, b, la, lb := utpConnPair(t, 0, 0, 0)
	start := time.Now()
	exchange(t, a, b, 3<<20, 30*time.Second)
	la.mu.Lock()
	lb.mu.Lock()
	t.Logf("%v; packets by type %v %v", time.Since(start).Round(time.Millisecond), la.types, lb.types)
	lb.mu.Unlock()
	la.mu.Unlock()
}

// Packets lost, overtaken and doubled on the way: everything still arrives, in order.
func TestUTPLossyLink(t *testing.T) {
	a, b, la, lb := utpConnPair(t, 0.08, 0.10, 0.03)
	start := time.Now()
	exchange(t, a, b, 1<<20, 90*time.Second)
	la.mu.Lock()
	lb.mu.Lock()
	t.Logf("%v; packets sent %d+%d (by type %v %v), lost %d+%d, doubled %d+%d", time.Since(start).Round(time.Millisecond), la.sent, lb.sent, la.types, lb.types, la.lost, lb.lost, la.copies, lb.copies)
	lb.mu.Unlock()
	la.mu.Unlock()
}

func TestUTPDeadlineAndReset(t *testing.T) {
	a, b, _, _ := utpConnPair(t, 0, 0, 0)
	a.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var ne net.Error
	if _, err := a.Read(make([]byte, 1)); !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read past its deadline: %v", err)
	}
	a.SetReadDeadline(time.Time{})
	b.(*utpConn).s.Close() // the other side goes away: it says so
	a.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := a.Read(make([]byte, 1)); err != errUTPReset {
		t.Fatalf("after a reset: %v", err)
	}
	if _, err := a.Write([]byte("x")); err == nil {
		t.Fatal("write after a reset")
	}
}

func TestUTPDialNobody(t *testing.T) {
	s, _, _ := utpEnd(t, 0, 0, 0, 3)
	quiet, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}) // takes packets, never answers
	defer quiet.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.DialContext(ctx, quiet.LocalAddr().(*net.UDPAddr).AddrPort()); err == nil {
		t.Fatal("connected to nobody")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("gave up after %v", d)
	}
	s.mu.Lock()
	n := len(s.conns)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d connections left in the table", n)
	}
}

// The shared port: uTP goes to uTP, the DHT's packets to the packet endpoint, and both answer
// from the same port.
func TestUDPMux(t *testing.T) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	u := newUDPMux(c)
	defer u.Close()
	port := c.LocalAddr().(*net.UDPAddr).AddrPort()
	peer, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer peer.Close()
	ping := []byte("d1:ad2:id20:abcdefghij0123456789e1:q4:ping1:t2:aa1:y1:qe")
	peer.WriteToUDPAddrPort(ping, port)
	pc := u.packetConn()
	pc.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 2048)
	n, from, err := pc.ReadFrom(buf)
	if err != nil || !bytes.Equal(buf[:n], ping) || from.String() != peer.LocalAddr().String() {
		t.Fatalf("DHT packet: %q from %v (%v)", buf[:n], from, err)
	}
	if _, err := pc.WriteTo([]byte("d1:y1:re"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, src, err := peer.ReadFromUDPAddrPort(buf)
	if err != nil || string(buf[:n]) != "d1:y1:re" || src != port {
		t.Fatalf("reply %q from %v (%v)", buf[:n], src, err)
	}
	// a uTP SYN gets a uTP answer, and nothing reaches the DHT side
	syn := utpHdr{typ: utpSyn, conn: 77, ts: utpNow(), wnd: 1 << 20, seq: 1}
	peer.WriteToUDPAddrPort(syn.appendTo(nil), port)
	n, _, err = peer.ReadFromUDPAddrPort(buf)
	h, _, ok := parseUTP(buf[:n])
	if err != nil || !ok || h.typ != utpState || h.conn != 77 || h.ack != 1 {
		t.Fatalf("SYN answered with %+v %v %v", h, ok, err)
	}
	pc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, _, err := pc.ReadFrom(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the DHT side got something: %v", err)
	}
	if conn, err := u.utp.Accept(); err != nil || conn.RemoteAddr().String() != peer.LocalAddr().String() {
		t.Fatalf("accepted %v %v", conn, err)
	}
	// closing the packet endpoint leaves uTP working
	pc.Close()
	if _, _, err := pc.ReadFrom(buf); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
}

// A reader that falls behind: the window shuts, the sender waits, and reading opens it again.
func TestUTPSlowReader(t *testing.T) {
	a, b, _, _ := utpConnPair(t, 0, 0, 0)
	data := randData(3 << 20)
	done := make(chan error, 1)
	go func() {
		a.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_, err := a.Write(data)
		done <- err
	}()
	time.Sleep(300 * time.Millisecond) // the writer fills our window meanwhile
	if c := b.(*utpConn); func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.rwndLocked() > 2*utpPayload }() {
		t.Fatal("the window should be full by now")
	}
	got := make([]byte, 0, len(data))
	buf := make([]byte, 32<<10)
	b.SetReadDeadline(time.Now().Add(30 * time.Second))
	for len(got) < len(data) {
		n, err := b.Read(buf)
		if err != nil {
			t.Fatalf("after %d bytes: %v", len(got), err)
		}
		got = append(got, buf[:n]...)
		if len(got)%(512<<10) < n {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if err := <-done; err != nil || !bytes.Equal(got, data) {
		t.Fatalf("write %v, equal %v", err, bytes.Equal(got, data))
	}
}
