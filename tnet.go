package main

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// The torrent port's other half: the UDP socket on the TCP listener's port, shared by uTP and
// the DHT (udpmux.go), and the router's port mapping for both (portmap.go). It starts with the
// listener and moves with it.

type torrentNet struct {
	port int
	mux  *udpMux // nil when the UDP port couldn't be had
	pm   *portMapper
}

// startTorrentNet opens the UDP side of the torrent port and starts mapping it. Caller holds m.mu.
func (m *Manager) startTorrentNet(port int) {
	tn := &torrentNet{port: port}
	if c, err := net.ListenUDP("udp", &net.UDPAddr{Port: port}); err == nil {
		tn.mux = newUDPMux(c)
		go m.acceptPeers(noProxyListener{tn.mux.utp})
	} else {
		logf("torrents: UDP port %d isn't free (%v): no uTP on it", port, err)
	}
	tn.pm = newPortMapper(m, port, tn.mux != nil)
	m.tnet = tn
}

// stopTorrentNet closes the UDP side (its uTP connections end) and removes the router's mappings
// in the background: the port is changing.
func (m *Manager) stopTorrentNet() {
	m.mu.Lock()
	tn := m.tnet
	m.tnet = nil
	m.mu.Unlock()
	if tn != nil {
		if tn.mux != nil {
			tn.mux.Close()
		}
		tn.pm.stop(0)
	}
}

// closeTorrentNet is for quitting: the mappings come off the router before Zipline goes.
func (m *Manager) closeTorrentNet() {
	m.mu.Lock()
	tn := m.tnet
	m.mu.Unlock()
	if tn != nil {
		tn.pm.stop(4 * time.Second)
		if tn.mux != nil {
			tn.mux.Close()
		}
	}
}

// torrentNetChanged has the port mapping look at the settings again (mapping or the proxy changed).
func (m *Manager) torrentNetChanged() {
	m.mu.Lock()
	tn := m.tnet
	m.mu.Unlock()
	if torrentProxy() != nil { // (refreshes the cached answer now)
		m.dropDHT() // a node answering others directly would go round the proxy
	}
	if tn != nil {
		tn.pm.poke()
	}
}

// portMapper is the router mapping for the torrent port (nil when there's none). Caller holds m.mu.
func (tn *torrentNet) portMapper() *portMapper {
	if tn == nil {
		return nil
	}
	return tn.pm
}

// torrentUTP is the uTP side of the torrent port (nil when there's none).
func (m *Manager) torrentUTP() *utpSocket {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tnet == nil || m.tnet.mux == nil {
		return nil
	}
	return m.tnet.mux.utp
}

// torrentPacketConn is the shared port's non-uTP traffic, for the DHT (nil when there's none).
// Caller holds m.mu.
func (m *Manager) torrentPacketConn() net.PacketConn {
	if m.tnet == nil || m.tnet.mux == nil {
		return nil
	}
	return m.tnet.mux.packetConn()
}

// noProxyListener turns incoming peers away while torrents go through a proxy.
type noProxyListener struct{ net.Listener }

func (l noProxyListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil || torrentProxy() == nil {
			return c, err
		}
		c.Close()
	}
}

// listenTorrentAny listens on any port whose UDP twin is free too (the chosen port is taken).
func listenTorrentAny() (net.Listener, error) {
	var last error
	for range 8 {
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			return nil, err
		}
		u, err := net.ListenUDP("udp", &net.UDPAddr{Port: ln.Addr().(*net.TCPAddr).Port})
		if err == nil {
			u.Close()
			return ln, nil
		}
		ln.Close()
		last = err
	}
	if ln, err := net.Listen("tcp", ":0"); err == nil { // TCP alone, then
		return ln, nil
	}
	return nil, last
}

// utpHeadStart is how long uTP has before TCP is tried too (about one uTP connect timeout).
var utpHeadStart = time.Second

// utpQuiet remembers peers that didn't answer uTP lately: they get TCP alone for a while.
var utpQuiet sync.Map // netip.AddrPort → time.Time

// dialPeer connects to a peer for torrent.dial: through the proxy when torrents use one (TCP
// only), otherwise uTP first with TCP joining after a second, and whichever connects first is
// kept. uTP first is libtorrent's habit too: its LEDBAT congestion control gives way to the rest
// of the line's traffic, so a big torrent doesn't make browsing and calls lag. The head start
// keeps peers that only speak TCP from waiting long.
func (t *torrent) dialPeer(ctx context.Context, a netip.AddrPort) (net.Conn, error) {
	addr := a.String()
	if pu := torrentProxyFor(a.Addr().String()); pu != nil {
		return dialPeerProxy(ctx, pu, addr)
	}
	u := t.m.torrentUTP()
	if at, ok := utpQuiet.Load(a); ok && time.Since(at.(time.Time)) < 30*time.Minute || u == nil {
		return dialNet(ctx, "tcp", addr)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		c   net.Conn
		err error
		utp bool
	}
	ch := make(chan result, 2)
	go func() {
		c, err := u.DialContext(ctx, a)
		ch <- result{c, err, true}
	}()
	head := time.NewTimer(utpHeadStart)
	defer head.Stop()
	running, tcp := 1, false
	startTCP := func() {
		if !tcp {
			tcp = true
			running++
			go func() {
				c, err := dialNet(ctx, "tcp", addr)
				ch <- result{c, err, false}
			}()
		}
	}
	var utpErr, tcpErr error
	for running > 0 {
		select {
		case <-head.C:
			startTCP()
		case r := <-ch:
			running--
			if r.err == nil {
				if r.utp {
					utpQuiet.Delete(a)
				}
				if running > 0 { // the other one: stopped, or closed if it got through as well
					go func() {
						if x := <-ch; x.c != nil {
							x.c.Close()
						}
					}()
				}
				return r.c, nil
			}
			if r.utp {
				utpErr = r.err
				if ctx.Err() == nil {
					utpQuiet.Store(a, time.Now())
				}
				startTCP()
			} else {
				tcpErr = r.err
			}
		}
	}
	if tcpErr != nil {
		return nil, tcpErr
	}
	return nil, utpErr
}

// TorrentNetView is the torrent port's state for Options → Torrents.
type TorrentNetView struct {
	Port  int         `json:"port"`  // 0 until the first torrent starts
	Proxy string      `json:"proxy"` // torrents go through a proxy: "socks" or "http" ("" = no)
	UTP   bool        `json:"utp"`   // the UDP side is open (uTP and the DHT)
	Map   PortMapView `json:"map"`
}

func (m *Manager) torrentNetView() TorrentNetView {
	var v TorrentNetView
	if pu := torrentProxy(); pu != nil {
		v.Proxy = "http"
		if isSocks(pu) {
			v.Proxy = "socks"
		}
	}
	m.mu.Lock()
	tn, off := m.tnet, m.settings.Torrent.NoPortMap
	m.mu.Unlock()
	switch {
	case tn != nil:
		v.Port, v.UTP = tn.port, tn.mux != nil
		v.Map = tn.pm.state()
	case off:
		v.Map = PortMapView{State: "off"}
	case v.Proxy != "":
		v.Map = PortMapView{State: "proxy"}
	default:
		v.Map = PortMapView{State: "idle"}
	}
	return v
}

func (s *Server) torrentNet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.m.torrentNetView())
}
