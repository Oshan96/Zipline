//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// System proxy settings on macOS (`scutil --proxy`) and on Linux/BSD desktops (GNOME or
// KDE, proxy_desktop.go), including PAC scripts and auto-detect (WPAD), which Zipline
// runs itself (proxy_pac.go). Environment variables, when set, win before this is asked.

// sysProxyConf is the OS's proxy configuration.
type sysProxyConf struct {
	pacURL             string // auto-config script; "" for none
	wpad               bool   // auto-detect: look for http://wpad/wpad.dat
	http, https, socks string // static proxies, "host:port"
	bypass             string // hosts that go direct, ";"-separated
}

// list gives the static proxies for a scheme as a PAC-style list: the protocol's own
// proxy, then SOCKS as a fallback.
func (c sysProxyConf) list(scheme string) string {
	var l []string
	p := c.http
	if scheme == "https" {
		p = c.https
	}
	if p != "" {
		l = append(l, "PROXY "+p)
	}
	if c.socks != "" {
		l = append(l, "SOCKS5 "+c.socks)
	}
	return strings.Join(l, "; ")
}

// loadSysProxy reads the OS settings; tests swap it for a fake.
var loadSysProxy = func() sysProxyConf {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("scutil", "--proxy").Output()
		if err != nil {
			return sysProxyConf{}
		}
		return parseScutil(out)
	}
	return desktopProxy()
}

var sysProxy struct {
	mu   sync.Mutex
	at   time.Time
	conf sysProxyConf
}

func systemProxyReset() {
	sysProxy.mu.Lock()
	sysProxy.at = time.Time{}
	sysProxy.mu.Unlock()
	pacReset()
}

func systemProxy(u *url.URL) (*url.URL, error) {
	sysProxy.mu.Lock()
	if time.Since(sysProxy.at) > time.Minute { // reading the settings runs a command
		sysProxy.conf, sysProxy.at = loadSysProxy(), time.Now()
	}
	c := sysProxy.conf
	sysProxy.mu.Unlock()
	if c.pacURL != "" || c.wpad {
		// a configured script is worth waiting for; WPAD is a guess, looked for in the background
		pac, wait := c.pacURL, true
		if pac == "" {
			pac, wait = wpadURL, false
		}
		if list, ok := pacFor(pac, u, wait); ok {
			return proxyFromList(list, u)
		}
	}
	if bypassed(u.Hostname(), c.bypass) {
		return nil, nil
	}
	return proxyFromList(c.list(u.Scheme), u)
}

// parseScutil reads `scutil --proxy` (macOS):
//
//	<dictionary> {
//	  ExceptionsList : <array> {
//	    0 : *.local
//	  }
//	  HTTPEnable : 1
//	  HTTPPort : 8080
//	  HTTPProxy : proxy.corp
//	  ProxyAutoConfigEnable : 1
//	  ProxyAutoConfigURLString : http://pac.corp/proxy.pac
//	}
func parseScutil(out []byte) sysProxyConf {
	vals := map[string]string{}
	var except []string
	inExc := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "ExceptionsList") {
			inExc = true
			continue
		}
		k, v, ok := strings.Cut(line, " : ")
		switch {
		case inExc && line == "}":
			inExc = false
		case inExc && ok:
			except = append(except, strings.TrimSpace(v))
		case ok:
			vals[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	hostPort := func(prefix string) string {
		h := vals[prefix+"Proxy"]
		if vals[prefix+"Enable"] != "1" || h == "" {
			return ""
		}
		if p := vals[prefix+"Port"]; p != "" {
			h = net.JoinHostPort(h, p)
		}
		return h
	}
	c := sysProxyConf{http: hostPort("HTTP"), https: hostPort("HTTPS"), socks: hostPort("SOCKS")}
	if vals["ProxyAutoConfigEnable"] == "1" {
		c.pacURL = vals["ProxyAutoConfigURLString"]
	}
	c.wpad = vals["ProxyAutoDiscoveryEnable"] == "1"
	if vals["ExcludeSimpleHostnames"] == "1" {
		except = append(except, "<local>")
	}
	c.bypass = strings.Join(except, ";")
	return c
}
