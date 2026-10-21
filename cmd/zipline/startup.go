package main

import (
	"slices"
	"time"
)

// Picking up where you left off: downloads that were running (or waiting for a free slot), queues
// that were running and finished torrents that were sharing carry on when Zipline starts again,
// whether it was closed or it crashed (Options → General → When Zipline starts). Zipline first
// finds out whether it's on a metered connection, then counts down a few seconds so the window can
// cancel, then starts them the usual way, so "downloads at once", the queues' limits, "at most so
// many sharing at once" and the traffic profile's share switch apply. A torrent that carries on
// sharing does so as Share again does (treshare.go): its files are looked at first, and its ratio
// and time count from now.

// ResumeState is saved with the settings: the queues running at the last save, or when the
// exit began.
type ResumeState struct {
	Queues []ResumeQueue `json:"queues,omitempty"`
	At     time.Time     `json:"at"`
}

type ResumeQueue struct {
	ID      string `json:"id"`
	BySched bool   `json:"bySched,omitempty"` // its schedule started it ("when finished" still runs)
}

// resumePlan is what start-up carries on with.
type resumePlan struct {
	ids    []string
	shares []string // finished torrents that were sharing
	queues []ResumeQueue
	since  time.Time // they were last seen running then
	at     time.Time // carries on then; zero = waiting for the metered-connection check
}

// resumeDelay is the countdown before carrying on (tests shorten it).
var resumeDelay = 10 * time.Second

// wantsResume: d should carry on next time. Downloads a running queue runs come back with
// their queue. Caller holds m.mu.
func (m *Manager) wantsResume(d *Download) bool {
	if d.Provisional {
		return false
	}
	if d.Status == StCompleted { // only a torrent that is sharing (or waits for a normal connection to)
		return d.Torrent != nil && (m.torrents[d.ID] != nil || m.meterPaused[d.ID] || m.sx.resume != nil && slices.Contains(m.sx.resume.shares, d.ID))
	}
	if p := m.sx.resume; p != nil && slices.Contains(p.ids, d.ID) {
		return true // (not started yet)
	}
	return (d.running || d.Waiting || m.meterPaused[d.ID]) && !(d.Queue != "" && m.qRun[d.Queue])
}

// noteResume keeps d's Resume flag current for the next save. Caller holds m.mu.
func (m *Manager) noteResume(d *Download) {
	if m.sx.frozen == nil {
		d.Resume = m.wantsResume(d)
	}
}

// resumeState is what the settings save: the queues running now, or when the exit began.
// Caller holds m.mu.
func (m *Manager) resumeState(now time.Time) *ResumeState {
	if m.sx.frozen != nil {
		return m.sx.frozen
	}
	st := &ResumeState{At: now}
	for _, q := range m.settings.Queues {
		if m.qRun[q.ID] && q.ID != planQueueID { // (the free hours queue follows its window)
			st.Queues = append(st.Queues, ResumeQueue{q.ID, m.qBySched[q.ID]})
		}
	}
	if p := m.sx.resume; p != nil { // still to carry on from last time
		st.Queues = append(st.Queues, p.queues...)
		st.At = p.since
	}
	return st
}

// rememberRunning records what runs before the exit pauses everything. Called by main.
func (m *Manager) rememberRunning() {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.resumeState(time.Now())
	for _, d := range m.list {
		d.Resume = m.wantsResume(d)
	}
	m.sx.frozen = st
}

// takeResumePlan picks up what was running last time (called once at start-up). False when
// there's nothing to carry on with, or the user starts with everything paused.
func (m *Manager) takeResumePlan() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := &resumePlan{}
	if rs := m.settings.Resume; rs != nil {
		p.since = rs.At
		for _, q := range rs.Queues {
			if m.queueByID(q.ID) != nil && !slices.ContainsFunc(p.queues, func(x ResumeQueue) bool { return x.ID == q.ID }) {
				p.queues = append(p.queues, q)
			}
		}
	}
	m.settings.Resume = nil
	for _, d := range m.list {
		if d.Resume && d.Status != StCompleted {
			p.ids = append(p.ids, d.ID)
		} else if d.Resume && d.Torrent != nil {
			p.shares = append(p.shares, d.ID)
		}
		d.Resume = false
	}
	if m.settings.StartPaused || len(p.ids)+len(p.shares)+len(p.queues) == 0 {
		return false
	}
	m.sx.resume = p
	return true
}

// resumeAtStart waits for the metered-connection check, counts down, then carries on.
func (m *Manager) resumeAtStart() {
	select {
	case <-m.meterReady():
	case <-m.quit:
		return
	}
	m.mu.Lock()
	if p := m.sx.resume; p != nil {
		p.at = time.Now().Add(resumeDelay)
	}
	m.mu.Unlock()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-m.quit:
			return
		case <-t.C:
		}
		m.mu.Lock()
		p := m.sx.resume
		due := p != nil && !time.Now().Before(p.at)
		m.mu.Unlock()
		if p == nil {
			logf("start-up: carrying on cancelled")
			return
		}
		if due {
			m.resumeNow()
			return
		}
	}
}

// resumeNow starts what was running last time. On a metered connection the downloads and the
// sharing wait for the normal one, as if it had paused them; a queue whose stop time passed while
// Zipline was closed stays stopped. No more torrents start sharing than "at most so many at once"
// allows: the ones that finished last. Under a traffic profile that doesn't share, they start but
// upload nothing until the profile changes, as a torrent that finishes then does (gate.go).
func (m *Manager) resumeNow() {
	now := time.Now()
	m.mu.Lock()
	p := m.sx.resume
	m.sx.resume = nil
	if p == nil {
		m.mu.Unlock()
		return
	}
	var ids []string
	for _, id := range p.ids {
		if d := m.byID[id]; d != nil && !d.running && d.Status != StCompleted {
			ids = append(ids, id)
		}
	}
	var share []string
	for _, id := range p.shares {
		if d := m.byID[id]; d != nil && !d.running && d.Status == StCompleted && m.torrents[id] == nil {
			share = append(share, id)
		}
	}
	if max := m.settings.Torrent.MaxSeeds; max > 0 && len(share) > max {
		slices.SortFunc(share, func(a, b string) int { return m.byID[b].Finished.Compare(m.byID[a].Finished) })
		share = share[:max]
	}
	if m.metered {
		for _, id := range slices.Concat(ids, share) {
			m.meterPaused[id] = true
		}
		ids, share = nil, nil
	}
	var qs []ResumeQueue
	for _, rq := range p.queues {
		q := m.queueByID(rq.ID)
		if q == nil || m.qRun[rq.ID] {
			continue
		}
		if e, ok := q.Sched.lastStop(now); ok && e.After(p.since) {
			continue // its window ended meanwhile
		}
		qs = append(qs, rq)
	}
	note := ""
	if m.metered {
		note = " (on a metered connection: the downloads wait for the normal one)"
	}
	m.mu.Unlock()
	logf("start-up: carrying on with %s, %s and %s%s", plural(len(p.ids), "download"), plural(len(p.shares), "sharing torrent"), plural(len(qs), "queue"), note)
	m.Start(slices.Concat(ids, share))
	for _, q := range qs {
		m.StartQueue(q.ID, q.BySched)
	}
}
