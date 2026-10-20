package main

import (
	"sync"
	"time"
)

// The scheduler works with real dates rather than matching the clock's minute, so a start
// missed while the computer slept or Zipline was closed still happens (once), the hour skipped
// when clocks go forward doesn't swallow a start, and the hour repeated when they go back
// doesn't start a queue twice. Each queue remembers the last scheduled start it acted on
// (Queue.LastRun, saved with the settings), so every start runs exactly once, even across
// restarts.

// schedEvery is how often schedules are looked at (and right after the network changes or the
// computer wakes up).
const schedEvery = 15 * time.Second

// missedGrace: without a stop time, a missed start still runs this long after it.
const missedGrace = 12 * time.Hour

// schedState is the scheduler's, power actions', retries' and start-up's own state. Guarded
// by Manager.mu unless noted.
type schedState struct {
	stopDone map[string]time.Time  // per queue: the scheduled stop last acted on
	retry    map[string]*retryInfo // failed queue downloads that try again (retry.go)

	allDone string         // "When all downloads finish" (one-shot), "" = nothing
	allBusy bool           // something ran since allDone was set
	pend    *pendingAction // a power action waiting for others to finish or counting down (whendone.go)

	resume *resumePlan  // what start-up carries on with (startup.go)
	frozen *ResumeState // the exit sequence recorded what was running (rememberRunning)

	awake     bool      // keeping the computer awake (power.go)
	idleSince time.Time // nothing to keep it awake for since then
	wakeAt    time.Time // the wake timer is set for then (Windows)

	meterOnce  sync.Once
	meterFirst chan struct{} // closed after the first metered-connection check
}

// meterReady is closed once the first metered-connection check is done. Scheduled starts,
// grabber projects and the start-up resume wait for it, so nothing starts on a phone hotspot
// before Zipline knows it's on one.
func (m *Manager) meterReady() chan struct{} {
	m.sx.meterOnce.Do(func() { m.sx.meterFirst = make(chan struct{}) })
	return m.sx.meterFirst
}

// startScheduler starts the scheduler, the power-action and retry ticks, and carries on with
// what was running last time. Called once from NewManager.
func (m *Manager) startScheduler() {
	plan := m.takeResumePlan()
	go m.schedLoop()
	go m.powerLoop()
	if plan {
		go m.resumeAtStart()
	}
}

func (m *Manager) schedLoop() {
	select {
	case <-m.meterReady():
	case <-m.quit:
		return
	}
	t := time.NewTicker(schedEvery)
	defer t.Stop()
	for {
		wake := netChanged() // taken before looking, so a change meanwhile isn't missed
		now := time.Now()
		m.checkSchedule(now)
		m.grab.tick(now)
		m.armWake(now)
		select {
		case <-m.quit:
			wakeHook(time.Time{})
			return
		case <-t.C:
		case <-wake:
		}
	}
}

// clockAt is the moment the local clock shows mins (after midnight) on the given day. A time
// the clock skips that day (clocks going forward) becomes the first minute after it that
// exists; a time it shows twice (clocks going back) is the first of the two.
func clockAt(y int, mo time.Month, d, mins int, loc *time.Location) time.Time {
	for k := 0; k <= 180; k++ {
		t := time.Date(y, mo, d, 0, mins+k, 0, 0, loc)
		want := time.Date(y, mo, d, 0, mins+k, 0, 0, time.UTC)
		if t.Day() != want.Day() || t.Hour() != want.Hour() || t.Minute() != want.Minute() {
			continue // skipped by the clock
		}
		_, off := t.Zone()
		if _, before := t.Add(-6 * time.Hour).Zone(); before > off {
			if e := t.Add(-time.Duration(before-off) * time.Second); e.Hour() == t.Hour() && e.Minute() == t.Minute() {
				t = e // the clock went back: the first time it showed this
			}
		}
		return t
	}
	return time.Date(y, mo, d, 0, mins, 0, 0, loc)
}

func (s Schedule) onDay(w time.Weekday) bool {
	for _, d := range s.Days {
		if d == int(w) {
			return true
		}
	}
	return false
}

// startOn is the scheduled start on the day that is `back` days before now (false when the
// schedule doesn't run that day).
func (s Schedule) startOn(now time.Time, back int) (time.Time, bool) {
	mins, ok := clockMinutes(s.Start)
	y, mo, d := now.Date()
	day := time.Date(y, mo, d-back, 12, 0, 0, 0, now.Location())
	if !ok || !s.onDay(day.Weekday()) {
		return time.Time{}, false
	}
	return clockAt(day.Year(), day.Month(), day.Day(), mins, now.Location()), true
}

// lastStart is the latest scheduled start at or before now.
func (s Schedule) lastStart(now time.Time) (time.Time, bool) {
	for back := 0; back <= 8; back++ {
		if t, ok := s.startOn(now, back); ok && !t.After(now) {
			return t, true
		}
	}
	return time.Time{}, false
}

// nextStart is the first scheduled start after now.
func (s Schedule) nextStart(now time.Time) (time.Time, bool) {
	for ahead := 0; ahead <= 8; ahead++ {
		if t, ok := s.startOn(now, -ahead); ok && t.After(now) {
			return t, true
		}
	}
	return time.Time{}, false
}

// windowEnd is when the window that opened at start closes: the stop time (the next day when
// it's earlier than the start), or missedGrace later without one.
func (s Schedule) windowEnd(start time.Time) time.Time {
	sm, ok1 := clockMinutes(s.Start)
	em, ok2 := clockMinutes(s.Stop)
	if !s.StopEnabled || !ok1 || !ok2 {
		return start.Add(missedGrace)
	}
	y, mo, d := start.Date()
	if em <= sm {
		d++
	}
	return clockAt(y, mo, d, em, start.Location())
}

// lastStop is the latest scheduled stop at or before now. A stop belongs to the window of a
// scheduled day, so a window running past midnight stops the next morning.
func (s Schedule) lastStop(now time.Time) (time.Time, bool) {
	if !s.StopEnabled {
		return time.Time{}, false
	}
	for back := 0; back <= 8; back++ {
		if t, ok := s.startOn(now, back); ok {
			if e := s.windowEnd(t); !e.After(now) {
				return e, true
			}
		}
	}
	return time.Time{}, false
}

// inWindow: now is between a scheduled start and its stop time.
func (s Schedule) inWindow(now time.Time) bool {
	t, ok := s.lastStart(now)
	return ok && now.Before(s.windowEnd(t))
}

// schedStep is what a schedule asks for at one look.
type schedStep struct {
	start, stop bool
	missed      bool      // the start happened while the computer slept or Zipline was closed
	lastRun     time.Time // the scheduled start acted on (Queue.LastRun)
	stopDone    time.Time // the scheduled stop acted on
}

// step decides whether a queue starts or stops now. lastRun and stopDone are the start and
// stop last acted on; a zero stopDone means this is the first look since Zipline started,
// which only notes where things stand. lastStart is when the queue last started.
func (s Schedule) step(now, lastRun, stopDone time.Time, running bool, lastStart time.Time) schedStep {
	st := schedStep{lastRun: lastRun, stopDone: stopDone}
	if lastRun.After(now) { // the clock was put back: what's past is done
		st.lastRun, _ = s.lastStart(now)
	}
	if stopDone.After(now) {
		st.stopDone = time.Time{}
	}
	if s.StopEnabled {
		if e, ok := s.lastStop(now); ok && e.After(st.stopDone) {
			st.stop = !st.stopDone.IsZero() && running
			st.stopDone = e
		} else if !ok && st.stopDone.IsZero() {
			st.stopDone = now // (nothing to note yet: later stops count)
		}
	}
	if s.Enabled {
		if t, ok := s.lastStart(now); ok && t.After(st.lastRun) {
			st.lastRun = t // acted on, even when it's too late to run
			if now.Before(s.windowEnd(t)) {
				st.start = true
				st.missed = now.Sub(t) > 2*schedEvery
			}
		}
	}
	// "start again every N minutes": inside the window when there's a stop time
	if s.RepeatEnabled && s.RepeatMins > 0 && !st.start && !running &&
		now.Sub(lastStart) >= time.Duration(s.RepeatMins)*time.Minute {
		if s.StopEnabled {
			st.start = s.inWindow(now)
		} else {
			st.start = s.onDay(now.Weekday())
		}
	}
	return st
}

// checkSchedule starts and stops queues by their schedules.
func (m *Manager) checkSchedule(now time.Time) {
	type act struct {
		id, name      string
		start, stop   bool
		missed, fresh bool
		at            time.Time
	}
	var acts []act
	m.mu.Lock()
	if m.sx.stopDone == nil {
		m.sx.stopDone = map[string]time.Time{}
	}
	for i := range m.settings.Queues {
		q := &m.settings.Queues[i]
		running := m.qRun[q.ID]
		st := q.Sched.step(now, q.LastRun, m.sx.stopDone[q.ID], running, m.qLastStart[q.ID])
		if !st.lastRun.Equal(q.LastRun) {
			q.LastRun = st.lastRun
			m.dirty = true
		}
		m.sx.stopDone[q.ID] = st.stopDone
		if st.start || st.stop {
			acts = append(acts, act{q.ID, q.Name, st.start, st.stop, st.missed, !running || st.stop, q.LastRun})
		}
	}
	m.mu.Unlock()
	for _, a := range acts {
		if a.stop {
			logf("schedule: stopping %s", a.name)
			m.StopQueue(a.id)
		}
		if !a.start {
			continue
		}
		logf("schedule: starting %s", a.name)
		m.StartQueue(a.id, true)
		m.mu.Lock()
		if a.fresh {
			m.qLastStart[a.id] = now
		}
		if a.missed {
			m.notify("sched", a.id, iso(a.name)+" started now: its "+a.at.Format("15:04")+" start was missed while the computer was asleep or Zipline was closed")
		}
		m.mu.Unlock()
	}
}

// normalizeSched fills in and checks the queues' schedule and retry settings. Caller holds
// m.mu (or is in setup).
func (m *Manager) normalizeSched() {
	now := time.Now()
	for i := range m.settings.Queues {
		q := &m.settings.Queues[i]
		switch q.Sched.WhenDone {
		case "none", "quit", "sleep", "hibernate", "shutdown":
		default:
			q.Sched.WhenDone = "none"
		}
		if q.LastRun.IsZero() { // a new queue, or one from before Zipline remembered: starts from now
			q.LastRun = now
		}
		n := 3
		if q.Retries != nil {
			n = min(max(*q.Retries, 0), 20)
		}
		q.Retries = &n
		if q.RetryMins <= 0 {
			q.RetryMins = 5
		}
		q.RetryMins = min(q.RetryMins, 24*60)
	}
}

// keepSchedState carries over what the scheduler knows about each queue when the settings are
// saved from the window (which may hold an older copy). A changed start time, day or switch
// counts from now: starts before the change don't run. Caller holds m.mu.
func (m *Manager) keepSchedState(old []Queue) {
	now := time.Now()
	for i := range m.settings.Queues {
		q := &m.settings.Queues[i]
		q.LastRun = now
		for _, o := range old {
			if o.ID == q.ID && sameTiming(o.Sched, q.Sched) {
				q.LastRun = o.LastRun
			}
		}
	}
}

func sameTiming(a, b Schedule) bool {
	if a.Enabled != b.Enabled || a.Start != b.Start || len(a.Days) != len(b.Days) {
		return false
	}
	for i := range a.Days {
		if a.Days[i] != b.Days[i] {
			return false
		}
	}
	return true
}

// armWake keeps the wake timer (Windows) set for the next start of a queue that may wake the
// computer.
func (m *Manager) armWake(now time.Time) {
	var next time.Time
	m.mu.Lock()
	for _, q := range m.settings.Queues {
		if !q.Sched.Enabled || !q.Sched.Wake {
			continue
		}
		if t, ok := q.Sched.nextStart(now); ok && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	same := next.Equal(m.sx.wakeAt)
	m.sx.wakeAt = next
	m.mu.Unlock()
	if same {
		return
	}
	if err := wakeHook(next); err != nil {
		logf("wake timer: %v", err)
	} else if !next.IsZero() {
		logf("wake timer set for %s", next.Format("Mon 15:04"))
	}
}
