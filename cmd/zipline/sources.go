package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zipline/internal/psl"
)

// A download can have several addresses for the same file: the main link plus mirrors
// (typed in, read from a metalink, or announced by the server with "Link: rel=duplicate").
// Each new connection goes to the address that's least busy for its speed. An address that
// fails rests for a while; one that serves a different file is dropped. A signed link that
// runs out part-way is renewed through the address it came from, and when a server turns
// away extra connections Zipline settles on the number it accepts.

// statusErr is an HTTP reply that isn't the file.
type statusErr struct {
	code   int
	status string
	wait   time.Duration // the server asked to be left alone this long (Retry-After), 0 = didn't say
}

func (e statusErr) Error() string { return "server replied " + e.status }

// expiredStatus: replies a signed or session link gives once it has run out.
func expiredStatus(code int) bool { return code == 403 || code == 404 || code == 410 }

var (
	errLinkExpired = errors.New("the download link has expired")
	errNoSlot      = errors.New("the server accepts no more connections")
)

// linkExpiry reads the expiry time that signed links carry (Amazon S3 and CloudFront, Google
// Cloud Storage, Azure, and the common expires=/exp= parameters). Zero when there's none.
func linkExpiry(raw string) time.Time {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return time.Time{}
	}
	q := u.Query()
	get := func(k string) string {
		for kk, v := range q {
			if strings.EqualFold(kk, k) && len(v) > 0 {
				return v[0]
			}
		}
		return ""
	}
	for _, p := range []string{"X-Amz", "X-Goog"} { // V4 signatures: date + seconds
		if ds, es := get(p+"-Date"), get(p+"-Expires"); ds != "" && es != "" {
			if t, err := time.Parse("20060102T150405Z", ds); err == nil {
				if n, err := strconv.Atoi(es); err == nil && n > 0 {
					return t.Add(time.Duration(n) * time.Second)
				}
			}
		}
	}
	if se := get("se"); se != "" && get("sig") != "" { // Azure shared access signature
		for _, f := range []string{time.RFC3339, "2006-01-02T15:04Z", "2006-01-02"} {
			if t, err := time.Parse(f, se); err == nil {
				return t
			}
		}
	}
	now := time.Now()
	for _, k := range []string{"Expires", "expire", "exp", "e", "x-expires", "validto"} { // Unix seconds
		v := get(k)
		if v == "" {
			continue
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			t := time.Unix(n, 0)
			if t.Year() >= 2015 && t.Before(now.AddDate(10, 0, 0)) {
				return t
			}
		}
	}
	return time.Time{}
}

type source struct {
	url        string // as given
	final      string // where it led; a signed link is renewed through url
	pri        int    // lower is preferred (metalink priority)
	mirror     bool
	active     int
	siteMax    int       // the site rule's connection limit (Options → Sites), 0 = none: a hard cap
	learned    int       // the connections the server accepted when it turned more away, 0 = no limit seen
	learnedAt  time.Time // when learned last changed (it creeps back up: the server may only have been busy)
	rate       float64   // bytes/s per connection (moving average)
	bytes      int64     // atomic: received from it
	fails      int
	strikes    int // "expired"-looking replies with no other connection open
	rest       time.Time
	dead       string // why it's no longer used
	distrusted bool   // sent damaged data earlier: only used when nothing else works
	bad        int    // damaged pieces it sent this run (piececheck.go)
	deadErr    error
	expired    bool
}

// addr is where to send requests. Caller holds the set's lock (final changes on renewal).
func (s *source) addr() string {
	if s.final != "" {
		return s.final
	}
	return s.url
}

// limit is how many connections the address takes now (0 = no limit). A limit learned from a
// server that turned connections away goes up by one a while after it last changed, but never
// past the site rule. Caller holds the set's lock.
func (s *source) limit(now time.Time) int {
	if s.learned > 0 && now.Sub(s.learnedAt) >= learnedCreep && (s.siteMax == 0 || s.learned < s.siteMax) {
		s.learned++
		s.learnedAt = now
	}
	switch {
	case s.learned == 0:
		return s.siteMax
	case s.siteMax == 0:
		return s.learned
	}
	return min(s.learned, s.siteMax)
}

type sourceSet struct {
	mu        sync.Mutex
	list      []*source
	writes    []span          // which address sent which bytes (to find who sent a damaged piece)
	distrust  map[string]bool // sent damaged pieces before: used only when nothing else is left
	hosts     *hostState      // shared with the other downloads (nil: no sharing)
	d         *Download
	validator string // If-Range for requests to the main address ("" = none)
}

// connHost is the server a request to raw connects to (lower-case host name, no port).
func connHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.ToLower(u.Hostname())
	}
	return raw
}

// budget is how many connections all downloads together may hold to the server of s: its site
// rule's limit, else the usual number per download. Caller holds the set's lock.
func (ss *sourceSet) budget(s *source) int {
	if l := siteConns(s.addr()); l > 0 {
		return l
	}
	return ss.hosts.perDownload()
}

// canTake reports whether some address would take one more connection now: not resting or
// asked to wait, under its server's limit, and within the download's fair share of the server.
func (ss *sourceSet) canTake(now time.Time) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	trusted := ss.hasTrusted()
	for _, s := range ss.list {
		if s.dead != "" || s.distrusted && trusted || now.Before(s.rest) {
			continue
		}
		if l := s.limit(now); l > 0 && s.active >= l {
			continue
		}
		h := connHost(s.addr())
		if now.Before(ss.hosts.until(h)) || !ss.hosts.allowed(h, ss.d, ss.budget(s)) {
			continue
		}
		return true
	}
	return false
}

// overShare: the download already holds its fair share of every server it could use, so a
// connection that finished its piece makes way for the other downloads there.
func (ss *sourceSet) overShare() bool {
	if ss.hosts == nil {
		return false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	usable := false
	for _, s := range ss.list {
		if s.dead != "" {
			continue
		}
		usable = true
		if ss.hosts.allowed(connHost(s.addr()), ss.d, ss.budget(s)) {
			return false
		}
	}
	return usable
}

type span struct {
	s    *source
	a, b int64 // [a, b)
}

func (ss *sourceSet) wrote(s *source, a, b int64) {
	if b <= a {
		return
	}
	ss.mu.Lock()
	if n := len(ss.writes); n > 0 && ss.writes[n-1].s == s && ss.writes[n-1].b == a {
		ss.writes[n-1].b = b
	} else if len(ss.writes) < 100000 {
		ss.writes = append(ss.writes, span{s, a, b})
	}
	ss.mu.Unlock()
}

// sentRange lists the addresses that sent any of [a, b).
func (ss *sourceSet) sentRange(a, b int64) []string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var out []string
	for _, w := range ss.writes {
		if w.a < b && a < w.b && !slices.Contains(out, w.s.url) {
			out = append(out, w.s.url)
		}
	}
	return out
}

func (ss *sourceSet) add(s *source) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, x := range ss.list {
		if x.url == s.url {
			return
		}
	}
	if l := siteConns(s.url); l > 0 {
		s.siteMax = l
	}
	if ss.distrust[s.url] && s.dead == "" {
		s.pri += 1000 // last resort
		s.distrusted = true
	}
	ss.list = append(ss.list, s)
}

func (ss *sourceSet) primary() *source {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if len(ss.list) == 0 {
		return nil
	}
	return ss.list[0]
}

// pick reserves a connection on the best usable address (and on its server, host). When none is
// free right now it says how long to wait (-1: every address is gone), and whether the only ones
// left are full (as many connections as their servers accept, or as the download's fair share).
func (ss *sourceSet) pick(now time.Time, skip map[*source]bool) (best *source, host string, wait time.Duration, full bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	known := 0.0
	for _, s := range ss.list {
		if s.rate > known {
			known = s.rate
		}
	}
	trusted := ss.hasTrusted()
	taken := map[*source]bool{} // another download got the last slot on its server meanwhile
	for {
		var bestCost float64
		best, host, wait = nil, "", -1
		resting := false
		for _, s := range ss.list {
			if s.dead != "" || skip[s] || taken[s] || s.distrusted && trusted {
				continue
			}
			h := connHost(s.addr())
			rest := s.rest
			if u := ss.hosts.until(h); u.After(rest) { // the server asked to be left alone (Retry-After)
				rest = u
			}
			if now.Before(rest) {
				if w := rest.Sub(now); wait < 0 || w < wait {
					wait = w
				}
				resting = true
				continue
			}
			if l := s.limit(now); l > 0 && s.active >= l || !ss.hosts.allowed(h, ss.d, ss.budget(s)) {
				full = true
				continue
			}
			r := s.rate
			if r <= 0 {
				r = max(known, 1) // untried: assume it's as good as the best, so it gets a chance
			}
			cost := float64(s.active+1)/r + float64(s.pri)*1e-9
			if best == nil || cost < bestCost {
				best, host, bestCost = s, h, cost
			}
		}
		if best == nil {
			if full && !resting {
				wait = time.Second
			}
			return nil, "", wait, full && !resting
		}
		if ss.hosts.take(host, ss.d, ss.budget(best)) {
			best.active++
			return best, host, 0, false
		}
		taken[best], full = true, true
	}
}

func (ss *sourceSet) release(s *source, host string, got int64, took time.Duration) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s.active--
	ss.hosts.put(host, ss.d)
	if got > 256<<10 && took > 300*time.Millisecond {
		r := float64(got) / took.Seconds()
		if s.rate == 0 {
			s.rate = r
		} else {
			s.rate = s.rate*0.6 + r*0.4
		}
	}
}

func (ss *sourceSet) ok(s *source) {
	ss.mu.Lock()
	s.fails, s.strikes = 0, 0
	ss.mu.Unlock()
}

// fail rests an address after an error; a mirror that keeps failing is dropped.
func (ss *sourceSet) fail(s *source, err error, restFor time.Duration) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s.fails++
	if restFor <= 0 {
		restFor = time.Duration(min(s.fails, 6)) * retryBase
	}
	s.rest = time.Now().Add(restFor)
	if s.mirror && s.fails >= 8 && ss.othersUsable(s) {
		s.dead, s.deadErr = "keeps failing: "+err.Error(), err
	}
}

func (ss *sourceSet) kill(s *source, why string, err error) {
	ss.mu.Lock()
	s.dead, s.deadErr = why, err
	ss.mu.Unlock()
}

// hasTrusted: an address that never sent damaged data is usable. Caller holds ss.mu.
func (ss *sourceSet) hasTrusted() bool {
	for _, x := range ss.list {
		if x.dead == "" && !x.distrusted {
			return true
		}
	}
	return false
}

func (ss *sourceSet) othersUsable(s *source) bool {
	for _, x := range ss.list {
		if x != s && x.dead == "" {
			return true
		}
	}
	return false
}

// gone explains why no address is left.
func (ss *sourceSet) gone() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, s := range ss.list {
		if s.expired {
			return permanentErr{errLinkExpired}
		}
	}
	for _, s := range ss.list {
		if s.deadErr != nil {
			var pe permanentErr
			if errors.As(s.deadErr, &pe) {
				return s.deadErr
			}
			return permanentErr{s.deadErr}
		}
	}
	return permanentErr{errors.New("no link for this file works any more")}
}

// SourceView is one address in the progress window.
type SourceView struct {
	Host   string `json:"host"`
	URL    string `json:"url"`
	Conns  int    `json:"conns"`
	Bytes  int64  `json:"bytes"`
	Dead   string `json:"dead,omitempty"`
	Mirror bool   `json:"mirror"`

	Bad     int  `json:"bad,omitempty"`     // damaged pieces it sent (piececheck.go)
	Damaged bool `json:"damaged,omitempty"` // dropped for sending damaged data
}

func (ss *sourceSet) view() []SourceView {
	if ss == nil {
		return nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]SourceView, 0, len(ss.list))
	for _, s := range ss.list {
		h := s.url
		if u, err := url.Parse(s.url); err == nil && u.Host != "" {
			h = u.Host // with the port when it isn't the usual one
		}
		out = append(out, SourceView{Host: h, URL: stripURLPass(s.url), Conns: s.active, Bytes: atomic.LoadInt64(&s.bytes), Dead: s.dead, Mirror: s.mirror,
			Bad: s.bad, Damaged: s.dead != "" && errors.Is(s.deadErr, errBadData)})
	}
	return out
}

// srcBody counts what a connection receives and frees its slot when closed.
type srcBody struct {
	io.ReadCloser
	ss   *sourceSet
	s    *source
	host string // the server it holds a slot on
	t0   time.Time
	got  int64
	left int64 // of the reply still unread (-1 = unknown)
	once sync.Once
}

func (b *srcBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.got += int64(n)
		if b.left > 0 {
			b.left -= int64(n)
		}
		atomic.AddInt64(&b.s.bytes, int64(n))
	}
	return n, err
}

func (b *srcBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() { b.ss.release(b.s, b.host, b.got, time.Since(b.t0)) })
	return err
}

// openRange opens a stream starting at pos for one connection, on the best address.
func (m *Manager) openRange(ctx context.Context, d *Download, pos, end int64) (io.ReadCloser, error) {
	ss := d.srcs
	if ss == nil {
		ss = &sourceSet{}
		ss.add(&source{url: d.URL, final: d.FinalURL})
	}
	tried := d.pc.avoiding(pos) // (a piece that arrived damaged is fetched again from another address)
	avoided := len(tried) > 0
	var lastErr error
	for {
		s, host, wait, full := ss.pick(time.Now(), tried)
		if s == nil && wait < 0 && avoided { // no other address is left: those get another go
			tried, avoided = map[*source]bool{}, false
			continue
		}
		if s == nil {
			if lastErr != nil && len(tried) > 0 {
				return nil, lastErr // every address had a go: let the worker back off
			}
			if full {
				return nil, errNoSlot // the others keep the servers busy: this connection bows out
			}
			if wait < 0 {
				return nil, ss.gone()
			}
			if !ss.restWait(ctx, min(wait, 2*time.Second)) {
				return nil, ctx.Err()
			}
			continue
		}
		body, n, err := m.openOn(ctx, d, ss, s, pos, end)
		if err == nil {
			return &srcBody{ReadCloser: body, ss: ss, s: s, host: host, t0: time.Now(), left: n}, nil
		}
		ss.release(s, host, 0, 0)
		var pe permanentErr
		if errors.As(err, &pe) || ctx.Err() != nil {
			return nil, err
		}
		tried[s] = true
		if !errors.Is(err, errNoSlot) {
			lastErr = err
		} else if lastErr == nil {
			lastErr = errNoSlot
		}
	}
}

// openOn requests the range from one address and sorts out what its reply means. It returns the
// stream and how long the reply is (-1 = unknown).
func (m *Manager) openOn(ctx context.Context, d *Download, ss *sourceSet, s *source, pos, end int64) (io.ReadCloser, int64, error) {
	ss.mu.Lock()
	u, final := s.addr(), s.final // (renewed by other connections meanwhile)
	validator := ""
	if !s.mirror { // only the main address is asked for "this version only": mirrors tag files their own way
		validator = ss.validator
	}
	ss.mu.Unlock()
	if isFTP(u) {
		b, err := openFTPRange(ctx, d, u, pos)
		if errors.Is(err, errNoSlot) { // "421 too many connections"
			ss.mu.Lock()
			if others := s.active - 1; others > 0 { // settle on what the server accepts
				if l := s.limit(time.Now()); l == 0 || l > others {
					s.learned, s.learnedAt = others, time.Now()
				}
				ss.mu.Unlock()
				return nil, 0, errNoSlot
			}
			ss.mu.Unlock()
			// full, but with none of ours on it: wait and try again (not errNoSlot, or the last
			// connection would bow out too)
			err = errors.New("the FTP server is full: " + err.Error())
			ss.fail(s, err, 2*time.Second)
			return nil, 0, err
		}
		if err != nil {
			ss.fail(s, err, 0)
			return nil, 0, err
		}
		ss.ok(s)
		return b, -1, nil
	}
	resp, err := m.rangeRequest(ctx, d, u, pos, end, validator)
	if err != nil && final != "" && final != s.url && ctx.Err() == nil {
		resp, err = m.rangeRequest(ctx, d, s.url, pos, end, validator) // the signed address may be gone
	}
	if err != nil {
		ss.fail(s, err, 0)
		return nil, 0, err
	}
	code := resp.StatusCode
	if challenged(resp) {
		drainClose(resp.Body)
		if s.mirror {
			ss.kill(s, "bot protection", errChallenge)
			return nil, 0, errChallenge
		}
		return nil, 0, permanentErr{errChallenge}
	}
	if code == http.StatusPartialContent {
		cr := resp.Header.Get("Content-Range")
		if total := parseContentRangeTotal(cr); d.Size > 0 && total > 0 && total != d.Size {
			drainClose(resp.Body)
			e := errors.New("serves a different file (size " + strconv.FormatInt(total, 10) + ")")
			if s.mirror {
				ss.kill(s, e.Error(), e)
				return nil, 0, e
			}
			return nil, 0, permanentErr{errChanged}
		}
		if !rightPart(cr, pos, end) { // a piece from somewhere else would be written here
			drainClose(resp.Body)
			if s.mirror {
				e := errors.New("serves the wrong part")
				ss.kill(s, e.Error(), e)
				return nil, 0, e
			}
			return nil, 0, permanentErr{errWrongPart}
		}
		ss.ok(s)
		return resp.Body, resp.ContentLength, nil
	}
	se := statusErr{code, resp.Status, retryAfterHeader(resp.Header)}
	// a web page instead of the file: a sign-in or "link expired" page from the server
	page := code == http.StatusOK && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html")
	if code == http.StatusOK && !page {
		resp.Body.Close() // the whole file: too big to read out
	} else {
		drainClose(resp.Body)
	}
	switch {
	case page && final != "" && final != s.url:
		if b, n, ok := m.renew(ctx, d, ss, s, pos, end); ok {
			return b, n, nil
		}
		ss.mu.Lock()
		s.strikes++
		if s.strikes >= 2 {
			s.dead, s.deadErr, s.expired = "link expired", errLinkExpired, true
			ss.mu.Unlock()
			if !ss.anyUsable() {
				return nil, 0, ss.gone()
			}
			return nil, 0, errCloudPage
		}
		ss.mu.Unlock()
		ss.fail(s, errCloudPage, 1500*time.Millisecond)
		return nil, 0, errCloudPage
	case code == http.StatusOK && validator != "" && !page:
		// asked for a piece "if it's still this version" and got the whole file: it changed
		return nil, 0, permanentErr{errChanged}
	case code == http.StatusOK:
		e := errors.New("server ignored byte range")
		if s.mirror {
			ss.kill(s, "doesn't support resuming", e)
			return nil, 0, e
		}
		return nil, 0, permanentErr{e}
	case code == http.StatusUnauthorized:
		e := loginError(resp.Request)
		if s.mirror {
			ss.kill(s, "needs a login", e)
			return nil, 0, e
		}
		return nil, 0, e
	case code == http.StatusProxyAuthRequired:
		return nil, 0, permanentErr{errors.New("the proxy needs a user name and password (Options → Proxy)")}
	case expiredStatus(code) || code == 429 || code == 503:
		// A signed link behind a redirect: the address it came from hands out a fresh one.
		if expiredStatus(code) && final != "" && final != s.url {
			if b, n, ok := m.renew(ctx, d, ss, s, pos, end); ok {
				return b, n, nil
			}
		}
		ss.mu.Lock()
		others := s.active - 1 // connections still streaming from this address
		past := !linkExpiry(u).IsZero() && time.Now().After(linkExpiry(u))
		if others > 0 && !past {
			// It's turning away extra connections, not the file: settle on what it accepts.
			if l := s.limit(time.Now()); l == 0 || l > others {
				s.learned, s.learnedAt = others, time.Now()
			}
			ss.mu.Unlock()
			return nil, 0, errNoSlot
		}
		if code == 429 || code == 503 {
			ss.mu.Unlock()
			ss.hosts.retryAfter(connHost(u), se.wait) // no new connections there until then
			ss.fail(s, se, retryAfter(resp.Header.Get("Retry-After")))
			return nil, 0, se
		}
		s.strikes++
		if s.strikes >= 2 || past {
			s.dead, s.deadErr, s.expired = "link expired", se, true
			ss.mu.Unlock()
			if !ss.anyUsable() {
				return nil, 0, ss.gone()
			}
			return nil, 0, se
		}
		ss.mu.Unlock()
		ss.fail(s, se, 1500*time.Millisecond)
		return nil, 0, se
	}
	ss.fail(s, se, 0)
	return nil, 0, se
}

// restWait waits while the addresses rest. A network change (it came back, the computer woke up)
// ends their rest: what failed may well work now.
func (ss *sourceSet) restWait(ctx context.Context, dur time.Duration) bool {
	changed := netChanged()
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	case <-changed:
		ss.mu.Lock()
		for _, s := range ss.list {
			s.rest = time.Time{}
		}
		ss.mu.Unlock()
	}
	return true
}

func (ss *sourceSet) anyUsable() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, s := range ss.list {
		if s.dead == "" {
			return true
		}
	}
	return false
}

// maxRetryAfter caps how long a server can ask Zipline to stay away.
const maxRetryAfter = time.Hour

func retryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		return min(time.Duration(n)*time.Second, maxRetryAfter)
	}
	if t, err := http.ParseTime(v); err == nil {
		return min(max(time.Until(t), time.Second), maxRetryAfter)
	}
	return 5 * time.Second
}

// retryAfterHeader is the wait a reply asks for, 0 when it doesn't say.
func retryAfterHeader(h http.Header) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		return retryAfter(v)
	}
	return 0
}

func (m *Manager) rangeRequest(ctx context.Context, d *Download, u string, pos, end int64, validator string) (*http.Response, error) {
	if cloudService(u) != "" { // a share link: its file address is handed out afresh
		ru, err := resolveCloud(ctx, d, u)
		if err != nil {
			return nil, err
		}
		u = ru
	}
	req, err := buildRequest(ctx, d, u, rangeHeader(pos, end))
	if err != nil {
		return nil, permanentErr{err}
	}
	if validator != "" {
		req.Header.Set("If-Range", validator)
	}
	return doReq(d, req)
}

// renew asks a signed link's original address for a fresh one and opens the range on it. The new
// address must serve the same bytes as those already downloaded.
func (m *Manager) renew(ctx context.Context, d *Download, ss *sourceSet, s *source, pos, end int64) (io.ReadCloser, int64, bool) {
	ss.mu.Lock()
	validator := ""
	if !s.mirror {
		validator = ss.validator
	}
	ss.mu.Unlock()
	resp, err := m.rangeRequest(ctx, d, s.url, pos, end, validator)
	if err != nil {
		return nil, 0, false
	}
	cr := resp.Header.Get("Content-Range")
	total := parseContentRangeTotal(cr)
	if resp.StatusCode != http.StatusPartialContent || d.Size > 0 && total != d.Size || !rightPart(cr, pos, end) {
		drainClose(resp.Body)
		return nil, 0, false
	}
	fresh := resp.Request.URL.String()
	if !m.partMatches(ctx, d, d, fresh, 2, "") {
		resp.Body.Close()
		return nil, 0, false
	}
	ss.mu.Lock()
	s.final, s.strikes, s.fails = fresh, 0, 0
	primary := len(ss.list) > 0 && ss.list[0] == s
	ss.mu.Unlock()
	if primary {
		m.mu.Lock()
		d.FinalURL = fresh
		m.dirty = true
		m.mu.Unlock()
	}
	return resp.Body, resp.ContentLength, true
}

// sameSite reports whether two hosts belong to the same site (so a sign-in cookie for one may
// go to the other), by the Public Suffix List: a.github.io and b.github.io are different sites.
func sameSite(a, b string) bool { return psl.SameSite(a, b) }

func (ss *sourceSet) count() int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return len(ss.list)
}

// mirrorCheck asks each mirror for the file's size while the main link is probed, so the
// mirrors that serve the same file can join in. Matching the size isn't proof: before a mirror
// takes connections, the checksums the servers send, the same version tag on the same server,
// or a few pieces compared with what's already downloaded must show it's the same file.
type mirrorCheck struct {
	ctx     context.Context
	m       *Manager
	d       *Download
	mu      sync.Mutex
	pending int
	results []mirrorResult
	got     chan struct{}
	ss      *sourceSet // after join: late answers go straight in
	size    int64
	etag    string            // the reference (the address the download started from): its version tag,
	host    string            // its server,
	digests map[string]string // and the checksums it sent (Repr-Digest)
	pieces  bool              // a metalink's piece checksums will catch a wrong mirror anyway
}

type mirrorResult struct {
	url, final string
	size       int64
	resumable  bool
	etag       string
	digests    map[string]string
	err        error
}

func newMirrorCheck(ctx context.Context, m *Manager, d *Download) *mirrorCheck {
	return &mirrorCheck{ctx: ctx, m: m, d: d, got: make(chan struct{}, 1)}
}

func (mc *mirrorCheck) start(urls []string) {
	for _, u := range urls {
		mc.mu.Lock()
		mc.pending++
		mc.mu.Unlock()
		go func() {
			ctx, cancel := context.WithTimeout(mc.ctx, 12*time.Second)
			defer cancel()
			r := mirrorResult{url: u}
			pr, err := probe(ctx, mc.d, u, 0, 0)
			if err == nil {
				pr.discard()
				r.final, r.size, r.resumable, r.etag, r.digests = pr.finalURL, pr.size, pr.resumable, pr.etag, reprDigests(pr.hdrs)
			}
			r.err = err
			mc.deliver(r)
		}()
	}
}

func (mc *mirrorCheck) deliver(r mirrorResult) {
	mc.mu.Lock()
	mc.pending--
	if mc.ss != nil {
		mc.accept(r)
	} else {
		mc.results = append(mc.results, r)
	}
	mc.mu.Unlock()
	select {
	case mc.got <- struct{}{}:
	default:
	}
}

// reference notes what the mirrors are compared with: the reply the download started from.
// Caller doesn't hold m.mu.
func (mc *mirrorCheck) reference(pr *probeResult) {
	mc.m.mu.Lock()
	pieces := mc.d.Pieces != nil
	mc.m.mu.Unlock()
	mc.mu.Lock()
	mc.etag, mc.host, mc.digests, mc.pieces = pr.etag, connHost(pr.finalURL), reprDigests(pr.hdrs), pieces
	mc.mu.Unlock()
}

// accept adds a mirror that serves the same file (one that doesn't is listed with the reason).
// One with the right size is checked further first. Caller holds mc.mu.
func (mc *mirrorCheck) accept(r mirrorResult) {
	s := &source{url: r.url, final: r.final, mirror: true, pri: 1}
	switch {
	case r.err != nil:
		s.dead, s.deadErr = downloadErrText(r.err, r.url), r.err
	case !r.resumable:
		s.dead = "doesn't support resuming"
	case r.size != mc.size:
		s.dead = "serves a different file"
	default:
		switch mc.vouched(r) {
		case -1:
			s.dead = "serves a different file"
		case 0:
			go mc.check(s, r)
			return
		}
	}
	mc.ss.add(s)
}

// vouched tells whether a mirror with the right size is known to serve the same file without
// comparing pieces: checksums both servers send settle it (1 or -1), and so does the same strong
// version tag from the same server, or a metalink's piece checksums (which catch a wrong mirror
// anyway). 0: compare pieces. Caller holds mc.mu.
func (mc *mirrorCheck) vouched(r mirrorResult) int {
	if v := digestsAgree(mc.digests, r.digests); v != 0 {
		return v
	}
	if mc.pieces || mc.etag != "" && !strings.HasPrefix(mc.etag, "W/") && r.etag == mc.etag && connHost(r.final) == mc.host {
		return 1
	}
	return 0
}

// check compares a few pieces already downloaded with what a mirror serves (a fresh download
// first waits for something to compare), then adds the mirror: dead when it differs.
func (mc *mirrorCheck) check(s *source, r mirrorResult) {
	m, d := mc.m, mc.d
	add := func(why string) {
		if why != "" {
			s.dead, s.deadErr = why, errors.New(why)
		}
		mc.mu.Lock()
		ss := mc.ss
		mc.mu.Unlock()
		ss.add(s)
	}
	deadline := time.Now().Add(sampleWait)
	for atomic.LoadInt64(&d.done) < 64<<10 && time.Now().Before(deadline) {
		if !sleepCtx(mc.ctx, 50*time.Millisecond) {
			return
		}
	}
	d.mu.Lock()
	running := d.file != nil
	d.mu.Unlock()
	if !running { // finished or stopped meanwhile: no use for it now
		return
	}
	ctx, cancel := context.WithTimeout(mc.ctx, 12*time.Second)
	defer cancel()
	ok, err := m.sameBytes(ctx, d, d, r.final, 3, "")
	switch {
	case mc.ctx.Err() != nil:
	case err != nil:
		add(downloadErrText(err, r.url))
	case !ok:
		add("serves a different file")
	default:
		add("")
	}
}

// digestsAgree compares the checksums two servers sent for their whole file: 1 when they share an
// algorithm and agree, -1 when they disagree, 0 when there's nothing to compare.
func digestsAgree(a, b map[string]string) int {
	res := 0
	for algo, x := range a {
		if y, ok := b[algo]; ok {
			if x != y {
				return -1
			}
			res = 1
		}
	}
	return res
}

// join adds the mirrors that answered (waiting up to wait for the rest); later answers are
// added as they arrive.
func (mc *mirrorCheck) join(ss *sourceSet, size int64, wait time.Duration) {
	mc.mu.Lock()
	mc.ss, mc.size = ss, size
	for _, r := range mc.results {
		mc.accept(r)
	}
	mc.results = nil
	mc.mu.Unlock()
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		mc.mu.Lock()
		p := mc.pending
		mc.mu.Unlock()
		if p == 0 {
			return
		}
		select {
		case <-mc.got:
		case <-t.C:
			return
		case <-mc.ctx.Done():
			return
		}
	}
}

// standIn finds a mirror that can start the download when the main link fails.
func (mc *mirrorCheck) standIn(ctx context.Context, d *Download, pos, end int64, fresh bool) (*probeResult, *source) {
	deadline := time.Now().Add(12 * time.Second)
	used := map[string]bool{}
	for time.Now().Before(deadline) {
		mc.mu.Lock()
		var pick *mirrorResult
		for i := range mc.results {
			r := &mc.results[i]
			if r.err == nil && r.resumable && r.size > 0 && (fresh || r.size == d.Size) && !used[r.url] {
				pick = r
				break
			}
		}
		p := mc.pending
		mc.mu.Unlock()
		if pick != nil {
			used[pick.url] = true
			if pr, err := probe(ctx, d, pick.final, pos, end); err == nil {
				if pr.resumable && (fresh || pr.size == d.Size) && (fresh || mc.m.partMatches(ctx, d, d, pr.finalURL, 2, d.partPath())) {
					return pr, &source{url: pick.url, final: pr.finalURL, mirror: true, pri: 1, active: 0}
				}
				pr.discard()
			}
			continue
		}
		if p == 0 {
			return nil, nil
		}
		select {
		case <-mc.got:
		case <-time.After(time.Until(deadline)):
		case <-ctx.Done():
			return nil, nil
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- same file?

// sampleSize is how much of the file one comparison reads.
const sampleSize = 16 << 10

// writtenSpans picks up to n stretches (of at most sampleSize bytes, [a, b)) of what's already
// written, spread over the file. Caller holds d.mu.
func (d *Download) writtenSpans(n int) [][2]int64 {
	var have [][2]int64
	for _, s := range d.Segments {
		if w := min(s.Pos, s.End+1); w > s.Start {
			have = append(have, [2]int64{s.Start, w})
		}
	}
	slices.SortFunc(have, func(x, y [2]int64) int { return cmp.Compare(x[0], y[0]) })
	var out [][2]int64
	k := min(n, len(have))
	for i := 0; i < k; i++ {
		h := have[i*len(have)/k]
		a := h[0] + max(h[1]-h[0]-sampleSize, 0)/2 // from the middle: away from where it joins its neighbours
		out = append(out, [2]int64{a, min(a+sampleSize, h[1])})
	}
	return out
}

// sameBytes compares up to n pieces already downloaded with what link serves: is it the same
// file? via supplies the request headers. It reads the open part file while the download runs,
// else the one at path. Nothing written yet counts as the same. err is network trouble or a
// reply that isn't a piece of a file (nothing was compared).
func (m *Manager) sameBytes(ctx context.Context, d, via *Download, link string, n int, path string) (bool, error) {
	d.mu.Lock()
	spans := d.writtenSpans(n)
	f := d.file
	d.mu.Unlock()
	if len(spans) == 0 {
		return true, nil
	}
	if f == nil {
		pf, err := os.Open(path)
		if err != nil {
			return false, err
		}
		defer pf.Close()
		f = pf
	}
	for _, sp := range spans {
		req, err := buildRequest(ctx, via, link, rangeHeader(sp[0], sp[1]-1))
		if err != nil {
			return false, err
		}
		resp, err := doReq(via, req)
		if err != nil {
			return false, err
		}
		if resp.StatusCode != http.StatusPartialContent {
			drainClose(resp.Body)
			return false, statusErr{resp.StatusCode, resp.Status, 0}
		}
		if !rightPart(resp.Header.Get("Content-Range"), sp[0], sp[1]-1) {
			drainClose(resp.Body)
			return false, nil
		}
		got := make([]byte, sp[1]-sp[0])
		k, err := io.ReadFull(resp.Body, got)
		resp.Body.Close()
		m.countDown(int64(k))
		if err != nil {
			return false, err
		}
		want := make([]byte, len(got))
		if _, err := f.ReadAt(want, sp[0]); err != nil {
			return false, err
		}
		if !bytes.Equal(got, want) {
			return false, nil
		}
	}
	return true, nil
}

// partMatches: a new address for d (a renewed link) serves the same bytes as those already
// downloaded, comparing up to n pieces. Network trouble counts as "not shown".
func (m *Manager) partMatches(ctx context.Context, d, via *Download, link string, n int, path string) bool {
	ok, err := m.sameBytes(ctx, d, via, link, n, path)
	return ok && err == nil
}
