package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync/atomic"
)

// ProxySettings: "system" follows HTTP(S)_PROXY env vars when set, else the OS (Windows
// Internet Options, macOS network settings, GNOME or KDE), including PAC scripts and
// auto-detect (WPAD); "none" connects directly, "manual" uses the address below.
type ProxySettings struct {
	Mode   string `json:"mode"`
	URL    string `json:"url"`
	User   string `json:"user"`
	Pass   string `json:"pass"`
	Bypass string `json:"bypass"`

	PassSet bool `json:"passSet,omitempty"` // the settings API: a password is saved (Pass is then sent empty; see vault_settings.go)
}

var proxyCfg atomic.Value // ProxySettings

func init() { proxyCfg.Store(ProxySettings{Mode: "system"}) }

func setProxySettings(p ProxySettings) {
	if p.Mode == "" {
		p.Mode = "system"
	}
	if old, _ := proxyCfg.Load().(ProxySettings); old != p {
		resetProxyHealth() // different proxies: forget which ones failed
	}
	proxyCfg.Store(p)
	systemProxyReset()
	if tr, ok := httpClient.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func proxyForRequest(req *http.Request) (*url.URL, error) { return proxyFor(req.URL) }

func proxyFor(u *url.URL) (*url.URL, error) {
	p, _ := proxyCfg.Load().(ProxySettings)
	return proxyForWith(p, u)
}

func proxyForWith(p ProxySettings, u *url.URL) (*url.URL, error) {
	host := u.Hostname()
	if isLoopback(host) {
		return nil, nil
	}
	switch p.Mode {
	case "none":
		return nil, nil
	case "manual":
		if strings.TrimSpace(p.URL) == "" || bypassed(host, p.Bypass) {
			return nil, nil
		}
		pu, err := parseProxyURL(p.URL)
		if err != nil {
			return nil, err
		}
		if pw, _ := pu.User.Password(); isSealed(pw) {
			// Its key isn't here: send no password rather than the sealed text.
			pu.User = url.User(pu.User.Username())
		}
		if p.User != "" {
			pu.User = url.UserPassword(p.User, usableSecret(p.Pass))
		}
		return pu, nil
	default:
		if pu, err := http.ProxyFromEnvironment(&http.Request{URL: u}); pu != nil || err != nil {
			return pu, err
		}
		return systemProxy(u)
	}
}

func parseProxyURL(s string) (*url.URL, error) {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad proxy address %q", s)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy type %q (use http:// or socks5://)", u.Scheme)
	}
	return u, nil
}

// bypassed reports whether host matches a bypass list ("<local>;*.corp.com;10.*;192.168.0.0/16").
func bypassed(host, list string) bool {
	host = strings.ToLower(host)
	for _, p := range strings.FieldsFunc(list, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if p == "<local>" {
			if !strings.Contains(host, ".") {
				return true
			}
			continue
		}
		if addr, bits, ok := strings.Cut(p, "/"); ok {
			if !strings.Contains(addr, ":") { // macOS writes 169.254/16 for 169.254.0.0/16
				addr += strings.Repeat(".0", max(0, 3-strings.Count(addr, ".")))
			}
			if _, n, err := net.ParseCIDR(addr + "/" + bits); err == nil {
				if ip := net.ParseIP(host); ip != nil && n.Contains(ip) {
					return true
				}
				continue
			}
		}
		p = strings.TrimPrefix(strings.TrimPrefix(p, "http://"), "https://")
		if ok, _ := path.Match(p, host); ok {
			return true
		}
		if strings.HasPrefix(p, ".") && strings.HasSuffix(host, p) {
			return true
		}
		if !strings.Contains(p, "*") && (host == p || strings.HasSuffix(host, "."+p)) {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
