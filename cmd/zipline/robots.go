package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// robots.txt the way RFC 9309 reads it, for the site grabber:
//   - our group is the one that names "Zipline" (several matching groups are joined), else "*";
//   - Allow / Disallow paths may use * (anything) and a closing $ (the end), and the longest
//     matching path wins, Allow winning a tie;
//   - paths are compared percent-encoded the same way on both sides;
//   - the file is read once per site and kept for 24 hours; a missing file (any 4xx but 429)
//     means everything is allowed, a server that can't answer (5xx, no connection) means nothing
//     is until it can, and up to 5 redirects are followed.

const (
	robotsToken   = "Zipline"
	robotsTTL     = 24 * time.Hour
	robotsMaxBody = 512 << 10 // (the RFC asks crawlers to read at least 500 KiB)
	robotsHops    = 5
)

// robotsDownTTL is how long "the server couldn't answer" is remembered, so a dozen workers
// don't all ask again at once; the next try after that asks afresh.
var robotsDownTTL = time.Second

type robotsRule struct {
	allow bool
	pat   string // percent-encoded the same way as the path it is matched against
}

// robotsRules is what one crawler is told by one robots.txt.
type robotsRules struct {
	rules    []robotsRule
	delay    time.Duration // Crawl-delay
	sitemaps []string
}

// parseRobots reads a robots.txt for the crawler called token.
func parseRobots(body, token string) *robotsRules {
	type group struct {
		agents []string
		rules  []robotsRule
		delay  time.Duration
	}
	var groups []*group
	var cur *group
	gathering := false // the last line was a User-agent line: the next one joins the same group
	out := &robotsRules{}
	body = strings.TrimPrefix(body, "\ufeff")
	for _, line := range strings.FieldsFunc(body, func(r rune) bool { return r == '\n' || r == '\r' }) {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if !gathering || cur == nil {
				cur = &group{}
				groups = append(groups, cur)
			}
			gathering = true
			cur.agents = append(cur.agents, v)
		case "allow", "disallow":
			gathering = false
			if cur != nil && v != "" { // (an empty Disallow allows everything: no rule at all)
				cur.rules = append(cur.rules, robotsRule{allow: k == "allow", pat: robotsEncode(v)})
			}
		case "crawl-delay":
			gathering = false
			if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && cur != nil {
				cur.delay = time.Duration(min(f, 3600) * float64(time.Second))
			}
		case "sitemap":
			if v != "" {
				out.sitemaps = append(out.sitemaps, v)
			}
		}
	}
	// the groups that name us, else the ones for everybody; joined if there are several
	pick := func(match func(string) bool) (found bool) {
		for _, g := range groups {
			for _, a := range g.agents {
				if match(a) {
					out.rules = append(out.rules, g.rules...)
					out.delay = max(out.delay, g.delay)
					found = true
					break
				}
			}
		}
		return found
	}
	if !pick(func(a string) bool { return strings.EqualFold(robotsProduct(a), token) }) {
		pick(func(a string) bool { return a == "*" })
	}
	return out
}

// robotsProduct is the product token at the start of a User-agent value ("Zipline/1.0" → "Zipline").
func robotsProduct(a string) string {
	i := 0
	for i < len(a) && (a[i] >= 'a' && a[i] <= 'z' || a[i] >= 'A' && a[i] <= 'Z' || a[i] == '-' || a[i] == '_') {
		i++
	}
	return a[:i]
}

// robotsEncode writes s the one way both a rule and an address are compared: bytes outside
// plain ASCII percent-encoded in capitals, and an encoded unreserved character (%62 for "b")
// written as itself. Reserved characters stay as they are, and so do * and $ in a pattern.
func robotsEncode(s string) string {
	const hex = "0123456789ABCDEF"
	isHex := func(c byte) bool { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }
	unreserved := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~'
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			n, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if unreserved(byte(n)) {
				b.WriteByte(byte(n))
			} else {
				b.WriteByte('%')
				b.WriteByte(hex[n>>4])
				b.WriteByte(hex[n&15])
			}
			i += 2
		case c <= ' ' || c >= 0x7f:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// robotsMatch: does the path match the rule's pattern from its start?
func robotsMatch(pat, s string) bool {
	anchored := strings.HasSuffix(pat, "$")
	if anchored {
		pat = pat[:len(pat)-1]
	}
	parts := strings.Split(pat, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	if len(parts) == 1 {
		return !anchored || s == ""
	}
	for i := 1; i < len(parts); i++ {
		p := parts[i]
		if i == len(parts)-1 && anchored {
			return strings.HasSuffix(s, p)
		}
		j := strings.Index(s, p)
		if j < 0 {
			return false
		}
		s = s[j+len(p):]
	}
	return true
}

// allowed: may the crawler fetch this address? The longest matching rule decides; Allow wins a tie.
func (r *robotsRules) allowed(u *url.URL) bool {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	p = robotsEncode(p)
	best, allow := -1, true
	for _, rule := range r.rules {
		if n := len(rule.pat); (n > best || n == best && rule.allow) && robotsMatch(rule.pat, p) {
			best, allow = n, rule.allow
		}
	}
	return allow
}

// robotsEntry is what is known about one site's robots.txt.
type robotsEntry struct {
	at     time.Time
	rules  *robotsRules // nil: no file, everything allowed
	down   bool         // the server couldn't answer: nothing is allowed for now
	why    string       // …and why, for the failure list
	status int
	body   string
}

func (e *robotsEntry) allowed(u *url.URL) bool {
	if e.down {
		return false
	}
	return e.rules == nil || e.rules.allowed(u)
}

func (e *robotsEntry) fresh() bool {
	if e.down {
		return time.Since(e.at) < robotsDownTTL
	}
	return time.Since(e.at) < robotsTTL
}

// robotsStore keeps the robots.txt of every site Zipline has crawled lately, for all scans.
type robotsStore struct {
	mu     sync.Mutex
	m      map[string]*robotsEntry
	flight map[string]chan struct{}
}

func newRobotsStore() *robotsStore {
	return &robotsStore{m: map[string]*robotsEntry{}, flight: map[string]chan struct{}{}}
}

func (s *robotsStore) get(origin string) *robotsEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.m[origin]; e != nil && e.fresh() {
		return e
	}
	return nil
}

func (s *robotsStore) put(origin string, e *robotsEntry) {
	s.mu.Lock()
	s.m[origin] = e
	s.mu.Unlock()
}

// load runs fetch for origin unless a fresh answer is kept or another worker is already asking.
func (s *robotsStore) load(ctx context.Context, origin string, fetch func() *robotsEntry) *robotsEntry {
	for {
		s.mu.Lock()
		if e := s.m[origin]; e != nil && e.fresh() {
			s.mu.Unlock()
			return e
		}
		ch, busy := s.flight[origin]
		if !busy {
			s.flight[origin] = make(chan struct{})
		}
		s.mu.Unlock()
		if !busy {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil
		}
	}
	e := fetch()
	s.mu.Lock()
	if e != nil {
		s.m[origin] = e
	}
	close(s.flight[origin])
	delete(s.flight, origin)
	s.mu.Unlock()
	return e
}

var errRobotsHops = errors.New("too many redirects")

// readRobots asks a site for its robots.txt (one request, already paced by the caller) and
// reads the answer the way RFC 9309 says. nil when the scan was stopped meanwhile.
func readRobots(ctx context.Context, d *Download, origin string, pace *hostPace) *robotsEntry {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := buildRequest(cctx, d, origin+"/robots.txt", "")
	if err != nil {
		return &robotsEntry{at: time.Now()}
	}
	c := d.client()
	c.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		if len(via) > robotsHops {
			return errRobotsHops
		}
		return nil
	}
	resp, err := c.Do(req)
	e := &robotsEntry{at: time.Now()}
	switch {
	case err != nil && ctx.Err() != nil:
		return nil
	case errors.Is(err, errRobotsHops): // a loop of redirects: as good as no file
		return e
	case err != nil:
		e.down, e.why = true, friendlyError(err, origin)
		return e
	}
	defer resp.Body.Close()
	e.status = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, robotsMaxBody))
		e.body = string(b)
		e.rules = parseRobots(e.body, robotsToken)
	case resp.StatusCode == http.StatusTooManyRequests:
		e.down, e.why = true, "the site asked to slow down"
		pace.slowDown(hostKeyOf(origin), resp.Header)
	case resp.StatusCode >= 500:
		e.down, e.why = true, "the server replied "+resp.Status
	}
	return e
}

func hostKeyOf(origin string) string {
	if u, err := url.Parse(origin); err == nil {
		return hostKey(u)
	}
	return origin
}
