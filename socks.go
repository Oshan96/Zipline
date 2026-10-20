package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"time"
)

// A SOCKS5 client (RFC 1928) with user name and password login (RFC 1929), for connections
// net/http doesn't make itself: FTP's control and data connections, torrent peers, and UDP
// trackers through UDP ASSOCIATE (tproxy.go).

const socksTimeout = 30 * time.Second // for the proxy to answer and reach the server

var (
	errSocksAuth      = errors.New("the SOCKS proxy refused the user name and password (Options → Proxy)")
	errSocksNeedsAuth = errors.New("the SOCKS proxy needs a user name and password (Options → Proxy)")
)

// socksConn is a connection through the proxy. host is the server as the proxy was told
// it: an IP when the name was resolved here (socks5://), the name itself with socks5h://.
type socksConn struct {
	net.Conn
	host string
}

// dialSocks connects to addr (host:port) through the SOCKS5 proxy pu. socks5:// resolves
// the name here; socks5h:// leaves it to the proxy, which may reach names we can't.
func dialSocks(ctx context.Context, pu *url.URL, addr string) (net.Conn, error) {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("bad port in %q", addr)
	}
	if pu.Scheme != "socks5h" && net.ParseIP(host) == nil {
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		host = ips[0].String()
		for _, ip := range ips { // IPv4 first: proxies often have no IPv6 route
			if ip.To4() != nil {
				host = ip.String()
				break
			}
		}
	}
	proxy := pu.Host
	if pu.Port() == "" {
		proxy = net.JoinHostPort(pu.Hostname(), "1080")
	}
	c, err := dialNet(ctx, "tcp", proxy)
	if err != nil {
		return nil, err
	}
	// The handshake ends with ctx or after socksTimeout; the tunnel itself has no deadline.
	dl := time.Now().Add(socksTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	c.SetDeadline(dl)
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(time.Unix(1, 0)) })
	err = socksHandshake(c, pu.User, host, port)
	if !stop() {
		err = ctx.Err()
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	return &socksConn{Conn: c, host: host}, nil
}

var socksReplies = [...]string{1: "general failure", 2: "connection not allowed by its rules",
	3: "network unreachable", 4: "host unreachable", 5: "connection refused", 6: "TTL expired",
	7: "command not supported", 8: "address type not supported"}

func socksHandshake(c net.Conn, user *url.Userinfo, host string, port int) error {
	if err := socksLogin(c, user); err != nil {
		return err
	}
	_, err := socksRequest(c, 1, host, port) // CONNECT
	return err
}

// socksLogin greets the proxy and logs in when it asks for a user name and password.
func socksLogin(c net.Conn, user *url.Userinfo) error {
	hello := []byte{5, 1, 0} // no login
	if user != nil {
		hello = []byte{5, 2, 0, 2} // no login, or user name and password
	}
	if _, err := c.Write(hello); err != nil {
		return err
	}
	var b [2]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return err
	}
	if b[0] != 5 {
		return errors.New("that's not a SOCKS5 proxy")
	}
	switch {
	case b[1] == 0:
	case b[1] == 2 && user != nil:
		name := user.Username()
		pass, _ := user.Password()
		if len(name) > 255 || len(pass) > 255 {
			return permanentErr{errors.New("the SOCKS proxy user name or password is too long")}
		}
		req := append([]byte{1, byte(len(name))}, name...)
		req = append(append(req, byte(len(pass))), pass...)
		if _, err := c.Write(req); err != nil {
			return err
		}
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return err
		}
		if b[1] != 0 {
			return permanentErr{errSocksAuth}
		}
	case user == nil:
		return permanentErr{errSocksNeedsAuth}
	default:
		return permanentErr{errors.New("the SOCKS proxy wants a kind of login Zipline doesn't support")}
	}
	return nil
}

// socksAddr is a SOCKS address: type, address, port.
func socksAddr(host string, port int) ([]byte, error) {
	var b []byte
	if ip := net.ParseIP(host); ip == nil {
		if len(host) > 255 {
			return nil, errors.New("host name too long for SOCKS")
		}
		b = append(append(b, 3, byte(len(host))), host...)
	} else if ip4 := ip.To4(); ip4 != nil {
		b = append(append(b, 1), ip4...)
	} else {
		b = append(append(b, 4), ip.To16()...)
	}
	return binary.BigEndian.AppendUint16(b, uint16(port)), nil
}

// socksRequest sends a command (1 CONNECT, 3 UDP ASSOCIATE) and returns the address the proxy
// gives back: for UDP, where its relay listens.
func socksRequest(c net.Conn, cmd byte, host string, port int) (netip.AddrPort, error) {
	addr, err := socksAddr(host, port)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if _, err := c.Write(append([]byte{5, cmd, 0}, addr...)); err != nil {
		return netip.AddrPort{}, err
	}
	// The reply: version, status, reserved, then the proxy's own address.
	var h [4]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return netip.AddrPort{}, err
	}
	if h[0] != 5 {
		return netip.AddrPort{}, errors.New("bad reply from the SOCKS proxy")
	}
	if h[1] != 0 {
		why := "error " + strconv.Itoa(int(h[1]))
		if int(h[1]) < len(socksReplies) {
			why = socksReplies[h[1]]
		}
		if cmd == 3 {
			return netip.AddrPort{}, fmt.Errorf("the SOCKS proxy won't carry UDP: %s", why)
		}
		return netip.AddrPort{}, fmt.Errorf("SOCKS proxy couldn't reach %s: %s", net.JoinHostPort(host, strconv.Itoa(port)), why)
	}
	n := 0
	var b [1]byte
	switch h[3] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		if _, err := io.ReadFull(c, b[:]); err != nil {
			return netip.AddrPort{}, err
		}
		n = int(b[0])
	default:
		return netip.AddrPort{}, errors.New("bad reply from the SOCKS proxy")
	}
	rest := make([]byte, n+2)
	if _, err := io.ReadFull(c, rest); err != nil {
		return netip.AddrPort{}, err
	}
	ip, _ := netip.AddrFromSlice(rest[:n]) // (a name, which proxies don't send here, gives none)
	if h[3] == 3 {
		ip = netip.Addr{}
	}
	return netip.AddrPortFrom(ip.Unmap(), binary.BigEndian.Uint16(rest[n:])), nil
}
