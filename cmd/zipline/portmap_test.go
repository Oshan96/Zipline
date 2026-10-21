package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests stay on this computer: no router is looked for unless a test sets up a fake one.
func init() {
	portMapEnv = pmEnv{ssdp: "127.0.0.1:9", pmpPort: 9, gateways: func() []netip.Addr { return nil }, wait: 200 * time.Millisecond}
}

// withPortMapEnv points router discovery at fakes for one test.
func withPortMapEnv(t *testing.T, e pmEnv) {
	old := portMapEnv
	portMapEnv = e
	t.Cleanup(func() { portMapEnv = old })
}

// fakeIGD is a UPnP router: an SSDP responder on a unicast loopback port, its description and
// its WANIPConnection control URL.
type fakeIGD struct {
	ssdp          *net.UDPConn
	srv           *httptest.Server
	permanentOnly bool // answers 725 to leases that end
	conflict      bool // answers 718
	external      string
	mu            sync.Mutex
	calls         []string // "AddPortMapping TCP 51413 3600 127.0.0.1"
}

func newFakeIGD(t *testing.T, permanentOnly, conflict bool) *fakeIGD {
	g := &fakeIGD{external: "203.0.113.7", permanentOnly: permanentOnly, conflict: conflict}
	field := func(body, name string) string {
		m := regexp.MustCompile("<" + name + ">([^<]*)</" + name + ">").FindStringSubmatch(body)
		if m == nil {
			return ""
		}
		return m[1]
	}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rootDesc.xml":
			fmt.Fprint(w, `<?xml version="1.0"?><root xmlns="urn:schemas-upnp-org:device-1-0"><specVersion><major>1</major></specVersion>
<device><deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType><serviceList><service><serviceType>urn:schemas-upnp-org:service:Layer3Forwarding:1</serviceType><controlURL>/ctl/L3F</controlURL></service></serviceList>
<deviceList><device><deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType><deviceList><device><deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
<serviceList><service><serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType><serviceId>urn:upnp-org:serviceId:WANIPConn1</serviceId><controlURL>/ctl/IPConn</controlURL></service></serviceList>
</device></deviceList></device></deviceList></device></root>`)
		case "/ctl/IPConn":
			b, _ := io.ReadAll(r.Body)
			body := string(b)
			action := r.Header.Get("SOAPAction")
			action = action[strings.Index(action, "#")+1 : len(action)-1]
			g.mu.Lock()
			g.calls = append(g.calls, strings.TrimSpace(strings.Join([]string{action, field(body, "NewProtocol"), field(body, "NewExternalPort"), field(body, "NewLeaseDuration"), field(body, "NewInternalClient")}, " ")))
			g.mu.Unlock()
			fault := func(code int, desc string) {
				w.WriteHeader(500)
				fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`, code, desc)
			}
			switch {
			case action == "AddPortMapping" && g.conflict:
				fault(718, "ConflictInMappingEntry")
			case action == "AddPortMapping" && g.permanentOnly && field(body, "NewLeaseDuration") != "0":
				fault(725, "OnlyPermanentLeasesSupported")
			case action == "GetExternalIPAddress":
				fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"><NewExternalIPAddress>%s</NewExternalIPAddress></u:GetExternalIPAddressResponse></s:Body></s:Envelope>`, g.external)
			case action == "GetSpecificPortMappingEntry":
				fault(714, "NoSuchEntryInArray")
			default:
				fmt.Fprint(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body/></s:Envelope>`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(g.srv.Close)
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	g.ssdp = c
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if strings.HasPrefix(string(buf[:n]), "M-SEARCH * HTTP/1.1") && strings.Contains(string(buf[:n]), "InternetGatewayDevice:1") {
				c.WriteToUDP([]byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\nST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\nUSN: uuid:fake::urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\nEXT:\r\nSERVER: Fake/1.0 UPnP/1.1 MiniUPnPd/2.3\r\nLOCATION: "+g.srv.URL+"/rootDesc.xml\r\n\r\n"), from)
			}
		}
	}()
	return g
}

func (g *fakeIGD) log() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// fakePMP is a NAT-PMP or (pcp) PCP gateway. A PCP one answers NAT-PMP too, as RFC 6887 allows.
type fakePMP struct {
	conn     *net.UDPConn
	pcp      bool
	external netip.Addr
	grant    uint32 // the longest lease it gives (seconds)
	refuse   int    // the result code for every mapping (0: none)
	mu       sync.Mutex
	maps     []string // "TCP 51413 3600", "UDP 51413 0" (0 = removed)
	nonces   map[string]bool
}

func newFakePMP(t *testing.T, pcp bool, external string, grant uint32, refuse int) *fakePMP {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	f := &fakePMP{conn: c, pcp: pcp, external: netip.MustParseAddr(external), grant: grant, refuse: refuse, nonces: map[string]bool{}}
	go func() {
		buf := make([]byte, 1100)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if out := f.answer(buf[:n]); out != nil {
				c.WriteToUDPAddrPort(out, from)
			}
		}
	}()
	return f
}

func (f *fakePMP) answer(b []byte) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(b) == 60 && b[0] == 2 && b[1] == 1: // PCP MAP
		if !f.pcp {
			return []byte{0, 129, 0, 1, 0, 0, 0, 9} // unsupported version
		}
		life := min(binary.BigEndian.Uint32(b[4:]), f.grant)
		proto := map[byte]string{6: "TCP", 17: "UDP"}[b[36]]
		f.maps = append(f.maps, fmt.Sprintf("%s %d %d", proto, binary.BigEndian.Uint16(b[40:]), life))
		f.nonces[string(b[24:36])] = true
		out := make([]byte, 60)
		copy(out, b)
		out[1], out[2], out[3] = 0x81, 0, byte(f.refuse)
		binary.BigEndian.PutUint32(out[4:], life)
		binary.BigEndian.PutUint32(out[8:], 9) // epoch
		clear(out[12:24])
		ext := netip.AddrFrom4(f.external.As4()).As16()
		copy(out[44:], ext[:])
		return out
	case len(b) == 2 && b[0] == 0 && b[1] == 0: // NAT-PMP: the address outside
		out := []byte{0, 128, 0, 0, 0, 0, 0, 9}
		a := f.external.As4()
		return append(out, a[:]...)
	case len(b) == 12 && b[0] == 0 && (b[1] == 1 || b[1] == 2):
		life := min(binary.BigEndian.Uint32(b[8:]), f.grant)
		port := binary.BigEndian.Uint16(b[4:])
		f.maps = append(f.maps, fmt.Sprintf("%s %d %d", map[byte]string{1: "UDP", 2: "TCP"}[b[1]], port, life))
		out := make([]byte, 16)
		out[1] = 128 + b[1]
		binary.BigEndian.PutUint16(out[2:], uint16(f.refuse))
		binary.BigEndian.PutUint32(out[4:], 9)
		copy(out[8:12], b[4:8])
		binary.BigEndian.PutUint32(out[12:], life)
		return out
	}
	return nil
}

func (f *fakePMP) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.maps...)
}

func (f *fakePMP) port() int { return f.conn.LocalAddr().(*net.UDPAddr).Port }

func waitMapState(t *testing.T, pm *portMapper, want string, limit time.Duration) PortMapView {
	t.Helper()
	end := time.Now().Add(limit)
	for {
		v := pm.state()
		if v.State == want {
			return v
		}
		if time.Now().After(end) {
			t.Fatalf("port mapping: %+v, want %q", v, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitLog(t *testing.T, log func() []string, want string, limit time.Duration) {
	t.Helper()
	end := time.Now().Add(limit)
	for {
		l := log()
		for _, x := range l {
			if x == want {
				return
			}
		}
		if time.Now().After(end) {
			t.Fatalf("no %q in %q", want, l)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// UPnP: found by an SSDP search, TCP and UDP mapped; a router that only has permanent leases
// (error 725) gets asked for one, as libtorrent does; the mappings go when it stops.
func TestUPnPPortMapping(t *testing.T) {
	g := newFakeIGD(t, true, false)
	withPortMapEnv(t, pmEnv{ssdp: g.ssdp.LocalAddr().String(), pmpPort: 9, gateways: func() []netip.Addr { return nil }, wait: time.Second})
	m := newTestManager(t)
	pm := newPortMapper(m, 51413, true)
	v := waitMapState(t, pm, "open", 10*time.Second)
	if v.Via != "UPnP" || v.External != "203.0.113.7" || v.Port != 51413 {
		t.Fatalf("%+v", v)
	}
	log := strings.Join(g.log(), "\n")
	for _, want := range []string{"AddPortMapping TCP 51413 3600 127.0.0.1", "AddPortMapping TCP 51413 0 127.0.0.1", "AddPortMapping UDP 51413 0 127.0.0.1", "GetExternalIPAddress"} {
		if !strings.Contains(log, want) {
			t.Errorf("no %q in\n%s", want, log)
		}
	}
	pm.stop(5 * time.Second)
	waitLog(t, g.log, "DeletePortMapping TCP 51413", time.Second)
	waitLog(t, g.log, "DeletePortMapping UDP 51413", time.Second)
}

// Another device holds the port on the router: say so.
func TestUPnPConflict(t *testing.T) {
	g := newFakeIGD(t, false, true)
	withPortMapEnv(t, pmEnv{ssdp: g.ssdp.LocalAddr().String(), pmpPort: 9, gateways: func() []netip.Addr { return nil }, wait: 500 * time.Millisecond})
	pm := newPortMapper(newTestManager(t), 51413, true)
	waitMapState(t, pm, "conflict", 10*time.Second)
	pm.stop(time.Second)
}

// NAT-PMP: the router answers PCP with "unsupported version", so NAT-PMP it is. Its address
// outside is a carrier-grade NAT one (mapping can't help: say so); the lease is renewed at half
// time, and removed at the end.
func TestNATPMPPortMapping(t *testing.T) {
	f := newFakePMP(t, false, "100.64.12.34", 4, 0)
	withPortMapEnv(t, pmEnv{ssdp: "127.0.0.1:9", pmpPort: f.port(), gateways: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }, wait: 300 * time.Millisecond})
	pm := newPortMapper(newTestManager(t), 51413, true)
	v := waitMapState(t, pm, "cgnat", 10*time.Second)
	if v.Via != "NAT-PMP" || v.External != "100.64.12.34" {
		t.Fatalf("%+v", v)
	}
	if l := strings.Join(f.log(), ","); l != "TCP 51413 4,UDP 51413 4" {
		t.Fatalf("mappings: %s", l)
	}
	time.Sleep(3 * time.Second) // renewed after 2
	if l := f.log(); len(l) < 4 || l[2] != "TCP 51413 4" {
		t.Fatalf("not renewed: %q", l)
	}
	pm.stop(5 * time.Second)
	waitLog(t, f.log, "TCP 51413 0", time.Second)
	waitLog(t, f.log, "UDP 51413 0", time.Second)
}

// PCP: preferred when the router speaks it (this one answers NAT-PMP as well). Its address
// outside is a private one: there's another router in front.
func TestPCPPortMapping(t *testing.T) {
	f := newFakePMP(t, true, "192.168.100.2", 7200, 0)
	withPortMapEnv(t, pmEnv{ssdp: "127.0.0.1:9", pmpPort: f.port(), gateways: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }, wait: 300 * time.Millisecond})
	pm := newPortMapper(newTestManager(t), 51413, true)
	v := waitMapState(t, pm, "private", 10*time.Second)
	if v.Via != "PCP" || v.External != "192.168.100.2" {
		t.Fatalf("%+v", v)
	}
	if l := strings.Join(f.log(), ","); l != "TCP 51413 3600,UDP 51413 3600" {
		t.Fatalf("mappings: %s", l)
	}
	pm.stop(5 * time.Second)
	waitLog(t, f.log, "TCP 51413 0", time.Second)
	f.mu.Lock()
	nonces := len(f.nonces)
	f.mu.Unlock()
	if nonces != 1 {
		t.Fatalf("%d nonces: a mapping is renewed and removed with the nonce it was made with", nonces)
	}
}

// No router answers; a router that refuses says why; the setting and a proxy turn it off and
// take what was mapped away.
func TestPortMapStates(t *testing.T) {
	withPortMapEnv(t, pmEnv{ssdp: "127.0.0.1:9", pmpPort: 9, gateways: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }, wait: 200 * time.Millisecond})
	pm := newPortMapper(newTestManager(t), 51413, true)
	waitMapState(t, pm, "none", 10*time.Second)
	pm.stop(time.Second)

	f := newFakePMP(t, false, "203.0.113.8", 7200, 2)
	withPortMapEnv(t, pmEnv{ssdp: "127.0.0.1:9", pmpPort: f.port(), gateways: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }, wait: 200 * time.Millisecond})
	pm = newPortMapper(newTestManager(t), 51413, true)
	if v := waitMapState(t, pm, "refused", 10*time.Second); v.Error != "NAT-PMP: not allowed" {
		t.Fatalf("%+v", v)
	}
	pm.stop(time.Second)

	f.mu.Lock()
	f.refuse = 0
	f.mu.Unlock()
	m := newTestManager(t)
	pm = newPortMapper(m, 51413, false)
	v := waitMapState(t, pm, "open", 10*time.Second)
	if v.Via != "NAT-PMP" || v.External != "203.0.113.8" {
		t.Fatalf("%+v", v)
	}
	m.mu.Lock()
	m.settings.Torrent.NoPortMap = true
	m.mu.Unlock()
	pm.poke()
	waitMapState(t, pm, "off", 5*time.Second)
	waitLog(t, f.log, "TCP 51413 0", time.Second)
	m.mu.Lock()
	m.settings.Torrent.NoPortMap = false
	m.mu.Unlock()
	pm.poke()
	waitMapState(t, pm, "open", 10*time.Second)
	setProxySettings(ProxySettings{Mode: "manual", URL: "socks5://127.0.0.1:1"})
	defer setProxySettings(ProxySettings{Mode: "system"})
	pm.poke()
	waitMapState(t, pm, "proxy", 5*time.Second)
	if l := f.log(); l[len(l)-1] != "TCP 51413 0" {
		t.Fatalf("still mapped while a proxy is on: %q", l)
	}
	pm.stop(time.Second)
}

// The torrent port in Options → Torrents: mapped as soon as the listener starts, and moved
// with the port.
func TestTorrentNetAPI(t *testing.T) {
	f := newFakePMP(t, true, "198.51.100.20", 7200, 0)
	withPortMapEnv(t, pmEnv{ssdp: "127.0.0.1:9", pmpPort: f.port(), gateways: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("127.0.0.1")} }, wait: 200 * time.Millisecond})
	m := torrentManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	if v := apiCall(t, h, "torrent/net", nil); v["map"].(map[string]any)["state"] != "idle" || v["proxy"] != "" {
		t.Fatalf("before any torrent: %v", v)
	}
	port := m.ensureTorrentListener()
	var v map[string]any
	for i := 0; i < 200; i++ {
		if v = apiCall(t, h, "torrent/net", nil); v["map"].(map[string]any)["state"] == "open" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	mv := v["map"].(map[string]any)
	if mv["via"] != "PCP" || mv["external"] != "198.51.100.20" || v["port"] != float64(port) || v["utp"] != true || mv["port"] != float64(port) {
		t.Fatalf("%v", v)
	}
	waitLog(t, f.log, fmt.Sprintf("UDP %d 3600", port), time.Second)
	// a new port: the old mapping goes, the new one comes
	ln, _ := net.Listen("tcp", ":0")
	free := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	m.mu.Lock()
	m.settings.Torrent.Port = free
	m.mu.Unlock()
	m.restartTorrentListener()
	waitLog(t, f.log, fmt.Sprintf("TCP %d 0", port), 5*time.Second)
	waitLog(t, f.log, fmt.Sprintf("TCP %d 3600", free), 5*time.Second)
	m.closeTorrentNet()
	waitLog(t, f.log, fmt.Sprintf("UDP %d 0", free), time.Second)
}

func TestPortMapHelpers(t *testing.T) {
	for _, c := range []struct {
		ext, state string
	}{{"203.0.113.7", "open"}, {"100.100.1.1", "cgnat"}, {"10.1.2.3", "private"}, {"172.20.0.1", "private"}, {"0.0.0.0", "open"}} {
		if v := mapView(pmResult{via: "UPnP", external: netip.MustParseAddr(c.ext)}, 1); v.State != c.state {
			t.Errorf("%s: %s, want %s", c.ext, v.State, c.state)
		}
	}
	if gw, ok := firstHost(netip.MustParsePrefix("192.168.1.23/24")); !ok || gw.String() != "192.168.1.1" {
		t.Fatal(gw)
	}
	if gw, ok := firstHost(netip.MustParsePrefix("10.20.30.40/16")); !ok || gw.String() != "10.20.0.1" {
		t.Fatal(gw)
	}
	if _, ok := firstHost(netip.MustParsePrefix("10.0.0.5/32")); ok {
		t.Fatal("a single address has no gateway")
	}
	if shorterLease(0, time.Hour) != time.Hour || shorterLease(time.Hour, 0) != time.Hour || shorterLease(time.Hour, time.Minute) != time.Minute {
		t.Fatal("shorterLease")
	}
}
