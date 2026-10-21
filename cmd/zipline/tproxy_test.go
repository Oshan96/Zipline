package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// tsocks is a SOCKS5 proxy for the torrent tests: CONNECT and UDP ASSOCIATE. Names and the
// documentation addresses 203.0.113.x stand for this computer, so only what goes through the
// proxy can reach anything; it records where it was asked to go.
type tsocks struct {
	ln      net.Listener
	mu      sync.Mutex
	connect []string // CONNECT destinations, as asked
	udpTo   []string // datagram destinations, as asked
}

func newTSocks(t *testing.T) *tsocks {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &tsocks{ln: ln}
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

// real is where a destination really is.
func (s *tsocks) real(host string, port int) string {
	if host == "tracker.test" || strings.HasPrefix(host, "203.0.113.") {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func (s *tsocks) seen(list *[]string, d string) {
	s.mu.Lock()
	*list = append(*list, d)
	s.mu.Unlock()
}

func (s *tsocks) asked(list *[]string, d string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range *list {
		if x == d {
			return true
		}
	}
	return false
}

// readSocksAddr reads type, address and port.
func readSocksAddr(r io.Reader) (string, int, error) {
	var t [1]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return "", 0, err
	}
	var host string
	switch t[0] {
	case 1, 4:
		b := make([]byte, map[byte]int{1: 4, 4: 16}[t[0]])
		io.ReadFull(r, b)
		ip, _ := netip.AddrFromSlice(b)
		host = ip.Unmap().String()
	case 3:
		var n [1]byte
		io.ReadFull(r, n[:])
		b := make([]byte, n[0])
		io.ReadFull(r, b)
		host = string(b)
	default:
		return "", 0, fmt.Errorf("address type %d", t[0])
	}
	var p [2]byte
	_, err := io.ReadFull(r, p[:])
	return host, int(binary.BigEndian.Uint16(p[:])), err
}

func (s *tsocks) serve(c net.Conn) {
	defer c.Close()
	var b [3]byte
	if _, err := io.ReadFull(c, b[:2]); err != nil || b[0] != 5 {
		return
	}
	io.ReadFull(c, make([]byte, b[1]))
	c.Write([]byte{5, 0})
	if _, err := io.ReadFull(c, b[:3]); err != nil {
		return
	}
	host, port, err := readSocksAddr(c)
	if err != nil {
		return
	}
	switch b[1] {
	case 1:
		s.seen(&s.connect, net.JoinHostPort(host, strconv.Itoa(port)))
		out, err := net.DialTimeout("tcp", s.real(host, port), 5*time.Second)
		if err != nil {
			c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
		defer out.Close()
		c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		go io.Copy(out, c)
		io.Copy(c, out)
	case 3:
		relay, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return
		}
		defer relay.Close()
		rp := relay.LocalAddr().(*net.UDPAddr).Port
		c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, byte(rp >> 8), byte(rp)}) // 0.0.0.0: "where you reached me"
		go s.relay(relay)
		io.Copy(io.Discard, c) // the relay lasts as long as this connection
	default:
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
	}
}

func (s *tsocks) relay(relay *net.UDPConn) {
	var client netip.AddrPort
	asked := map[netip.AddrPort][]byte{} // real address → the header the client used for it
	buf := make([]byte, 64<<10)
	for {
		n, from, err := relay.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if !client.IsValid() || from == client {
			client = from
			hl := socksUDPHeader(buf[:n])
			if hl == 0 {
				continue
			}
			host, port, _ := readSocksAddr(strings.NewReader(string(buf[3:hl])))
			s.seen(&s.udpTo, net.JoinHostPort(host, strconv.Itoa(port)))
			to, err := netip.ParseAddrPort(s.real(host, port))
			if err != nil {
				continue
			}
			asked[to] = append([]byte(nil), buf[:hl]...)
			relay.WriteToUDPAddrPort(buf[hl:n], to)
			continue
		}
		if head, ok := asked[from]; ok {
			relay.WriteToUDPAddrPort(append(head, buf[:n]...), client)
		}
	}
}

// Torrents through a SOCKS5 proxy: the HTTP tracker (known by a name only the proxy knows), the
// UDP tracker (through the proxy's UDP relay) and the peer (at an address only the proxy can
// reach) all go through it; uTP, the DHT and incoming peers stay off.
func TestTorrentThroughSOCKS5(t *testing.T) {
	py := ltPython(t)
	proxy := newTSocks(t)
	tt := newTestTracker(t)
	httpPort := tt.srv.Listener.Addr().(*net.TCPAddr).Port
	udpPort := tt.udp.LocalAddr().(*net.UDPAddr).Port
	httpTr := fmt.Sprintf("http://tracker.test:%d/announce", httpPort)
	udpTr := fmt.Sprintf("udp://203.0.113.9:%d", udpPort)

	dir := t.TempDir()
	root := filepath.Join(dir, "Holiday Pack")
	files := makeTestFiles(t, root)
	mk := ltStart(t, py, "make", root, httpTr+","+udpTr, "-", "65536")
	tor := mk.wait(t, "torrent", 30*time.Second)["torrent"].(string)
	seeder := ltStart(t, py, "seed", tor, dir)
	sp := int(seeder.wait(t, "port", 30*time.Second)["port"].(float64))
	b, _ := os.ReadFile(tor)
	mi, _ := parseTorrent(b)
	peer := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.5"), uint16(sp))
	tt.add("http", string(mi.InfoHash[:]), peer, "started") // (the seeder can't reach these trackers itself)
	tt.httpFail = 1                                         // so the UDP tracker, in the next tier, is asked too (BEP 12)

	watch, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}) // a DHT node that must hear nothing
	defer watch.Close()
	old := dhtBootstrap
	dhtBootstrap = []string{watch.LocalAddr().String()}
	defer func() { dhtBootstrap = old }()

	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.NoDHT = false
	m.settings.Proxy = ProxySettings{Mode: "manual", URL: "socks5h://" + proxy.ln.Addr().String()}
	m.mu.Unlock()
	setProxySettings(m.settings.Proxy) // (a new manager sets its own)
	defer setProxySettings(ProxySettings{Mode: "system"})
	d := addTorrentFile(t, m, tor, nil)
	waitFor(t, m, d, StCompleted, 90*time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), files)

	if !proxy.asked(&proxy.connect, fmt.Sprintf("tracker.test:%d", httpPort)) {
		t.Errorf("the HTTP tracker wasn't asked through the proxy: %v", proxy.connect)
	}
	if !proxy.asked(&proxy.connect, peer.String()) {
		t.Errorf("the peer wasn't reached through the proxy: %v", proxy.connect)
	}
	if !proxy.asked(&proxy.udpTo, fmt.Sprintf("203.0.113.9:%d", udpPort)) {
		t.Errorf("the UDP tracker wasn't asked through the proxy's relay: %v", proxy.udpTo)
	}
	tt.mu.Lock()
	hits := fmt.Sprint(tt.hits)
	tt.mu.Unlock()
	if !strings.Contains(hits, "udp:") || !strings.Contains(hits, "http:") {
		t.Errorf("tracker announces: %s", hits)
	}
	if u := m.torrentUTP(); u != nil && u.Dialed.Load() > 0 {
		t.Error("uTP was used while a proxy is on")
	}
	watch.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := watch.ReadFrom(make([]byte, 100)); err == nil {
		t.Error("the DHT was asked while a proxy is on")
	}
	if v := m.torrentNetView(); v.Proxy != "socks" || v.Map.State != "proxy" {
		t.Errorf("Options → Torrents would show %+v", v)
	}
	// someone connecting in directly is turned away
	m.mu.Lock()
	port := m.tport
	m.mu.Unlock()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write(handshakeBytes(mi.InfoHash, newPeerID()))
	if _, err := io.ReadFull(c, make([]byte, 68)); err == nil {
		t.Error("an incoming peer got in while a proxy is on")
	}
}

// With an HTTP proxy, peers go through CONNECT tunnels and UDP trackers aren't used.
func TestTorrentPeersThroughHTTPProxy(t *testing.T) {
	echo, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	var mu sync.Mutex
	var tunnels []string
	px := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Header.Get("Proxy-Authorization") != "Basic Ym9iOnMzY3JldA==" {
			http.Error(w, "no", 407)
			return
		}
		mu.Lock()
		tunnels = append(tunnels, r.Host)
		mu.Unlock()
		_, port, _ := net.SplitHostPort(r.Host)
		out, err := net.Dial("tcp", "127.0.0.1:"+port)
		if err != nil {
			http.Error(w, "unreachable", 502)
			return
		}
		c, rw, _ := w.(http.Hijacker).Hijack()
		c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() { defer out.Close(); io.Copy(out, rw) }()
		io.Copy(c, out)
		c.Close()
	})}
	pl, _ := net.Listen("tcp4", "127.0.0.1:0")
	go px.Serve(pl)
	defer px.Close()

	setProxySettings(ProxySettings{Mode: "manual", URL: pl.Addr().String(), User: "bob", Pass: "s3cret"})
	defer setProxySettings(ProxySettings{Mode: "system"})
	if pu := torrentProxy(); pu == nil || pu.Scheme != "http" {
		t.Fatalf("torrent proxy %v", pu)
	}
	if torrentProxyFor("127.0.0.1") != nil {
		t.Fatal("this computer never goes through the proxy")
	}
	_, port, _ := net.SplitHostPort(echo.Addr().String())
	target := "203.0.113.5:" + port
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialPeerProxy(ctx, torrentProxyFor("203.0.113.5"), target)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello\n"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if line, err := bufio.NewReader(c).ReadString('\n'); err != nil || line != "hello\n" {
		t.Fatalf("through the tunnel: %q %v", line, err)
	}
	mu.Lock()
	got := strings.Join(tunnels, " ")
	mu.Unlock()
	if got != target {
		t.Fatalf("tunnels: %q", got)
	}
	if _, err := trackerUDPDial(ctx, "203.0.113.9:6969"); err != errUDPTrackerHTTPProxy {
		t.Fatalf("UDP tracker with an HTTP proxy: %v", err)
	}
	// a host on the bypass list goes direct
	setProxySettings(ProxySettings{Mode: "manual", URL: pl.Addr().String(), Bypass: "203.0.113.*"})
	if torrentProxyFor("203.0.113.5") != nil || torrentProxyFor("198.51.100.1") == nil {
		t.Fatal("bypass list")
	}
	// a system proxy only counts for torrents when it's SOCKS
	setProxySettings(ProxySettings{Mode: "none"})
	if torrentProxy() != nil {
		t.Fatal("no proxy")
	}
}
