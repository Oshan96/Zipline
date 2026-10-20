package main

import (
	"context"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zipline/internal/sitemap"
)

// The site grabber's crawl. Every address to look at is an item in one queue: a page, a link of
// unknown kind to check (HEAD), a style sheet, a sitemap. Workers take the first item whose server
// is ready (see grabpace.go), so a server that asks for quiet doesn't hold up the others. A page
// that can't be read is tried again a couple of times, then listed with the reason.

var (
	grabRetryBase = 2 * time.Second // the wait before a page is tried again; triples each time
	grabTries     = 3               // tries for a page, the first included
	grabTries429  = 5               // …when the server keeps asking for quiet (429, 503)
)

const (
	grabWorkers  = 6
	pageAccept   = "text/html,application/xhtml+xml,*/*;q=0.5"
	maxFailures  = 200
	maxSheets    = 300
	maxSitemaps  = 50
	maxSitemapLs = 20000
)

type crawlItem struct {
	URL   string `json:"u"`
	Depth int    `json:"d"`           // links followed from the start page (for a sitemap or style sheet: how deeply nested)
	Kind  string `json:"k,omitempty"` // "" a page · "check" a link of unknown kind · "css" · "sitemap"
	From  string `json:"f,omitempty"` // the page it was found on
	Tries int    `json:"t,omitempty"`

	ready time.Time // not before (a retry)
	host  string
}

// GrabFailure is a page that could not be read, and why.
type GrabFailure struct {
	URL    string `json:"url"`
	Kind   string `json:"kind"` // status · net · robots
	Status int    `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
}

type crawler struct {
	g      *Grabber
	j      *GrabJob
	o      GrabOptions
	start  *url.URL
	want   map[string]bool
	known  map[string]bool // extensions that are plainly files (or parts of a page), whether or not they're wanted
	scope  *grabScope
	d      *Download
	wait   time.Duration
	sheets bool // pictures or fonts are wanted, so style sheets are read for them

	checks, nsheets, nsitemaps, sitemapURLs atomic.Int32
	robotsSeen                              sync.Map

	mu      sync.Mutex
	cond    *sync.Cond
	queue   []crawlItem
	running map[int]crawlItem
	closed  bool // no worker is left to take what is added
}

func (g *Grabber) newCrawler(j *GrabJob) *crawler {
	o := j.Opts
	start, _ := url.Parse(o.URL)
	cr := &crawler{g: g, j: j, o: o, start: start, want: g.wantedExts(o), d: &Download{Headers: o.Headers},
		scope: newGrabScope(start, o.Scope), known: map[string]bool{}, running: map[int]crawlItem{}}
	cr.cond = sync.NewCond(&cr.mu)
	cr.wait = time.Duration(o.Wait) * time.Millisecond
	g.m.mu.Lock()
	for _, c := range g.m.settings.Categories {
		for _, e := range c.Exts {
			cr.known[strings.ToLower(e)] = true
		}
	}
	g.m.mu.Unlock()
	for _, e := range webExts {
		cr.known[e] = true
	}
	for _, e := range cssAssetExts {
		cr.sheets = cr.sheets || cr.want[e]
	}
	j.mu.Lock()
	if f, err := url.Parse(j.scan.final); err == nil && j.scan.final != "" {
		cr.scope.add(f)
	}
	j.mu.Unlock()
	return cr
}

// crawl scans a site: from its start page, or from where a stopped scan left off.
func (g *Grabber) crawl(ctx context.Context, j *GrabJob) error {
	cr := g.newCrawler(j)
	j.mu.Lock()
	begun, left := j.scan.begun, j.scan.left
	j.scan.begun, j.scan.left, j.scan.cr = true, nil, cr
	j.mu.Unlock()
	if begun {
		pages := 0
		for _, it := range left {
			if it.Kind == "" {
				pages++
			}
		}
		j.mu.Lock()
		j.Queued = pages
		j.mu.Unlock()
		cr.push(left...)
	} else {
		// a start link that is itself a wanted file
		if cr.want[extOf(cr.start)] {
			j.addFile(GrabFile{URL: cr.start.String(), Name: path.Base(cr.start.Path), Ext: extOf(cr.start), Category: g.category(cr.start), From: cr.start.String()})
			j.mu.Lock()
			j.scan.cr = nil
			j.mu.Unlock()
			return nil
		}
		s := cleanURL(cr.start)
		j.mu.Lock()
		j.seen["p:"+dedupKey(s)] = true
		j.mu.Unlock()
		cr.push(crawlItem{URL: s.String()})
		if cr.o.Sitemap && cr.o.Depth >= 1 {
			cr.pushSitemap(&url.URL{Scheme: s.Scheme, Host: s.Host, Path: "/sitemap.xml"}, 0)
		}
	}
	cr.run(ctx)
	return nil
}

// run works through the queue until it is empty and nothing is on its way, or ctx ends.
func (cr *crawler) run(ctx context.Context) {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			cr.mu.Lock()
			cr.cond.Broadcast()
			cr.mu.Unlock()
		case <-stop:
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < grabWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cr.worker(ctx, i)
		}()
	}
	wg.Wait()
	cr.mu.Lock()
	cr.closed = true
	left := cr.queue
	cr.mu.Unlock()
	cr.j.mu.Lock()
	cr.j.scan.left, cr.j.scan.cr = append(cr.j.scan.left, left...), nil // (more may have been handed over as it ended)
	cr.j.mu.Unlock()
}

func (cr *crawler) worker(ctx context.Context, id int) {
	for {
		cr.mu.Lock()
		var it crawlItem
		var tk *paceTicket
		for {
			if ctx.Err() != nil {
				cr.mu.Unlock()
				return
			}
			i, t, wake := cr.pick()
			if t != nil {
				it, tk = cr.queue[i], t
				cr.queue = slices.Delete(cr.queue, i, i+1)
				break
			}
			if len(cr.queue) == 0 && len(cr.running) == 0 {
				cr.closed = true
				cr.cond.Broadcast()
				cr.mu.Unlock()
				return
			}
			// nothing is ready: sleep until something is added or finishes, or the first wait is over
			d := 250 * time.Millisecond // (other scans give tickets back without telling us)
			if !wake.IsZero() {
				d = min(d, max(time.Until(wake), time.Millisecond))
			}
			tm := time.AfterFunc(d, func() { cr.mu.Lock(); cr.cond.Broadcast(); cr.mu.Unlock() })
			cr.cond.Wait()
			tm.Stop()
		}
		cr.running[id] = it
		cr.mu.Unlock()
		again := cr.process(ctx, it, tk)
		tk.done()
		cr.mu.Lock()
		delete(cr.running, id)
		if again != nil {
			cr.queue = append(cr.queue, *again)
		}
		cr.cond.Broadcast()
		cr.mu.Unlock()
	}
}

// pick finds the first queued item whose server can take a request now (caller holds cr.mu);
// otherwise the moment to look again.
func (cr *crawler) pick() (int, *paceTicket, time.Time) {
	now := time.Now()
	var wake time.Time
	note := func(t time.Time) {
		if wake.IsZero() || t.Before(wake) {
			wake = t
		}
	}
	for i := range cr.queue {
		it := &cr.queue[i]
		if now.Before(it.ready) {
			note(it.ready)
			continue
		}
		if it.host == "" {
			it.host = itemHost(it.URL)
		}
		tk, w := cr.g.pace.try(it.host)
		if tk != nil {
			return i, tk, wake
		}
		if !w.IsZero() {
			note(w)
			cr.j.noteWait(it.host, w)
		}
	}
	return -1, nil, wake
}

func itemHost(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return hostKey(u)
	}
	return raw
}

// push adds items to the queue. False when the crawl has already ended (nobody would take them).
func (cr *crawler) push(items ...crawlItem) bool {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if cr.closed {
		return false
	}
	for _, it := range items {
		it.host = ""
		cr.queue = append(cr.queue, it)
	}
	cr.cond.Broadcast()
	return true
}

// ---------------------------------------------------------------- what to do with an item

// process handles one item with its server's ticket. It returns the item to try again, or nil.
func (cr *crawler) process(ctx context.Context, it crawlItem, tk *paceTicket) *crawlItem {
	u, err := url.Parse(it.URL)
	if err != nil {
		cr.finishPage(it)
		return nil
	}
	switch it.Kind {
	case "check":
		return cr.doCheck(ctx, it, u, tk)
	case "css":
		return cr.doSheet(ctx, it, u, tk)
	case "sitemap":
		return cr.doSitemap(ctx, it, u, tk)
	}
	return cr.doPage(ctx, it, u, tk)
}

// request sends one request for an item when its turn comes. The sign-in carried over from the
// browser goes only to the start page's own site.
func (cr *crawler) request(ctx context.Context, tk *paceTicket, method string, u *url.URL, rng, accept string) (*http.Response, error) {
	if !tk.turn(ctx, cr.wait) || !cr.g.m.netGateWait(ctx) {
		return nil, ctx.Err()
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	req, err := buildRequest(cctx, cr.d, u.String(), rng)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Method = method
	req.Header.Set("Accept", accept)
	if c := cr.j.cookieFor(u.Hostname()); c != "" {
		if old := req.Header.Get("Cookie"); old != "" {
			c = old + "; " + c
		}
		req.Header.Set("Cookie", c)
	}
	resp, err := doReq(cr.d, req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelBody{resp.Body, cancel}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

// robotsOK asks robots.txt about u (reading it first if need be, with the same ticket). nil when
// the scan was stopped meanwhile.
func (cr *crawler) robotsOK(ctx context.Context, tk *paceTicket, u *url.URL) (bool, *robotsEntry) {
	if !cr.o.Robots {
		return true, &robotsEntry{}
	}
	origin := u.Scheme + "://" + u.Host
	ent := cr.g.robots.get(origin)
	if ent == nil {
		ent = cr.g.robots.load(ctx, origin, func() *robotsEntry {
			if !tk.turn(ctx, cr.wait) || !cr.g.m.netGateWait(ctx) {
				return nil
			}
			return readRobots(ctx, cr.d, origin, cr.g.pace)
		})
		if ent == nil {
			return false, nil
		}
	}
	var delay time.Duration
	if ent.rules != nil {
		delay = ent.rules.delay
	}
	cr.g.pace.setCrawl(hostKey(u), delay)
	if delay > cr.wait {
		cr.j.noteDelay(u.Host, min(delay, grabCrawlDelayMax))
	}
	if _, done := cr.robotsSeen.LoadOrStore(origin, true); !done && ent.rules != nil && cr.o.Sitemap && cr.o.Depth >= 1 {
		for _, s := range ent.rules.sitemaps {
			if su, err := u.Parse(s); err == nil {
				cr.pushSitemap(su, 0)
			}
		}
	}
	return ent.allowed(u), ent
}

// finishPage counts a page as dealt with.
func (cr *crawler) finishPage(it crawlItem) {
	if it.Kind != "" {
		return
	}
	cr.j.mu.Lock()
	cr.j.Pages++
	if cr.j.Queued > 0 {
		cr.j.Queued--
	}
	cr.j.mu.Unlock()
}

// later decides what becomes of an item that could not be read now: another try after a wait,
// or (for a page) the failure list.
func (cr *crawler) later(it crawlItem, kind string, status int, detail string) *crawlItem {
	it.Tries++
	slow := status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
	limit := grabTries
	if slow {
		limit = grabTries429
	}
	if it.Tries < limit {
		if !slow { // (a server that asked for quiet is held back by its own wait)
			d := grabRetryBase
			for i := 1; i < it.Tries; i++ {
				d *= 3
			}
			it.ready = time.Now().Add(d + time.Duration(float64(d)*0.2*(rand.Float64()*2-1)))
		}
		return &it
	}
	if it.Kind == "" {
		cr.j.mu.Lock()
		cr.j.Failed++
		if len(cr.j.Failures) < maxFailures {
			cr.j.Failures = append(cr.j.Failures, GrabFailure{URL: it.URL, Kind: kind, Status: status, Detail: detail})
		}
		cr.j.mu.Unlock()
		cr.finishPage(it)
	}
	return nil
}

// refused handles an answer that isn't a page: ask again later (or give up) if the server is
// busy or broken, else nothing more to do. done is false when the item must be tried again.
func (cr *crawler) refused(it crawlItem, u *url.URL, resp *http.Response) (again *crawlItem, handled bool) {
	code := resp.StatusCode
	switch {
	case code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable:
		d := cr.g.pace.slowDown(hostKey(u), resp.Header)
		cr.j.noteWait(hostKey(u), time.Now().Add(d))
		return cr.later(it, "status", code, http.StatusText(code)), true
	case code >= 500 || code == http.StatusRequestTimeout:
		return cr.later(it, "status", code, http.StatusText(code)), true
	case code >= 400:
		if it.Kind == "" { // a page that isn't there (or isn't ours to read) is a page we couldn't read
			cr.j.mu.Lock()
			cr.j.Failed++
			if len(cr.j.Failures) < maxFailures {
				cr.j.Failures = append(cr.j.Failures, GrabFailure{URL: it.URL, Kind: "status", Status: code, Detail: http.StatusText(code)})
			}
			cr.j.mu.Unlock()
			cr.finishPage(it)
		}
		return nil, true
	case code < 200 || code >= 300:
		cr.finishPage(it)
		return nil, true
	}
	return nil, false
}

func (cr *crawler) doPage(ctx context.Context, it crawlItem, u *url.URL, tk *paceTicket) *crawlItem {
	ok, ent := cr.robotsOK(ctx, tk, u)
	switch {
	case ent == nil:
		return &it
	case ent.down:
		return cr.later(it, "robots", 0, ent.why)
	case !ok:
		cr.j.mu.Lock()
		cr.j.Skipped++
		cr.j.mu.Unlock()
		cr.finishPage(it)
		return nil
	}
	resp, err := cr.request(ctx, tk, "GET", u, "", pageAccept)
	if err != nil {
		if ctx.Err() != nil {
			return &it
		}
		return cr.later(it, "net", 0, friendlyError(err, u.String()))
	}
	defer resp.Body.Close()
	if again, handled := cr.refused(it, u, resp); handled {
		return again
	}
	cr.g.pace.served(hostKey(u))
	final := resp.Request.URL
	if it.Depth == 0 && final.String() != u.String() {
		cr.scope.add(final) // the start page redirected: that's the site too
		cr.j.mu.Lock()
		cr.j.Opts.Sites = append(cr.j.Opts.Sites, final.Hostname())
		cr.j.scan.final = final.String()
		cr.j.mu.Unlock()
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !isHTMLType(ct) {
		cr.fileFrom(resp, u, it.From) // (a download script that was linked as if it were a page)
		cr.finishPage(it)
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	cr.g.m.countDown(int64(len(b)))
	if ctx.Err() != nil {
		return &it
	}
	cr.scanPage(it, final, string(b))
	cr.finishPage(it)
	return nil
}

// scanPage takes the links, media addresses and style sheets from a page.
func (cr *crawler) scanPage(it crawlItem, base *url.URL, body string) {
	links := extractLinks(base, body)
	for _, l := range links {
		cr.offer(l, it.URL, it.Depth)
	}
	for _, l := range extractExtra(base, body) {
		cr.offer(l, it.URL, it.Depth)
	}
	if cr.sheets {
		for _, s := range extractSheets(base, body) {
			cr.pushSheet(s, it.URL, 0)
		}
	}
	if contentLinks(links, base) <= 2 && looksScriptBuilt(body) {
		cr.j.noteScripted(it.URL, it.Depth)
	}
}

// offer decides what a link found on a page at depth is: a wanted file, a page to follow, a
// file nobody asked for, or something to check.
func (cr *crawler) offer(l *url.URL, from string, depth int) {
	l = cleanURL(l)
	ext := extOf(l)
	switch {
	case cr.want[ext]:
		if cr.o.CrossSite || cr.scope.in(l) {
			cr.addFile(l, from)
		}
	case htmlExts[ext]:
		cr.followPage(l, from, depth)
	case ext != "" && cr.known[ext] && !scriptExts[ext]:
		// (a picture, script or style nobody asked for)
	default: // no extension, a script, or one we don't know: only the server can say what it is
		cr.checkLink(l, from, depth)
	}
}

func (cr *crawler) addFile(l *url.URL, from string) {
	cr.j.addFile(GrabFile{URL: l.String(), Name: path.Base(l.Path), Ext: extOf(l), Category: cr.g.category(l), From: from})
}

// offerAsset is offer for what a style sheet points to: files only.
func (cr *crawler) offerAsset(l *url.URL, from string) {
	l = cleanURL(l)
	if cr.want[extOf(l)] && (cr.o.CrossSite || cr.scope.in(l)) {
		cr.addFile(l, from)
	}
}

// followPage queues a page found at depth (one level deeper) if it is in scope and under the limits.
func (cr *crawler) followPage(l *url.URL, from string, depth int) {
	if depth >= cr.o.Depth || !cr.scope.in(l) || (l.Scheme != "http" && l.Scheme != "https") {
		return
	}
	j := cr.j
	key := "p:" + dedupKey(l)
	j.mu.Lock()
	dup := j.seen[key]
	over := j.Pages+j.Queued >= cr.o.MaxPages
	if !dup && !over {
		j.seen[key] = true
		j.Queued++
	} else if over && !dup {
		j.Skipped++
	}
	j.mu.Unlock()
	if !dup && !over {
		cr.push(crawlItem{URL: l.String(), Depth: depth + 1, From: from})
	}
}

// checkLink queues a link whose kind is unknown for a HEAD request. Links to other sites are
// only checked when they look like a download, so a page's outside links aren't all visited.
func (cr *crawler) checkLink(l *url.URL, from string, depth int) {
	if l.Scheme != "http" && l.Scheme != "https" {
		return
	}
	if !cr.scope.in(l) && !(cr.o.CrossSite && looksLikeDownload(l)) {
		return
	}
	if int(cr.checks.Load()) >= max(300, cr.o.MaxPages*2) {
		return
	}
	key := "c:" + dedupKey(l)
	cr.j.mu.Lock()
	dup := cr.j.seen[key]
	cr.j.seen[key] = true
	cr.j.mu.Unlock()
	if dup {
		return
	}
	cr.checks.Add(1)
	cr.push(crawlItem{URL: l.String(), Depth: depth + 1, Kind: "check", From: from})
}

func (cr *crawler) pushSheet(l *url.URL, from string, nest int) {
	l = cleanURL(l)
	if (l.Scheme != "http" && l.Scheme != "https") || !(cr.o.CrossSite || cr.scope.in(l)) || int(cr.nsheets.Load()) >= maxSheets {
		return
	}
	key := "s:" + dedupKey(l)
	cr.j.mu.Lock()
	dup := cr.j.seen[key]
	cr.j.seen[key] = true
	cr.j.mu.Unlock()
	if !dup {
		cr.nsheets.Add(1)
		cr.push(crawlItem{URL: l.String(), Depth: nest, Kind: "css", From: from})
	}
}

func (cr *crawler) pushSitemap(l *url.URL, nest int) {
	l = cleanURL(l)
	if (l.Scheme != "http" && l.Scheme != "https") || !(cr.scope.in(l) || sameSite(cr.start.Hostname(), l.Hostname())) || int(cr.nsitemaps.Load()) >= maxSitemaps {
		return
	}
	key := "m:" + dedupKey(l)
	cr.j.mu.Lock()
	dup := cr.j.seen[key]
	cr.j.seen[key] = true
	cr.j.mu.Unlock()
	if !dup {
		cr.nsitemaps.Add(1)
		cr.push(crawlItem{URL: l.String(), Depth: nest, Kind: "sitemap", From: cr.start.String()})
	}
}

// fileFrom adds the file an answer turned out to be, named the way a download of it would be,
// if it is one of the wanted kinds.
func (cr *crawler) fileFrom(resp *http.Response, link *url.URL, from string) {
	name := fileNameFrom(resp)
	ext, shown, ok := matchFile(cr.want, name, resp.Header.Get("Content-Type"))
	if !ok || !(cr.o.CrossSite || cr.scope.in(link)) {
		return
	}
	cr.j.addFile(GrabFile{URL: link.String(), Name: shown, Ext: ext, Category: cr.g.categoryOf(shown), From: from})
}

// doCheck finds out with a HEAD request (or, where the server won't take one, a ranged GET of
// one byte) whether a link is a page to follow or a file, from its Content-Type.
func (cr *crawler) doCheck(ctx context.Context, it crawlItem, u *url.URL, tk *paceTicket) *crawlItem {
	ok, ent := cr.robotsOK(ctx, tk, u)
	switch {
	case ent == nil:
		return &it
	case ent.down:
		return cr.later(it, "robots", 0, ent.why)
	case !ok:
		cr.j.mu.Lock()
		cr.j.Skipped++
		cr.j.mu.Unlock()
		return nil
	}
	resp, err := cr.request(ctx, tk, "HEAD", u, "", "*/*")
	if err == nil && (resp.StatusCode == 400 || resp.StatusCode == 403 || resp.StatusCode == 405 || resp.StatusCode == 501) {
		resp.Body.Close()
		resp, err = cr.request(ctx, tk, "GET", u, "bytes=0-0", "*/*")
	}
	if err != nil {
		if ctx.Err() != nil {
			return &it
		}
		return cr.later(it, "net", 0, friendlyError(err, u.String()))
	}
	defer resp.Body.Close()
	if again, handled := cr.refused(it, u, resp); handled {
		return again
	}
	cr.g.pace.served(hostKey(u))
	if isHTMLType(resp.Header.Get("Content-Type")) {
		cr.followPage(u, it.From, it.Depth-1)
		return nil
	}
	cr.fileFrom(resp, u, it.From)
	return nil
}

func (cr *crawler) doSheet(ctx context.Context, it crawlItem, u *url.URL, tk *paceTicket) *crawlItem {
	ok, ent := cr.robotsOK(ctx, tk, u)
	switch {
	case ent == nil:
		return &it
	case ent.down:
		return cr.later(it, "robots", 0, ent.why)
	case !ok:
		return nil
	}
	resp, err := cr.request(ctx, tk, "GET", u, "", "text/css,*/*;q=0.1")
	if err != nil {
		if ctx.Err() != nil {
			return &it
		}
		return cr.later(it, "net", 0, friendlyError(err, u.String()))
	}
	defer resp.Body.Close()
	if again, handled := cr.refused(it, u, resp); handled {
		return again
	}
	cr.g.pace.served(hostKey(u))
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	cr.g.m.countDown(int64(len(b)))
	css := string(b)
	for _, a := range cssAssets(resp.Request.URL, css) {
		cr.offerAsset(a, it.From)
	}
	if it.Depth < 3 {
		for _, s := range cssImports(resp.Request.URL, css) {
			cr.pushSheet(s, it.From, it.Depth+1)
		}
	}
	return nil
}

func (cr *crawler) doSitemap(ctx context.Context, it crawlItem, u *url.URL, tk *paceTicket) *crawlItem {
	resp, err := cr.request(ctx, tk, "GET", u, "", "application/xml,text/xml,text/plain,*/*;q=0.5")
	if err != nil {
		if ctx.Err() != nil {
			return &it
		}
		return cr.later(it, "net", 0, friendlyError(err, u.String()))
	}
	defer resp.Body.Close()
	if again, handled := cr.refused(it, u, resp); handled {
		return again // (most sites have none: a missing sitemap isn't news)
	}
	cr.g.pace.served(hostKey(u))
	urls, subs, err := sitemap.Read(resp.Body)
	if ctx.Err() != nil {
		return &it
	}
	if err != nil {
		return nil
	}
	for _, s := range subs {
		if su, err := resp.Request.URL.Parse(s); err == nil && it.Depth < 3 {
			cr.pushSitemap(su, it.Depth+1)
		}
	}
	for _, s := range urls {
		if int(cr.sitemapURLs.Add(1)) > maxSitemapLs {
			break
		}
		if l := webURL(resp.Request.URL, s); l != nil {
			cr.offer(l, cr.start.String(), 0) // (listed on the site's front: a page one link away)
		}
	}
	return nil
}
