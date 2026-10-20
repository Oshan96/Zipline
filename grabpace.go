package main

import (
	"context"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

// Politeness for the site grabber, shared by every scan: each server gets one request at a
// time per "turn", turns are spaced by the longer of the user's wait and the site's Crawl-delay
// (with a little jitter, so a crawl doesn't tick like a clock), at most grabHostParallel
// requests are on their way to one server whatever the number of workers, and a server that
// says 429 or 503 is left alone for as long as it asks while the other servers carry on.

var (
	grabHostParallel  = 2
	grabWaitDefault   = 250 * time.Millisecond // the user's wait when none is set
	grabWaitFloor     = 50 * time.Millisecond  // …and the least it can be
	grabCrawlDelayMax = time.Minute            // a Crawl-delay longer than this is taken as this
	grabBackoffMax    = 10 * time.Minute       // the longest a server's Retry-After is obeyed
	grabBackoffBase   = 5 * time.Second        // the first wait after 429/503 without a Retry-After; doubles
)

type hostPace struct {
	mu    sync.Mutex
	hosts map[string]*paceHost
}

type paceHost struct {
	last    time.Time     // when the latest turn starts
	next    time.Time     // the earliest start of the next turn
	until   time.Time     // the server asked to be left alone until then
	busy    int           // tickets held
	crawl   time.Duration // Crawl-delay from robots.txt
	strikes int           // 429/503 answers in a row
}

func newHostPace() *hostPace { return &hostPace{hosts: map[string]*paceHost{}} }

func (p *hostPace) host(k string) *paceHost {
	h := p.hosts[k]
	if h == nil {
		h = &paceHost{}
		p.hosts[k] = h
	}
	return h
}

// gap is the time between two turns: the site's Crawl-delay is a minimum (jitter only adds to
// it); the user's wait varies by a fifth either way.
func gapFor(wait, crawl time.Duration, r float64) time.Duration {
	if crawl >= wait {
		return crawl + time.Duration(float64(crawl)*0.2*r)
	}
	return time.Duration(float64(wait) * (0.8 + 0.4*r))
}

// paceTicket is the right to have one request at a time on its way to a server.
type paceTicket struct {
	p        *hostPace
	host     string
	released bool
}

// try takes a ticket for host if it has one free and isn't being left alone; otherwise the
// time to look again (zero: whenever a ticket comes back).
func (p *hostPace) try(host string) (*paceTicket, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h := p.host(host)
	if now := time.Now(); now.Before(h.until) {
		return nil, h.until
	}
	if h.busy >= grabHostParallel {
		return nil, time.Time{}
	}
	h.busy++
	return &paceTicket{p: p, host: host}, time.Time{}
}

// turn waits for this ticket's next request to be due and books the turn after it. False when
// ctx ended first.
func (t *paceTicket) turn(ctx context.Context, wait time.Duration) bool {
	p := t.p
	for {
		p.mu.Lock()
		h := p.host(t.host)
		start := time.Now()
		if h.next.After(start) {
			start = h.next
		}
		if h.until.After(start) {
			start = h.until
		}
		gap := gapFor(wait, h.crawl, rand.Float64())
		h.last, h.next = start, start.Add(gap)
		p.mu.Unlock()
		if !sleepCtx(ctx, time.Until(start)) {
			return false
		}
		p.mu.Lock()
		late := time.Now().Before(h.until) // asked to leave it alone while we waited
		p.mu.Unlock()
		if !late {
			return true
		}
	}
}

func (t *paceTicket) done() {
	if t == nil || t.released {
		return
	}
	t.released = true
	t.p.mu.Lock()
	t.p.host(t.host).busy--
	t.p.mu.Unlock()
}

// slowDown records a 429 or 503 from host: nothing more goes there for as long as the reply's
// Retry-After says (at most ten minutes), or for a time that doubles with each one in a row.
func (p *hostPace) slowDown(host string, h http.Header) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	ph := p.host(host)
	d := grabBackoffBase << min(ph.strikes, 8)
	if v := h.Get("Retry-After"); v != "" {
		d = retryAfter(v)
	}
	d = min(d, grabBackoffMax)
	ph.strikes++
	if t := time.Now().Add(d); t.After(ph.until) {
		ph.until = t
	}
	return d
}

// served: host answered normally.
func (p *hostPace) served(host string) {
	p.mu.Lock()
	p.host(host).strikes = 0
	p.mu.Unlock()
}

// setCrawl notes the Crawl-delay the host asks for; it counts from the latest request, which
// was often the one that read it.
func (p *hostPace) setCrawl(host string, d time.Duration) {
	p.mu.Lock()
	h := p.host(host)
	h.crawl = min(d, grabCrawlDelayMax)
	if t := h.last.Add(h.crawl); t.After(h.next) {
		h.next = t
	}
	p.mu.Unlock()
}

// crawlDelay is the Crawl-delay the host's robots.txt asked for.
func (p *hostPace) crawlDelay(host string) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.host(host).crawl
}

// leftAlone is until when host has asked for quiet (zero: it hasn't).
func (p *hostPace) leftAlone(host string) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h := p.hosts[host]; h != nil && time.Now().Before(h.until) {
		return h.until
	}
	return time.Time{}
}
