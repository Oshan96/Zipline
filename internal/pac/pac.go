// Package pac runs proxy auto-config (PAC) scripts with a small JavaScript interpreter of its own.
package pac

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"slices"
	"strings"
	"sync"
	"time"
)

// Proxy auto-config (PAC): a JavaScript function FindProxyForURL(url, host) that answers
// "PROXY a:8080; SOCKS5 b:1080; DIRECT". Windows runs these itself (proxy_windows.go);
// on macOS and Linux Zipline runs them with the small interpreter in this package.

// Resolver looks names up for PAC scripts; tests swap in a fake.
var Resolver = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// LocalIPs lists this machine's addresses, the one the default route uses first;
// tests swap in a fake.
var LocalIPs = func() []net.IP {
	var ips []net.IP
	add := func(ip net.IP) {
		if ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() &&
			!slices.ContainsFunc(ips, ip.Equal) {
			ips = append(ips, ip)
		}
	}
	// "connecting" a UDP socket sends nothing but picks the outgoing address
	for _, addr := range []string{"198.51.100.1:53", "[2001:db8::1]:53"} {
		if c, err := net.Dial("udp", addr); err == nil {
			if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
				add(ua.IP)
			}
			c.Close()
		}
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				add(n.IP)
			}
		}
	}
	return ips
}

// pacNow is the clock for weekdayRange, dateRange and timeRange.
var pacNow = time.Now

var pacDNS struct {
	mu sync.Mutex
	m  map[string]pacDNSEntry
}

type pacDNSEntry struct {
	ips []net.IP
	at  time.Time
}

// ForgetLookups empties the cache of looked-up names, for tests that swap the resolver.
func ForgetLookups() {
	pacDNS.mu.Lock()
	pacDNS.m = nil
	pacDNS.mu.Unlock()
}

// Resolve returns a host's addresses (none if it doesn't resolve). Lookups give up
// after 2 seconds and are cached: scripts tend to resolve the same names for every URL.
func Resolve(host string) []net.IP {
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return []net.IP{ip}
	}
	host = strings.ToLower(host)
	if host == "" {
		return nil
	}
	pacDNS.mu.Lock()
	e, ok := pacDNS.m[host]
	pacDNS.mu.Unlock()
	ttl := 5 * time.Minute
	if len(e.ips) == 0 {
		ttl = time.Minute // a name that didn't resolve may soon
	}
	if ok && time.Since(e.at) < ttl {
		return e.ips
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	ips, _ := Resolver(ctx, host)
	cancel()
	pacDNS.mu.Lock()
	if pacDNS.m == nil || len(pacDNS.m) >= 256 {
		pacDNS.m = map[string]pacDNSEntry{}
	}
	pacDNS.m[host] = pacDNSEntry{ips, time.Now()}
	pacDNS.mu.Unlock()
	return ips
}

func pacIPv4(ips []net.IP) net.IP {
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			return v4
		}
	}
	return nil
}

func pacJoinIPs(ips []net.IP) string {
	s := make([]string, len(ips))
	for i, ip := range ips {
		s[i] = ip.String()
	}
	return strings.Join(s, ";")
}

func (vm *pacVM) myIPs() []string {
	if !vm.gotIPs {
		for _, ip := range LocalIPs() {
			vm.localIPs = append(vm.localIPs, ip.String())
		}
		vm.gotIPs = true
	}
	return vm.localIPs
}

// shExpMatch matches a shell expression: * is any run of characters, ? any one, and
// everything else itself.
func shExpMatch(s, pat string) bool {
	si, pi, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(pat) && pat[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(pat) && (pat[pi] == '?' || pat[pi] == s[si]):
			si++
			pi++
		case star >= 0: // let the last * swallow one more character
			mark++
			si, pi = mark, star+1
		default:
			return false
		}
	}
	for pi < len(pat) && pat[pi] == '*' {
		pi++
	}
	return pi == len(pat)
}

var (
	pacDays   = []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}
	pacMonths = []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}
)

func pacName(names []string, v any) int {
	return slices.Index(names, strings.ToUpper(jsToString(v)))
}

// pacInRange checks a <= v <= b, where a range like FRI..MON wraps around.
func pacInRange(v, a, b int) bool {
	if a <= b {
		return a <= v && v <= b
	}
	return v >= a || v <= b
}

// pacTime splits off a trailing "GMT" argument, which switches the clock to UTC.
func (vm *pacVM) pacTime(a []any) ([]any, time.Time) {
	if n := len(a); n > 0 && jsToString(a[n-1]) == "GMT" {
		return a[:n-1], vm.now.UTC()
	}
	return a, vm.now
}

// weekdayRange("MON", "FRI"[, "GMT"])
func pacWeekdayRange(vm *pacVM, a []any) any {
	a, now := vm.pacTime(a)
	if len(a) == 0 || len(a) > 2 {
		return false
	}
	d1 := pacName(pacDays, a[0])
	d2 := d1
	if len(a) == 2 {
		d2 = pacName(pacDays, a[1])
	}
	return d1 >= 0 && d2 >= 0 && pacInRange(int(now.Weekday()), d1, d2)
}

// dateRange(day | month | year, …) with one value, a range of one kind, or ranges of
// day+month, month+year or day+month+year; numbers up to 31 are days, larger ones years.
func pacDateRange(vm *pacVM, a []any) any {
	a, now := vm.pacTime(a)
	type part struct {
		kind byte // d, m or y
		v    int
	}
	parts := make([]part, len(a))
	for i, x := range a {
		if m := pacName(pacMonths, x); m >= 0 {
			parts[i] = part{'m', m}
			continue
		}
		switch f := jsToNumber(x); {
		case math.IsNaN(f) || f < 1:
			return false
		case f <= 31:
			parts[i] = part{'d', int(f)}
		default:
			parts[i] = part{'y', int(f)}
		}
	}
	field := func(k byte) int {
		switch k {
		case 'd':
			return now.Day()
		case 'm':
			return int(now.Month()) - 1
		}
		return now.Year()
	}
	weight := map[byte]int{'y': 10000, 'm': 100, 'd': 1}
	switch len(parts) {
	case 1:
		return field(parts[0].kind) == parts[0].v
	case 2, 4, 6:
		h := len(parts) / 2
		lo, hi, cur, year := 0, 0, 0, false
		for i, p := range parts[:h] {
			if parts[h+i].kind != p.kind {
				return false
			}
			w := weight[p.kind]
			lo, hi, cur = lo+p.v*w, hi+parts[h+i].v*w, cur+field(p.kind)*w
			year = year || p.kind == 'y'
		}
		if year && lo > hi {
			return false
		}
		return pacInRange(cur, lo, hi)
	}
	return false
}

// timeRange(hour) or a range of hours, hour+minute or hour+minute+second.
func pacTimeRange(vm *pacVM, a []any) any {
	a, now := vm.pacTime(a)
	n := make([]int, len(a))
	for i, x := range a {
		f := jsToNumber(x)
		if math.IsNaN(f) {
			return false
		}
		n[i] = int(f)
	}
	var lo, hi int
	switch len(n) {
	case 1:
		return now.Hour() == n[0]
	case 2: // whole hours, both included: timeRange(9, 17) is 9:00 to 17:59
		lo, hi = n[0]*3600, n[1]*3600+3599
	case 4:
		lo, hi = n[0]*3600+n[1]*60, n[2]*3600+n[3]*60+59
	case 6:
		lo, hi = n[0]*3600+n[1]*60+n[2], n[3]*3600+n[4]*60+n[5]
	default:
		return false
	}
	return pacInRange(now.Hour()*3600+now.Minute()*60+now.Second(), lo, hi)
}

// pacFuncs are the PAC functions browsers provide, including Microsoft's IPv6 "Ex" set.
func pacFuncs() map[string]any {
	str := func(a []any, i int) string { return jsToString(jsArg(a, i)) }
	host := func(a []any) string { return strings.ToLower(str(a, 0)) }
	return map[string]any{
		"isPlainHostName": jsFn("isPlainHostName", func(_ *pacVM, a []any) any { return !strings.Contains(str(a, 0), ".") }),
		"dnsDomainIs": jsFn("dnsDomainIs", func(_ *pacVM, a []any) any {
			return strings.HasSuffix(host(a), strings.ToLower(str(a, 1)))
		}),
		"localHostOrDomainIs": jsFn("localHostOrDomainIs", func(_ *pacVM, a []any) any {
			h, full := host(a), strings.ToLower(str(a, 1))
			return h == full || !strings.Contains(h, ".") && strings.HasPrefix(full, h+".")
		}),
		"dnsDomainLevels": jsFn("dnsDomainLevels", func(_ *pacVM, a []any) any { return float64(strings.Count(str(a, 0), ".")) }),
		"shExpMatch":      jsFn("shExpMatch", func(_ *pacVM, a []any) any { return shExpMatch(str(a, 0), str(a, 1)) }),
		"isResolvable":    jsFn("isResolvable", func(_ *pacVM, a []any) any { return pacIPv4(Resolve(str(a, 0))) != nil }),
		"isResolvableEx":  jsFn("isResolvableEx", func(_ *pacVM, a []any) any { return len(Resolve(str(a, 0))) > 0 }),
		"dnsResolve": jsFn("dnsResolve", func(_ *pacVM, a []any) any {
			if ip := pacIPv4(Resolve(str(a, 0))); ip != nil {
				return ip.String()
			}
			return nil
		}),
		"dnsResolveEx": jsFn("dnsResolveEx", func(_ *pacVM, a []any) any { return pacJoinIPs(Resolve(str(a, 0))) }),
		"isInNet": jsFn("isInNet", func(_ *pacVM, a []any) any {
			ip, pat := pacIPv4(Resolve(str(a, 0))), net.ParseIP(str(a, 1)).To4()
			mask := net.ParseIP(str(a, 2)).To4()
			if ip == nil || pat == nil || mask == nil {
				return false
			}
			return ip.Mask(net.IPMask(mask)).Equal(pat.Mask(net.IPMask(mask)))
		}),
		"isInNetEx": jsFn("isInNetEx", func(_ *pacVM, a []any) any {
			_, n, err := net.ParseCIDR(str(a, 1))
			return err == nil && slices.ContainsFunc(Resolve(str(a, 0)), n.Contains)
		}),
		"convert_addr": jsFn("convert_addr", func(_ *pacVM, a []any) any {
			if ip := net.ParseIP(str(a, 0)).To4(); ip != nil {
				return float64(int32(binary.BigEndian.Uint32(ip))) // as browsers do: (a << 24) | … is signed
			}
			return 0.0
		}),
		"myIpAddress": jsFn("myIpAddress", func(vm *pacVM, _ []any) any {
			for _, ip := range vm.myIPs() {
				if !strings.Contains(ip, ":") {
					return ip
				}
			}
			return "127.0.0.1"
		}),
		"myIpAddressEx": jsFn("myIpAddressEx", func(vm *pacVM, _ []any) any { return strings.Join(vm.myIPs(), ";") }),
		"sortIpAddressList": jsFn("sortIpAddressList", func(_ *pacVM, a []any) any {
			var ips []net.IP
			for _, s := range strings.Split(str(a, 0), ";") {
				ip := net.ParseIP(strings.TrimSpace(s))
				if ip == nil {
					return false
				}
				ips = append(ips, ip)
			}
			slices.SortStableFunc(ips, func(x, y net.IP) int { // IPv6 first, then by address
				if x4, y4 := x.To4() != nil, y.To4() != nil; x4 != y4 {
					return jsBoolInt(x4) - jsBoolInt(y4)
				}
				return bytes.Compare(x.To16(), y.To16())
			})
			return pacJoinIPs(ips)
		}),
		"getClientVersion": jsFn("getClientVersion", func(*pacVM, []any) any { return "1.0" }),
		"weekdayRange":     jsFn("weekdayRange", pacWeekdayRange),
		"dateRange":        jsFn("dateRange", pacDateRange),
		"timeRange":        jsFn("timeRange", pacTimeRange),
		"alert":            jsFn("alert", func(*pacVM, []any) any { return jsUndefined }),
	}
}

func init() {
	vars := jsGlobals()
	for k, v := range pacFuncs() {
		vars[k] = v
	}
	pacBuiltins = &jsScope{vars: vars}
}

var pacProgs struct {
	mu sync.Mutex
	m  map[string]pacProg // by script text
}

type pacProg struct {
	f   *jsFuncNode
	err error
}

func compilePAC(script string) (*jsFuncNode, error) {
	pacProgs.mu.Lock()
	p, ok := pacProgs.m[script]
	pacProgs.mu.Unlock()
	if ok {
		return p.f, p.err
	}
	f, err := parseJS(script)
	pacProgs.mu.Lock()
	if pacProgs.m == nil || len(pacProgs.m) >= 8 {
		pacProgs.m = map[string]pacProg{}
	}
	pacProgs.m[script] = pacProg{f, err}
	pacProgs.mu.Unlock()
	return f, err
}

// Eval runs a PAC script's FindProxyForURLEx (when it has one) or FindProxyForURL for
// a URL and its host name, returning the answer, e.g. "PROXY a:8080; SOCKS5 b:1080; DIRECT".
func Eval(script, rawURL, host string) (res string, err error) {
	prog, err := compilePAC(script)
	if err != nil {
		return "", fmt.Errorf("PAC script: %w", err)
	}
	vm := &pacVM{global: &jsScope{parent: pacBuiltins}, now: pacNow(), deadline: time.Now().Add(pacMaxTime)}
	defer func() {
		if r := recover(); r != nil {
			switch r := r.(type) {
			case pacAbort:
				err = r.err
			case *jsThrow:
				err = fmt.Errorf("PAC script error: %s", jsToString(r.v))
			default: // a bug in the interpreter: fail this lookup rather than the app
				err = fmt.Errorf("PAC script failed: %v", r)
			}
			res = ""
		}
	}()
	for _, name := range prog.vars {
		vm.global.declare(name, jsUndefined)
	}
	vm.execBlock(prog.body, vm.global)
	fn, _ := vm.lookup("FindProxyForURLEx", vm.global)
	if jsTypeof(fn) != "function" {
		fn, _ = vm.lookup("FindProxyForURL", vm.global)
		if jsTypeof(fn) != "function" {
			return "", errors.New("PAC script has no FindProxyForURL function")
		}
	}
	v := vm.call(fn, []any{rawURL, host}, "FindProxyForURL")
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("PAC script returned %s, not a string", jsToString(v))
	}
	return s, nil
}
