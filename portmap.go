package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

// Port mapping: asking the home router to forward the torrent port (TCP, and UDP for uTP and the
// DHT) to this computer, so peers on the internet can connect in. Three ways, tried together:
// UPnP IGD (upnp.go), NAT-PMP (RFC 6886) and its successor PCP (RFC 6887, natpmp.go). Leases
// last an hour and are renewed at half time, as libtorrent does; the mappings go when Zipline
// quits, when the port changes, when mapping is turned off and while a proxy is on.

// PortMapView is the mapping's state for Options → Torrents.
type PortMapView struct {
	State    string `json:"state"`              // idle, off, proxy, looking, open, none, refused, conflict, cgnat, private
	Via      string `json:"via,omitempty"`      // UPnP, NAT-PMP or PCP
	Port     int    `json:"port"`               // ours
	ExtPort  int    `json:"extPort,omitempty"`  // the router's, when it gave another
	External string `json:"external,omitempty"` // the router's address on the internet side
	Error    string `json:"error,omitempty"`    // what the router said when it refused
}

// pmEnv is where routers are looked for; tests point it at fakes on loopback.
type pmEnv struct {
	ssdp     string              // where UPnP discovery is sent (the SSDP multicast group)
	pmpPort  int                 // the NAT-PMP and PCP port on the gateway
	gateways func() []netip.Addr // the likely routers
	wait     time.Duration       // how long discovery listens for answers
}

var portMapEnv = pmEnv{ssdp: "239.255.255.250:1900", pmpPort: 5351, gateways: likelyGateways, wait: 3 * time.Second}

const (
	pmLease = time.Hour        // asked for (libtorrent's 3,600 s)
	pmCheck = 30 * time.Minute // a permanent mapping is put back this often (routers restart)
	pmRetry = 10 * time.Minute // after finding no router
)

var errPMNoAnswer = errors.New("no answer")

// pmRefused is a router's no.
type pmRefused struct {
	msg      string
	conflict bool // another device has the port
}

func (e *pmRefused) Error() string { return e.msg }

type pmResult struct {
	via      string
	ext      int
	external netip.Addr
	lease    time.Duration // 0 = until it's removed
}

// pmRouter is a router that maps ports one way or another.
type pmRouter interface {
	add(ctx context.Context, port int, udp bool) (pmResult, error) // maps TCP (and UDP), or renews
	del(ctx context.Context, port int, udp bool)
}

type portMapper struct {
	m     *Manager
	port  int
	udp   bool
	env   pmEnv
	kick  chan struct{}
	stopc chan struct{}
	done  chan struct{}
	once  sync.Once

	mu   sync.Mutex
	view PortMapView
}

func newPortMapper(m *Manager, port int, udp bool) *portMapper {
	pm := &portMapper{m: m, port: port, udp: udp, env: portMapEnv, kick: make(chan struct{}, 1), stopc: make(chan struct{}), done: make(chan struct{}),
		view: PortMapView{State: "looking", Port: port}}
	go pm.run()
	return pm
}

func (pm *portMapper) state() PortMapView {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.view
}

func (pm *portMapper) set(v PortMapView) {
	v.Port = pm.port
	pm.mu.Lock()
	was := pm.reachableLocked()
	pm.view = v
	now := pm.reachableLocked()
	pm.mu.Unlock()
	if now != was && pm.udp { // the DHT node can serve others once its port is open (BEP 43)
		pm.m.mu.Lock()
		d := pm.m.dht
		pm.m.mu.Unlock()
		if d != nil {
			d.SetReachable(now)
		}
	}
}

// reachable: the port is open from outside (as far as the router can tell).
func (pm *portMapper) reachable() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.reachableLocked()
}

func (pm *portMapper) reachableLocked() bool { return pm.view.State == "open" && pm.view.ExtPort == 0 }

// poke has it look at the settings again now.
func (pm *portMapper) poke() {
	select {
	case pm.kick <- struct{}{}:
	default:
	}
}

// stop removes the mappings and ends it; wait says how long to wait for the router (0: don't).
func (pm *portMapper) stop(wait time.Duration) {
	pm.once.Do(func() { close(pm.stopc) })
	select {
	case <-pm.done:
	case <-time.After(wait):
	}
}

// unwanted says why there's no mapping to make ("" when there is).
func (pm *portMapper) unwanted() string {
	pm.m.mu.Lock()
	off := pm.m.settings.Torrent.NoPortMap
	pm.m.mu.Unlock()
	switch {
	case off:
		return "off"
	case torrentProxy() != nil:
		return "proxy"
	}
	return ""
}

func (pm *portMapper) run() {
	defer close(pm.done)
	var routers []pmRouter
	var renew, retry time.Time
	unmap := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var wg sync.WaitGroup
		for _, r := range routers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.del(ctx, pm.port, pm.udp)
			}()
		}
		wg.Wait()
		routers = nil
	}
	defer func() { unmap() }()
	for {
		now := time.Now()
		next := pmCheck
		switch why := pm.unwanted(); {
		case why != "":
			unmap()
			pm.set(PortMapView{State: why})
			retry = time.Time{}
		case len(routers) == 0 && !now.Before(retry):
			pm.set(PortMapView{State: "looking"})
			var v PortMapView
			var lease time.Duration
			routers, v, lease = pm.discover()
			if pm.stopping() {
				return
			}
			if pm.unwanted() != "" { // (turned off meanwhile)
				continue
			}
			pm.set(v)
			if len(routers) == 0 {
				retry = now.Add(pmRetry)
			} else {
				renew = now.Add(renewAfter(lease))
			}
		case len(routers) > 0 && !now.Before(renew):
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			var kept []pmRouter
			var lease time.Duration
			for _, r := range routers {
				if res, err := r.add(ctx, pm.port, pm.udp); err == nil {
					if len(kept) == 0 { // (the address outside may have changed)
						pm.set(mapView(res, pm.port))
					}
					kept = append(kept, r)
					lease = shorterLease(lease, res.lease)
				}
			}
			cancel()
			if routers = kept; len(routers) == 0 {
				retry = time.Time{} // the router forgot or went away: look again
				next = 0
			} else {
				renew = now.Add(renewAfter(lease))
			}
		}
		if len(routers) > 0 {
			next = time.Until(renew)
		} else if !retry.IsZero() {
			next = time.Until(retry)
		}
		select {
		case <-pm.kick: // (settings saved: a router that said no or wasn't found is asked again)
			retry = time.Time{}
		case <-pm.stopc:
			return
		case <-pm.m.quit:
			return
		case <-time.After(max(next, 0)):
		}
	}
}

func (pm *portMapper) stopping() bool {
	select {
	case <-pm.stopc:
		return true
	default:
		return false
	}
}

// shorterLease keeps the shortest lease that ends (0 = none ends).
func shorterLease(a, b time.Duration) time.Duration {
	if a == 0 || b > 0 && b < a {
		return b
	}
	return a
}

// renewAfter is half the lease (a permanent one is checked every half hour).
func renewAfter(lease time.Duration) time.Duration {
	if lease <= 0 {
		return pmCheck
	}
	return max(lease/2, 2*time.Second)
}

// discover asks every way at once: UPnP by SSDP search, and NAT-PMP/PCP at each likely gateway
// (plus the router that answers SSDP). Every router that maps the port is kept.
func (pm *portMapper) discover() (routers []pmRouter, v PortMapView, lease time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), pm.env.wait+10*time.Second)
	defer cancel()
	type found struct {
		r   pmRouter
		res pmResult
		err error
	}
	ch := make(chan found, 8)
	var wg sync.WaitGroup
	var tmu sync.Mutex
	tried := map[netip.Addr]bool{}
	tryPMP := func(gw netip.Addr) {
		tmu.Lock()
		seen := tried[gw]
		tried[gw] = true
		tmu.Unlock()
		if seen || !gw.IsValid() {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := &pmpClient{gw: netip.AddrPortFrom(gw, uint16(pm.env.pmpPort))}
			res, err := c.add(ctx, pm.port, pm.udp)
			ch <- found{c, res, err}
		}()
	}
	for _, gw := range pm.env.gateways() {
		tryPMP(gw)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		igd, src, err := discoverIGD(ctx, pm.env.ssdp, pm.env.wait)
		tryPMP(src) // the router that answered is most likely the gateway too
		if err != nil {
			ch <- found{nil, pmResult{}, err}
			return
		}
		res, err := igd.add(ctx, pm.port, pm.udp)
		ch <- found{igd, res, err}
	}()
	go func() {
		wg.Wait()
		close(ch)
	}()
	v = PortMapView{State: "none"}
	for f := range ch {
		var rf *pmRefused
		switch {
		case f.err == nil:
			if len(routers) == 0 {
				v = mapView(f.res, pm.port)
			}
			routers = append(routers, f.r)
			lease = shorterLease(lease, f.res.lease)
		case len(routers) > 0:
		case errors.As(f.err, &rf) && rf.conflict:
			v = PortMapView{State: "conflict"}
		case errors.As(f.err, &rf) && v.State != "conflict":
			v = PortMapView{State: "refused", Error: rf.msg}
		}
	}
	return routers, v, lease
}

// mapView describes a working mapping, with a warning when the router's own address outside is
// shared or private: then the mapping can't bring anyone in.
func mapView(r pmResult, port int) PortMapView {
	v := PortMapView{State: "open", Via: r.via}
	if r.ext != 0 && r.ext != port {
		v.ExtPort = r.ext
	}
	if ip := r.external.Unmap(); ip.IsValid() && !ip.IsUnspecified() {
		v.External = ip.String()
		switch {
		case cgnatRange.Contains(ip):
			v.State = "cgnat"
		case ip.IsPrivate() || ip.IsLinkLocalUnicast():
			v.State = "private"
		}
	}
	return v
}

var cgnatRange = netip.MustParsePrefix("100.64.0.0/10") // carrier-grade NAT (RFC 6598)

// likelyGateways guesses the router's address: the default route's gateway as the network
// check reads it (netid*.go: /proc/net/route on Linux, GetBestRoute on Windows, the route
// command on macOS), and the first address of the network the internet is reached through
// (192.168.1.1 for 192.168.1.23/24), where home routers sit. UPnP discovery adds the address the
// router answers from.
func likelyGateways() []netip.Addr {
	var out []netip.Addr
	if k, ok := strings.CutPrefix(netIdentify().key, "gw:"); ok {
		ip, _, _ := strings.Cut(k, "/")
		if a, err := netip.ParseAddr(ip); err == nil {
			out = append(out, a.Unmap())
		}
	}
	if gw, ok := subnetGateway(); ok && !slices.Contains(out, gw) {
		out = append(out, gw)
	}
	return out
}

// subnetGateway is the first address of the local network the internet is reached through.
func subnetGateway() (netip.Addr, bool) {
	c, err := net.Dial("udp4", "192.0.2.1:9") // (nothing is sent: it only picks the route)
	if err != nil {
		return netip.Addr{}, false
	}
	local, _ := netip.ParseAddrPort(c.LocalAddr().String())
	c.Close()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			ip, _ := netip.AddrFromSlice(n.IP)
			bits, _ := n.Mask.Size()
			if ip.Unmap() == local.Addr().Unmap() {
				return firstHost(netip.PrefixFrom(ip.Unmap(), bits))
			}
		}
	}
	return netip.Addr{}, false
}

func firstHost(p netip.Prefix) (netip.Addr, bool) {
	if !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 30 {
		return netip.Addr{}, false
	}
	return p.Masked().Addr().Next(), true
}

// localAddrTowards is this computer's address as a router at to sees it.
func localAddrTowards(to netip.AddrPort) (netip.Addr, error) {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(to))
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(), nil
}
