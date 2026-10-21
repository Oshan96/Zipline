package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// socksd is a tiny SOCKS5 proxy (RFC 1928/1929). Names in hosts resolve to the given IPs,
// as the proxy's own DNS would; it records every destination it's asked for.
type socksd struct {
	ln         net.Listener
	user, pass string // "" = no login
	hosts      map[string]string
	mu         sync.Mutex
	asked      []string
}

func newSocksd(t *testing.T, user, pass string, hosts map[string]string) *socksd {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &socksd{ln: ln, user: user, pass: pass, hosts: hosts}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return s
}

func (s *socksd) url(scheme, login string) *url.URL {
	u, _ := url.Parse(scheme + "://" + login + s.ln.Addr().String())
	return u
}

func (s *socksd) destinations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

func (s *socksd) serve(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	b := make([]byte, 2)
	if _, err := io.ReadFull(c, b); err != nil || b[0] != 5 {
		return
	}
	methods := make([]byte, b[1])
	io.ReadFull(c, methods)
	want := byte(0)
	if s.user != "" {
		want = 2
	}
	if !bytes.Contains(methods, []byte{want}) {
		c.Write([]byte{5, 0xff})
		return
	}
	c.Write([]byte{5, want})
	if want == 2 {
		field := func() string {
			var n [1]byte
			io.ReadFull(c, n[:])
			f := make([]byte, n[0])
			io.ReadFull(c, f)
			return string(f)
		}
		var ver [1]byte
		io.ReadFull(c, ver[:])
		if u, p := field(), field(); u != s.user || p != s.pass {
			c.Write([]byte{1, 1})
			return
		}
		c.Write([]byte{1, 0})
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1, 4:
		ip := make([]byte, map[byte]int{1: 4, 4: 16}[req[3]])
		io.ReadFull(c, ip)
		host = net.IP(ip).String()
	case 3:
		var n [1]byte
		io.ReadFull(c, n[:])
		name := make([]byte, n[0])
		io.ReadFull(c, name)
		host = string(name)
	}
	var pb [2]byte
	io.ReadFull(c, pb[:])
	port := strconv.Itoa(int(binary.BigEndian.Uint16(pb[:])))
	s.mu.Lock()
	s.asked = append(s.asked, net.JoinHostPort(host, port))
	s.mu.Unlock()
	if ip, ok := s.hosts[host]; ok {
		host = ip
	}
	dst, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 5*time.Second)
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0}) // connection refused
		return
	}
	defer dst.Close()
	if req[3] == 3 { // answer names with a name, to cover that reply form too
		c.Write(append([]byte{5, 0, 0, 3, 9}, "bnd.proxy\x04\x38"...))
	} else {
		c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 4, 56})
	}
	c.SetDeadline(time.Time{})
	go func() {
		io.Copy(dst, c)
		dst.(*net.TCPConn).CloseWrite()
	}()
	io.Copy(c, dst)
}

func echoServer(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func socksEcho(t *testing.T, c net.Conn) {
	t.Helper()
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("ping"))
	b := make([]byte, 4)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "ping" {
		t.Fatalf("echo through the proxy: %q %v", b, err)
	}
}

func TestSocksDial(t *testing.T) {
	port := echoServer(t)
	ctx := context.Background()
	for _, auth := range []struct{ user, pass, login string }{{"", "", ""}, {"bob", "s3cret", "bob:s3cret@"}} {
		proxy := newSocksd(t, auth.user, auth.pass, map[string]string{"echo.test": "127.0.0.1"})
		for _, c := range []struct{ scheme, addr, asked string }{
			{"socks5", "127.0.0.1:" + port, "127.0.0.1:" + port},
			{"socks5", "localhost:" + port, "127.0.0.1:" + port},  // resolved here
			{"socks5h", "echo.test:" + port, "echo.test:" + port}, // the proxy resolves it
		} {
			conn, err := dialSocks(ctx, proxy.url(c.scheme, auth.login), c.addr)
			if err != nil {
				t.Fatalf("%s %s (login %q): %v", c.scheme, c.addr, auth.login, err)
			}
			if h, _, _ := net.SplitHostPort(c.asked); conn.(*socksConn).host != h {
				t.Fatalf("%s: tunnel to %q, want %q", c.addr, conn.(*socksConn).host, h)
			}
			socksEcho(t, conn)
			if got := proxy.destinations(); got[len(got)-1] != c.asked {
				t.Fatalf("%s %s: proxy was asked for %q, want %q", c.scheme, c.addr, got[len(got)-1], c.asked)
			}
		}
	}
}

func TestSocksRefused(t *testing.T) {
	ctx := context.Background()
	port := echoServer(t)
	proxy := newSocksd(t, "bob", "s3cret", nil)
	var pe permanentErr
	_, err := dialSocks(ctx, proxy.url("socks5", "bob:nope@"), "127.0.0.1:"+port)
	if !errors.Is(err, errSocksAuth) || !errors.As(err, &pe) {
		t.Fatalf("wrong password: %v", err)
	}
	_, err = dialSocks(ctx, proxy.url("socks5", ""), "127.0.0.1:"+port)
	if !errors.Is(err, errSocksNeedsAuth) || !errors.As(err, &pe) {
		t.Fatalf("no login: %v", err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	_, err = dialSocks(ctx, proxy.url("socks5", "bob:s3cret@"), closed)
	if err == nil || !strings.Contains(err.Error(), "connection refused") || errors.As(err, &pe) {
		t.Fatalf("unreachable server: %v", err)
	}
}

func TestSocksHonoursContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0") // accepts, then never answers
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	pu, _ := url.Parse("socks5://" + ln.Addr().String())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, err := dialSocks(ctx, pu, "127.0.0.1:1"); err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("deadline: %v after %v", err, time.Since(t0))
	}
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	t0 = time.Now()
	if _, err := dialSocks(ctx, pu, "127.0.0.1:1"); !errors.Is(err, context.Canceled) || time.Since(t0) > 3*time.Second {
		t.Fatalf("cancel: %v after %v", err, time.Since(t0))
	}
}

// With a SOCKS5 proxy set, FTP's control and data connections both go through it, here with
// the proxy resolving the name and TLS checked against that name.
func TestFTPThroughSocks(t *testing.T) {
	cfg, pool := testCert(t)
	trustFTPTLS(t, pool)
	data := randData(3 << 20)
	srv := newFTPD(t, func(s *ftpd) { s.files["/pub/file.bin"] = data; s.tls, s.rate = cfg, 2<<20 })
	proxy := newSocksd(t, "bob", "s3cret", map[string]string{"files.test": "127.0.0.1"})
	m := newTestManager(t)
	m.settings.Conns = 4
	defer setProxySettings(ProxySettings{Mode: "system"})
	setProxySettings(ProxySettings{Mode: "manual", URL: "socks5h://" + proxy.ln.Addr().String(), User: "bob", Pass: "s3cret"})
	_, port, _ := net.SplitHostPort(srv.ln.Addr().String())
	d := m.Add(AddReq{URL: "ftp://files.test:" + port + "/pub/file.bin", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	fileIs(t, d, data)
	control, dataConns := 0, 0
	for _, a := range proxy.destinations() {
		switch {
		case a == "files.test:"+port:
			control++
		case strings.HasPrefix(a, "files.test:"):
			dataConns++
		default:
			t.Fatalf("proxy asked for %q", a)
		}
	}
	if control < 2 || dataConns < 2 || srv.tlsData != srv.retrs || srv.plainLogins != 0 {
		t.Fatalf("%d control and %d data connections through the proxy; %d of %d transfers over TLS, %d plain logins",
			control, dataConns, srv.tlsData, srv.retrs, srv.plainLogins)
	}
}
