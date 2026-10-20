// Package psl tells sites apart with the Public Suffix List.
package psl

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"net/netip"
	"strings"
	"sync"
	"unicode/utf8"
)

// Sites are told apart with the Public Suffix List, which names the endings anyone can register
// under: "co.uk" and "github.io" as much as "com". a.github.io and b.github.io belong to different
// people, so a sign-in for one must never go to the other.
//
// psl.dat is the list with comments and blank lines removed (the section markers stay, since the
// PRIVATE section is what splits github.io, blogspot.com and the like). To refresh it:
//
//	curl -sS https://publicsuffix.org/list/public_suffix_list.dat |
//		awk '/===BEGIN (ICANN|PRIVATE) DOMAINS===/ {print; next} !/^\/\// && NF {print $1}'
//
// keeping the two header lines (source, date and licence) at the top.
//
//go:embed psl.dat
var pslData string

// Rule kinds; each has an ICANN twin 3 bits up, set when the rule is in the ICANN section.
const (
	pslExact  = 1 << iota // "suffix"
	pslWild               // "*.suffix": every name directly under suffix is a public suffix
	pslExcept             // "!suffix": not a public suffix after all (overrides a wildcard)
)

var (
	pslOnce  sync.Once
	pslTable map[string]uint8 // punycoded suffix → rule kinds
)

// pslRules parses the list the first time it's needed.
func pslRules() map[string]uint8 {
	pslOnce.Do(func() {
		rules := make(map[string]uint8, 10500)
		icann := true
		for line := range strings.Lines(pslData) {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "//") {
				if strings.Contains(line, "===BEGIN PRIVATE DOMAINS===") {
					icann = false
				} else if strings.Contains(line, "===BEGIN ICANN DOMAINS===") {
					icann = true
				}
				continue
			}
			if i := strings.IndexAny(line, " \t"); i >= 0 {
				line = line[:i] // a rule ends at the first space
			}
			kind := uint8(pslExact)
			if r, ok := strings.CutPrefix(line, "!"); ok {
				kind, line = pslExcept, r
			} else if r, ok := strings.CutPrefix(line, "*."); ok {
				kind, line = pslWild, r
			}
			// The list spells IDN rules in Unicode; hosts may come either way, so both are
			// matched in punycode.
			key, ok := idnaASCII(line)
			if !ok {
				continue
			}
			if icann {
				kind |= kind << 3
			}
			rules[key] |= kind
		}
		pslTable = rules
	})
	return pslTable
}

// pslMatch returns how many trailing labels of d (lower-case ASCII, no empty labels) form its
// public suffix, per https://github.com/publicsuffix/list/wiki/Format: an exception rule wins,
// else the matching rule with the most labels, else the default rule "*" (the last label).
func pslMatch(d string) (n int, icann bool) {
	rules := pslRules()
	n = 1
	k := 0 // labels in d[i:]
	for i := len(d) + 1; i > 0; {
		i = strings.LastIndexByte(d[:i-1], '.') + 1
		k++
		f := rules[d[i:]]
		if f&pslExcept != 0 {
			// "!city.kobe.jp": the suffix is what's left without the leftmost label
			return max(k-1, 1), f&(pslExcept<<3) != 0
		}
		if f&pslExact != 0 {
			n, icann = k, f&(pslExact<<3) != 0
		}
		if f&pslWild != 0 && i > 0 {
			n, icann = k+1, f&(pslWild<<3) != 0
		}
	}
	return n, icann
}

// publicSuffix returns the public suffix of a domain name ("co.uk" for "www.bbc.co.uk",
// "github.io" for "me.github.io"), spelled as given but lower-case and without a trailing dot.
// icann is false for the PRIVATE section and for unlisted endings. It's "" for a name with empty
// labels (".com", "a..b"). Callers handle IP addresses themselves.
func publicSuffix(domain string) (suffix string, icann bool) {
	name := strings.ToLower(strings.TrimSuffix(domain, "."))
	ace, ok := idnaASCII(name)
	if !ok {
		return "", false
	}
	n, icann := pslMatch(ace)
	return lastLabels(name, n), icann
}

// pslRegistrable is the list's own "registrable domain": the public suffix plus one label, or ""
// when domain is a public suffix itself (single-label names included) or not a valid name.
func pslRegistrable(domain string) string {
	name := strings.ToLower(strings.TrimSuffix(domain, "."))
	ace, ok := idnaASCII(name)
	if !ok {
		return ""
	}
	n, _ := pslMatch(ace)
	return lastLabels(name, n+1)
}

// registrableDomain returns the site a host belongs to: "bbc.co.uk" for "www.bbc.co.uk",
// "me.github.io" for "x.me.github.io", or "" when the host is itself a public suffix. IP addresses
// (in canonical form) and single-label intranet names ("nas", "localhost") are their own site.
func registrableDomain(host string) string {
	_, reg := pslSite(host, false)
	return reg
}

// SameSite reports whether two hosts belong to the same site, so a sign-in for one may go to
// the other: equal hosts, or the same registrable domain. An IP address only matches itself.
func SameSite(a, b string) bool {
	ha, sa := pslSite(a, true)
	hb, sb := pslSite(b, true)
	return ha == hb || sa != "" && sa == sb
}

// pslSite tidies host (lower-case, no brackets or trailing dot, punycoded when ascii is set) and
// returns it with its registrable domain spelled the same way.
func pslSite(host string, ascii bool) (h, reg string) {
	h = strings.ToLower(strings.TrimSuffix(host, "."))
	if a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")); err == nil {
		h = a.Unmap().String()
		return h, h
	}
	ace, ok := idnaASCII(h)
	if !ok {
		return h, ""
	}
	if ascii {
		h = ace
	}
	if last := lastLabels(h, 1); last == h || strings.Trim(last, "0123456789") == "" {
		return h, h // an intranet name, or a numeric address (no top-level domain is all digits)
	}
	n, _ := pslMatch(ace)
	return h, lastLabels(h, n+1)
}

// lastLabels returns the last n dot-separated labels of s, or "" if it has fewer.
func lastLabels(s string, n int) string {
	i := len(s) + 1
	for ; n > 0; n-- {
		if i == 0 {
			return ""
		}
		i = strings.LastIndexByte(s[:i-1], '.') + 1
	}
	return s[i:]
}

// List hands the list (PRIVATE section included) to net/http/cookiejar, so a cookie set by one
// github.io site can't be scoped to all of github.io.
type List struct{}

func (List) PublicSuffix(domain string) string {
	s, _ := publicSuffix(domain)
	return s
}

func (List) String() string {
	sum := sha256.Sum256([]byte(pslData))
	return "psl " + hex.EncodeToString(sum[:6])
}

// idnaASCII lower-cases a dotted name and punycodes its non-ASCII labels, so the Unicode and
// "xn--" spellings of a name compare equal. ok is false for empty or unencodable labels.
func idnaASCII(name string) (string, bool) {
	name = strings.ToLower(name)
	if name == "" || name[0] == '.' || name[len(name)-1] == '.' || strings.Contains(name, "..") {
		return "", false
	}
	for i := 0; i < len(name); i++ {
		if name[i] >= utf8.RuneSelf {
			labels := strings.Split(name, ".")
			for j, l := range labels {
				if p, ok := punycode(l); !ok {
					return "", false
				} else if p != l {
					labels[j] = "xn--" + p
				}
			}
			return strings.Join(labels, "."), true
		}
	}
	return name, true
}

// punycode encodes one label per RFC 3492, leaving an all-ASCII label as it is. Labels are
// capped at 63 characters (the DNS limit), which also keeps the arithmetic from overflowing.
func punycode(label string) (string, bool) {
	const base, tmin, tmax, skew, damp = 36, 1, 26, 38, 700
	runes := []rune(label)
	if !utf8.ValidString(label) || len(runes) > 63 {
		return "", false
	}
	var out []byte
	for _, r := range runes {
		if r < utf8.RuneSelf {
			out = append(out, byte(r))
		}
	}
	b := len(out)
	if b == len(runes) {
		return label, true
	}
	if b > 0 {
		out = append(out, '-')
	}
	digit := func(d int) byte {
		if d < 26 {
			return byte('a' + d)
		}
		return byte('0' + d - 26)
	}
	n, delta, bias := utf8.RuneSelf, 0, 72
	for h := b; h < len(runes); {
		m := int(utf8.MaxRune) + 1
		for _, r := range runes {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		delta += (m - n) * (h + 1)
		n = m
		for _, r := range runes {
			if int(r) < n {
				delta++
			}
			if int(r) != n {
				continue
			}
			q := delta
			for k := base; ; k += base {
				t := min(max(k-bias, tmin), tmax)
				if q < t {
					break
				}
				out = append(out, digit(t+(q-t)%(base-t)))
				q = (q - t) / (base - t)
			}
			out = append(out, digit(q))
			// adapt the bias (RFC 3492 §6.1)
			if h == b {
				delta /= damp
			} else {
				delta /= 2
			}
			delta += delta / (h + 1)
			k := 0
			for delta > (base-tmin)*tmax/2 {
				delta /= base - tmin
				k += base
			}
			bias = k + (base-tmin+1)*delta/(delta+skew)
			delta = 0
			h++
		}
		delta++
		n++
	}
	return string(out), true
}
