package main

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"
)

// Expired links. Many sites hand out download links that only work for a while. When one
// runs out part-way, Zipline gets a fresh link and carries on from where it stopped:
//  1. a signed address behind a redirect is renewed through the address it came from
//     (sources.go) — nothing to do for anyone;
//  2. otherwise the browser extension opens the page the download came from in a
//     background tab and picks out the same file (same size and name);
//  3. and if that doesn't work, clicking the download link again (or pasting it) is
//     recognised: the new link goes to the waiting download instead of starting a new one.

type refreshJob struct {
	ID    string `json:"id"`
	Page  string `json:"page"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	added time.Time
	taken time.Time
}

// openPage shows a web page in the default browser (replaced in tests).
var openPage = openPath

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return strings.TrimPrefix(u.Hostname(), "www.")
	}
	return raw
}

func pageOf(d *Download) string {
	p := d.Headers["Referer"]
	if p == "" { // (most have none: skip parsing, the window's list asks for this often)
		return ""
	}
	if u, err := url.Parse(p); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return p
}

// extActive: the browser extension has checked in lately (it does every 30 s while the
// browser is open). Caller holds m.mu.
func (m *Manager) extActive() bool { return time.Since(m.extSeen) < 2*time.Minute }

// linkExpired handles a download whose link ran out. Caller holds m.mu.
func (m *Manager) linkExpired(d *Download) {
	d.Status = StError
	page := pageOf(d)
	if m.settings.AutoRefresh && page != "" && m.extActive() && d.linkTries < 2 && (d.Kind == "" || d.Kind == "hls") && !d.Private { // (the page would open in a normal window)
		d.linkTries++
		d.Link = "refreshing"
		d.Error = "The link expired — getting a fresh one from " + hostOf(page)
		m.refresh[d.ID] = &refreshJob{ID: d.ID, Page: page, Name: refreshName(d), Size: max(d.Size, 0), added: time.Now()}
		m.dirty = true
		return
	}
	m.linkNeeded(d)
}

// linkNeeded: Zipline couldn't renew the link by itself. Clicking it again picks it up.
// Caller holds m.mu.
func (m *Manager) linkNeeded(d *Download) {
	delete(m.refresh, d.ID)
	d.Link = "expired"
	if page := pageOf(d); page != "" {
		d.Error = "The link expired. Open " + hostOf(page) + " and click the download link again — Zipline picks it up and carries on"
	} else {
		d.Error = "The link expired. Paste a new link for this file in Properties, or add it again — Zipline recognises it and carries on"
	}
	m.dirty = true
	m.notify("error", d.ID)
	if !d.Provisional {
		m.playEvent("failed")
	}
}

// sameDownload: is the file at a new link the one d was downloading?
func sameDownload(d *Download, pr *probeResult, link string) bool {
	if d.Size <= 0 || pr.size != d.Size || !pr.resumable {
		return false
	}
	if pr.etag != "" && pr.etag == d.ETag {
		return true
	}
	lp := link
	if u, err := url.Parse(link); err == nil {
		lp = u.Path
	}
	return sameFileName(pr.name, d.FileName) || sameFileName(path.Base(lp), d.FileName)
}

// renewLink switches d to a fresh link and carries on. Caller holds m.mu.
func (m *Manager) renewLink(d *Download, link string, headers map[string]string, pr *probeResult) {
	d.URL, d.FinalURL = link, pr.finalURL
	d.Method, d.Body = "", nil // (a fresh link is fetched plainly)
	if d.Headers == nil {
		d.Headers = map[string]string{}
	}
	for k, v := range headers {
		if v != "" {
			d.Headers[k] = v
		}
	}
	// requests go to the new link from now on: "only this version" uses what it says (another
	// server of the same site may tag the file differently, or not at all)
	d.ETag, d.LastMod, d.LMStrong = pr.etag, pr.lastMod, pr.lmStrong
	d.Link, d.Error, d.linkTries, d.linkWait = "", "", 0, time.Time{}
	delete(m.refresh, d.ID)
	m.dirty = true
	m.notify("linkRenewed", d.ID, "Got a fresh link for "+iso(d.FileName)+" — carrying on")
	if !d.running && d.Status != StCompleted {
		m.startLocked(d)
	}
}

type linkCand struct {
	URL     string `json:"url"`
	Referer string `json:"referer"`
	Cookie  string `json:"cookie"`
}

func candHeaders(c linkCand, ua string) map[string]string {
	h := map[string]string{}
	if c.Referer != "" {
		h["Referer"] = c.Referer
	}
	if c.Cookie != "" {
		h["Cookie"] = c.Cookie
	}
	if ua != "" {
		h["User-Agent"] = ua
	}
	return h
}

// tryLinks checks candidate links (from the browser) against the waiting downloads and
// renews the first that matches. only limits it to one download ("" = any waiting one).
func (m *Manager) tryLinks(ctx context.Context, cands []linkCand, ua, only string) *Download {
	m.mu.Lock()
	var waiting []*Download
	for _, d := range m.list {
		if (only == "" && d.Link != "" || d.ID == only) && !d.running && (d.Size > 0 && d.Kind == "" || d.Kind == "hls" && !d.Live) {
			waiting = append(waiting, d)
		}
	}
	m.mu.Unlock()
	if len(waiting) == 0 || len(cands) == 0 {
		return nil
	}
	if len(cands) > 25 {
		cands = cands[:25]
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var found *Download
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, c := range cands {
		if !strings.HasPrefix(c.URL, "http://") && !strings.HasPrefix(c.URL, "https://") && !isFTP(c.URL) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			h := candHeaders(c, ua)
			pctx, pcancel := context.WithTimeout(ctx, 12*time.Second)
			defer pcancel()
			via := &Download{URL: c.URL, Headers: h}
			pr, err := probe(pctx, via, c.URL, 0, 0)
			if err != nil {
				return
			}
			pr.discard()
			if looksLikeHLS(c.URL) || isHLSMime(pr.mime) { // a stream's playlist: the same stream?
				for _, d := range waiting {
					if d.Kind != "hls" || !m.sameStream(pctx, d, h, c.URL) {
						continue
					}
					mu.Lock()
					if found == nil {
						found = d
						m.mu.Lock()
						if !d.running {
							m.renewStream(d, c.URL, h)
						}
						m.mu.Unlock()
						cancel()
					}
					mu.Unlock()
					return
				}
				return
			}
			for _, d := range waiting {
				m.mu.Lock()
				ok := !d.running && sameDownload(d, pr, c.URL)
				part := d.partPath()
				m.mu.Unlock()
				// same size and name: the pieces already downloaded must match too
				if !ok || !m.partMatches(pctx, d, via, pr.finalURL, 2, part) {
					continue
				}
				mu.Lock()
				if found == nil {
					found = d
					m.mu.Lock()
					m.renewLink(d, c.URL, h, pr)
					m.mu.Unlock()
					cancel()
				}
				mu.Unlock()
				return
			}
		}()
	}
	wg.Wait()
	return found
}

// hasWaitingLinks: some download is waiting for a fresh link.
func (m *Manager) hasWaitingLinks() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.list {
		if d.Link != "" && !d.running {
			return true
		}
	}
	return false
}

// RefreshLink is "Refresh download link": get a fresh link for a download now.
// It returns "auto" (the browser extension is fetching one), "page" (the page was
// opened: click the link there) or "none" (no page known: paste a new link).
func (m *Manager) RefreshLink(id string) string {
	m.mu.Lock()
	d := m.byID[id]
	if d == nil || d.Status == StCompleted || d.Live || pageOf(d) == "" {
		m.mu.Unlock()
		return "none"
	}
	page, running := pageOf(d), d.running
	m.mu.Unlock()
	if running {
		m.Pause([]string{id})
		m.waitStopped(d)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.extActive() && (d.Size > 0 || d.Kind == "hls") {
		d.linkTries = 1
		d.Link = "refreshing"
		if d.Status != StError {
			d.Status = StError
		}
		d.Error = "Getting a fresh link from " + hostOf(page)
		m.refresh[d.ID] = &refreshJob{ID: d.ID, Page: page, Name: refreshName(d), Size: max(d.Size, 0), added: time.Now()}
		m.dirty = true
		return "auto"
	}
	d.Link, d.linkWait = "waiting", time.Now().Add(15*time.Minute)
	m.dirty = true
	go openPage(page)
	return "page"
}

// takeRefreshJobs hands the extension the pages to open. Caller holds m.mu.
func (m *Manager) takeRefreshJobs() []refreshJob {
	var out []refreshJob
	for _, j := range m.refresh {
		if j.taken.IsZero() {
			j.taken = time.Now()
			out = append(out, *j)
		}
	}
	return out
}

// refreshTick gives up on jobs nobody finished and lets "waiting" links go. Called from the loop.
func (m *Manager) refreshTick(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, j := range m.refresh {
		d := m.byID[id]
		if d == nil || d.Link != "refreshing" {
			delete(m.refresh, id)
			continue
		}
		if !j.taken.IsZero() && now.Sub(j.taken) > 100*time.Second || j.taken.IsZero() && now.Sub(j.added) > 90*time.Second {
			m.linkNeeded(d)
		}
	}
	for _, d := range m.list {
		if d.Link == "waiting" && !d.linkWait.IsZero() && now.After(d.linkWait) && d.Status != StError {
			d.Link, d.linkWait = "", time.Time{}
			m.dirty = true
		}
	}
}

func humanSize(b int64) string {
	if b < 0 {
		return "unknown size"
	}
	u := []string{"B", "KB", "MB", "GB", "TB"}
	v, i := float64(b), 0
	for v >= 1024 && i < len(u)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.1f %s", v, u[i])
}

// waitingMatch finds a stopped download of the same file that's waiting for a fresh link (or
// failed). Caller holds m.mu.
func (m *Manager) waitingMatch(pr *probeResult, link string) *Download {
	for _, d := range m.list {
		if !d.running && d.Kind == "" && !d.Provisional && (d.Link != "" || d.Status == StError) && sameDownload(d, pr, link) {
			return d
		}
	}
	return nil
}
