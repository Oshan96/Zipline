//go:build !windows

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseScutil(t *testing.T) {
	out := `<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
    1 : 169.254/16
  }
  ExcludeSimpleHostnames : 1
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 8080
  HTTPProxy : proxy.corp
  HTTPSEnable : 1
  HTTPSPort : 8443
  HTTPSProxy : secure.corp
  ProxyAutoConfigEnable : 1
  ProxyAutoConfigURLString : http://pac.corp/proxy.pac
  ProxyAutoDiscoveryEnable : 1
  SOCKSEnable : 0
  SOCKSPort : 1080
  SOCKSProxy : socks.corp
}`
	want := sysProxyConf{pacURL: "http://pac.corp/proxy.pac", wpad: true, http: "proxy.corp:8080", https: "secure.corp:8443",
		bypass: "*.local;169.254/16;<local>"}
	if got := parseScutil([]byte(out)); got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	if got := (sysProxyConf{http: "h:1", socks: "s:2"}).list("http"); got != "PROXY h:1; SOCKS5 s:2" {
		t.Errorf("static list %q", got)
	}
}

func TestParseGsettings(t *testing.T) {
	manual := `org.gnome.system.proxy autoconfig-url ''
org.gnome.system.proxy ignore-hosts ['localhost', '127.0.0.0/8', '::1', '*.corp']
org.gnome.system.proxy mode 'manual'
org.gnome.system.proxy use-same-proxy false
org.gnome.system.proxy.ftp host ''
org.gnome.system.proxy.ftp port 0
org.gnome.system.proxy.http authentication-password 'p@ss'
org.gnome.system.proxy.http authentication-user 'bob'
org.gnome.system.proxy.http enabled false
org.gnome.system.proxy.http host 'proxy.corp'
org.gnome.system.proxy.http port 3128
org.gnome.system.proxy.http use-authentication true
org.gnome.system.proxy.https host 'secure.corp'
org.gnome.system.proxy.https port 3129
org.gnome.system.proxy.socks host 'socks.corp'
org.gnome.system.proxy.socks port 1080
`
	want := sysProxyConf{http: "bob:p%40ss@proxy.corp:3128", https: "secure.corp:3129", socks: "socks.corp:1080",
		bypass: "localhost;127.0.0.0/8;::1;*.corp"}
	if got := parseGsettings([]byte(manual)); got != want {
		t.Errorf("manual: got %+v\nwant %+v", got, want)
	}
	pu, err := proxyFromList(want.list("http"), &url.URL{Scheme: "http", Host: "x"})
	if err != nil || pu.User.String() != "bob:p%40ss" || pu.Host != "proxy.corp:3128" {
		t.Errorf("credentials lost: %v %v", pu, err)
	}
	auto := "org.gnome.system.proxy mode 'auto'\norg.gnome.system.proxy autoconfig-url 'http://pac.corp/x.pac'\n"
	if got := parseGsettings([]byte(auto)); got != (sysProxyConf{pacURL: "http://pac.corp/x.pac"}) {
		t.Errorf("auto: %+v", got)
	}
	detect := "org.gnome.system.proxy mode 'auto'\norg.gnome.system.proxy autoconfig-url ''\n"
	if got := parseGsettings([]byte(detect)); got != (sysProxyConf{wpad: true}) {
		t.Errorf("auto-detect: %+v", got)
	}
	none := strings.Replace(manual, "'manual'", "'none'", 1)
	if got := parseGsettings([]byte(none)); got != (sysProxyConf{}) {
		t.Errorf("none: %+v", got)
	}
}

func TestParseKioslaverc(t *testing.T) {
	manual := `[$Version]
update_info=kioslave.upd:kioslave-qt5

[Proxy Settings]
NoProxyFor=localhost,127.0.0.1,.corp.example
Proxy Config Script=http://pac.corp/proxy.pac
ProxyType=1
httpProxy=http://proxy.corp 8080
httpsProxy=http://user:pw@proxy.corp 8443
socksProxy=socks://socks.corp:1080
ftpProxy=

[Other]
httpProxy=http://ignored 1
`
	want := sysProxyConf{http: "proxy.corp:8080", https: "user:pw@proxy.corp:8443", socks: "socks.corp:1080",
		bypass: "localhost,127.0.0.1,.corp.example"}
	if got := parseKioslaverc([]byte(manual)); got != want {
		t.Errorf("manual: got %+v\nwant %+v", got, want)
	}
	if got := parseKioslaverc([]byte(strings.Replace(manual, "ProxyType=1", "ProxyType=2", 1))); got != (sysProxyConf{pacURL: "http://pac.corp/proxy.pac"}) {
		t.Errorf("PAC: %+v", got)
	}
	if got := parseKioslaverc([]byte(strings.Replace(manual, "ProxyType=1", "ProxyType=3", 1))); got != (sysProxyConf{wpad: true}) {
		t.Errorf("WPAD: %+v", got)
	}
	if got := parseKioslaverc([]byte(strings.Replace(manual, "ProxyType=1", "ProxyType=0", 1))); got != (sysProxyConf{}) {
		t.Errorf("none: %+v", got)
	}
}

// useSysProxy makes systemProxy see conf, as if read from the OS.
func useSysProxy(t *testing.T, conf sysProxyConf) {
	t.Helper()
	old := loadSysProxy
	loadSysProxy = func() sysProxyConf { return conf }
	systemProxyReset()
	resetProxyHealth()
	t.Cleanup(func() {
		loadSysProxy = old
		systemProxyReset()
		resetProxyHealth()
	})
}

func sysProxyFor(t *testing.T, raw string) string {
	t.Helper()
	u, _ := url.Parse(raw)
	pu, err := systemProxy(u)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	if pu == nil {
		return "DIRECT"
	}
	return pu.String()
}

const testPAC = `function FindProxyForURL(url, host) {
	if (dnsDomainIs(host, ".local.test")) return "DIRECT";
	return "PROXY 10.1.1.1:8080; PROXY 10.1.1.2:8080; DIRECT";
}`

func TestSystemProxyPAC(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		switch r.URL.Path {
		case "/proxy.pac":
			w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
			fmt.Fprint(w, testPAC)
		case "/broken.pac":
			fmt.Fprint(w, "function FindProxyForURL(url, host) { return ")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	useSysProxy(t, sysProxyConf{pacURL: srv.URL + "/proxy.pac", http: "static.test:3128", bypass: "*.static-bypass.test"})
	if got := sysProxyFor(t, "http://files.example/a.bin"); got != "http://10.1.1.1:8080" {
		t.Errorf("PAC answer not used: %s", got)
	}
	if got := sysProxyFor(t, "https://files.example/b.bin"); got != "http://10.1.1.1:8080" {
		t.Errorf("https: %s", got)
	}
	if got := sysProxyFor(t, "http://nas.local.test/"); got != "DIRECT" {
		t.Errorf("PAC DIRECT: %s", got)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("PAC fetched %d times, want once", n)
	}
	markProxyBad(&url.URL{Scheme: "http", Host: "10.1.1.1:8080"})
	if got := sysProxyFor(t, "http://files.example/a.bin"); got != "http://10.1.1.2:8080" {
		t.Errorf("no failover within the PAC answer: %s", got)
	}

	// a PAC file that's missing, or that doesn't run, falls back to the static settings
	for _, path := range []string{"/missing.pac", "/broken.pac"} {
		useSysProxy(t, sysProxyConf{pacURL: srv.URL + path, http: "static.test:3128", bypass: "*.static-bypass.test"})
		if got := sysProxyFor(t, "http://files.example/"); got != "http://static.test:3128" {
			t.Errorf("%s: no fallback to static: %s", path, got)
		}
		if got := sysProxyFor(t, "http://a.static-bypass.test/"); got != "DIRECT" {
			t.Errorf("%s: static bypass ignored: %s", path, got)
		}
	}

	// file://
	f := filepath.Join(t.TempDir(), "proxy.pac")
	os.WriteFile(f, []byte(testPAC), 0o644)
	useSysProxy(t, sysProxyConf{pacURL: "file://" + f})
	if got := sysProxyFor(t, "http://files.example/"); got != "http://10.1.1.1:8080" {
		t.Errorf("file:// PAC: %s", got)
	}
}

func TestSystemProxyWPAD(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // a slow WPAD server mustn't hold up downloads
		fmt.Fprint(w, testPAC)
	}))
	defer srv.Close()
	defer close(release)
	old := wpadURL
	wpadURL = srv.URL + "/wpad.dat"
	defer func() { wpadURL = old }()

	useSysProxy(t, sysProxyConf{wpad: true, http: "static.test:3128"})
	start := time.Now()
	if got := sysProxyFor(t, "http://files.example/"); got != "http://static.test:3128" {
		t.Errorf("before WPAD answers: %s", got)
	}
	if time.Since(start) > time.Second {
		t.Error("waited for WPAD")
	}
	release <- struct{}{}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		pacCache.mu.Lock()
		s := pacCache.scripts[wpadURL]
		fetched := s != nil && s.ok
		pacCache.mu.Unlock()
		if fetched {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("WPAD script never arrived")
		}
	}
	if got := sysProxyFor(t, "http://other.example/"); got != "http://10.1.1.1:8080" {
		t.Errorf("after WPAD answered: %s", got)
	}
}

func TestFetchPAC(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big.pac":
			w.Write(make([]byte, pacMaxSize+1))
		case "/error.pac":
			http.Error(w, "nope", 500)
		default:
			fmt.Fprint(w, testPAC)
		}
	}))
	defer srv.Close()
	if s, err := fetchPAC(srv.URL + "/ok.pac"); err != nil || s != testPAC {
		t.Errorf("ok: %v", err)
	}
	for _, u := range []string{srv.URL + "/big.pac", srv.URL + "/error.pac", "ftp://x/y.pac", "file:///no/such/file.pac"} {
		if _, err := fetchPAC(u); err == nil {
			t.Errorf("%s: no error", u)
		}
	}
}
