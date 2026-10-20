package main

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Several internet connections at once (Options → Connection). Each new connection to a server
// goes out through the next network adapter in turn (say Wi-Fi, Ethernet and a phone tethered
// over USB), so their speeds add up. Zipline's dynamic splitting gives the faster adapters more
// of the work. An adapter that can't reach the internet is skipped for a minute.

type MultiNet struct {
	On  bool     `json:"on"`
	Use []string `json:"use"` // adapter names; empty = every adapter with an address
}

type netIf struct {
	Name  string `json:"name"`
	Index int    `json:"-"`
	IP    string `json:"ip"`
	Kind  string `json:"kind"` // wifi, ethernet, mobile, vpn, other
}

// listNetIfs returns the adapters that could carry traffic (replaced in tests).
var listNetIfs = systemNetIfs

func ifKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "wi-fi") || strings.Contains(n, "wifi") || strings.Contains(n, "wlan") || strings.HasPrefix(n, "wl") || strings.Contains(n, "wireless"):
		return "wifi"
	case strings.Contains(n, "rmnet") || strings.Contains(n, "wwan") || strings.Contains(n, "rndis") || strings.Contains(n, "cellular") || strings.Contains(n, "mobile") || strings.HasPrefix(n, "usb"):
		return "mobile"
	case strings.HasPrefix(n, "tun") || strings.HasPrefix(n, "tap") || strings.HasPrefix(n, "wg") || strings.HasPrefix(n, "utun") || strings.HasPrefix(n, "ppp") || strings.Contains(n, "vpn"):
		return "vpn"
	case strings.Contains(n, "ethernet") || strings.HasPrefix(n, "eth") || strings.HasPrefix(n, "en") || strings.HasPrefix(n, "lan"):
		return "ethernet"
	}
	return "other"
}

func systemNetIfs() []netIf {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []netIf
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLinkLocalUnicast() || ipn.IP.IsLoopback() {
				continue
			}
			out = append(out, netIf{Name: i.Name, Index: i.Index, IP: ipn.IP.String(), Kind: ifKind(i.Name)})
			break
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

type multiDialer struct {
	mu    sync.Mutex
	on    bool
	use   map[string]bool
	ifs   []netIf
	at    time.Time // when ifs was read
	next  int
	bad   map[string]time.Time
	bytes map[string]*int64 // received, per adapter
	sent  map[string]*int64 // sent, per adapter
	lims  map[string]*adapterLims
}

var mdial = &multiDialer{bad: map[string]time.Time{}, bytes: map[string]*int64{}, sent: map[string]*int64{}}

func (md *multiDialer) configure(c MultiNet) {
	md.mu.Lock()
	defer md.mu.Unlock()
	md.on = c.On
	md.use = map[string]bool{}
	for _, n := range c.Use {
		md.use[n] = true
	}
	md.at = time.Time{} // re-read the adapters
}

// adapters returns the adapters in use (refreshed every few seconds). Caller holds md.mu.
func (md *multiDialer) adapters() []netIf {
	if time.Since(md.at) > 5*time.Second {
		md.ifs, md.at = listNetIfs(), time.Now()
	}
	var out []netIf
	for _, i := range md.ifs {
		if len(md.use) == 0 || md.use[i.Name] {
			out = append(out, i)
		}
	}
	return out
}

func (md *multiDialer) counter(name string) *int64 {
	md.mu.Lock()
	defer md.mu.Unlock()
	c := md.bytes[name]
	if c == nil {
		c = new(int64)
		md.bytes[name] = c
	}
	return c
}

func (md *multiDialer) sentCounter(name string) *int64 {
	md.mu.Lock()
	defer md.mu.Unlock()
	if md.sent == nil {
		md.sent = map[string]*int64{}
	}
	c := md.sent[name]
	if c == nil {
		c = new(int64)
		md.sent[name] = c
	}
	return c
}

// adapterConn wraps a connection made through one adapter: counted, and slowed by polite mode's
// limits for that adapter.
func adapterConn(c net.Conn, i netIf) *countingConn {
	return &countingConn{Conn: c, n: mdial.counter(i.Name), sent: mdial.sentCounter(i.Name), lim: mdial.limits(i.Name), local: i}
}

// pick returns the adapters to try for the next connection, best first.
func (md *multiDialer) pick() []netIf {
	md.mu.Lock()
	defer md.mu.Unlock()
	if !md.on {
		return nil
	}
	ifs := md.adapters()
	if len(ifs) < 2 {
		return nil // nothing to combine: dial normally
	}
	var good, bad []netIf
	n := len(ifs)
	for k := 0; k < n; k++ {
		i := ifs[(md.next+k)%n]
		if t, ok := md.bad[i.Name]; ok && time.Since(t) < time.Minute {
			bad = append(bad, i)
		} else {
			good = append(good, i)
		}
	}
	md.next = (md.next + 1) % n
	return append(good, bad...)
}

func (md *multiDialer) markBad(name string, bad bool) {
	md.mu.Lock()
	if bad {
		md.bad[name] = time.Now()
	} else {
		delete(md.bad, name)
	}
	md.mu.Unlock()
}

var baseDialer = &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}

// dialVia opens a connection through one adapter.
func dialVia(ctx context.Context, network, addr string, i netIf) (net.Conn, error) {
	d := &net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second,
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(i.IP)},
		Control: func(_, _ string, c syscall.RawConn) error {
			var serr error
			c.Control(func(fd uintptr) { serr = bindToInterface(fd, i) })
			return serr
		}}
	return d.DialContext(ctx, network, addr)
}

// dialNet is the dialer for every download connection (HTTP, FTP, proxies).
func dialNet(ctx context.Context, network, addr string) (net.Conn, error) {
	cands := mdial.pick()
	if len(cands) == 0 {
		return baseDialer.DialContext(ctx, network, addr)
	}
	var lastErr error
	for k, i := range cands {
		if k >= 3 {
			break
		}
		c, err := dialVia(ctx, network, addr, i)
		if err == nil {
			mdial.markBad(i.Name, false)
			return adapterConn(c, i), nil
		}
		if ctx.Err() != nil {
			return nil, err
		}
		mdial.markBad(i.Name, true)
		lastErr = err
	}
	if c, err := baseDialer.DialContext(ctx, network, addr); err == nil {
		return c, nil
	}
	return nil, lastErr
}

// dialFrom opens a connection from the same adapter as an existing one (FTP data connections:
// servers often refuse them from another address).
func dialFrom(ctx context.Context, network, addr string, like net.Conn) (net.Conn, error) {
	if cc, ok := like.(*countingConn); ok {
		c, err := dialVia(ctx, network, addr, cc.local)
		if err != nil {
			return nil, err
		}
		return adapterConn(c, cc.local), nil
	}
	return baseDialer.DialContext(ctx, network, addr)
}

type countingConn struct {
	net.Conn
	n     *int64
	sent  *int64
	lim   *adapterLims
	local netIf
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	atomic.AddInt64(c.n, int64(n))
	if c.lim != nil && n > 0 {
		c.lim.down.Wait(context.Background(), n) // polite mode on this adapter (pays for what came in)
	}
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	if c.lim != nil {
		c.lim.up.Wait(context.Background(), len(p))
	}
	n, err := c.Conn.Write(p)
	if c.sent != nil {
		atomic.AddInt64(c.sent, int64(n))
	}
	return n, err
}

// NetIfView is one adapter as the Options window shows it.
type NetIfView struct {
	netIf
	Use   bool    `json:"use"`
	Speed float64 `json:"speed"`
	Bad   bool    `json:"bad"`
}

var (
	netSpeedMu   sync.Mutex
	netSpeedLast = map[string]int64{}
	netSpeed     = map[string]float64{}
	netSpeedAt   time.Time
)

// netStats updates per-adapter speeds (from the loop) and returns the adapter list.
func netStats(c MultiNet) []NetIfView {
	mdial.mu.Lock()
	if time.Since(mdial.at) > 5*time.Second {
		mdial.ifs, mdial.at = listNetIfs(), time.Now()
	}
	ifs := append([]netIf(nil), mdial.ifs...)
	bad := map[string]bool{}
	for n, t := range mdial.bad {
		bad[n] = time.Since(t) < time.Minute
	}
	counts := map[string]int64{}
	for n, p := range mdial.bytes {
		counts[n] = atomic.LoadInt64(p)
	}
	mdial.mu.Unlock()

	netSpeedMu.Lock()
	if dt := time.Since(netSpeedAt).Seconds(); dt >= 0.9 {
		for n, v := range counts {
			inst := float64(v-netSpeedLast[n]) / dt
			netSpeed[n] = netSpeed[n]*0.5 + max(inst, 0)*0.5
			netSpeedLast[n] = v
		}
		netSpeedAt = time.Now()
	}
	use := map[string]bool{}
	for _, n := range c.Use {
		use[n] = true
	}
	out := make([]NetIfView, 0, len(ifs))
	for _, i := range ifs {
		out = append(out, NetIfView{netIf: i, Use: len(c.Use) == 0 || use[i.Name], Speed: netSpeed[i.Name], Bad: bad[i.Name]})
	}
	netSpeedMu.Unlock()
	return out
}

var errNoAdapter = errors.New("that network adapter isn't available")

// testNetIf checks that an adapter reaches the internet, returning the round trip.
func testNetIf(ctx context.Context, name, addr string) (time.Duration, error) {
	for _, i := range listNetIfs() {
		if i.Name == name {
			t0 := time.Now()
			c, err := dialVia(ctx, "tcp", addr, i)
			if err != nil {
				return 0, err
			}
			c.Close()
			return time.Since(t0), nil
		}
	}
	return 0, errNoAdapter
}
