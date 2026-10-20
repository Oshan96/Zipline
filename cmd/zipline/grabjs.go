package main

import (
	"errors"
	"net/url"
	"time"
)

// Pages that build their links with JavaScript. A fetched page like that has next to no links, so
// the scan says so, and the window can ask the browser extension to open the page in a background
// tab, let it run, and send back the links it ended up with (the extension's "Download all links"
// collector, all frames). Those links then go through the scan like links read from a fetched page.

type GrabJSPage struct {
	URL   string `json:"url"`
	Depth int    `json:"depth"`
}

// GrabCollect is one page the browser was asked to open.
type GrabCollect struct {
	ID    string `json:"id"`
	Page  string `json:"page"`
	Depth int    `json:"depth"`
	State string `json:"state"` // waiting · done · failed
	Links int    `json:"links,omitempty"`
	Error string `json:"error,omitempty"` // timeout · empty · blocked, or what the extension said

	added, taken time.Time
}

// the browser has this long to answer before the scan gives up on it
const (
	collectWait  = 90 * time.Second // from asking, to the extension picking it up
	collectTotal = 100 * time.Second
)

// expire turns an ask nobody answered into a failure (the job's lock is held).
func (c *GrabCollect) expire(now time.Time) {
	if c.State != "waiting" {
		return
	}
	if c.taken.IsZero() && now.Sub(c.added) > collectWait || !c.taken.IsZero() && now.Sub(c.taken) > collectTotal {
		c.State, c.Error = "failed", "timeout"
	}
}

var errNoBrowser = errors.New("the Zipline browser extension isn't connected")

// AskBrowser asks the extension to open the scripted pages of a scan (all of them, or just page).
func (g *Grabber) AskBrowser(jobID, page string) error {
	j := g.Job(jobID)
	if j == nil {
		return errors.New("that scan has expired; run it again")
	}
	g.m.mu.Lock()
	on := g.m.extActive()
	g.m.mu.Unlock()
	if !on {
		return errNoBrowser
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	n := 0
	for _, p := range j.scan.jsPages {
		if page != "" && p.URL != page {
			continue
		}
		busy := false
		for _, c := range j.scan.collects {
			c.expire(now)
			busy = busy || c.Page == p.URL && c.State == "waiting"
		}
		if busy {
			continue
		}
		j.scan.collects = append(j.scan.collects, &GrabCollect{ID: newID(), Page: p.URL, Depth: p.Depth, State: "waiting", added: now})
		n++
	}
	if n == 0 && len(j.scan.jsPages) == 0 {
		return errors.New("no page of this scan needs the browser")
	}
	return nil
}

// browserAsk is what the extension is told to open.
type browserAsk struct {
	ID    string `json:"id"`
	JobID string `json:"jobId"`
	Page  string `json:"page"`
}

// takeAsks hands the extension the pages to open (once each); it picks them up with its ping.
func (g *Grabber) takeAsks() []browserAsk {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []browserAsk
	now := time.Now()
	for _, j := range g.jobs {
		j.mu.Lock()
		for _, c := range j.scan.collects {
			c.expire(now)
			if c.State == "waiting" && c.taken.IsZero() {
				c.taken = now
				out = append(out, browserAsk{ID: c.ID, JobID: j.ID, Page: c.Page})
			}
		}
		j.mu.Unlock()
	}
	return out
}

// BrowserLinks is what the extension sends back for one ask.
type BrowserLinks struct {
	ID      string            `json:"id"`
	JobID   string            `json:"jobId"`
	Links   []PageLink        `json:"links"`
	Cookies map[string]string `json:"cookies"`
	Error   string            `json:"error"`
}

// FeedLinks puts the links the browser found on a scripted page into its scan, one level below
// that page, and carries the scan on (it may well have finished by now).
func (g *Grabber) FeedLinks(b BrowserLinks) error {
	j := g.Job(b.JobID)
	if j == nil {
		return errors.New("that scan has expired")
	}
	j.mu.Lock()
	var c *GrabCollect
	for _, x := range j.scan.collects {
		if x.ID == b.ID {
			c = x
		}
	}
	if c == nil || c.State != "waiting" {
		j.mu.Unlock()
		return errors.New("nobody asked for that page")
	}
	if b.Error != "" || len(b.Links) == 0 {
		c.State, c.Error = "failed", b.Error
		if c.Error == "" {
			c.Error = "empty"
		}
		j.mu.Unlock()
		return nil
	}
	c.State, c.Links = "done", len(b.Links)
	if j.Cookies == nil {
		j.Cookies = map[string]string{}
	}
	for h, ck := range b.Cookies { // (for downloading the files, each to its own host)
		if ck != "" && len(j.Cookies) < 200 {
			j.Cookies[h] = ck
		}
	}
	page, depth := c.Page, c.Depth
	j.mu.Unlock()

	if len(b.Links) > 5000 {
		b.Links = b.Links[:5000]
	}
	tmp := g.newCrawler(j) // (works out what each link is; the scan itself takes the result)
	for _, l := range b.Links {
		if u, err := url.Parse(l.URL); err == nil && u.Host != "" {
			tmp.offer(u, page, depth)
		}
	}
	tmp.mu.Lock()
	work := tmp.queue
	tmp.mu.Unlock()
	return g.addWork(j, work)
}

// addWork gives a scan more to do: to its running crawl, or, if that has ended, by resuming it.
func (g *Grabber) addWork(j *GrabJob, items []crawlItem) error {
	if len(items) == 0 {
		return nil
	}
	j.mu.Lock()
	cr := j.scan.cr
	j.mu.Unlock()
	if cr != nil && cr.push(items...) {
		return nil
	}
	j.mu.Lock()
	j.scan.left = append(j.scan.left, items...)
	j.mu.Unlock()
	_, err := g.Resume(j.ID)
	return err
}
