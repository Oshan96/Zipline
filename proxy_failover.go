package main

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Proxy failover. A PAC script or proxy list can name several proxies
// ("PROXY a:8080; PROXY b:8080; DIRECT"); one that can't be reached is skipped for half
// an hour so new connections go to the next entry.

const proxyBadFor = 30 * time.Minute

var proxyHealth struct {
	mu     sync.Mutex
	bad    map[string]time.Time // proxy host:port → when it couldn't be reached
	recent map[string][]string  // proxy host:port → the entries after it in its list ("" = DIRECT)
	byURL  map[string]string    // scheme://host fetched → proxy host:port handed out for it
}

// proxyKey identifies a proxy by address, with the port Go's transport would use.
func proxyKey(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

var pacKeywords = map[string]bool{"PROXY": true, "HTTP": true, "HTTPS": true, "SOCKS": true, "SOCKS4": true, "SOCKS5": true}

// proxyChoices splits a proxy list into the entries to try for a scheme, in order, with
// "" for DIRECT. It reads PAC answers ("PROXY a:1; HTTPS b:2; SOCKS5 c:3; DIRECT") and
// Windows lists ("host:port", "http=h:p;https=h:p", "socks=h:p").
func proxyChoices(list, scheme string) []string {
	var out, socks []string
	for _, e := range strings.Split(list, ";") {
		e = strings.TrimSpace(e)
		kw, rest, _ := strings.Cut(e, " ")
		rest = strings.TrimSpace(rest)
		switch kw = strings.ToUpper(kw); {
		case e == "":
		case kw == "DIRECT":
			out = append(out, "")
		case rest == "" && strings.Contains(e, "="):
			k, v, _ := strings.Cut(e, "=")
			switch k = strings.ToLower(strings.TrimSpace(k)); k {
			case scheme:
				out = append(out, strings.TrimSpace(v))
			case "socks": // Windows uses SOCKS only for what has no proxy of its own
				socks = append(socks, "socks5://"+strings.TrimSpace(v))
			}
		case rest == "":
			if !pacKeywords[kw] { // a bare host:port (a keyword with no address is skipped)
				out = append(out, e)
			}
		case kw == "PROXY" || kw == "HTTP":
			out = append(out, rest)
		case kw == "HTTPS":
			out = append(out, "https://"+rest)
		case pacKeywords[kw]: // SOCKS, SOCKS4, SOCKS5: Go speaks SOCKS5
			out = append(out, "socks5://"+rest)
		}
	}
	return append(out, socks...)
}

// pickChoice returns the index of the entry to use: the first proxy not marked bad, or
// -1 for a direct connection when DIRECT comes first. When every proxy is bad (and there's
// no DIRECT), the first is tried again.
func pickChoice(cands []string) int {
	proxyHealth.mu.Lock()
	defer proxyHealth.mu.Unlock()
	first := -1
	for i, c := range cands {
		if c == "" {
			return -1
		}
		pu, err := parseProxyURL(c)
		if err != nil {
			continue
		}
		if first < 0 {
			first = i
		}
		if !proxyIsBad(proxyKey(pu)) {
			return i
		}
	}
	return first
}

// pickProxy returns the entry to use from a proxy list ("" for a direct connection).
func pickProxy(list, scheme string) string {
	cands := proxyChoices(list, scheme)
	if i := pickChoice(cands); i >= 0 {
		return cands[i]
	}
	return ""
}

// proxyFromList picks the proxy for target from a list (a PAC answer or the system's
// static settings), skipping ones that recently failed.
func proxyFromList(list string, target *url.URL) (*url.URL, error) {
	cands := proxyChoices(list, target.Scheme)
	i := pickChoice(cands)
	if i < 0 {
		// nothing usable: a malformed setting shouldn't pass silently as "direct"
		if len(cands) > 0 && cands[0] != "" {
			if _, err := parseProxyURL(cands[0]); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	pu, err := parseProxyURL(cands[i])
	if err != nil {
		return nil, err
	}
	k := proxyKey(pu)
	proxyHealth.mu.Lock()
	if proxyHealth.recent == nil || len(proxyHealth.recent) >= 64 {
		proxyHealth.recent = map[string][]string{}
	}
	if proxyHealth.byURL == nil || len(proxyHealth.byURL) >= 1024 {
		proxyHealth.byURL = map[string]string{}
	}
	proxyHealth.recent[k] = cands[i+1:] // to tell later whether anything is left to try
	proxyHealth.byURL[target.Scheme+"://"+target.Host] = k
	proxyHealth.mu.Unlock()
	return pu, nil
}

// proxyIsBad needs proxyHealth.mu held.
func proxyIsBad(key string) bool {
	t, ok := proxyHealth.bad[key]
	return ok && time.Since(t) < proxyBadFor
}

// markProxyBad keeps a proxy out of use for half an hour (unless nothing else is left).
func markProxyBad(p *url.URL) {
	proxyHealth.mu.Lock()
	markBadLocked(proxyKey(p))
	proxyHealth.mu.Unlock()
}

func markBadLocked(key string) {
	if proxyHealth.bad == nil {
		proxyHealth.bad = map[string]time.Time{}
	}
	if _, was := proxyHealth.bad[key]; !was {
		logf("proxy %s isn't answering: the next one in the list is used for 30 minutes", key)
	}
	proxyHealth.bad[key] = time.Now()
}

// resetProxyHealth forgets failures, for when the proxy settings change.
func resetProxyHealth() {
	proxyHealth.mu.Lock()
	proxyHealth.bad, proxyHealth.recent, proxyHealth.byURL = nil, nil, nil
	proxyHealth.mu.Unlock()
}

// noteProxyFailure looks at a download error: if it's a failure to reach a proxy that
// came from a proxy list (system settings or PAC), that proxy is marked bad. It returns
// true when another entry of the list is still usable, so retrying right away goes a
// different way. Any error (or nil) is fine to pass.
func noteProxyFailure(err error) bool {
	addr, target, ok := proxyConnectFailure(err)
	if !ok {
		return false
	}
	key := failedProxyKey(addr, target)
	if key == "" {
		return false
	}
	proxyHealth.mu.Lock()
	defer proxyHealth.mu.Unlock()
	rest, ok := proxyHealth.recent[key]
	if !ok {
		return false
	}
	markBadLocked(key)
	for _, e := range rest {
		if e == "" {
			return true
		}
		if pu, err := parseProxyURL(e); err == nil && !proxyIsBad(proxyKey(pu)) {
			return true
		}
	}
	return false
}

// proxyConnectFailure reports whether err is a failure to reach a proxy, with what it
// says about which one (an address, or a name that didn't resolve) and the URL being
// fetched. Go's net/http reports a failed connection to an HTTP, HTTPS or SOCKS5 proxy as
// "proxyconnect tcp: …" (a *net.OpError with Op "proxyconnect"), and a failed SOCKS5
// handshake as "socks connect tcp proxy->target: …" (Op "socks connect").
func proxyConnectFailure(err error) (addr, target string, ok bool) {
	if err == nil || errors.Is(err, context.Canceled) {
		return "", "", false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		target = ue.URL
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		switch op.Op {
		case "proxyconnect":
			var dns *net.DNSError
			var dial *net.OpError
			if errors.As(op.Err, &dns) {
				addr = dns.Name
			} else if errors.As(op.Err, &dial) && dial.Addr != nil {
				addr = dial.Addr.String()
			}
			return addr, target, true
		case "socks connect":
			// a SOCKS reply code means the proxy works and the destination didn't
			if strings.HasPrefix(op.Err.Error(), "unknown error ") {
				return "", "", false
			}
			if op.Source != nil {
				addr = op.Source.String()
			}
			return addr, target, true
		}
	}
	// the typed error may have been flattened to text on the way here
	msg := err.Error()
	if target == "" {
		if _, q, ok := strings.Cut(msg, ` "`); ok {
			target, _, _ = strings.Cut(q, `"`)
		}
	}
	if _, rest, ok := strings.Cut(msg, "proxyconnect tcp: "); ok {
		if _, a, ok := strings.Cut(rest, "dial tcp "); ok {
			addr, _, _ = strings.Cut(a, ": ")
		} else if _, a, ok := strings.Cut(rest, "lookup "); ok {
			if i := strings.IndexAny(a, " :"); i >= 0 {
				a = a[:i]
			}
			addr = a
		}
		return addr, target, true
	}
	if _, rest, ok := strings.Cut(msg, "socks connect tcp "); ok && !strings.Contains(rest, ": unknown error ") {
		addr, _, _ = strings.Cut(rest, "->")
		return addr, target, true
	}
	return "", "", false
}

// failedProxyKey finds which recently used proxy an error is about.
func failedProxyKey(addr, target string) string {
	proxyHealth.mu.Lock()
	keys := make([]string, 0, len(proxyHealth.recent))
	for k := range proxyHealth.recent {
		keys = append(keys, k)
	}
	viaURL := ""
	if u, err := url.Parse(target); err == nil && target != "" {
		viaURL = proxyHealth.byURL[u.Scheme+"://"+u.Host]
	}
	proxyHealth.mu.Unlock()

	if addr == "" { // nothing names the proxy (say, a TLS failure with an https:// proxy)
		if viaURL != "" {
			return viaURL
		}
		if len(keys) == 1 {
			return keys[0]
		}
		return ""
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "" // a name that didn't resolve
	}
	var named []string // proxies given by name, on the failed port
	for _, k := range keys {
		kh, kp, _ := net.SplitHostPort(k)
		if port != "" && kp != port {
			continue
		}
		if strings.EqualFold(kh, host) {
			return k
		}
		if net.ParseIP(kh) == nil {
			named = append(named, k)
		}
	}
	ip := net.ParseIP(host)
	if ip == nil || len(named) == 0 {
		return ""
	}
	if len(named) == 1 {
		return named[0]
	}
	// several names on that port: see which one the failed address belongs to
	for _, k := range named {
		kh, _, _ := net.SplitHostPort(k)
		for _, a := range pacResolve(kh) {
			if a.Equal(ip) {
				return k
			}
		}
	}
	return ""
}
