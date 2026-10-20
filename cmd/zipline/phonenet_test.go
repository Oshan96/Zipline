package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// loopbackLAN makes the phone listener treat 127.0.0.1 as a home network (what the tests can reach),
// accept any address on that subnet, and leave the window alone when a phone asks to connect.
func loopbackLAN(ph *phoneServer) {
	ph.mu.Lock()
	defer ph.mu.Unlock()
	ph.env = fakePhoneEnv("lo-test", "127.0.0.1/8", nil)
}

// fakePhoneEnv is a machine with one adapter at addr (CIDR) whose network the system says is v.
func fakePhoneEnv(adapter, cidr string, v *netVerdict) phoneEnv {
	ip, ipn, _ := net.ParseCIDR(cidr)
	ipn.IP = ip
	env := phoneEnv{
		adapters: func() []rawIf {
			return []rawIf{{Name: adapter, Index: 7, Flags: net.FlagUp, Addrs: []net.Addr{ipn}}}
		},
		private: func(ip net.IP) bool { return ip.IsLoopback() || privateLAN(ip) },
		remoteOK: func(remote net.Addr, ifc phoneIf) bool {
			a, ok := remote.(*net.TCPAddr)
			return ok && ifc.Net.Contains(a.IP)
		},
		every: time.Hour,
		wake:  func() {},
	}
	if v != nil {
		env.verdicts = func([]phoneIf) map[string]netVerdict { return map[string]netVerdict{adapter: *v} }
	}
	return env
}

// allowPhone is a phone's first visit with its new code and the window's yes.
func allowPhone(t *testing.T, m *Manager, c *phoneClient, name string) {
	t.Helper()
	st, body := c.do("POST", "hello", map[string]string{"name": name}, nil)
	if st != 200 || !strings.Contains(body, `"wait":true`) {
		t.Fatalf("first visit: %d %s", st, body)
	}
	m.phone.mu.Lock()
	token := ""
	for _, a := range m.phone.asks {
		token = a.Token
	}
	m.phone.mu.Unlock()
	if err := m.answerPhone(token, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Millisecond) // (a request signed in the same millisecond as the last would be a replay)
	if st, body := c.do("POST", "hello", map[string]string{"name": name}, nil); st != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("hello after the yes: %d %s", st, body)
	}
	time.Sleep(3 * time.Millisecond)
}

func TestPrivateLAN(t *testing.T) {
	for ip, want := range map[string]bool{
		"10.0.0.1": true, "10.255.255.254": true, "172.16.0.1": true, "172.31.255.1": true, "192.168.1.20": true,
		"169.254.10.2": true, "fd12:3456::1": true, "fe80::1": true, "::ffff:192.168.1.5": true,
		"172.15.255.255": false, "172.32.0.1": false, "100.64.0.1": false, "100.127.255.254": false, // (a provider's shared space is not a home)
		"8.8.8.8": false, "192.0.2.2": false, "2001:db8::1": false, "::ffff:8.8.8.8": false,
		"127.0.0.1": false, "::1": false, "0.0.0.0": false, "::": false, "224.0.0.1": false, "ff02::1": false,
	} {
		if got := privateLAN(net.ParseIP(ip)); got != want {
			t.Errorf("privateLAN(%s) = %v, want %v", ip, got, want)
		}
	}
	if privateLAN(nil) {
		t.Error("no address is private")
	}
}

func cidr(s string) *net.IPNet {
	ip, n, _ := net.ParseCIDR(s)
	n.IP = ip
	return n
}

// Only adapters that are up, aren't loopback or tunnels, and have a private address are used, and only
// that address.
func TestLanAddrsOf(t *testing.T) {
	up := net.FlagUp | net.FlagBroadcast
	ifs := []rawIf{
		{"lo", 1, net.FlagUp | net.FlagLoopback, []net.Addr{cidr("127.0.0.1/8")}},
		{"eth0", 2, up, []net.Addr{cidr("203.0.113.9/24"), cidr("192.168.1.5/24")}},
		{"wlan0", 3, up, []net.Addr{cidr("fe80::1/64"), cidr("fd00:1::5/64"), cidr("10.1.2.3/16")}},
		{"down0", 4, net.FlagBroadcast, []net.Addr{cidr("192.168.9.9/24")}},
		{"wg0", 5, up | net.FlagPointToPoint, []net.Addr{cidr("10.8.0.2/24")}},
		{"cgnat0", 6, up, []net.Addr{cidr("100.72.1.1/10")}},
		{"public0", 7, up, []net.Addr{cidr("198.51.100.4/24")}},
		{"docker0", 8, up, []net.Addr{cidr("172.17.0.1/16")}},
	}
	var got []string
	for _, i := range lanAddrsOf(ifs, privateLAN) {
		got = append(got, i.Name+" "+i.IP.String()+" "+i.Net.String())
	}
	want := []string{"eth0 192.168.1.5 192.168.1.0/24", "wlan0 10.1.2.3 10.1.0.0/16", "docker0 172.17.0.1 172.17.0.0/16", "wlan0 fd00:1::5 fd00:1::/64", "wlan0 fe80::1 fe80::/64"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("addresses:\n got  %q\n want %q", got, want)
	}
}

// A connection is let in only from a private or link-local address on the same subnet.
func TestLanRemoteOK(t *testing.T) {
	home := phoneIf{Name: "eth0", IP: net.ParseIP("192.168.1.5"), Net: cidr("192.168.1.5/24")}
	v6 := phoneIf{Name: "eth0", IP: net.ParseIP("fd00:1::5"), Net: cidr("fd00:1::5/64")}
	big := phoneIf{Name: "eth0", IP: net.ParseIP("192.168.1.5"), Net: cidr("192.0.0.5/8")} // (a mistaken mask must not let the world in)
	tcp := func(ip string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: 5000} }
	for _, c := range []struct {
		ifc    phoneIf
		remote net.Addr
		want   bool
	}{
		{home, tcp("192.168.1.77"), true},
		{home, tcp("192.168.1.5"), true}, // (this computer's own browser)
		{home, tcp("192.168.2.77"), false},
		{home, tcp("10.0.0.7"), false},
		{home, tcp("8.8.8.8"), false},
		{home, tcp("127.0.0.1"), false},
		{home, tcp("100.64.3.3"), false},
		{v6, tcp("fd00:1::99"), true},
		{v6, tcp("fd00:2::99"), false},
		{v6, tcp("2001:db8::1"), false},
		{big, tcp("192.77.7.7"), false}, // on the (mistaken) subnet, but public
		{home, &net.UnixAddr{Name: "x", Net: "unix"}, false},
	} {
		if got := lanRemoteOK(c.remote, c.ifc); got != c.want {
			t.Errorf("%s from %s: %v, want %v", c.ifc.IP, c.remote, got, c.want)
		}
	}
}

func TestWinProfiles(t *testing.T) {
	out := "12|Public|Q2Fmw6kgV2ktRmk=\r\n7|Private|SG9tZQ==\n3|DomainAuthenticated|\nnonsense\n9|Weird\n"
	ps := parseWinProfiles(out)
	if len(ps) != 4 || ps[0] != (winProfile{12, "Public", "Café Wi-Fi"}) || ps[1].Name != "Home" || ps[2].Cat != "DomainAuthenticated" {
		t.Fatalf("profiles %+v", ps)
	}
	ifs := []phoneIf{{Name: "Wi-Fi", Index: 12}, {Name: "Ethernet", Index: 7}, {Name: "Other", Index: 99}, {Name: "Corp", Index: 3}}
	v := winVerdicts(ifs, ps)
	if !v["Wi-Fi"].Public || v["Wi-Fi"].Cat != "public" || v["Wi-Fi"].Name != "Café Wi-Fi" {
		t.Errorf("Wi-Fi: %+v", v["Wi-Fi"])
	}
	if v["Ethernet"].Public || v["Ethernet"].Cat != "private" || v["Corp"].Cat != "domain" {
		t.Errorf("Ethernet %+v, Corp %+v", v["Ethernet"], v["Corp"])
	}
	if _, ok := v["Other"]; ok {
		t.Error("an adapter without a profile has no verdict")
	}
}

// NetworkManager's connection zone, else firewalld's default zone; no nmcli means no verdict.
func TestNMVerdicts(t *testing.T) {
	ifs := []phoneIf{{Name: "wlan0"}, {Name: "eth0"}, {Name: "eth0"}, {Name: "veth9"}}
	run := func(def string) func(string, ...string) string {
		return func(name string, args ...string) string {
			switch {
			case name == "nmcli" && strings.Join(args, " ") == "-t -g GENERAL.CONNECTION device show wlan0":
				return "Caf\\:e Wi-Fi\n"
			case name == "nmcli" && strings.Join(args, " ") == "-t -g GENERAL.CONNECTION device show eth0":
				return "Wired connection 1"
			case name == "nmcli" && strings.Join(args, " ") == "-t -g GENERAL.CONNECTION device show veth9":
				return "--"
			case name == "nmcli" && strings.Join(args, " ") == "-t -g connection.zone connection show id Caf:e Wi-Fi":
				return "public"
			case name == "nmcli" && strings.Join(args, " ") == "-t -g connection.zone connection show id Wired connection 1":
				return "" // the default zone
			case name == "firewall-cmd":
				return def
			}
			return ""
		}
	}
	v := nmVerdicts(ifs, run("home"))
	if !v["wlan0"].Public || v["wlan0"].Name != "Caf:e Wi-Fi" || v["eth0"].Public || v["eth0"].Cat != "private" {
		t.Errorf("zone home: %+v", v)
	}
	if _, ok := v["veth9"]; ok {
		t.Error("an adapter NetworkManager doesn't manage has no verdict")
	}
	if v := nmVerdicts(ifs, run("public")); !v["eth0"].Public {
		t.Errorf("a connection in the default zone follows firewalld: %+v", v["eth0"])
	}
	if v := nmVerdicts(ifs, func(string, ...string) string { return "" }); len(v) != 0 {
		t.Errorf("without nmcli nothing is said: %+v", v)
	}
}

func TestNetworksKeyAndAllow(t *testing.T) {
	env := fakePhoneEnv("Wi-Fi", "192.168.1.50/24", &netVerdict{Name: "Café", Cat: "public", Public: true})
	ns := env.networks(nil)
	if len(ns) != 1 || ns[0].Key != "Café|192.168.1.0/24" || ns[0].serving() || ns[0].Name != "Café" || ns[0].Subnet != "192.168.1.0/24" {
		t.Fatalf("networks %+v", ns)
	}
	ns = env.networks([]PhoneNet{{Key: "Café|192.168.1.0/24", Name: "Café"}})
	if !ns[0].Allowed || !ns[0].serving() {
		t.Fatalf("allowed: %+v", ns[0])
	}
	// the same name on another subnet is another network
	if ns := env.networks([]PhoneNet{{Key: "Café|192.168.7.0/24"}}); ns[0].serving() {
		t.Fatal("an allowance for another subnet counted")
	}
	// a network the system doesn't mark Public needs no allowance
	env = fakePhoneEnv("eth0", "10.0.0.5/8", nil)
	if ns := env.networks(nil); len(ns) != 1 || !ns[0].serving() || ns[0].Name != "eth0" {
		t.Fatalf("home: %+v", ns)
	}
}

// phoneAt starts phone control on a free port with the given machine, and waits for it to settle.
func phoneAt(t *testing.T, env phoneEnv) (*Manager, int) {
	t.Helper()
	m := newTestManager(t)
	m.phone.mu.Lock()
	m.phone.env = env
	m.phone.mu.Unlock()
	port := freePort(t)
	m.mu.Lock()
	m.settings.Phone.On, m.settings.Phone.Port = true, port
	m.applyPhone()
	m.mu.Unlock()
	m.phone.sync()
	t.Cleanup(m.phone.stop)
	return m, port
}

func pageStatus(port int) (int, error) {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/m", port))
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, nil
}

// On a network the system marks Public the page is off until it's allowed there, and the window can
// say so; allowing it is remembered per network.
func TestPhoneOffOnPublicNetwork(t *testing.T) {
	v := &netVerdict{Name: "Café", Cat: "public", Public: true}
	m, port := phoneAt(t, fakePhoneEnv("Wi-Fi", "127.0.0.1/8", v))
	if _, err := pageStatus(port); err == nil {
		t.Fatal("the phone page is served on a network marked Public")
	}
	view := m.phoneView()
	if view.Running || len(view.Addrs) != 0 || len(view.Nets) != 1 || !view.Nets[0].Public || view.Nets[0].Allowed || !view.Nets[0].Here {
		t.Fatalf("view %+v", view)
	}
	// no code to scan while it's off
	h := (&Server{m: m, token: "t"}).routes()
	if r := apiCall(t, h, "phone", map[string]any{"action": "pair"}); r["code"] != "public" {
		t.Fatalf("pair while off: %v", r)
	}
	if n := len(m.phoneView().Devices); n != 0 {
		t.Fatalf("a code was left behind: %d", n)
	}
	// a program with the secret can't allow it; only the window can
	if code := postCode(h, "phone", map[string]any{"action": "allowNet", "key": view.Nets[0].Key, "allow": true}); code != 403 {
		t.Fatalf("a program allowed a Public network: %d", code)
	}
	if err := m.allowPhoneNet(view.Nets[0].Key, true); err != nil {
		t.Fatal(err)
	}
	m.phone.sync()
	if code, err := pageStatus(port); err != nil || code != 200 {
		t.Fatalf("after allowing: %d %v", code, err)
	}
	view = m.phoneView()
	if !view.Running || len(view.Addrs) != 1 || view.Addrs[0] != "127.0.0.1" || !view.Nets[0].Allowed {
		t.Fatalf("view after allowing %+v", view)
	}
	// forgotten again
	if err := m.allowPhoneNet(view.Nets[0].Key, false); err != nil {
		t.Fatal(err)
	}
	m.phone.sync()
	if _, err := pageStatus(port); err == nil {
		t.Fatal("still served after the allowance was forgotten")
	}
	if err := m.allowPhoneNet("nowhere|10.9.9.0/24", true); err == nil {
		t.Fatal("allowed a network the computer isn't on")
	}
}

// The listener follows the network: a new address is served, one that went is dropped (and its
// connections with it), without Options being touched.
func TestPhoneRebindsWhenTheNetworkChanges(t *testing.T) {
	env := fakePhoneEnv("Wi-Fi", "127.0.0.1/8", nil)
	adapters := env.adapters
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	present := true
	env.adapters = func() []rawIf {
		<-mu
		defer func() { mu <- struct{}{} }()
		if !present {
			return nil
		}
		return adapters()
	}
	m, port := phoneAt(t, env)
	if code, err := pageStatus(port); err != nil || code != 200 {
		t.Fatalf("before: %d %v", code, err)
	}
	// a long-lived connection to the page
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /m HTTP/1.1\r\nHost: x\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	if _, err := conn.Read(buf); err != nil {
		t.Fatal(err)
	}
	<-mu
	present = false
	mu <- struct{}{}
	signalNetChange()
	waitUntil(t, "the listener to go", func() bool { _, run, _ := m.phone.status(); return !run })
	if _, err := pageStatus(port); err == nil {
		t.Fatal("still served after the network went")
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		if _, err := conn.Read(buf); err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				t.Fatal("the connection from the network that went stayed open")
			}
			break
		}
	}
	<-mu
	present = true
	mu <- struct{}{}
	signalNetChange()
	waitUntil(t, "the listener to come back", func() bool { _, run, _ := m.phone.status(); return run })
	if code, err := pageStatus(port); err != nil || code != 200 {
		t.Fatalf("after: %d %v", code, err)
	}
}

// postCode is apiCall's status: a program with the secret posts to the window's API.
func postCode(h http.Handler, path string, body any) int {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/"+path, bytes.NewReader(b))
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func waitUntil(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if f() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// With the real address check, a connection from an address that isn't private is closed before any
// request is read. (Here the only "home network" is loopback, which the check turns away.)
func TestPhoneTurnsAwayOtherAddresses(t *testing.T) {
	env := fakePhoneEnv("lo-test", "127.0.0.1/8", nil)
	env.remoteOK = lanRemoteOK
	_, port := phoneAt(t, env)
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "GET /m HTTP/1.1\r\nHost: x\r\n\r\n")
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, err := io.ReadAll(conn)
	if len(b) != 0 {
		t.Fatalf("a connection from a non-private address got an answer: %q (%v)", b, err)
	}
}

func TestPhoneNetViewShowsSavedNetworks(t *testing.T) {
	v := &netVerdict{Name: "Café", Cat: "public", Public: true}
	m, _ := phoneAt(t, fakePhoneEnv("Wi-Fi", "127.0.0.1/8", v))
	m.mu.Lock()
	m.settings.Phone.Allow = []PhoneNet{{Key: "Hotel|10.1.0.0/16", Name: "Hotel"}}
	m.mu.Unlock()
	nets := m.phoneView().Nets
	if len(nets) != 2 || !nets[0].Here || nets[1].Name != "Hotel" || nets[1].Here || !nets[1].Allowed {
		t.Fatalf("nets %+v", nets)
	}
	b, _ := json.Marshal(nets[1])
	if !strings.Contains(string(b), `"here":false`) {
		t.Fatal(string(b))
	}
}
