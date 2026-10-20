package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// The rule gate: one place every start and every byte goes through, whatever started it (the
// window, the tray, the phone, the command line, a schedule, the site grabber or a sync queue).
//
// Starting work: the Free data hours queue runs only inside the free hours (outside them, only
// when you say so in the window); queues wait out a metered connection and a used-up data cap or
// quota; site crawls and sync checks wait for the same.
//
// Every byte: downloads, uploads, video playlists, grabbed pages and sync checks are counted, in
// the rolling meter behind the data cap and in the daily totals per network behind quotas. A
// direction the data cap or a quota holds back waits (bytes in free hours don't count towards
// quotas). Torrent uploads also pause on a metered connection, outside free hours for torrents
// from the Free data hours queue, and while the traffic profile says not to share; they carry on
// when that's over.

type gate struct {
	upUsage  *usageMeter // uploads alone (m.usage counts everything moved)
	days     *dayMeter
	upFile   string
	daysFile string

	freeNow atomic.Bool  // inside the free data hours (their whole window)
	upFree  atomic.Bool  // uploads are free in them too
	mode    atomic.Int32 // what counts: countBoth, countDownOnly or countUpOnly
	net     atomic.Pointer[netInfo]
	capLeft atomic.Int64 // what the rolling data cap still allows
	qLeft   atomic.Int64 // what the tightest quota on this network still allows
	prof    atomic.Pointer[TrafficProfile]
	netBusy atomic.Bool // reading the network's identity

	politeUp *Limiter // polite mode's upload limit (the usual connection)

	// guarded by Manager.mu
	politeUpRate float64 // polite mode's upload limits now (for the window)
	politeLines  []PoliteLineView
	downRate     int64                // the overall download limit last set (speed limiter, profile)
	slow         map[string]*slowMark // torrents downloading under slowRate, since when
	quotas       QuotasView
	netAt        time.Time                    // the network was last read then
	held         bool                         // downloads were held back at the last look
	planRun      time.Time                    // the Free data hours queue's run you started outside its hours (see queueMayStart)
	planIn       bool                         // inside its hours at the last look
	meteredFn    func() (metered, known bool) // stands in for the system's answer (tests)
	slowFor      time.Duration                // see slowAfter (shorter in tests)
	metaUnit     time.Duration                // a "minute" of a magnet link's wait for its file list (shorter in tests)
}

const (
	countBoth int32 = iota
	countDownOnly
	countUpOnly
)

func countMode(s string) int32 {
	switch s {
	case "down":
		return countDownOnly
	case "up":
		return countUpOnly
	}
	return countBoth
}

func newGate(dataDir string) *gate {
	g := &gate{upFile: filepath.Join(dataDir, "usage-up.json"), daysFile: filepath.Join(dataDir, "usage-days.json"),
		politeUp: &Limiter{}, downRate: -1, slow: map[string]*slowMark{},
		slowFor: slowAfter, metaUnit: time.Minute}
	g.upUsage = newUsageMeter(g.upFile)
	g.days = newDayMeter(g.daysFile)
	pc := thisComputer
	g.net.Store(&pc)
	g.capLeft.Store(math.MaxInt64)
	g.qLeft.Store(math.MaxInt64)
	return g
}

func (g *gate) save() {
	if g != nil {
		g.upUsage.save(g.upFile)
		g.days.save(g.daysFile)
	}
}

// ---------------------------------------------------------------- every byte

func (m *Manager) countDown(n int64) { m.countTraffic(n, false) }
func (m *Manager) countUp(n int64)   { m.countTraffic(n, true) }

// countTraffic records n bytes moved over the internet: the rolling meter (data cap, the plan's
// figures) and today's total on this network (quotas), less what the cap and quotas still allow.
func (m *Manager) countTraffic(n int64, up bool) {
	if n <= 0 {
		return
	}
	m.usage.add(n)
	g := m.gate
	if g == nil {
		return
	}
	if up {
		g.upUsage.add(n)
	}
	free := g.freeNow.Load() && (!up || g.upFree.Load())
	ni := g.net.Load()
	g.days.add(ni.key, ni.name, time.Now(), n, up, free)
	if mode := g.mode.Load(); mode == countBoth || mode == countUpOnly && up || mode == countDownOnly && !up {
		g.capLeft.Add(-n)
		if !free {
			g.qLeft.Add(-n)
		}
	}
}

// downHeld: downloads wait now, because the data cap or a quota is used up.
func (g *gate) downHeld() bool {
	if g == nil {
		return false
	}
	if g.mode.Load() == countUpOnly || g.qLeft.Load() > 0 && g.capLeft.Load() > 0 {
		return false
	}
	return g.capLeft.Load() <= 0 || !g.freeNow.Load()
}

// upHeld: uploads wait now, because the data cap or a quota is used up.
func (g *gate) upHeld() bool {
	if g == nil {
		return false
	}
	if g.mode.Load() == countDownOnly || g.qLeft.Load() > 0 && g.capLeft.Load() > 0 {
		return false
	}
	return g.capLeft.Load() <= 0 || !(g.freeNow.Load() && g.upFree.Load())
}

// upWait waits until n bytes may be uploaded: the upload limit (Options → Torrents and the
// traffic profile), then polite mode's.
func (m *Manager) upWait(ctx context.Context, n int) {
	m.upLimiter().Wait(ctx, n)
	if m.gate != nil {
		m.gate.politeUp.Wait(ctx, n)
	}
}

// upCounter counts what a connection sends.
type upCounter struct {
	io.Writer
	m *Manager
}

func (w upCounter) Write(b []byte) (int, error) {
	n, err := w.Writer.Write(b)
	w.m.countUp(int64(n))
	return n, err
}

// ---------------------------------------------------------------- starting work

var errOutsideFree = errors.New("outside free hours")

// FreeHoursError: the Free data hours queue (or a download in it) was asked to start outside the
// free hours by something that can't ask first (the tray, the phone, the command line).
type FreeHoursError struct{ Text string }

func (e *FreeHoursError) Error() string { return e.Text }
func (e *FreeHoursError) Unwrap() error { return errOutsideFree }

// outsideFree says why work in the Free data hours queue shouldn't start now ("" = it may).
// Caller holds m.mu.
func (m *Manager) outsideFree(now time.Time) string {
	p := m.settings.Plan
	if !p.On || m.queueByID(planQueueID) == nil {
		return ""
	}
	if _, _, in := p.windowAt(now); in {
		return ""
	}
	when := "your next free hours"
	if from, to, ok := p.nextWindow(now); ok {
		when = "your free hours (" + from.Format("Mon 15:04") + "–" + to.Format("15:04") + ")"
	}
	return "Free data hours: it's outside your free hours, so starting now would use your normal data. It starts by itself in " + when + ", or start it from the Zipline window if you're sure."
}

// StartQueueChecked starts a queue unless the gate says no: the Free data hours queue outside
// the free hours needs sure (the window asked you first). A metered connection or a used-up
// quota doesn't refuse: the queue starts and waits for them.
func (m *Manager) StartQueueChecked(qid string, sure bool) error {
	m.mu.Lock()
	why := ""
	if qid == planQueueID {
		why = m.outsideFree(time.Now())
	}
	m.mu.Unlock()
	if why != "" && !sure {
		return &FreeHoursError{why}
	}
	m.StartQueue(qid, false)
	if why != "" { // you said yes: this run may use normal data
		m.mu.Lock()
		m.gate.planRun = m.qLastStart[qid]
		m.mu.Unlock()
		m.kick()
	}
	return nil
}

// StartChecked starts downloads by hand unless one of them waits for the free data hours and
// nobody could be asked about using normal data for it (sure is false).
func (m *Manager) StartChecked(ids []string, sure bool) error {
	if !sure {
		m.mu.Lock()
		why := ""
		for _, id := range ids {
			if d := m.byID[id]; d != nil && d.Queue == planQueueID && d.Status != StCompleted {
				why = m.outsideFree(time.Now())
				break
			}
		}
		m.mu.Unlock()
		if why != "" {
			return &FreeHoursError{why}
		}
	}
	m.Start(ids)
	return nil
}

// StartAllQueues starts every queue, as the tray's "Start all queues" does: through the gate, so
// the Free data hours queue doesn't start outside its free hours (the window and the tray say why).
func (m *Manager) StartAllQueues() {
	m.mu.Lock()
	var ids []string
	for _, q := range m.settings.Queues {
		ids = append(ids, q.ID)
	}
	m.mu.Unlock()
	for _, id := range ids {
		if err := m.StartQueueChecked(id, false); err != nil {
			m.mu.Lock()
			m.notify("refused", id, err.Error())
			m.mu.Unlock()
			trayNotify("Free data hours queue not started", err.Error(), false)
		}
	}
}

// queueMayStart: a queue may start more downloads now — not on a metered connection (when the
// option is on), nor while the data cap or a quota holds downloads back, nor the Free data hours
// queue outside its hours (however it was started: a schedule, the grabber, an import), unless
// you started this run of it yourself and said yes. Caller holds m.mu.
func (m *Manager) queueMayStart(qid string) bool {
	if m.metered || m.gate.downHeld() {
		return false
	}
	return qid != planQueueID || m.outsideFree(time.Now()) == "" || m.gate.planRun.Equal(m.qLastStart[qid])
}

// netGateWait holds back background traffic (site crawls, project re-checks, sync checks) while
// the connection is metered (when that option is on) or the data cap or a quota is used up.
// False when ctx ended first.
func (m *Manager) netGateWait(ctx context.Context) bool {
	for {
		m.mu.Lock()
		wait := m.metered || m.gate.downHeld()
		m.mu.Unlock()
		if !wait {
			return ctx.Err() == nil
		}
		if !sleepCtx(ctx, time.Second) {
			return false
		}
	}
}

// ---------------------------------------------------------------- uploads

// uploadHold says why a torrent mustn't upload now ("" = it may): a metered connection (with the
// option on), outside free hours for a torrent from the Free data hours queue (unless uploads
// aren't free in them either, when it makes no difference), or a profile that doesn't share
// finished torrents. Caller holds m.mu.
func (m *Manager) uploadHold(planQ, seeding bool, now time.Time) string {
	p := m.settings.Plan
	switch {
	case m.metered:
		return "metered connection"
	case planQ && p.On && !p.UpPaid && m.queueByID(planQueueID) != nil:
		if _, _, in := p.windowAt(now); !in {
			return "outside free hours"
		}
	}
	if pr := m.gate.prof.Load(); seeding && pr != nil && pr.NoSeed {
		return "traffic profile"
	}
	return ""
}

// isMetered asks whether the connection is metered: the system, or this manager's stand-in.
func (m *Manager) isMetered() (metered, known bool) {
	m.mu.Lock()
	f := m.gate.meteredFn
	m.mu.Unlock()
	if f != nil {
		return f()
	}
	return meteredCheck()
}

// upHeld: this torrent mustn't upload now (see uploadHold, and the data cap and quotas).
func (t *torrent) upHeld() bool {
	return t.upHold.Load() || t.m.gate.upHeld()
}

// ---------------------------------------------------------------- the gate's clock

// slowRate and slowAfter: a torrent downloading slower than this for that long doesn't hold a
// place in its queue (qBittorrent's "slow torrents" rule).
const (
	slowRate  = 2048.0
	slowAfter = 60 * time.Second
)

// slowTorrent: a running torrent that has been downloading under 2 KiB/s for a minute (no seeds,
// or a magnet link still waiting for its file list). It doesn't count towards "downloads at once",
// so the next download starts, and it keeps trying in the background. Caller holds m.mu.
func (m *Manager) slowTorrent(d *Download) bool {
	if m.gate == nil || d.Kind != "torrent" || !d.running {
		return false
	}
	s := m.gate.slow[d.ID]
	return s != nil && time.Since(s.since) >= m.gate.slowFor
}

type slowMark struct {
	since time.Time
	told  bool // its queue was told (kicked)
}

func (m *Manager) gateLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		changed := netChanged()
		select {
		case <-m.quit:
			return
		case now := <-t.C:
			m.gateTick(now)
		case <-changed:
			m.mu.Lock()
			m.gate.netAt = time.Time{} // read the network's identity again
			m.mu.Unlock()
		}
	}
}

// gateTick runs every second: free hours, network, the data cap and quotas, the traffic
// profile's schedule, slow torrents, and which torrents may upload.
func (m *Manager) gateTick(now time.Time) {
	g := m.gate
	m.mu.Lock()
	if now.Sub(g.netAt) > 30*time.Second && g.netBusy.CompareAndSwap(false, true) {
		g.netAt = now
		go func() {
			ni := netIdentify()
			g.net.Store(&ni)
			g.netBusy.Store(false)
		}()
	}
	m.gateRecompute(now)
	kick := false
	if id, _ := m.settings.Traffic.active(now); g.prof.Load() == nil || g.prof.Load().ID != id || m.settings.Traffic.Override != "" && !now.Before(m.settings.Traffic.Until) {
		m.applyTraffic()
		if p := g.prof.Load(); p != nil {
			m.notify("profile", p.ID, "Traffic profile: "+iso(p.Name))
		}
		kick = true
	}
	held := g.downHeld()
	if g.held && !held { // the cap or a quota let go: queues can start more
		kick = true
	}
	g.held = held
	if in := m.outsideFree(now) == ""; in != g.planIn { // the free hours began (or ended)
		g.planIn, kick = in, true
	}
	for _, d := range m.list {
		s, ok := g.slow[d.ID]
		switch {
		case !d.running || d.Kind != "torrent" || d.speed >= slowRate:
			delete(g.slow, d.ID)
		case !ok:
			g.slow[d.ID] = &slowMark{since: now}
		case !s.told && now.Sub(s.since) >= g.slowFor:
			s.told, kick = true, true // just became slow: its queue's next download can start
		}
	}
	for id := range g.slow {
		if m.byID[id] == nil {
			delete(g.slow, id)
		}
	}
	type tstate struct {
		t     *torrent
		planQ bool
	}
	var ts []tstate
	for _, t := range m.torrents {
		ts = append(ts, tstate{t, t.d.Queue == planQueueID})
	}
	maxSeeds := m.settings.Torrent.MaxSeeds
	m.mu.Unlock()

	// uploads: hold or let go, torrent by torrent (t.mu is never taken under m.mu)
	type seeder struct {
		t  *torrent
		at time.Time
	}
	var seeders []seeder
	for _, x := range ts {
		t := x.t
		t.mu.Lock()
		seeding, at := t.seeding, t.seedAt
		t.mu.Unlock()
		if x.planQ { // still in the Free data hours queue (a finished torrent remembers it)
			t.planQ.Store(true)
		} else if !seeding {
			t.planQ.Store(false)
		}
		m.mu.Lock()
		hold := m.uploadHold(t.planQ.Load(), seeding, now) != ""
		m.mu.Unlock()
		t.upHold.Store(hold)
		if held := t.upHeld(); held != t.wasHeld.Load() {
			t.wasHeld.Store(held)
			t.mu.Lock()
			t.rechoke() // choke everyone now, or give the slots out again
			t.mu.Unlock()
		}
		if seeding {
			seeders = append(seeders, seeder{t, at})
		}
	}
	// at most so many torrents share at once: the longest-sharing ones stop, as if done
	if maxSeeds > 0 && len(seeders) > maxSeeds {
		sort.Slice(seeders, func(i, j int) bool { return seeders[i].at.Before(seeders[j].at) })
		for _, s := range seeders[:len(seeders)-maxSeeds] {
			s.t.stop("stopped")
		}
	}
	if kick {
		m.kick()
	}
}

// gateRecompute works out the free hours, what counts, and what the data cap and the quotas
// still allow, and says so once when a quota passes its warning level or is used up. Caller
// holds m.mu.
func (m *Manager) gateRecompute(now time.Time) {
	g := m.gate
	p := m.settings.Plan
	free := false
	if p.On && m.queueByID(planQueueID) != nil {
		whole := p
		whole.Margin = 0 // the provider's window: the early stop is only for downloads
		_, _, free = whole.windowAt(now)
	}
	g.freeNow.Store(free)
	g.upFree.Store(!p.UpPaid)
	qs := m.settings.Quota
	g.mode.Store(countMode(qs.Count))

	capLeft := int64(math.MaxInt64)
	if c := m.capConfig(); c.On && c.MB > 0 {
		capLeft = int64(c.MB)<<20 - m.capUsed(c.Hours)
	}
	g.capLeft.Store(capLeft)

	ni := g.net.Load()
	view := QuotasView{NetKey: ni.key, NetName: ni.name, Free: free, Count: qs.Count, Today: g.days.day(ni.key, now)}
	left := int64(math.MaxInt64)
	for _, q := range qs.Rules {
		if !q.On {
			continue
		}
		used := g.days.used(q, qs.Count, p.UpPaid, now)
		_, to := q.period(now)
		pk := q.periodKey(now)
		v := QuotaView{ID: q.ID, Period: q.Period, Net: q.NetName, Used: used, Limit: q.MB << 20, Resets: to,
			Applies: q.Network == "" || q.Network == ni.key}
		v.Over = used >= v.Limit
		v.Warn = !v.Over && used*100 >= v.Limit*int64(qs.Warn)
		carry := g.days.carryingOn(q.ID, pk)
		v.CarryOn = v.Over && carry
		if v.Applies {
			if !carry {
				left = min(left, v.Limit-used)
			}
			switch {
			case v.Over && !carry && !g.days.said(q.ID, "stop:"+pk):
				text := quotaText(v, now) + " is used up. " + waitText(qs.Count) + " until it starts again " + resetText(v.Resets, now) + "."
				m.notify("quotaStop", q.ID, text)
				trayNotify("Data quota used up", text, true)
			case v.Warn && !g.days.said(q.ID, "warn:"+pk):
				m.notify("quotaWarn", q.ID, fmt.Sprintf("%s is %d%% used (%s of %s).", quotaText(v, now), used*100/max(v.Limit, 1), quotaSize(used), quotaSize(v.Limit)))
			}
			if view.Show == nil || float64(used)/float64(v.Limit) > float64(view.Show.Used)/float64(view.Show.Limit) {
				c := v
				view.Show = &c
			}
		}
		view.All = append(view.All, v)
	}
	g.qLeft.Store(left)
	g.quotas = view
}

// capUsed is what counts towards the rolling data cap in its window.
func (m *Manager) capUsed(hours int) int64 {
	all, _ := m.usage.used(hours)
	if m.gate == nil {
		return all
	}
	up, _ := m.gate.upUsage.used(hours)
	switch m.gate.mode.Load() {
	case countDownOnly:
		return max(all-up, 0)
	case countUpOnly:
		return up
	}
	return all
}

// quotaText names a quota: "Today's 2 GB on Home Wi-Fi", "This month's 100 GB".
func quotaText(v QuotaView, now time.Time) string {
	s := "Today's " + quotaSize(v.Limit) + " data quota"
	if v.Period == "month" {
		s = "This month's " + quotaSize(v.Limit) + " data quota"
	}
	if v.Net != "" {
		s += " on " + v.Net
	}
	return s
}

// quotaSize is humanSize without a needless ".0" (2 GB, 1.5 GB).
func quotaSize(n int64) string { return strings.Replace(humanSize(n), ".0 ", " ", 1) }

// waitText says what waits for a used-up quota.
func waitText(count string) string {
	switch count {
	case "down":
		return "Downloads wait"
	case "up":
		return "Uploads wait"
	}
	return "Downloads and uploads wait"
}

func resetText(t, now time.Time) string {
	switch {
	case t.Sub(dayStart(now, 0)) <= 24*time.Hour:
		return "at midnight"
	case t.Sub(dayStart(now, 0)) <= 48*time.Hour:
		return "tomorrow night"
	}
	return "on " + t.Format("Jan 2")
}

// QuotaCarryOn lets a used-up quota go over for the rest of its period ("carry on anyway this
// time" in the window).
func (m *Manager) QuotaCarryOn(id string) error {
	m.mu.Lock()
	var q *Quota
	for i := range m.settings.Quota.Rules {
		if m.settings.Quota.Rules[i].ID == id {
			q = &m.settings.Quota.Rules[i]
		}
	}
	if q == nil {
		m.mu.Unlock()
		return errors.New("that quota isn't there any more")
	}
	m.gate.days.setCarryOn(id, q.periodKey(time.Now()))
	m.gateRecompute(time.Now())
	m.mu.Unlock()
	m.kick()
	return nil
}

// trafficView fills in the traffic profile and quotas for the window. Caller holds m.mu.
func (m *Manager) trafficView(s *Snapshot) {
	if m.gate == nil {
		return
	}
	s.Profile = m.profileView(time.Now())
	s.Polite.UpRate, s.Polite.Lines = m.gate.politeUpRate, m.gate.politeLines
	s.Quota = m.gate.quotas
	s.Quota.Nets = m.gate.days.networks()
}

// normalizeGate tidies the settings the gate goes by (called from normalizeSettings).
func normalizeGate(s *Settings) {
	normalizeTraffic(&s.Traffic)
	normalizeQuotas(&s.Quota)
	tp := &s.Torrent
	if tp.MaxSeeds <= 0 { // (settings from before it)
		tp.MaxSeeds = defaultTorrentSettings().MaxSeeds
	}
	tp.MaxSeeds = min(tp.MaxSeeds, 100)
	switch {
	case tp.MetaMins == 0:
		tp.MetaMins = defaultTorrentSettings().MetaMins
	case tp.MetaMins < 0:
		tp.MetaMins = -1 // never give up
	}
	tp.MetaMins = min(tp.MetaMins, 24*60)
}
