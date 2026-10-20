package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Saved scans. A scan is written to the data folder (scans/<id>.json) every few seconds while it
// runs and when it ends, with its options, what it found, the pages it has seen and the pages
// still to do, so one that was stopped, or cut short by closing Zipline, can be opened again and
// resumed. The last 20 are kept. A browser sign-in rides along in the file (only this user can
// read it) so a resumed scan is still signed in.

const maxSavedScans = 20

// scanState is what a scan keeps beyond the plain job fields (guarded by the job's lock).
type scanState struct {
	cr          *crawler // the crawl that is running, if any
	begun       bool     // the start page has been queued
	left        []crawlItem
	final       string // where the start page redirected to
	jsPages     []GrabJSPage
	collects    []*GrabCollect
	waitHost    string // a server being left alone, and until when
	waitUntil   time.Time
	delays      map[string]time.Duration // Crawl-delays from robots.txt that are longer than the user's wait
	interrupted bool                     // it was running when Zipline closed
	saved       string                   // what was last written, to skip a write that would change nothing
}

type savedScan struct {
	Version  int               `json:"version"`
	ID       string            `json:"id"`
	Opts     GrabOptions       `json:"opts"`
	Sites    []string          `json:"sites,omitempty"`
	State    string            `json:"state"`
	Error    string            `json:"error,omitempty"`
	Pages    int               `json:"pages"`
	Skipped  int               `json:"skipped"`
	Failed   int               `json:"failed"`
	Files    []GrabFile        `json:"files"`
	Started  time.Time         `json:"started"`
	Finished time.Time         `json:"finished"`
	Title    string            `json:"title,omitempty"`
	Final    string            `json:"final,omitempty"`
	Begun    bool              `json:"begun"`
	Seen     []string          `json:"seen,omitempty"`
	Left     []crawlItem       `json:"left,omitempty"`
	Failures []GrabFailure     `json:"failures,omitempty"`
	JSPages  []GrabJSPage      `json:"jsPages,omitempty"`
	SignedIn []string          `json:"signedIn,omitempty"`
	Cookies  map[string]string `json:"cookies,omitempty"`
}

// initScans sets up what scans share (politeness, robots.txt, sign-ins from the browser) and
// loads the saved ones.
func (g *Grabber) initScans() {
	g.pace = newHostPace()
	g.robots = newRobotsStore()
	g.sessions = map[string]*grabSession{}
	g.scanDir = filepath.Join(g.m.dataDir, "scans")
	os.MkdirAll(g.scanDir, 0o700)
	ents, _ := os.ReadDir(g.scanDir)
	var loaded []*GrabJob
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		s, _ := loadState(filepath.Join(g.scanDir, e.Name()), savedScan{}, "saved scan")
		if s.ID == "" {
			continue
		}
		g.m.vault.revealHeaders(s.Opts.Headers)
		g.m.vault.revealAll(s.Cookies)
		loaded = append(loaded, jobFromSaved(s))
	}
	sort.Slice(loaded, func(a, b int) bool { return loaded[a].Started.After(loaded[b].Started) })
	for i, j := range loaded {
		if i >= maxSavedScans {
			g.dropScanFile(j.ID)
			continue
		}
		g.jobs[j.ID] = j
	}
}

func jobFromSaved(s savedScan) *GrabJob {
	j := &GrabJob{ID: s.ID, Opts: s.Opts, State: s.State, Error: s.Error, Pages: s.Pages, Skipped: s.Skipped, Failed: s.Failed, Files: s.Files,
		Started: s.Started, Finished: s.Finished, Title: s.Title, Cookies: s.Cookies, SignedIn: s.SignedIn, Failures: s.Failures, seen: map[string]bool{}}
	j.Opts.Sites = s.Sites
	j.scan = scanState{begun: s.Begun, left: s.Left, final: s.Final, jsPages: s.JSPages}
	for _, k := range s.Seen {
		j.seen[k] = true
	}
	for _, f := range j.Files {
		j.seen["f:"+dedupKeyOf(f.URL)] = true
	}
	for _, it := range j.scan.left {
		if it.Kind == "" {
			j.Queued++
		}
	}
	if j.State == "running" { // Zipline closed (or crashed) while it ran
		j.State, j.scan.interrupted = "stopped", true
		if j.Finished.IsZero() {
			j.Finished = s.Started
		}
	}
	return j
}

func (g *Grabber) scanFile(id string) string { return filepath.Join(g.scanDir, id+".json") }

func (g *Grabber) dropScanFile(id string) {
	if g.scanDir == "" || id == "" || strings.ContainsAny(id, `/\.`) {
		return
	}
	for _, ext := range []string{"", ".bak", ".tmp"} {
		os.Remove(g.scanFile(id) + ext)
	}
}

// savable: is this a scan the user started (not a page's links, nor a timer's run)?
func (j *GrabJob) savable() bool { return !j.FromPage && j.Project == "" }

// persist writes a scan to its file, unless nothing changed since the last write.
func (g *Grabber) persist(j *GrabJob) {
	if g == nil || g.scanDir == "" || !j.savable() {
		return
	}
	// One save at a time: a flush waits for the autosave's write (and on Windows two writers would
	// find each other's temporary file open when renaming it).
	scanWriteMu.Lock()
	defer scanWriteMu.Unlock()
	j.mu.Lock()
	cr := j.scan.cr
	j.mu.Unlock()
	var work []crawlItem
	if cr != nil { // what is queued and what is on its way both still need doing
		cr.mu.Lock()
		work = append(work, cr.queue...)
		for _, it := range cr.running {
			work = append(work, it)
		}
		cr.mu.Unlock()
	}
	j.mu.Lock()
	if cr == nil {
		work = j.scan.left
	}
	sig := fmt.Sprintf("%s|%d|%d|%d|%d|%d|%d", j.State, j.Pages, len(j.Files), j.Failed, len(work), j.Skipped, len(j.seen))
	if sig == j.scan.saved {
		j.mu.Unlock()
		return
	}
	j.scan.saved = sig
	s := savedScan{Version: 1, ID: j.ID, Opts: j.Opts, Sites: j.Opts.Sites, State: j.State, Error: j.Error, Pages: j.Pages, Skipped: j.Skipped,
		Failed: j.Failed, Files: j.Files, Started: j.Started, Finished: j.Finished, Title: j.Title, Final: j.scan.final, Begun: j.scan.begun,
		Left: work, Failures: j.Failures, JSPages: j.scan.jsPages, SignedIn: j.SignedIn, Cookies: j.Cookies}
	s.Opts.Session = ""
	// (the file holds the logins and cookies sealed)
	s.Opts.Headers, s.Cookies = g.m.vault.protectHeaders(s.Opts.Headers), g.m.vault.protectAll(s.Cookies)
	for k := range j.seen { // (a file is found again from the list of files)
		if !strings.HasPrefix(k, "f:") {
			s.Seen = append(s.Seen, k)
		}
	}
	b, err := json.Marshal(s)
	j.mu.Unlock()
	if err == nil {
		writeAtomicPerm(g.scanFile(j.ID), b, 0o600)
	}
}

var scanWriteMu sync.Mutex

// autosave writes a running scan every few seconds until stop is closed.
func (g *Grabber) autosave(j *GrabJob, stop <-chan struct{}) {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			g.persist(j)
		case <-stop:
			return
		}
	}
}

// flush writes every scan that has changed (called when Zipline saves, and so on exit).
func (g *Grabber) flush() {
	if g == nil {
		return
	}
	g.mu.Lock()
	jobs := slices.Collect(maps.Values(g.jobs))
	g.mu.Unlock()
	for _, j := range jobs {
		g.persist(j)
	}
}

// run is the goroutine of a scan: it crawls, then records how it ended.
func (g *Grabber) run(ctx context.Context, cancel context.CancelFunc, j *GrabJob) {
	stop := make(chan struct{})
	if j.savable() {
		go g.autosave(j, stop)
	}
	var err error
	for { // (once more if work was handed over just as the crawl ended)
		if isFTP(j.Opts.URL) {
			err = g.crawlFTP(ctx, j)
		} else {
			err = g.crawl(ctx, j)
		}
		j.mu.Lock()
		more := len(j.scan.left) > 0 && ctx.Err() == nil && err == nil
		j.mu.Unlock()
		if !more {
			break
		}
	}
	j.mu.Lock()
	j.Finished = time.Now()
	switch {
	case ctx.Err() != nil:
		j.State = "stopped"
	case err != nil:
		j.State, j.Error = "error", err.Error()
	default:
		j.State = "done"
	}
	project := j.Project
	j.mu.Unlock()
	cancel()
	close(stop)
	g.persist(j)
	if project != "" {
		g.projectFinished(project, j)
	}
}

// stop ends a running scan (it can be resumed).
func (j *GrabJob) stop() {
	j.mu.Lock()
	c := j.cancel
	j.mu.Unlock()
	if c != nil {
		c()
	}
}

// Resume carries on with a stopped scan from the pages it still had to do.
func (g *Grabber) Resume(id string) (*GrabJob, error) {
	j := g.Job(id)
	if j == nil {
		return nil, errors.New("that scan is gone; start it again")
	}
	j.mu.Lock()
	switch {
	case j.State == "running":
		j.mu.Unlock()
		return j, nil
	case len(j.scan.left) == 0:
		j.mu.Unlock()
		return nil, errors.New("there is nothing left to scan")
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel, j.State, j.Error, j.Finished, j.scan.interrupted = cancel, "running", "", time.Time{}, false
	j.mu.Unlock()
	go g.run(ctx, cancel, j)
	return j, nil
}

// DeleteScan forgets a scan and its file.
func (g *Grabber) DeleteScan(id string) {
	if j := g.Job(id); j != nil {
		j.stop()
	}
	g.mu.Lock()
	delete(g.jobs, id)
	g.mu.Unlock()
	g.dropScanFile(id)
}

// Scans lists the scans that can be opened again, newest first.
func (g *Grabber) Scans() []map[string]any {
	g.mu.Lock()
	jobs := slices.Collect(maps.Values(g.jobs))
	g.mu.Unlock()
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Started.After(jobs[b].Started) })
	out := []map[string]any{}
	for _, j := range jobs {
		if !j.savable() {
			continue
		}
		j.mu.Lock()
		out = append(out, map[string]any{"id": j.ID, "url": j.Opts.URL, "state": j.State, "started": j.Started, "finished": j.Finished,
			"pages": j.Pages, "files": len(j.Files), "left": len(j.scan.left), "resumable": j.State != "running" && len(j.scan.left) > 0,
			"interrupted": j.scan.interrupted, "signedIn": len(j.SignedIn) > 0})
		j.mu.Unlock()
	}
	return out
}

// ---------------------------------------------------------------- what a scan tells the window

// viewExtra adds the politeness, failure and browser details to the scan's status (j.mu held).
func (j *GrabJob) viewExtra(v map[string]any) {
	now := time.Now()
	v["failed"] = j.Failed
	v["failures"] = append([]GrabFailure{}, j.Failures...)
	v["signedIn"] = append([]string{}, j.SignedIn...)
	v["jsPages"] = append([]GrabJSPage{}, j.scan.jsPages...)
	v["left"] = len(j.scan.left)
	v["resumable"] = j.State != "running" && len(j.scan.left) > 0
	v["interrupted"] = j.scan.interrupted
	if j.scan.waitUntil.After(now) {
		v["wait"] = map[string]any{"host": j.scan.waitHost, "secs": int(math.Ceil(j.scan.waitUntil.Sub(now).Seconds()))}
	}
	delays := map[string]float64{}
	for h, d := range j.scan.delays {
		delays[h] = d.Seconds()
	}
	v["delays"] = delays
	cs := []GrabCollect{}
	for _, c := range j.scan.collects {
		c.expire(now)
		cs = append(cs, *c)
	}
	v["collects"] = cs
}

// noteWait remembers that a server is being left alone until then, for the status line.
func (j *GrabJob) noteWait(host string, until time.Time) {
	j.mu.Lock()
	if until.After(j.scan.waitUntil) {
		j.scan.waitHost, j.scan.waitUntil = host, until
	}
	j.mu.Unlock()
}

func (j *GrabJob) noteDelay(host string, d time.Duration) {
	j.mu.Lock()
	if j.scan.delays == nil {
		j.scan.delays = map[string]time.Duration{}
	}
	j.scan.delays[host] = d
	j.mu.Unlock()
}

// noteScripted records a page that seems to build its links with JavaScript.
func (j *GrabJob) noteScripted(page string, depth int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, p := range j.scan.jsPages {
		if p.URL == page {
			return
		}
	}
	if len(j.scan.jsPages) < 20 {
		j.scan.jsPages = append(j.scan.jsPages, GrabJSPage{URL: page, Depth: depth})
	}
}

// cookieFor is the browser sign-in to send to host: only the site the scan started on (by the
// public suffix list, so a.github.io's cookies never go to b.github.io), and for a sibling host
// the start page's own.
func (j *GrabJob) cookieFor(host string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.Cookies) == 0 {
		return ""
	}
	start := ""
	if u, err := url.Parse(j.Opts.URL); err == nil {
		start = u.Hostname()
	}
	if !grabSite(start, j.Opts.Sites, host) {
		return ""
	}
	if c := j.Cookies[host]; c != "" {
		return c
	}
	return j.Cookies[start]
}

// fileCookies picks the browser cookies each file's download should carry: those of its own host,
// or the scan's sign-in for another host of the same site.
func (j *GrabJob) fileCookies(files []GrabFile) map[string]string {
	j.mu.Lock()
	own := maps.Clone(j.Cookies)
	j.mu.Unlock()
	if len(own) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, f := range files {
		u, err := url.Parse(f.URL)
		if err != nil {
			continue
		}
		if c := own[u.Hostname()]; c != "" {
			out[u.Hostname()] = c
		} else if c := j.cookieFor(u.Hostname()); c != "" {
			out[u.Hostname()] = c
		}
	}
	return out
}

func (g *Grabber) categoryOf(name string) string {
	g.m.mu.Lock()
	defer g.m.mu.Unlock()
	return g.m.categoryFor(name)
}

// ---------------------------------------------------------------- "Scan this site" from the browser

// grabSession is the sign-in the browser extension sends with "Scan this site": it waits here
// until the window starts the scan.
type grabSession struct {
	ID      string
	Page    string
	Cookies map[string]string
	Made    time.Time
}

func (g *Grabber) NewSession(page string, cookies map[string]string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, s := range g.sessions {
		if time.Since(s.Made) > 6*time.Hour {
			delete(g.sessions, id)
		}
	}
	if len(g.sessions) >= 20 { // (the oldest goes)
		var oldest string
		for id, s := range g.sessions {
			if oldest == "" || s.Made.Before(g.sessions[oldest].Made) {
				oldest = id
			}
		}
		delete(g.sessions, oldest)
	}
	s := &grabSession{ID: newID(), Page: page, Cookies: map[string]string{}, Made: time.Now()}
	for h, c := range cookies {
		if len(s.Cookies) < 40 && c != "" {
			s.Cookies[strings.ToLower(h)] = c
		}
	}
	g.sessions[s.ID] = s
	return s.ID
}

// signIn attaches a browser session's cookies to a new scan, for the hosts of the site it starts on.
func (g *Grabber) signIn(j *GrabJob, id string) {
	g.mu.Lock()
	s := g.sessions[id]
	g.mu.Unlock()
	u, err := url.Parse(j.Opts.URL)
	if s == nil || err != nil {
		return
	}
	j.Cookies = map[string]string{}
	for h, c := range s.Cookies {
		if sameSite(u.Hostname(), h) {
			j.Cookies[h] = c
			j.SignedIn = append(j.SignedIn, h)
		}
	}
	sort.Strings(j.SignedIn)
	if len(j.Cookies) == 0 {
		j.Cookies = nil
	}
}
