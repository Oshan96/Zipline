package main

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Per-site rules (Options → Sites): how many connections a site gets, and a saved login
// that's sent automatically. The most specific matching rule wins.

type SiteRule struct {
	Site     string `json:"site"`               // example.com, *.example.com, example.com:8080 or example.com/private
	MaxConns int    `json:"maxConns,omitempty"` // 0 = the usual number
	User     string `json:"user,omitempty"`
	Pass     string `json:"pass,omitempty"`

	ID      string `json:"id,omitempty"`      // names the rule, so the window can keep its saved password (vault_settings.go)
	PassSet bool   `json:"passSet,omitempty"` // the settings API: a password is saved (Pass is then sent empty)
}

// Browser hotkeys: hold these while clicking a link. Values: alt, ctrlalt, ctrlshift, shiftalt, off.
type KeySettings struct {
	Zipline string `json:"zipline"` // send the link to Zipline
	Browser string `json:"browser"` // let the browser download it (when takeover is on)
}

var siteRules atomic.Pointer[[]SiteRule]

func (m *Manager) applySites() {
	rules := append([]SiteRule(nil), m.settings.Sites...)
	siteRules.Store(&rules)
}

// splitSite normalises a rule: lower-case host (without "*." or a scheme), optional port and path.
func splitSite(s string) (host, port, path string) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s, path = s[:i], strings.TrimRight(s[i:], "/")
	}
	if i := strings.LastIndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimPrefix(strings.ToLower(s), "*.")
	if h, p, ok := strings.Cut(s, ":"); ok && !strings.Contains(p, ":") {
		s, port = h, p
	}
	return strings.Trim(s, "."), port, path
}

func defaultPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "ftp":
		return "21"
	case "ftps":
		return "990"
	}
	return "80"
}

// siteMatch reports whether rule covers u and how specific it is (higher = more specific).
func siteMatch(rule string, u *url.URL) (int, bool) {
	host, port, path := splitSite(rule)
	if host == "" || u == nil {
		return 0, false
	}
	uh := strings.ToLower(u.Hostname())
	exact := uh == host
	if !exact && !strings.HasSuffix(uh, "."+host) {
		return 0, false
	}
	if port != "" && port != defaultPort(u) {
		return 0, false
	}
	if path != "" && u.Path != path && !strings.HasPrefix(u.Path, path+"/") {
		return 0, false
	}
	score := len(path)*1000 + len(host)*2
	if exact {
		score++
	}
	return score, true
}

func siteRuleFor(u *url.URL, want func(*SiteRule) bool) *SiteRule {
	p := siteRules.Load()
	if p == nil {
		return nil
	}
	var best *SiteRule
	bestScore := -1
	for i := range *p {
		r := &(*p)[i]
		if !want(r) {
			continue
		}
		if s, ok := siteMatch(r.Site, u); ok && s > bestScore {
			best, bestScore = r, s
		}
	}
	return best
}

// siteConns returns the connection limit for a link (0 = none).
func siteConns(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	if r := siteRuleFor(u, func(r *SiteRule) bool { return r.MaxConns > 0 }); r != nil {
		return r.MaxConns
	}
	return 0
}

// siteLogin returns the saved login for a link, if any.
func siteLogin(u *url.URL) (user, pass string, ok bool) {
	if r := siteRuleFor(u, func(r *SiteRule) bool { return r.User != "" }); r != nil {
		return r.User, usableSecret(r.Pass), true
	}
	return "", "", false
}

// connsFor caps a download's connections by the site rules for its link and where it ended up.
func connsFor(d *Download, conns int) int {
	for _, u := range []string{d.URL, d.FinalURL} {
		if l := siteConns(u); l > 0 && l < conns {
			conns = l
		}
	}
	return max(conns, 1)
}

// hostState is what all downloads share about each server (by host name): how many connections
// each download holds there, so a server's connection budget (its site rule, else the usual
// number per download) is shared fairly, and until when it asked to be left alone (Retry-After).
// Its lock is taken last, under a source set's.
type hostState struct {
	mu    sync.Mutex
	conns map[string]map[*Download]int
	quiet map[string]time.Time
	def   atomic.Int32 // connections per download (settings): the budget of a site without a rule
}

func newHostState() *hostState {
	h := &hostState{conns: map[string]map[*Download]int{}, quiet: map[string]time.Time{}}
	h.def.Store(16)
	return h
}

func (h *hostState) setDefault(n int) {
	if h != nil {
		h.def.Store(int32(max(n, 1)))
	}
}

func (h *hostState) perDownload() int {
	if h == nil {
		return 0
	}
	return int(h.def.Load())
}

// allowed: may d open one more connection to host? A download may always hold one; beyond that
// it keeps to an equal share of the budget among the downloads connected there, and the host's
// total to the budget (a newcomer grows as the others make way).
func (h *hostState) allowed(host string, d *Download, budget int) bool {
	if h == nil || d == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.allowedLocked(host, d, budget)
}

func (h *hostState) allowedLocked(host string, d *Download, budget int) bool {
	use := h.conns[host]
	have := use[d]
	if have == 0 {
		return true
	}
	total := 0
	for _, n := range use {
		total += n
	}
	return have < max(1, budget/len(use)) && total < budget
}

// take reserves a connection for d on host if its share allows.
func (h *hostState) take(host string, d *Download, budget int) bool {
	if h == nil || d == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.allowedLocked(host, d, budget) {
		return false
	}
	h.addLocked(host, d)
	return true
}

// add counts a connection d already has (a download's first request).
func (h *hostState) add(host string, d *Download) {
	if h == nil || d == nil {
		return
	}
	h.mu.Lock()
	h.addLocked(host, d)
	h.mu.Unlock()
}

func (h *hostState) addLocked(host string, d *Download) {
	if h.conns[host] == nil {
		h.conns[host] = map[*Download]int{}
	}
	h.conns[host][d]++
}

func (h *hostState) put(host string, d *Download) {
	if h == nil || d == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	use := h.conns[host]
	if use[d] <= 1 {
		delete(use, d)
		if len(use) == 0 {
			delete(h.conns, host)
		}
		return
	}
	use[d]--
}

// held is how many connections d holds to host.
func (h *hostState) held(host string, d *Download) int {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[host][d]
}

// retryAfter: the server asked for no new connections for a while (at most an hour).
func (h *hostState) retryAfter(host string, wait time.Duration) {
	if h == nil || wait <= 0 {
		return
	}
	t := time.Now().Add(min(wait, maxRetryAfter))
	h.mu.Lock()
	if t.After(h.quiet[host]) {
		h.quiet[host] = t
	}
	h.mu.Unlock()
}

// until is when host may be asked again (zero: now).
func (h *hostState) until(host string) time.Time {
	if h == nil {
		return time.Time{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.quiet[host]
	if !t.IsZero() && time.Now().After(t) {
		delete(h.quiet, host)
		return time.Time{}
	}
	return t
}

// wait sleeps until host may be asked again. False when ctx ended first.
func (h *hostState) wait(ctx context.Context, host string) bool {
	if t := h.until(host); !t.IsZero() {
		return sleepCtx(ctx, time.Until(t))
	}
	return ctx.Err() == nil
}

// siteListed reports whether a host matches one of the plain site patterns (takeover exclusions).
func siteListed(list []string, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	for _, s := range list {
		if _, ok := siteMatch(s, u); ok {
			return true
		}
	}
	return false
}
