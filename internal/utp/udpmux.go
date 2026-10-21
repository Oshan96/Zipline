package utp

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"
)

// Mux owns the UDP socket on the torrent port and shares it out: uTP packets go to the uTP
// layer, everything else (the DHT's bencoded messages, which start with 'd') to packetConn().
// One port for TCP, uTP and the DHT is what the router maps and what other peers expect.
type Mux struct {
	conn  *net.UDPConn
	UTP   *Socket
	other *muxPacketConn
	done  chan struct{}
	once  sync.Once
}

func NewMux(conn *net.UDPConn) *Mux {
	conn.SetReadBuffer(4 << 20) // a window's worth can arrive at once
	conn.SetWriteBuffer(1 << 20)
	u := &Mux{conn: conn, done: make(chan struct{})}
	u.UTP = newUTPSocket(func(b []byte, to netip.AddrPort) error {
		_, err := conn.WriteToUDPAddrPort(b, to)
		return err
	}, conn.LocalAddr())
	u.other = &muxPacketConn{u: u, in: make(chan muxPacket, 256), closed: make(chan struct{})}
	go u.readLoop()
	return u
}

// PacketConn is the socket's non-uTP traffic, for the DHT: what it writes goes out of the shared port.
func (u *Mux) PacketConn() net.PacketConn { return u.other }

func (u *Mux) Close() error {
	u.once.Do(func() {
		close(u.done)
		u.UTP.Close()
		u.other.Close()
		u.conn.Close()
	})
	return nil
}

func (u *Mux) readLoop() {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := u.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			select {
			case <-u.done:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond) // e.g. an ICMP error reported on Windows: carry on
			continue
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		if looksUTP(buf[:n]) {
			u.UTP.handle(buf[:n], from)
			continue
		}
		select { // (a reader that falls behind loses packets, as UDP would)
		case u.other.in <- muxPacket{append([]byte(nil), buf[:n]...), from}:
		default:
		}
	}
}

type muxPacket struct {
	b    []byte
	from netip.AddrPort
}

// muxPacketConn is a net.PacketConn over the shared socket; closing it leaves the socket open.
type muxPacketConn struct {
	u      *Mux
	in     chan muxPacket
	closed chan struct{}
	once   sync.Once

	mu   sync.Mutex
	rdl  time.Time
	wake chan struct{} // closed when the read deadline changes
}

func (c *muxPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		c.mu.Lock()
		dl := c.rdl
		if c.wake == nil {
			c.wake = make(chan struct{})
		}
		wake := c.wake
		c.mu.Unlock()
		select {
		case <-c.closed:
			return 0, nil, net.ErrClosed
		default:
		}
		if !dl.IsZero() && !time.Now().Before(dl) {
			return 0, nil, os.ErrDeadlineExceeded
		}
		n, from, again, err := c.wait(b, dl, wake)
		if !again {
			return n, from, err
		}
	}
}

// wait takes the next packet; again is true when the deadline changed meanwhile.
func (c *muxPacketConn) wait(b []byte, dl time.Time, wake chan struct{}) (n int, from net.Addr, again bool, err error) {
	var timeout <-chan time.Time
	if !dl.IsZero() {
		t := time.NewTimer(time.Until(dl))
		defer t.Stop()
		timeout = t.C
	}
	select {
	case p := <-c.in:
		return copy(b, p.b), net.UDPAddrFromAddrPort(p.from), false, nil
	case <-c.closed:
		return 0, nil, false, net.ErrClosed
	case <-timeout:
		return 0, nil, false, os.ErrDeadlineExceeded
	case <-wake:
		return 0, nil, true, nil
	}
}

func (c *muxPacketConn) WriteTo(b []byte, to net.Addr) (int, error) {
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}
	if ua, ok := to.(*net.UDPAddr); ok {
		return c.u.conn.WriteToUDP(b, ua)
	}
	return c.u.conn.WriteTo(b, to)
}

func (c *muxPacketConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *muxPacketConn) LocalAddr() net.Addr { return c.u.conn.LocalAddr() }

func (c *muxPacketConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

func (c *muxPacketConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.rdl = t
	if c.wake != nil {
		close(c.wake)
	}
	c.wake = make(chan struct{})
	c.mu.Unlock()
	return nil
}

func (c *muxPacketConn) SetWriteDeadline(time.Time) error { return nil } // (UDP writes don't wait)
