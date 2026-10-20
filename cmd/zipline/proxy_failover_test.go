package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"zipline/internal/pac"
)

func TestProxyChoices(t *testing.T) {
	cases := []struct {
		list, scheme string
		want         []string
	}{
		{"PROXY a:8080; PROXY b:8080; DIRECT", "http", []string{"a:8080", "b:8080", ""}},
		{"PROXY a:1; SOCKS5 s:2; HTTPS h:3; SOCKS t:4; QUIC q:5", "https", []string{"a:1", "socks5://s:2", "https://h:3", "socks5://t:4"}},
		{"http=a:1;https=b:2;socks=s:3", "https", []string{"b:2", "socks5://s:3"}},
		{"socks=s:3;http=a:1", "http", []string{"a:1", "socks5://s:3"}},
		{"proxy.corp:8080", "http", []string{"proxy.corp:8080"}},
		{"a:1;b:2", "http", []string{"a:1", "b:2"}}, // WinHTTP's answer for a PAC list
		{"PROXY ; direct", "http", []string{""}},
		{"", "http", nil},
	}
	for _, c := range cases {
		if got := proxyChoices(c.list, c.scheme); !reflect.DeepEqual(got, c.want) {
			t.Errorf("proxyChoices(%q, %q) = %q, want %q", c.list, c.scheme, got, c.want)
		}
	}
	if !bypassed("127.0.0.5", "localhost;127.0.0.0/8") || !bypassed("169.254.3.4", "*.local, 169.254/16") ||
		bypassed("10.0.0.1", "127.0.0.0/8") || !bypassed("fd00::1", "fd00::/8") {
		t.Error("CIDR bypass entries not honoured")
	}
}

func TestProxyFailoverList(t *testing.T) {
	resetProxyHealth()
	defer resetProxyHealth()
	target, _ := url.Parse("http://files.example/f.bin")
	pick := func(list string) string {
		t.Helper()
		pu, err := proxyFromList(list, target)
		if err != nil {
			t.Fatal(err)
		}
		if pu == nil {
			return "DIRECT"
		}
		return pu.Host
	}
	list := "PROXY 10.9.9.1:8080; PROXY 10.9.9.2:8080; DIRECT"
	if got := pick(list); got != "10.9.9.1:8080" {
		t.Fatalf("first pick %s", got)
	}
	markProxyBad(&url.URL{Scheme: "http", Host: "10.9.9.1:8080"})
	if got := pick(list); got != "10.9.9.2:8080" {
		t.Fatalf("after the first failed: %s", got)
	}
	if got := pickProxy(list, "http"); got != "10.9.9.2:8080" {
		t.Errorf("pickProxy ignores failures: %s", got)
	}
	markProxyBad(&url.URL{Scheme: "http", Host: "10.9.9.2:8080"})
	if got := pick(list); got != "DIRECT" {
		t.Fatalf("after both failed: %s", got)
	}
	if got := pick("PROXY 10.9.9.1:8080; PROXY 10.9.9.2:8080"); got != "10.9.9.1:8080" {
		t.Errorf("all bad, no DIRECT: %s (want the first again)", got)
	}
	// a failure is forgotten after half an hour
	proxyHealth.mu.Lock()
	proxyHealth.bad["10.9.9.1:8080"] = time.Now().Add(-proxyBadFor - time.Second)
	proxyHealth.mu.Unlock()
	if got := pick(list); got != "10.9.9.1:8080" {
		t.Errorf("expired failure still counted: %s", got)
	}
	if _, err := proxyFromList("PROXY http://[bad", target); err == nil {
		t.Error("malformed list accepted")
	}
	if got := pick("PROXY http://[bad; PROXY 10.9.9.3:3128"); got != "10.9.9.3:3128" {
		t.Errorf("malformed entry not skipped: %s", got)
	}

	// changing the settings starts over
	defer setProxySettings(ProxySettings{Mode: "system"})
	setProxySettings(ProxySettings{Mode: "none"})
	if got := pick(list); got != "10.9.9.1:8080" {
		t.Errorf("settings change kept old failures: %s", got)
	}
}

// closedAddr is a local address nothing listens on.
func closedAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func getVia(list string, target string) (string, error) {
	tr := &http.Transport{Proxy: func(r *http.Request) (*url.URL, error) { return proxyFromList(list, r.URL) }}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get(target)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return string(b), err
}

func TestNoteProxyFailureHTTP(t *testing.T) {
	resetProxyHealth()
	defer resetProxyHealth()
	// a forward proxy that answers every request itself
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "via proxy for "+r.URL.Host)
	}))
	defer good.Close()
	dead := closedAddr(t)
	list := "PROXY " + dead + "; PROXY " + strings.TrimPrefix(good.URL, "http://") + "; DIRECT"

	_, err := getVia(list, "http://files.example/f.bin")
	if err == nil {
		t.Fatal("request through a dead proxy worked")
	}
	var op *net.OpError
	if !strings.Contains(err.Error(), "proxyconnect tcp: dial tcp "+dead) || !errors.As(err, &op) || op.Op != "proxyconnect" {
		t.Fatalf("unexpected error shape: %v", err)
	}
	if !noteProxyFailure(fmt.Errorf("segment 3: %w", err)) {
		t.Fatal("proxy failure not recognised")
	}
	body, err := getVia(list, "http://files.example/f.bin")
	if err != nil || body != "via proxy for files.example" {
		t.Fatalf("retry didn't fail over: %q, %v", body, err)
	}

	// the same failure, flattened to text on the way
	resetProxyHealth()
	_, err = getVia(list, "http://files.example/f.bin")
	if !noteProxyFailure(errors.New(err.Error())) {
		t.Errorf("flattened error not recognised: %v", err)
	}
	if got := pickProxy(list, "http"); got == dead {
		t.Error("flattened error didn't mark the proxy")
	}

	for _, e := range []error{nil, context.Canceled, io.EOF, errors.New("connection reset by peer"),
		&url.Error{Op: "Get", URL: "http://x/", Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: context.Canceled}}} {
		if noteProxyFailure(e) {
			t.Errorf("noteProxyFailure(%v) = true", e)
		}
	}
}

// fakeSOCKS5 accepts SOCKS5 handshakes and answers each connect request with reply
// (0 = success is never sent: 4 means host unreachable), or hangs up when reply < 0.
func fakeSOCKS5(t *testing.T, reply int) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				if reply < 0 {
					return
				}
				buf := make([]byte, 262)
				if _, err := io.ReadFull(c, buf[:2]); err != nil {
					return
				}
				io.ReadFull(c, buf[:buf[1]])
				c.Write([]byte{5, 0}) // no authentication
				io.ReadFull(c, buf[:4])
				switch buf[3] {
				case 1:
					io.ReadFull(c, buf[:4+2])
				case 3:
					io.ReadFull(c, buf[:1])
					io.ReadFull(c, buf[:int(buf[0])+2])
				case 4:
					io.ReadFull(c, buf[:16+2])
				}
				c.Write([]byte{5, byte(reply), 0, 1, 0, 0, 0, 0, 0, 0})
			}()
		}
	}()
	return l.Addr().String()
}

func TestNoteProxyFailureSOCKS(t *testing.T) {
	resetProxyHealth()
	defer resetProxyHealth()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "direct") }))
	defer site.Close()

	// nothing listening: "proxyconnect tcp: dial tcp …: connection refused", as for HTTP proxies
	dead := closedAddr(t)
	list := "SOCKS5 " + dead + "; DIRECT"
	_, err := getVia(list, site.URL)
	if err == nil || !strings.Contains(err.Error(), "proxyconnect tcp:") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !noteProxyFailure(err) {
		t.Fatalf("dead SOCKS proxy not recognised: %v", err)
	}
	if body, err := getVia(list, site.URL); err != nil || body != "direct" {
		t.Fatalf("no failover to DIRECT: %q, %v", body, err)
	}

	// listening but hanging up: "socks connect tcp proxy->site: EOF"
	resetProxyHealth()
	hangup := fakeSOCKS5(t, -1)
	_, err = getVia("SOCKS5 "+hangup+"; DIRECT", site.URL)
	var op *net.OpError
	if err == nil || !errors.As(err, &op) || op.Op != "socks connect" {
		t.Fatalf("unexpected error: %v", err)
	}
	if !noteProxyFailure(err) || !noteProxyFailure(errors.New(err.Error())) {
		t.Errorf("broken SOCKS proxy not recognised: %v", err)
	}

	// a SOCKS reply code means the proxy works but couldn't reach the site: not its fault
	resetProxyHealth()
	unreachable := fakeSOCKS5(t, 4)
	_, err = getVia("SOCKS5 "+unreachable+"; DIRECT", site.URL)
	if err == nil || !strings.Contains(err.Error(), "host unreachable") {
		t.Fatalf("unexpected error: %v", err)
	}
	if noteProxyFailure(err) || noteProxyFailure(errors.New(err.Error())) {
		t.Errorf("destination failure blamed on the proxy: %v", err)
	}
}

// fakeProxyDNS answers the proxy scripts' lookups from hosts.
func fakeProxyDNS(t *testing.T, hosts map[string][]string) {
	t.Helper()
	old := pac.Resolver
	pac.Resolver = func(_ context.Context, host string) ([]net.IP, error) {
		var ips []net.IP
		for _, s := range hosts[host] {
			ips = append(ips, net.ParseIP(s))
		}
		if len(ips) == 0 {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		return ips, nil
	}
	pac.ForgetLookups()
	t.Cleanup(func() {
		pac.Resolver = old
		pac.ForgetLookups()
	})
}

func TestNoteProxyFailureWhich(t *testing.T) {
	resetProxyHealth()
	defer resetProxyHealth()
	fakeProxyDNS(t, map[string][]string{"proxy-a.test": {"10.0.0.1"}, "proxy-b.test": {"10.0.0.2"}})
	ua, _ := url.Parse("http://one.example/")
	ub, _ := url.Parse("http://two.example/")
	la, lb := "PROXY proxy-a.test:8080; DIRECT", "PROXY proxy-b.test:8080; DIRECT"
	proxyFromList(la, ua)
	proxyFromList(lb, ub)
	refused := func(target string, inner error) error {
		return &url.Error{Op: "Get", URL: target, Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: inner}}
	}
	// the error carries the address proxy-b.test resolved to
	err := refused("http://two.example/", &net.OpError{Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.2"), Port: 8080}, Err: errors.New("connect: connection refused")})
	if !noteProxyFailure(err) {
		t.Fatal("not recognised")
	}
	if pu, _ := proxyFromList(lb, ub); pu != nil {
		t.Errorf("proxy-b still used: %v", pu)
	}
	if pu, _ := proxyFromList(la, ua); pu == nil || pu.Host != "proxy-a.test:8080" {
		t.Errorf("proxy-a blamed: %v", pu)
	}
	// a proxy name that doesn't resolve
	if !noteProxyFailure(refused("", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "proxy-a.test"}})) {
		t.Fatal("DNS failure not recognised")
	}
	if pu, _ := proxyFromList(la, ua); pu != nil {
		t.Errorf("proxy-a still used: %v", pu)
	}
	// a proxy Zipline didn't pick from a list (manual settings): left alone
	if noteProxyFailure(refused("http://x/", &net.OpError{Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 3128}, Err: errors.New("i/o timeout")})) {
		t.Error("unknown proxy marked")
	}
	// the only entry: marked, but nothing else to try
	resetProxyHealth()
	only := "PROXY proxy-a.test:8080"
	proxyFromList(only, ua)
	if noteProxyFailure(refused("http://one.example/", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Name: "proxy-a.test"}})) {
		t.Error("true with no other entry to try")
	}
}
