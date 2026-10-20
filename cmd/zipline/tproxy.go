package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Torrents through the proxy (Options → Connection → Proxy). With a SOCKS5 proxy, peers and
// HTTP trackers go through it, and UDP trackers through its UDP relay (UDP ASSOCIATE). With an
// HTTP proxy, HTTP trackers go through it and peers through CONNECT tunnels, the way libtorrent
// does; UDP trackers can't. Either way, as in libtorrent and qBittorrent, nothing goes around
// the proxy while it's on: no DHT, no uTP, no incoming peers and no port mapping, since each
// would show this computer's own address.
//
// Which proxy: one set by hand in Options applies (its bypass list too). A system proxy applies
// only when it's SOCKS: a system HTTP proxy is there for browsing and usually tunnels nothing
// but web ports, so torrents go direct, as other BitTorrent apps do.

var (
	errUDPTrackerHTTPProxy = errors.New("UDP trackers can't go through an HTTP proxy")
	torrentProxyOn         atomic.Bool // the last answer of torrentProxy (for code that mustn't wait)
)

var tproxyCache struct {
	mu  sync.Mutex
	cfg ProxySettings
	at  time.Time
	pu  *url.URL
}

// tproxyProbe is the address asked about for the system's proxy: one on the internet.
var tproxyProbe = &url.URL{Scheme: "http", Host: "192.0.2.1:6881"}

// torrentProxy is the proxy torrents go through, nil for none.
func torrentProxy() *url.URL {
	p, _ := proxyCfg.Load().(ProxySettings)
	tproxyCache.mu.Lock()
	defer tproxyCache.mu.Unlock()
	if tproxyCache.cfg == p && time.Since(tproxyCache.at) < time.Minute {
		return tproxyCache.pu
	}
	var pu *url.URL
	switch p.Mode {
	case "manual":
		if u, err := proxyForWith(p, tproxyProbe); err == nil {
			pu = u
		}
	case "none":
	default: // the system's, if it's SOCKS (a PAC script is asked once a minute at most)
		if u, err := proxyFor(tproxyProbe); err == nil && u != nil && (u.Scheme == "socks5" || u.Scheme == "socks5h") {
			pu = u
		}
	}
	tproxyCache.cfg, tproxyCache.at, tproxyCache.pu = p, time.Now(), pu
	torrentProxyOn.Store(pu != nil)
	return pu
}

// torrentProxyFor is the proxy for a connection to host (an address or a name): none for this
// computer and, with a proxy set by hand, for what its bypass list names.
func torrentProxyFor(host string) *url.URL {
	pu := torrentProxy()
	if pu == nil || tproxyDirect(host) {
		return nil
	}
	if p, _ := proxyCfg.Load().(ProxySettings); p.Mode == "manual" && bypassed(host, p.Bypass) {
		return nil
	}
	return pu
}

// tproxyDirect: hosts that never go through the proxy (tests let loopback through it).
var tproxyDirect = isLoopback

func isSocks(pu *url.URL) bool { return pu.Scheme == "socks5" || pu.Scheme == "socks5h" }

// dialPeerProxy connects to a peer through the proxy: SOCKS5 CONNECT or an HTTP CONNECT tunnel.
func dialPeerProxy(ctx context.Context, pu *url.URL, addr string) (net.Conn, error) {
	if isSocks(pu) {
		return dialSocks(ctx, pu, addr)
	}
	return dialHTTPConnect(ctx, pu, addr)
}

// dialHTTPConnect opens a tunnel to addr through an HTTP(S) proxy.
func dialHTTPConnect(ctx context.Context, pu *url.URL, addr string) (net.Conn, error) {
	proxy := pu.Host
	if pu.Port() == "" {
		proxy = net.JoinHostPort(pu.Hostname(), map[bool]string{true: "443", false: "80"}[pu.Scheme == "https"])
	}
	c, err := dialNet(ctx, "tcp", proxy)
	if err != nil {
		return nil, err
	}
	dl := time.Now().Add(socksTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	c.SetDeadline(dl)
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Unix(1, 0)) })
	conn, err := httpConnect(c, pu, addr)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

func httpConnect(c net.Conn, pu *url.URL, addr string) (net.Conn, error) {
	if pu.Scheme == "https" {
		tc := tls.Client(c, &tls.Config{ServerName: pu.Hostname()})
		if err := tc.Handshake(); err != nil {
			return nil, err
		}
		c = tc
	}
	req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\nUser-Agent: Zipline/" + appVersion + "\r\n"
	if pu.User != nil {
		pass, _ := pu.User.Password()
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(pu.User.Username()+":"+pass)) + "\r\n"
	}
	if _, err := c.Write([]byte(req + "\r\n")); err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == 407:
		return nil, permanentErr{errors.New("the proxy wants a user name and password (Options → Proxy)")}
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("the proxy didn't open a tunnel to %s: %s", addr, resp.Status)
	}
	return &bufConn{Conn: c, r: br}, nil
}

// bufConn reads what the reader already took in before the connection itself.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *bufConn) NetConn() net.Conn          { return c.Conn }

// trackerUDPDial opens a UDP tracker's socket: through the SOCKS proxy's relay when torrents use one.
var trackerUDPDial = func(ctx context.Context, hostport string) (net.Conn, error) {
	if pu := torrentProxyFor(hostOnly(hostport)); pu != nil {
		if !isSocks(pu) {
			return nil, errUDPTrackerHTTPProxy
		}
		return dialSocksUDP(ctx, pu, hostport)
	}
	var d net.Dialer
	return d.DialContext(ctx, "udp", hostport)
}

// socksUDPConn carries one tracker's datagrams through a SOCKS5 relay (RFC 1928 §7). The relay
// lasts as long as the control connection that asked for it.
type socksUDPConn struct {
	*net.UDPConn // to the relay
	ctrl         net.Conn
	head         []byte // the header every datagram to the tracker starts with
	raddr        net.Addr
}

// dialSocksUDP sets up a UDP relay through the SOCKS5 proxy pu for the tracker at hostport.
func dialSocksUDP(ctx context.Context, pu *url.URL, hostport string) (net.Conn, error) {
	host, ps, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("bad port in %q", hostport)
	}
	var raddr net.Addr = socksName(hostport)
	if pu.Scheme != "socks5h" && net.ParseIP(host) == nil { // socks5://: names are looked up here
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		ip := ips[0]
		for _, x := range ips { // IPv4 first: proxies often have no IPv6 route
			if x.Unmap().Is4() {
				ip = x
				break
			}
		}
		host = ip.Unmap().String()
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		raddr = net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip.Unmap(), uint16(port)))
	}
	proxy := pu.Host
	if pu.Port() == "" {
		proxy = net.JoinHostPort(pu.Hostname(), "1080")
	}
	ctrl, err := dialNet(ctx, "tcp", proxy)
	if err != nil {
		return nil, err
	}
	dl := time.Now().Add(socksTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	ctrl.SetDeadline(dl)
	relay, err := func() (netip.AddrPort, error) {
		if err := socksLogin(ctrl, pu.User); err != nil {
			return netip.AddrPort{}, err
		}
		return socksRequest(ctrl, 3, "0.0.0.0", 0) // (we don't know our own address and port yet)
	}()
	if err != nil {
		ctrl.Close()
		return nil, err
	}
	ctrl.SetDeadline(time.Time{})
	if !relay.Addr().IsValid() || relay.Addr().IsUnspecified() { // "the proxy's own address"
		pa, _ := netip.ParseAddrPort(ctrl.RemoteAddr().String())
		relay = netip.AddrPortFrom(pa.Addr().Unmap(), relay.Port())
	}
	uc, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(relay))
	if err != nil {
		ctrl.Close()
		return nil, err
	}
	addr, err := socksAddr(host, port)
	if err != nil {
		ctrl.Close()
		uc.Close()
		return nil, err
	}
	c := &socksUDPConn{UDPConn: uc, ctrl: ctrl, head: append([]byte{0, 0, 0}, addr...), raddr: raddr}
	go func() { // the proxy ends the relay by closing the control connection
		ctrl.Read(make([]byte, 1))
		uc.Close()
	}()
	return c, nil
}

func (c *socksUDPConn) Write(b []byte) (int, error) {
	pkt := append(append(make([]byte, 0, len(c.head)+len(b)), c.head...), b...)
	if _, err := c.UDPConn.Write(pkt); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *socksUDPConn) Read(b []byte) (int, error) {
	buf := make([]byte, len(b)+262)
	for {
		n, err := c.UDPConn.Read(buf)
		if err != nil {
			return 0, err
		}
		if hl := socksUDPHeader(buf[:n]); hl > 0 {
			return copy(b, buf[hl:n]), nil
		}
	}
}

// socksUDPHeader is the length of a relayed datagram's header (0: malformed, or a fragment,
// which Zipline doesn't put together).
func socksUDPHeader(b []byte) int {
	if len(b) < 4 || b[2] != 0 {
		return 0
	}
	n := 0
	switch b[3] {
	case 1:
		n = 4 + 4
	case 4:
		n = 4 + 16
	case 3:
		if len(b) < 5 {
			return 0
		}
		n = 5 + int(b[4])
	default:
		return 0
	}
	if len(b) < n+2 {
		return 0
	}
	return n + 2
}

func (c *socksUDPConn) Close() error {
	c.ctrl.Close()
	return c.UDPConn.Close()
}

func (c *socksUDPConn) RemoteAddr() net.Addr { return c.raddr }

// socksName is a tracker known by name only (the proxy looks it up).
type socksName string

func (socksName) Network() string  { return "udp" }
func (a socksName) String() string { return string(a) }
