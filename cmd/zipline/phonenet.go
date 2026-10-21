package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Where phone control listens, and who may reach it. The phone page is plain HTTP, so Zipline keeps
// it to home and office networks:
//
//   - It listens only on the addresses of adapters that are up and on a private network: 10/8,
//     172.16/12, 192.168/16, an IPv6 unique-local address (fc00::/7) or a link-local one. Never on
//     every address, never on a public one, and not on 100.64/10 (a provider's shared address space is
//     not a home), nor on tunnels (VPN adapters).
//   - It binds again when the network changes (netwatch.go), so the page is on the Wi-Fi you are on now.
//   - A connection is let in only from a private or link-local address on the same subnet as the
//     address it reached. A phone on mobile data, a router that forwards the port, or a computer on
//     another network is turned away before any request is read.
//   - When the system marks the network Public (Windows, or NetworkManager's firewall zone on Linux),
//     the page is off there until you say "Allow on this network anyway" in Options → Phone.
//
// What the machine reports (its adapters, its network labels, a connection's address) is behind
// phoneEnv, so the tests can stand in for it.

// PhoneNet is a network that was marked Public but where the person allowed the phone page anyway.
type PhoneNet struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

const (
	phoneRecheck = 30 * time.Second // a network can be marked Public without any address changing
	maxPhoneNets = 20
)

// privateLAN: is this a home or office address, one the phone page may be served on and to?
// RFC 1918, unique-local IPv6 and link-local addresses are; 100.64/10 (carrier-grade NAT), public
// addresses, loopback and multicast aren't.
func privateLAN(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
		return false
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// phoneIf is one address of a network adapter that phone control may listen on.
type phoneIf struct {
	Name  string     // the adapter ("Wi-Fi", "wlan0")
	Index int        // its number (what Windows names it by)
	IP    net.IP     // its address on this network
	Net   *net.IPNet // the subnet it is on
}

// rawIf is an adapter as the system lists it (replaced in tests).
type rawIf struct {
	Name  string
	Index int
	Flags net.Flags
	Addrs []net.Addr
}

func systemAdapters() []rawIf {
	ifs, _ := net.Interfaces()
	var out []rawIf
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		out = append(out, rawIf{i.Name, i.Index, i.Flags, addrs})
	}
	return out
}

// lanAddrsOf picks the addresses phone control may use: private (as private says), on adapters that
// are up, not loopback and not point-to-point (VPN and other tunnels aren't a home network).
func lanAddrsOf(ifs []rawIf, private func(net.IP) bool) []phoneIf {
	var out []phoneIf
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		for _, a := range i.Addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || !private(ipn.IP) {
				continue
			}
			out = append(out, phoneIf{Name: i.Name, Index: i.Index, IP: ipn.IP, Net: &net.IPNet{IP: ipn.IP.Mask(ipn.Mask), Mask: ipn.Mask}})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return addrRank(out[a].IP) < addrRank(out[b].IP) })
	return out
}

// addrRank orders addresses best first: 192.168, 10, 172.16, a unique-local IPv6, link-local.
func addrRank(ip net.IP) int {
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 192 && v4[1] == 168:
			return 0
		case v4[0] == 10:
			return 1
		case v4[0] == 172:
			return 2
		}
		return 4
	}
	if ip.IsLinkLocalUnicast() {
		return 6
	}
	return 3
}

// shown: an address worth putting in a QR code or a link. A link-local IPv6 address needs a zone
// ("%wlan0") that phone browsers don't take.
func (i phoneIf) shown() bool { return !(i.IP.To4() == nil && i.IP.IsLinkLocalUnicast()) }

// host is the address as it goes in a link: IPv6 in brackets.
func (i phoneIf) host() string {
	if i.IP.To4() == nil {
		return "[" + i.IP.String() + "]"
	}
	return i.IP.String()
}

// bind is where to listen: network and address (an IPv6 link-local address needs its adapter as zone).
func (i phoneIf) bind(port int) (network, addr string) {
	p := strconv.Itoa(port)
	if i.IP.To4() != nil {
		return "tcp4", net.JoinHostPort(i.IP.String(), p)
	}
	ip := i.IP.String()
	if i.IP.IsLinkLocalUnicast() {
		ip += "%" + strconv.Itoa(i.Index)
	}
	return "tcp6", net.JoinHostPort(ip, p)
}

func (i phoneIf) id() string { return i.IP.String() + "%" + strconv.Itoa(i.Index) }

// lanRemoteOK: a connection to address ifc is let in only from a private or link-local address on
// that address's own subnet.
func lanRemoteOK(remote net.Addr, ifc phoneIf) bool {
	a, ok := remote.(*net.TCPAddr)
	return ok && privateLAN(a.IP) && ifc.Net.Contains(a.IP)
}

// netVerdict is what the system says about the network an adapter is on.
type netVerdict struct {
	Name   string // the network's own name when the system gives one (a Wi-Fi's name)
	Cat    string // "public", "private" or "domain"; "" when the system doesn't say
	Public bool
}

// phoneEnv is what phone control asks the machine. The tests replace its parts.
type phoneEnv struct {
	adapters func() []rawIf
	private  func(net.IP) bool                         // which addresses are a home or office network's
	verdicts func(ifs []phoneIf) map[string]netVerdict // by adapter name; nil when the system can't tell
	remoteOK func(remote net.Addr, ifc phoneIf) bool
	every    time.Duration // how often the network's label is read again
	wake     func()        // brings the window up when a phone asks to connect
}

func systemPhoneEnv() phoneEnv {
	return phoneEnv{adapters: systemAdapters, private: privateLAN, verdicts: systemNetworks, remoteOK: lanRemoteOK, every: phoneRecheck, wake: func() { go bringToFront() }}
}

// lanNet is one network phone control could be served on: an adapter and what the system says of it.
type lanNet struct {
	Adapter string
	Name    string // as shown: the network's own name, else the adapter's
	Key     string // what "Allow anyway" is remembered by: its name and subnet
	Subnet  string
	Cat     string
	Addrs   []phoneIf
	Public  bool
	Allowed bool // marked Public, and allowed anyway
}

// serving: the page is on here.
func (n lanNet) serving() bool { return !n.Public || n.Allowed }

// networks lists the networks the computer is on and how each stands.
func (e phoneEnv) networks(allow []PhoneNet) []lanNet {
	ifs := lanAddrsOf(e.adapters(), e.private)
	if len(ifs) == 0 {
		return nil
	}
	var verdicts map[string]netVerdict
	if e.verdicts != nil {
		verdicts = e.verdicts(ifs)
	}
	by := map[string]*lanNet{}
	var out []*lanNet
	for _, i := range ifs {
		n := by[i.Name]
		if n == nil {
			n = &lanNet{Adapter: i.Name}
			by[i.Name] = n
			out = append(out, n)
		}
		n.Addrs = append(n.Addrs, i)
	}
	res := make([]lanNet, 0, len(out))
	for _, n := range out {
		v := verdicts[n.Adapter]
		n.Name, n.Cat, n.Public = v.Name, v.Cat, v.Public
		if n.Name == "" {
			n.Name = n.Adapter
		}
		first := n.Addrs[0] // (the best address names the subnet; the others are the same network)
		n.Subnet = first.Net.String()
		n.Key = n.Name + "|" + n.Subnet
		for _, a := range allow {
			n.Allowed = n.Allowed || n.Public && a.Key == n.Key
		}
		res = append(res, *n)
	}
	return res
}

// ---------------------------------------------------------------- what the systems say

// winProfile is a line of Windows' Get-NetConnectionProfile (phonenet_windows.go asks for
// "InterfaceIndex|NetworkCategory|base64 of Name": plain ASCII whatever the console's code page).
type winProfile struct {
	Index int
	Cat   string // Public, Private or DomainAuthenticated
	Name  string
}

func parseWinProfiles(out string) []winProfile {
	var ps []winProfile
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) < 2 {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(f[0]))
		if err != nil {
			continue
		}
		p := winProfile{Index: idx, Cat: strings.TrimSpace(f[1])}
		if len(f) > 2 {
			if b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(f[2])); err == nil {
				p.Name = strings.TrimSpace(string(b))
			}
		}
		ps = append(ps, p)
	}
	return ps
}

// winVerdicts matches Windows' network profiles to the adapters by their number.
func winVerdicts(ifs []phoneIf, ps []winProfile) map[string]netVerdict {
	out := map[string]netVerdict{}
	for _, i := range ifs {
		for _, p := range ps {
			if p.Index != i.Index {
				continue
			}
			v := netVerdict{Name: p.Name}
			switch strings.ToLower(p.Cat) {
			case "public":
				v.Cat, v.Public = "public", true
			case "private":
				v.Cat = "private"
			case "domainauthenticated":
				v.Cat = "domain"
			}
			out[i.Name] = v
		}
	}
	return out
}

// nmVerdicts asks NetworkManager (through nmcli, run is how a command is run) which firewall zone each
// adapter's connection is in; a connection without one is in firewalld's default zone. "public" is the
// zone for networks nobody vouches for. Without nmcli, or for an adapter it doesn't manage, nothing is
// said, and that counts as not Public.
func nmVerdicts(ifs []phoneIf, run func(name string, args ...string) string) map[string]netVerdict {
	out := map[string]netVerdict{}
	def, haveDef := "", false
	for _, i := range ifs {
		if _, done := out[i.Name]; done {
			continue
		}
		conn := nmUnescape(run("nmcli", "-t", "-g", "GENERAL.CONNECTION", "device", "show", i.Name))
		if conn == "" || conn == "--" {
			continue
		}
		zone := strings.ToLower(strings.TrimSpace(run("nmcli", "-t", "-g", "connection.zone", "connection", "show", "id", conn)))
		if zone == "" || zone == "--" {
			if !haveDef {
				def, haveDef = strings.ToLower(strings.TrimSpace(run("firewall-cmd", "--get-default-zone"))), true
			}
			zone = def
		}
		v := netVerdict{Name: conn}
		switch zone {
		case "":
		case "public":
			v.Cat, v.Public = "public", true
		default:
			v.Cat = "private"
		}
		out[i.Name] = v
	}
	return out
}

// nmUnescape undoes nmcli's terse-mode escapes (\: and \\).
func nmUnescape(s string) string {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, `\`) {
		return s
	}
	return strings.NewReplacer(`\:`, ":", `\\`, `\`).Replace(s)
}

// ---------------------------------------------------------------- the listener

type phoneAsk struct {
	ID, Token, Name, Addr string
	At                    time.Time
	State                 string // askPending or askDenied
}

const (
	askPending = "pending"
	askDenied  = "denied"
)

func newPhoneServer(m *Manager) *phoneServer {
	return &phoneServer{m: m, env: systemPhoneEnv(), seen: map[string]time.Time{}, fails: map[string][]time.Time{}, blocked: map[string]time.Time{}, asks: map[string]*phoneAsk{}}
}

// guardedListener drops a connection the moment it arrives unless ok says its address may come in.
type guardedListener struct {
	net.Listener
	ok func(remote net.Addr) bool
}

func (l guardedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.ok(c.RemoteAddr()) {
			return c, nil
		}
		c.Close()
	}
}

// sync starts, moves or stops the listeners to match the latest settings and the network: one
// listener per private address of a network the page may be on.
func (p *phoneServer) sync() {
	p.syncMu.Lock()
	defer p.syncMu.Unlock()
	p.mu.Lock()
	on, port, allow, env := p.wantOn && !p.stopped, p.wantPort, p.wantAllow, p.env
	p.mu.Unlock()
	var nets []lanNet
	if on {
		nets = env.networks(allow) // (may run system commands: nothing is locked meanwhile)
	}
	var want []phoneIf
	for _, n := range nets {
		if n.serving() {
			want = append(want, n.Addrs...)
		}
	}
	ids := make([]string, len(want))
	for i, w := range want {
		ids[i] = w.id()
	}
	sort.Strings(ids)
	same := strings.Join(ids, " ")

	p.mu.Lock()
	defer p.mu.Unlock()
	p.nets = nets
	if p.srv != nil && (len(want) == 0 || p.port != port || p.bound != same) {
		// (leaving a network cuts its connections off at once; switching off or moving lets a reply finish)
		if !on || p.port != port {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			p.srv.Shutdown(ctx)
			cancel()
		} else {
			p.srv.Close()
		}
		p.srv, p.bound = nil, ""
	}
	if len(want) == 0 {
		p.err = ""
		return
	}
	if p.srv != nil {
		return
	}
	srv := &http.Server{Handler: p.routes(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	bound := 0
	var lastErr error
	for _, w := range want {
		network, addr := w.bind(port)
		ln, err := net.Listen(network, addr)
		if err != nil {
			lastErr = err
			continue
		}
		w := w
		bound++
		go srv.Serve(guardedListener{ln, func(remote net.Addr) bool { return env.remoteOK(remote, w) }})
	}
	if bound == 0 {
		p.err = fmt.Sprintf("Couldn't use port %d: %v", port, lastErr)
		if strings.Contains(lastErr.Error(), "in use") || strings.Contains(lastErr.Error(), "Only one usage") {
			p.err = fmt.Sprintf("Port %d is used by another program. Choose another port.", port)
		}
		return
	}
	p.port, p.err, p.srv, p.bound = port, "", srv, same // (an address that wouldn't bind is tried again when the network changes)
}

// watch binds again when the network changes, and reads the network's label now and then (a network
// can be marked Public without an address changing), as long as the manager runs.
func (p *phoneServer) watch(quit <-chan struct{}, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		changed := netChanged()
		select {
		case <-changed:
		case <-tick.C:
		case <-quit:
			return
		}
		p.mu.Lock()
		on := p.wantOn && !p.stopped
		p.mu.Unlock()
		if on {
			p.sync()
		}
	}
}

// listening says where the phone page can be reached now: the addresses of the networks it is
// served on, best first.
func (p *phoneServer) listening() []phoneIf {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []phoneIf
	if p.srv == nil {
		return nil
	}
	for _, n := range p.nets {
		if n.serving() {
			for _, a := range n.Addrs {
				if a.shown() {
					out = append(out, a)
				}
			}
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return addrRank(out[a].IP) < addrRank(out[b].IP) })
	return out
}

// PhoneNetView is a network for Options → Phone.
type PhoneNetView struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Subnet  string `json:"subnet,omitempty"`
	Cat     string `json:"cat,omitempty"`
	Public  bool   `json:"public"`
	Allowed bool   `json:"allowed"`
	Here    bool   `json:"here"` // the computer is on it now
}

// netViews are the networks now, then the ones allowed before that the computer isn't on.
func (p *phoneServer) netViews(saved []PhoneNet) []PhoneNetView {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := []PhoneNetView{}
	have := map[string]bool{}
	for _, n := range p.nets {
		have[n.Key] = true
		out = append(out, PhoneNetView{n.Key, n.Name, n.Subnet, n.Cat, n.Public, n.Allowed, true})
	}
	for _, s := range saved {
		if !have[s.Key] {
			out = append(out, PhoneNetView{Key: s.Key, Name: s.Name, Public: true, Allowed: true})
		}
	}
	return out
}

// allowPhoneNet remembers (or forgets) that the phone page may be served on a network marked Public.
func (m *Manager) allowPhoneNet(key string, allow bool) error {
	ph := m.phone
	name := ""
	if ph != nil {
		ph.mu.Lock()
		for _, n := range ph.nets {
			if n.Key == key {
				name = n.Name
			}
		}
		ph.mu.Unlock()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	list := m.settings.Phone.Allow
	keep := make([]PhoneNet, 0, len(list)+1)
	for _, a := range list {
		if a.Key != key {
			keep = append(keep, a)
		} else if name == "" {
			name = a.Name
		}
	}
	if allow {
		if name == "" {
			return errors.New("that network isn't one this computer is on")
		}
		if len(keep) >= maxPhoneNets {
			return errors.New("forget a network first (20 at most)")
		}
		keep = append(keep, PhoneNet{Key: key, Name: name})
	}
	m.settings.Phone.Allow = keep
	m.dirty = true
	m.applyPhone()
	return nil
}
