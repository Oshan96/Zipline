package main

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Tracker etiquette. Trackers are tried the way BEP 12 asks: they come in tiers, each tier is
// shuffled once (when the torrent is first added), the trackers of a tier are tried in order and
// the one that answers goes to the front of its tier, and the next tier is only tried when every
// tracker of the tier before it failed. A failing tracker is left alone for longer each time (BEP 31's
// "retry in" and the tracker's min interval are kept to), a tracker id is sent back, one random key
// per torrent is sent with every announce, and a tracker's warnings and refusals are shown in words.

const (
	trackerSlow = 10 * time.Second // a tracker still silent after this long: the next one may try too
	maxTrackers = 100              // in one torrent
	refusedText = "This tracker doesn't accept Zipline (client not whitelisted)"
)

// ---------------------------------------------------------------- tiers

func flattenTiers(tiers [][]string) []string {
	var out []string
	for _, tier := range tiers {
		out = append(out, tier...)
	}
	return out
}

func cloneTiers(tiers [][]string) [][]string {
	out := make([][]string, len(tiers))
	for i, tier := range tiers {
		out[i] = slices.Clone(tier)
	}
	return out
}

// cleanTiers checks the tiers someone typed: no blanks or repeats, no empty tiers, every address one
// a tracker can have.
func cleanTiers(in [][]string) ([][]string, error) {
	var out [][]string
	seen := map[string]bool{}
	n := 0
	for _, tier := range in {
		var keep []string
		for _, u := range tier {
			if u = strings.TrimSpace(u); u == "" || seen[u] {
				continue
			}
			if !validTracker(u) {
				return nil, fmt.Errorf("%q isn't a tracker address (it starts with http://, https:// or udp://)", oneLine(u, 60))
			}
			if n++; n > maxTrackers {
				return nil, fmt.Errorf("a torrent can have at most %d trackers", maxTrackers)
			}
			seen[u] = true
			keep = append(keep, u)
		}
		if len(keep) > 0 {
			out = append(out, keep)
		}
	}
	return out, nil
}

// initialTiers lays out a new torrent's trackers: the tiers of its torrent file (when there is one),
// then the addresses from a magnet link as one more tier, each shuffled once. This is the only place
// the order is made up; after it, the order is the one the trackers earned by answering.
func initialTiers(extra []string, mi *Metainfo) [][]string {
	var tiers [][]string
	seen := map[string]bool{}
	add := func(list []string) {
		var tier []string
		for _, u := range list {
			if u != "" && !seen[u] {
				seen[u] = true
				tier = append(tier, u)
			}
		}
		rand.Shuffle(len(tier), func(i, j int) { tier[i], tier[j] = tier[j], tier[i] })
		if len(tier) > 0 {
			tiers = append(tiers, tier)
		}
	}
	if mi != nil {
		for _, tier := range mi.Trackers {
			add(tier)
		}
	}
	add(extra)
	return tiers
}

// setTiers replaces a torrent's trackers; Trackers (the flat list the magnet link is made from) follows.
func (ts *TorrentState) setTiers(tiers [][]string) {
	ts.Tiers = tiers
	ts.Trackers = flattenTiers(tiers)
}

// trackerTiersFor is the tiers a torrent starts with. A torrent saved before tiers existed gets them
// now, from its torrent file and the addresses it had. Caller holds m.mu.
func (m *Manager) trackerTiersFor(ts *TorrentState) [][]string {
	if len(ts.Tiers) == 0 && len(ts.Trackers) > 0 {
		var mi *Metainfo
		if ih, ok := ts.hash(); ok && ts.HasMeta {
			mi = m.loadTorrent(ih)
		}
		ts.setTiers(initialTiers(ts.Trackers, mi))
		m.dirty = true
	}
	return cloneTiers(ts.Tiers)
}

// newTrackerKey is the random number a torrent sends with every announce (never 0: that means none).
func newTrackerKey() uint32 {
	for {
		var b [4]byte
		crand.Read(b[:])
		if k := binary.BigEndian.Uint32(b[:]); k != 0 {
			return k
		}
	}
}

// trackerKey is this torrent's key, made on first use and kept in its saved state.
func (t *torrent) trackerKey() uint32 {
	t.m.mu.Lock()
	defer t.m.mu.Unlock()
	ts := t.d.Torrent
	if ts == nil {
		return 0
	}
	if ts.Key == 0 {
		ts.Key = newTrackerKey()
		t.m.dirty = true
	}
	return ts.Key
}

// ---------------------------------------------------------------- the trackers of a running torrent

// probing: this engine only fetches a magnet link's file list for the Add dialog. It asks every
// tracker at once, as it's gone in a minute and someone is waiting.
func (t *torrent) probing() bool { return strings.HasPrefix(t.d.ID, "meta:") }

// applyTiers sets the torrent's trackers to these tiers, keeping what's known about the ones that stay.
// It returns the trackers that went. Caller holds t.mu.
func (t *torrent) applyTiers(tiers [][]string) (dropped []*trackerState) {
	old := map[string]*trackerState{}
	for _, tr := range t.trackers {
		old[tr.url] = tr
	}
	var list []*trackerState
	for i, tier := range tiers {
		for _, u := range tier {
			tr := old[u]
			if tr == nil {
				tr = &trackerState{url: u, status: "Waiting"}
			}
			delete(old, u)
			tr.tier = i
			list = append(list, tr)
		}
	}
	for _, tr := range old {
		dropped = append(dropped, tr)
	}
	t.trackers = list // (a new slice: others may still be reading the old one)
	return dropped
}

// tierURLs is the trackers as tiers, in the order they're tried. Caller holds t.mu.
func (t *torrent) tierURLs() [][]string {
	var tiers [][]string
	for _, tr := range t.trackers {
		if tr.tier >= len(tiers) {
			tiers = append(tiers, nil)
		}
		tiers[tr.tier] = append(tiers[tr.tier], tr.url)
	}
	return tiers
}

// trackerTell is what a "stopped" needs to know about a tracker.
type trackerTell struct{ url, id string }

// startedTrackers lists the trackers that have heard from us (and should hear we've gone). Caller holds t.mu.
func startedTrackers(list []*trackerState) []trackerTell {
	var out []trackerTell
	for _, tr := range list {
		if tr.started {
			out = append(out, trackerTell{tr.url, tr.id})
		}
	}
	return out
}

// tellStopped says goodbye to trackers, in the background.
func (t *torrent) tellStopped(list []trackerTell, req announceReq) {
	if len(list) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, tr := range list {
			r := req
			r.TrackerID = tr.id
			announce(ctx, tr.url, r)
		}
	}()
}

// reloadTiers sets the running trackers to the saved ones (as the torrent starts).
func (t *torrent) reloadTiers() {
	if t.probing() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m.mu.Lock()
	var tiers [][]string
	if ts := t.d.Torrent; ts != nil {
		tiers = t.m.trackerTiersFor(ts)
	}
	t.m.mu.Unlock()
	t.applyTiers(tiers)
}

// SetTorrentTrackers replaces a torrent's trackers (tiers of addresses) for the running torrent and
// for next time. The trackers that went hear that we've left.
func (m *Manager) SetTorrentTrackers(id string, in [][]string) ([][]string, error) {
	tiers, err := cleanTiers(in)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	d, t := m.byID[id], m.torrents[id]
	if d == nil || d.Torrent == nil {
		m.mu.Unlock()
		return nil, errors.New("not a torrent")
	}
	if t == nil {
		d.Torrent.setTiers(tiers)
		m.dirty = true
		m.mu.Unlock()
		return tiers, nil
	}
	m.mu.Unlock()
	t.mu.Lock() // (the saved order is only written with t.mu held, so an answer arriving now can't put the old one back)
	var gone []trackerTell
	var req announceReq
	if !t.probing() {
		gone = startedTrackers(t.applyTiers(tiers))
		req = announceReq{InfoHash: t.ih, PeerID: t.peerID, Port: t.lport(), Uploaded: atomic.LoadInt64(&t.uploaded),
			Downloaded: atomic.LoadInt64(&t.downloaded), Left: t.wantedLeft(), Event: "stopped", Key: t.trackerKey()}
	}
	m.mu.Lock()
	d.Torrent.setTiers(tiers)
	m.dirty = true
	m.mu.Unlock()
	t.mu.Unlock()
	t.tellStopped(gone, req)
	return tiers, nil
}

// ---------------------------------------------------------------- asking

// trackerLoop asks for peers: the tracker in use when it's due, else the next one that may be tried.
func (t *torrent) trackerLoop() {
	t.reloadTiers() // (an edit made while this run was starting up)
	for {
		t.mu.Lock()
		now := time.Now()
		due := t.dueTrackers(now)
		reqs := make([]announceReq, len(due))
		for i, tr := range due {
			reqs[i] = t.announceReqFor(tr)
			tr.asking, tr.status, tr.code, tr.last = true, "Asking…", "asking", now
		}
		t.mu.Unlock()
		for i, tr := range due {
			go t.announceTo(tr, reqs[i])
		}
		select {
		case <-t.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// dueTrackers picks the trackers to ask now. Caller holds t.mu.
func (t *torrent) dueTrackers(now time.Time) []*trackerState {
	if t.probing() {
		var all []*trackerState
		for _, tr := range t.trackers {
			if !tr.asking && !now.Before(tr.retryAt) && t.trackerDue(tr, now) {
				all = append(all, tr)
			}
		}
		return all
	}
	// the first tracker, in tier order, that isn't failing is the one in use
	for _, tr := range t.trackers {
		switch {
		case tr.asking && (tr.started && tr.fails == 0 || now.Sub(tr.last) < trackerSlow):
			return nil // waiting for its answer
		case tr.asking, now.Before(tr.retryAt):
			continue // too slow to wait for (the next one tries as well), or backing off
		case t.trackerDue(tr, now):
			return []*trackerState{tr}
		}
		return nil // in use, and not due yet
	}
	return nil
}

// trackerDue: it's time to ask this tracker again. Short of peers it's asked sooner, but never
// before the tracker's min interval (at least 2 minutes with no peers at all, 5 with a few).
// Caller holds t.mu.
func (t *torrent) trackerDue(tr *trackerState, now time.Time) bool {
	gap := now.Sub(tr.last)
	few := tr.started && (len(t.peers) == 0 && gap > max(tr.minGap, 2*time.Minute) || len(t.peers) < 8 && gap > max(tr.minGap, 5*time.Minute))
	return now.After(tr.next) || few
}

// announceReqFor is what to tell a tracker now. Caller holds t.mu.
func (t *torrent) announceReqFor(tr *trackerState) announceReq {
	ev := ""
	if !tr.started {
		ev = "started"
	} else if t.finishedHere && !tr.sentDone { // BEP 3: once, and only if it finished in this run
		ev = "completed"
	}
	return announceReq{InfoHash: t.ih, PeerID: t.peerID, Port: t.lport(), Uploaded: atomic.LoadInt64(&t.uploaded),
		Downloaded: atomic.LoadInt64(&t.downloaded), Left: t.wantedLeft(), Event: ev, NumWant: 200, Key: t.trackerKey(), TrackerID: tr.id}
}

func (t *torrent) announceTo(tr *trackerState, req announceReq) {
	ctx, cancel := context.WithTimeout(t.ctx, 30*time.Second)
	r, err := announce(ctx, tr.url, req)
	cancel()
	t.mu.Lock()
	tr.asking = false
	if err != nil {
		t.trackerFailed(tr, err, time.Now())
		t.mu.Unlock()
		return
	}
	if t.trackerWorked(tr, req, r, time.Now()) {
		t.saveTiers()
	}
	t.mu.Unlock()
	t.addCands(r.Peers, "tracker")
}

// trackerWorked records an answer. It reports whether the tracker moved up in its tier. Caller holds t.mu.
func (t *torrent) trackerWorked(tr *trackerState, req announceReq, r *announceResp, now time.Time) bool {
	tr.fails, tr.started, tr.retryAt = 0, true, time.Time{}
	if req.Event == "completed" {
		tr.sentDone = true
	}
	tr.minGap = min(r.MinInterval, time.Hour)
	if r.TrackerID != "" {
		tr.id = r.TrackerID
	}
	tr.interval = max(min(r.Interval, time.Hour), 2*time.Minute, tr.minGap)
	tr.next = now.Add(tr.interval)
	if req.Event == "started" && t.finishedHere && !tr.sentDone {
		tr.next = now // it finished while this tracker was being found: say so now
	}
	tr.peers, tr.seeders, tr.leechers = len(r.Peers), r.Seeders, r.Leechers
	tr.status, tr.code, tr.arg, tr.warning, tr.detail = "Working", "working", "", oneLine(r.Warning, 200), ""
	return t.promote(tr)
}

// trackerFailed records a failure and when to try again. Caller holds t.mu.
func (t *torrent) trackerFailed(tr *trackerState, err error, now time.Time) {
	tr.fails++
	tr.code, tr.arg = trackerErrCode(err)
	tr.status, tr.warning, tr.detail = trackerStatusText(tr.code, tr.arg), "", ""
	wait := trackerBackoff(tr.fails, tr.minGap)
	if tf := failureOf(err); tf != nil {
		switch {
		case clientRefused(tf.Reason):
			tr.detail = tr.arg // (the status says it in other words)
			wait = time.Hour   // trying again won't change its mind
		case tf.Never:
			wait = 24 * time.Hour
		case tf.RetryIn > 0:
			wait = max(wait, min(tf.RetryIn, 24*time.Hour))
		}
	}
	tr.retryAt = now.Add(wait)
	tr.next = tr.retryAt
}

// trackerBackoff is how long a tracker that failed this many times in a row is left alone: a minute
// times the square of the failures, an hour at most, and never less than the tracker's min interval.
func trackerBackoff(fails int, minGap time.Duration) time.Duration {
	f := min(fails, 60)
	return min(max(time.Duration(f*f)*time.Minute, minGap), time.Hour)
}

// promote moves a tracker that answered to the front of its tier. Caller holds t.mu.
func (t *torrent) promote(tr *trackerState) bool {
	i := slices.Index(t.trackers, tr)
	first := i
	for first > 0 && t.trackers[first-1].tier == tr.tier {
		first--
	}
	if i < 0 || first == i {
		return false
	}
	list := make([]*trackerState, 0, len(t.trackers))
	list = append(list, t.trackers[:first]...)
	list = append(list, tr)
	list = append(list, t.trackers[first:i]...)
	list = append(list, t.trackers[i+1:]...)
	t.trackers = list // (a new slice: others may still be reading the old one)
	return true
}

// saveTiers keeps the order the trackers earned in the saved torrent. Caller holds t.mu.
func (t *torrent) saveTiers() {
	if t.probing() {
		return
	}
	tiers := t.tierURLs()
	t.m.mu.Lock()
	if ts := t.d.Torrent; ts != nil {
		ts.setTiers(tiers)
		t.m.dirty = true
	}
	t.m.mu.Unlock()
}

// trackerViews describes the trackers for the window. Caller holds t.mu.
func (t *torrent) trackerViews(now time.Time) []TrackerView {
	active := slices.IndexFunc(t.trackers, func(tr *trackerState) bool { return tr.started && tr.fails == 0 })
	out := make([]TrackerView, 0, len(t.trackers))
	for i, tr := range t.trackers {
		v := TrackerView{URL: trackerName(tr.url), Announce: tr.url, Tier: tr.tier, Status: tr.status, State: tr.code, Arg: tr.arg, Warning: tr.warning,
			Detail: tr.detail, Peers: tr.peers, Seeders: tr.seeders, Leechers: tr.leechers}
		if v.State == "" {
			v.State = "waiting"
		}
		if tr.fails > 0 && now.Before(tr.retryAt) {
			v.Fails, v.RetryIn = tr.fails, int(tr.retryAt.Sub(now)/time.Second)+1
		}
		if !t.probing() && active >= 0 && i > active && !tr.started && !tr.asking && tr.fails == 0 {
			v.Status, v.State = "Standby", "standby" // not needed while the one before it works
		}
		out = append(out, v)
	}
	return out
}

// ---------------------------------------------------------------- what a tracker said

// trackerFailure is a tracker's "failure reason" (BEP 3), with BEP 31's "retry in" when it gave one.
type trackerFailure struct {
	Reason  string
	RetryIn time.Duration
	Never   bool
}

func (e *trackerFailure) Error() string { return "tracker: " + e.Reason }

// failureOf: the tracker's failure reason in this error, if it is one (a UDP tracker's comes as text).
func failureOf(err error) *trackerFailure {
	var tf *trackerFailure
	if errors.As(err, &tf) {
		return tf
	}
	if r, ok := strings.CutPrefix(err.Error(), "tracker: "); ok {
		return &trackerFailure{Reason: r}
	}
	return nil
}

func newTrackerFailure(d map[string]any, reason string) *trackerFailure {
	f := &trackerFailure{Reason: reason}
	if n, ok := bint(d, "retry in"); ok && n > 0 {
		f.RetryIn = time.Duration(n) * time.Minute
	} else if strings.EqualFold(bstr(d, "retry in"), "never") {
		f.Never = true
	}
	return f
}

// clientRefused: the failure reason says this program isn't allowed on the tracker (private
// trackers keep a list of clients).
// trackerErrCode says in a word why a tracker didn't answer, and gives the tracker's own words when it said any.
func trackerErrCode(err error) (code, arg string) {
	var dns *net.DNSError
	var ne net.Error
	msg := err.Error()
	switch {
	case errors.As(err, &dns):
		return "nodns", ""
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout():
		return "noanswer", ""
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "actively refused"):
		return "noconn", ""
	}
	if tf := failureOf(err); tf != nil {
		if clientRefused(tf.Reason) {
			return "refused", oneLine(tf.Reason, 200)
		}
		return "says", oneLine(tf.Reason, 200)
	}
	return "error", shortErr(err)
}

// trackerStatusText puts that in plain words (the window has its own, in its language).
func trackerStatusText(code, arg string) string {
	switch code {
	case "nodns":
		return "Couldn't find the tracker"
	case "noanswer":
		return "No answer"
	case "noconn":
		return "Couldn't connect"
	case "refused":
		return refusedText
	case "says":
		return "The tracker says: " + arg
	}
	return "Error: " + arg
}

func trackerErrText(err error) string { return trackerStatusText(trackerErrCode(err)) }

func clientRefused(reason string) bool {
	s := strings.ToLower(reason)
	for _, w := range []string{"whitelist", "white list", "white-list", "blacklist", "black list", "black-list"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	if !strings.Contains(s, "client") {
		return false
	}
	for _, w := range []string{"not allowed", "not permitted", "not supported", "not accepted", "not approved", "not recognized", "not recognised",
		"not registered", "banned", "blocked", "rejected", "forbidden", "denied", "unsupported", "unauthorized", "unauthorised", "invalid", "unknown"} {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// oneLine makes a tracker's words fit a line of the window: runs of white space become one space,
// and a long text is cut (at a letter, not in the middle of one).
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}
