//go:build !windows

package main

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Linux and BSD desktops keep proxy settings their own way: GNOME (and most GTK-based
// desktops) in gsettings, KDE in ~/.config/kioslaverc.

func desktopProxy() sysProxyConf {
	kde := strings.Contains(strings.ToUpper(os.Getenv("XDG_CURRENT_DESKTOP")), "KDE") || os.Getenv("KDE_FULL_SESSION") != ""
	if !kde {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// one command for every key, rather than a gsettings get for each
		if out, err := exec.CommandContext(ctx, "gsettings", "list-recursively", "org.gnome.system.proxy").Output(); err == nil {
			return parseGsettings(out)
		}
	}
	if b, err := os.ReadFile(kioslavercPath()); err == nil {
		return parseKioslaverc(b)
	}
	return sysProxyConf{}
}

// parseGsettings reads `gsettings list-recursively org.gnome.system.proxy`:
//
//	org.gnome.system.proxy mode 'manual'
//	org.gnome.system.proxy ignore-hosts ['localhost', '127.0.0.0/8']
//	org.gnome.system.proxy.http host 'proxy.corp'
//	org.gnome.system.proxy.http port 8080
func parseGsettings(out []byte) sysProxyConf {
	v := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.SplitN(strings.TrimSpace(line), " ", 3); len(f) == 3 {
			v[strings.TrimPrefix(f[0], "org.gnome.system.proxy")+" "+f[1]] = strings.TrimSpace(f[2])
		}
	}
	str := func(k string) string {
		if s := gvariantStrings(v[k]); len(s) > 0 {
			return s[0]
		}
		return ""
	}
	var c sysProxyConf
	switch str(" mode") {
	case "auto":
		c.pacURL = str(" autoconfig-url")
		c.wpad = c.pacURL == "" // GNOME's "automatic" with no URL means auto-detect
	case "manual":
		hostPort := func(schema string) string {
			h, port := str(schema+" host"), v[schema+" port"]
			if h != "" && port != "" && port != "0" {
				h = net.JoinHostPort(h, port)
			}
			return h
		}
		c.http, c.https, c.socks = hostPort(".http"), hostPort(".https"), hostPort(".socks")
		if c.http != "" && v[".http use-authentication"] == "true" {
			c.http = url.UserPassword(str(".http authentication-user"), str(".http authentication-password")).String() + "@" + c.http
		}
		if c.https == "" && v[" use-same-proxy"] == "true" {
			c.https = c.http
		}
		c.bypass = strings.Join(gvariantStrings(v[" ignore-hosts"]), ";")
	}
	return c
}

// gvariantStrings pulls the quoted strings out of a GVariant value: 'a' or ['a', 'b'].
func gvariantStrings(s string) []string {
	var out []string
	for i := 0; i < len(s); i++ {
		q := s[i]
		if q != '\'' && q != '"' {
			continue
		}
		var b strings.Builder
		for i++; i < len(s) && s[i] != q; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
			}
			b.WriteByte(s[i])
		}
		out = append(out, b.String())
	}
	return out
}

func kioslavercPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "kioslaverc")
}

// parseKioslaverc reads KDE's [Proxy Settings]: ProxyType 1 is manual (httpProxy,
// httpsProxy, socksProxy, NoProxyFor), 2 a PAC script, 3 auto-detect (WPAD).
func parseKioslaverc(b []byte) sysProxyConf {
	v := map[string]string{}
	section := ""
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		k, val, ok := strings.Cut(line, "=")
		if !ok || section != "Proxy Settings" {
			continue
		}
		k, _, _ = strings.Cut(k, "[") // httpProxy[$e]
		v[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	var c sysProxyConf
	switch v["ProxyType"] {
	case "1":
		c.http, c.https, c.socks = kdeProxy(v["httpProxy"]), kdeProxy(v["httpsProxy"]), kdeProxy(v["socksProxy"])
		c.bypass = v["NoProxyFor"]
	case "2":
		c.pacURL = v["Proxy Config Script"]
	case "3":
		c.wpad = true
	}
	return c
}

// kdeProxy turns KDE's "http://host 8080" (or "host:8080") into "host:8080".
func kdeProxy(s string) string {
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "/")
	user := ""
	if i := strings.LastIndex(s, "@"); i >= 0 {
		user, s = s[:i+1], s[i+1:]
	}
	if h, port, ok := strings.Cut(s, " "); ok {
		s = h
		if port = strings.TrimSpace(port); port != "" && port != "0" {
			s = net.JoinHostPort(h, port)
		}
	}
	if s == "" {
		return ""
	}
	return user + s
}
