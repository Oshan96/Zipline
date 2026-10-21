package main

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Mirror order. A metalink ranks its mirrors (priority 1 is best; the older format's preference
// 100 is best) and says which country each server is in, and so does a server listing mirrors in
// Link headers (pri=, geo=). Like aria2 with --metalink-location, mirrors in the user's own
// country come first, each group in the metalink's order: a nearby server is usually the faster
// one, and only the first ten mirrors are used. The country is a modest guess from the system:
// see systemCountry.

// orderMirrors sorts a metalink's mirrors, best first.
func orderMirrors(us []mlURL) {
	sort.SliceStable(us, func(i, j int) bool { return mirrorBefore(us[i].Pri, us[i].Loc, us[j].Pri, us[j].Loc) })
}

// mirrorBefore: a mirror (priority pa, country la) goes before another (pb, lb).
func mirrorBefore(pa int, la string, pb int, lb string) bool {
	if c := userCountry(); c != "" {
		if ha, hb := countryCode(la) == c, countryCode(lb) == c; ha != hb {
			return ha
		}
	}
	return pa < pb
}

// countryCode normalises a two-letter country code (lower case; "uk", as metalinks often write
// it, is ISO's "gb"). "" when it isn't one.
func countryCode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 2 || s[0] < 'a' || s[0] > 'z' || s[1] < 'a' || s[1] > 'z' {
		return ""
	}
	if s == "uk" {
		return "gb"
	}
	return s
}

var (
	ownCountry      = sync.OnceValue(func() string { return countryCode(systemCountry()) })
	countryOverride atomic.Pointer[string] // tests
)

// userCountry is the user's country (lower case, "" = unknown), worked out once.
func userCountry() string {
	if c := countryOverride.Load(); c != nil {
		return *c
	}
	return ownCountry()
}

// localeCountry reads the territory of a locale name: "en_US.UTF-8", "de-AT", "sr_RS@latin".
func localeCountry(l string) string {
	l, _, _ = strings.Cut(l, ".")
	l, _, _ = strings.Cut(l, "@")
	parts := strings.FieldsFunc(l, func(r rune) bool { return r == '_' || r == '-' })
	for _, p := range parts[min(1, len(parts)):] { // (after the language; a script like "Latn" is longer)
		if c := countryCode(p); c != "" {
			return c
		}
	}
	return ""
}

// zoneCountry finds a time zone's country ("Asia/Colombo" → "lk") in the system's zone.tab,
// which Linux and macOS both have.
func zoneCountry(zone string) string {
	zone = strings.TrimPrefix(strings.TrimSpace(zone), ":")
	if i := strings.Index(zone, "zoneinfo/"); i >= 0 { // a path: /usr/share/zoneinfo/Asia/Colombo
		zone = zone[i+len("zoneinfo/"):]
	}
	if zone == "" || filepath.IsAbs(zone) {
		return ""
	}
	f, err := os.Open("/usr/share/zoneinfo/zone.tab")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// "LK	+0656+07951	Asia/Colombo"
		if fs := strings.Split(sc.Text(), "\t"); len(fs) >= 3 && fs[2] == zone {
			return countryCode(fs[0])
		}
	}
	return ""
}
