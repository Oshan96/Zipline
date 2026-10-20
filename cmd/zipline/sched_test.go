package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	_ "time/tzdata" // (the zones below, also where the system has none: Windows under Wine)
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

var allDays = []int{0, 1, 2, 3, 4, 5, 6}

// simulate looks at a schedule every 15 s from `from` to `to`, as the scheduler does, and
// returns when the queue started and stopped.
func simulate(s Schedule, from, to, lastRun time.Time) (starts, stops []time.Time) {
	var stopDone, lastStart time.Time
	running := false
	for now := from; now.Before(to); now = now.Add(schedEvery) {
		st := s.step(now, lastRun, stopDone, running, lastStart)
		lastRun, stopDone = st.lastRun, st.stopDone
		if st.stop {
			stops = append(stops, now)
			running = false
		}
		if st.start {
			starts = append(starts, now)
			running, lastStart = true, now
		}
	}
	return
}

func TestClockAtDST(t *testing.T) {
	ny, lon := zone(t, "America/New_York"), zone(t, "Europe/London")
	for _, c := range []struct {
		got  time.Time
		want string // UTC
	}{
		{clockAt(2026, 3, 8, 2*60+30, ny), "2026-03-08T07:00:00Z"}, // 02:30 doesn't exist: 03:00 EDT
		{clockAt(2026, 3, 8, 3*60, ny), "2026-03-08T07:00:00Z"},
		{clockAt(2026, 3, 8, 1*60+59, ny), "2026-03-08T06:59:00Z"},
		{clockAt(2026, 11, 1, 1*60+30, ny), "2026-11-01T05:30:00Z"},   // 01:30 twice: the first (EDT)
		{clockAt(2026, 3, 29, 1*60+30, lon), "2026-03-29T01:00:00Z"},  // 01:30 doesn't exist: 02:00 BST
		{clockAt(2026, 10, 25, 1*60+30, lon), "2026-10-25T00:30:00Z"}, // 01:30 twice: the first (BST)
		{clockAt(2026, 10, 25, 2*60, lon), "2026-10-25T02:00:00Z"},
		{clockAt(2026, 7, 1, 23*60, lon), "2026-07-01T22:00:00Z"},
	} {
		if g := c.got.UTC().Format(time.RFC3339); g != c.want {
			t.Errorf("got %s, want %s", g, c.want)
		}
	}
}

// D2: on the nights the clocks change, each scheduled start and stop happens exactly once.
func TestScheduleOnceAcrossDST(t *testing.T) {
	ny, lon := zone(t, "America/New_York"), zone(t, "Europe/London")
	for _, c := range []struct {
		name      string
		loc       *time.Location
		day       [3]int // the night's date
		start     string
		stop      string
		wantStart string // UTC
		wantStop  string
	}{
		{"New York spring, start skipped", ny, [3]int{2026, 3, 8}, "02:30", "", "2026-03-08T07:00:00Z", ""},
		{"New York autumn, start twice", ny, [3]int{2026, 11, 1}, "01:30", "", "2026-11-01T05:30:00Z", ""},
		{"New York autumn, stop twice", ny, [3]int{2026, 11, 1}, "00:00", "01:15", "2026-11-01T04:00:00Z", "2026-11-01T05:15:00Z"},
		{"London spring, start skipped", lon, [3]int{2026, 3, 29}, "01:30", "02:30", "2026-03-29T01:00:00Z", "2026-03-29T01:30:00Z"},
		{"London autumn, start twice", lon, [3]int{2026, 10, 25}, "01:30", "05:00", "2026-10-25T00:30:00Z", "2026-10-25T05:00:00Z"},
	} {
		s := Schedule{Enabled: true, Start: c.start, Days: allDays}
		if c.stop != "" {
			s.StopEnabled, s.Stop = true, c.stop
		}
		y, mo, d := c.day[0], time.Month(c.day[1]), c.day[2]
		from := time.Date(y, mo, d-1, 12, 0, 0, 0, c.loc)
		to := time.Date(y, mo, d, 12, 0, 0, 0, c.loc)
		starts, stops := simulate(s, from, to, from)
		if len(starts) != 1 || starts[0].Sub(mustParse(t, c.wantStart)) >= schedEvery || starts[0].Before(mustParse(t, c.wantStart)) {
			t.Errorf("%s: started at %v, want once at %s", c.name, starts, c.wantStart)
		}
		if c.stop != "" && (len(stops) != 1 || stops[0].Sub(mustParse(t, c.wantStop)) >= schedEvery || stops[0].Before(mustParse(t, c.wantStop))) {
			t.Errorf("%s: stopped at %v, want once at %s", c.name, stops, c.wantStop)
		}
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// D2: a start missed while the computer slept or Zipline was closed runs once, if it's still
// inside its window, and only the latest missed start runs.
func TestScheduleMissedStart(t *testing.T) {
	lon := zone(t, "Europe/London")
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, lon) }
	s := Schedule{Enabled: true, Start: "02:00", StopEnabled: true, Stop: "07:00", Days: allDays}

	st := s.step(at(12, 5, 0), at(11, 2, 0), time.Time{}, false, time.Time{})
	if !st.start || !st.missed || !st.lastRun.Equal(at(12, 2, 0)) {
		t.Fatalf("missed start inside the window: %+v", st)
	}
	if again := s.step(at(12, 5, 0).Add(schedEvery), st.lastRun, st.stopDone, true, at(12, 5, 0)); again.start {
		t.Fatal("started twice")
	}
	// past the stop time: too late, but counted as handled
	st = s.step(at(12, 8, 0), at(11, 2, 0), time.Time{}, false, time.Time{})
	if st.start || !st.lastRun.Equal(at(12, 2, 0)) {
		t.Fatalf("after the window: %+v", st)
	}
	// several days asleep: one start, for the latest
	st = s.step(at(12, 3, 0), at(8, 2, 0), time.Time{}, false, time.Time{})
	if !st.start || !st.lastRun.Equal(at(12, 2, 0)) {
		t.Fatalf("after days asleep: %+v", st)
	}
	// no stop time: within 12 hours
	s.StopEnabled = false
	if st := s.step(at(12, 13, 30), at(11, 2, 0), time.Time{}, false, time.Time{}); !st.start {
		t.Fatal("no stop time: should start within 12 hours")
	}
	if st := s.step(at(12, 14, 30), at(11, 2, 0), time.Time{}, false, time.Time{}); st.start {
		t.Fatal("no stop time: too late after 12 hours")
	}
	// on time: not "missed"
	if st := s.step(at(12, 2, 0).Add(5*time.Second), at(11, 2, 0), time.Time{}, false, time.Time{}); !st.start || st.missed {
		t.Fatalf("on time: %+v", st)
	}
}

// D2: Zipline starting (or waking) inside a window whose start already ran doesn't start it
// again; a stop slept through stops the queue once on waking.
func TestScheduleRestartAndWakeInsideWindow(t *testing.T) {
	ny := zone(t, "America/New_York")
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, ny) }
	s := Schedule{Enabled: true, Start: "02:00", StopEnabled: true, Stop: "07:00", Days: allDays}
	// restart at 03:00: the 02:00 start ran before (saved LastRun); first look since start-up
	st := s.step(at(12, 3, 0), at(12, 2, 0), time.Time{}, false, time.Time{})
	if st.start || st.stop || !st.stopDone.Equal(at(11, 7, 0)) {
		t.Fatalf("restart inside the window: %+v", st)
	}
	// asleep from 06:00 to 09:00 while running: stops once on waking
	st = s.step(at(12, 9, 0), at(12, 2, 0), at(11, 7, 0), true, at(12, 2, 0))
	if !st.stop || st.start || !st.stopDone.Equal(at(12, 7, 0)) {
		t.Fatalf("woke after the stop time: %+v", st)
	}
	if again := s.step(at(12, 9, 0).Add(schedEvery), st.lastRun, st.stopDone, false, at(12, 2, 0)); again.stop || again.start {
		t.Fatalf("stopped twice: %+v", again)
	}
	// the clock was put back a day: what lies "ahead" counts as done, the next start runs
	st = s.step(at(11, 1, 0), at(12, 2, 0), at(12, 7, 0), false, time.Time{})
	if st.start || st.lastRun.After(at(11, 1, 0)) {
		t.Fatalf("clock put back: %+v", st)
	}
	if st2 := s.step(at(11, 2, 0), st.lastRun, st.stopDone, false, time.Time{}); !st2.start {
		t.Fatalf("after the clock was put back, the next start should run: %+v", st2)
	}
}

// D2: a window that runs past midnight starts on its day and stops the next morning.
func TestScheduleAcrossMidnight(t *testing.T) {
	lon := zone(t, "Europe/London")
	at := func(d, h, m int) time.Time { return time.Date(2026, 10, d, h, m, 0, 0, lon) }
	s := Schedule{Enabled: true, Start: "23:00", StopEnabled: true, Stop: "06:00", Days: []int{1}} // Mondays
	if at(12, 0, 0).Weekday() != time.Monday {
		t.Fatal("test dates")
	}
	starts, stops := simulate(s, at(12, 12, 0), at(14, 12, 0), at(12, 12, 0))
	if len(starts) != 1 || !starts[0].Equal(at(12, 23, 0)) || len(stops) != 1 || !stops[0].Equal(at(13, 6, 0)) {
		t.Fatalf("starts %v stops %v", starts, stops)
	}
	if !s.inWindow(at(13, 1, 0)) || s.inWindow(at(13, 7, 0)) || s.inWindow(at(14, 1, 0)) {
		t.Fatal("inWindow")
	}
	// Zipline started at 01:00 on Tuesday: the missed Monday 23:00 start runs
	if st := s.step(at(13, 1, 0), at(5, 23, 0), time.Time{}, false, time.Time{}); !st.start || !st.lastRun.Equal(at(12, 23, 0)) {
		t.Fatalf("missed start after midnight: %+v", st)
	}
	if next, _ := s.nextStart(at(13, 1, 0)); !next.Equal(at(19, 23, 0)) {
		t.Fatalf("next start %v", next)
	}
}

// D2: "start again every N minutes" runs only inside the window when there's a stop time.
func TestScheduleRepeat(t *testing.T) {
	lon := zone(t, "Europe/London")
	at := func(h, m int) time.Time { return time.Date(2026, 10, 12, h, m, 0, 0, lon) }
	s := Schedule{Start: "02:00", StopEnabled: true, Stop: "07:00", Days: allDays, RepeatEnabled: true, RepeatMins: 30}
	if st := s.step(at(3, 0), at(2, 0), at(3, 0), false, at(2, 0)); !st.start {
		t.Fatal("repeat inside the window")
	}
	if st := s.step(at(2, 20), at(2, 0), at(2, 20), false, at(2, 0)); st.start {
		t.Fatal("repeat too soon")
	}
	if st := s.step(at(3, 0), at(2, 0), at(3, 0), true, at(2, 0)); st.start {
		t.Fatal("repeat while running")
	}
	if st := s.step(at(8, 0), at(2, 0), at(7, 0), false, at(6, 0)); st.start {
		t.Fatal("repeat outside the window")
	}
	starts, stops := simulate(s, at(1, 0), at(9, 0), at(1, 0))
	if len(starts) == 0 || starts[0].Before(at(2, 0)) || starts[len(starts)-1].After(at(7, 0)) || len(stops) != 1 {
		t.Fatalf("repeats %v, stops %v", starts, stops)
	}
	// without a stop time: on the schedule's days
	s.StopEnabled = false
	if st := s.step(at(8, 0), at(2, 0), time.Time{}, false, at(6, 0)); !st.start {
		t.Fatal("repeat without a stop time")
	}
	s.Days = []int{int(at(8, 0).Weekday()+1) % 7}
	if st := s.step(at(8, 0), at(2, 0), time.Time{}, false, at(6, 0)); st.start {
		t.Fatal("repeat on another day")
	}
}

// bareManager is a manager without its background loops, for driving the scheduler, retries
// and power actions with a test clock. Its queues hold their downloads (as on a metered
// connection), so nothing is fetched.
func bareManager(t *testing.T) *Manager {
	m := &Manager{byID: map[string]*Download{}, settings: defaultSettings(), dataDir: t.TempDir(), limiter: &Limiter{}, quit: make(chan struct{}),
		qRun: map[string]bool{}, qBySched: map[string]bool{}, qSyncing: map[string]bool{}, qLastStart: map[string]time.Time{},
		meterPaused: map[string]bool{}, politeLim: &Limiter{}, refresh: map[string]*refreshJob{}, torrents: map[string]*torrent{}, metaSessions: map[string]*metaSession{}}
	m.hosts = newHostState()
	m.normalizeSettings()
	m.metered = true
	return m
}

func addFake(m *Manager, id, queue, status string) *Download {
	d := &Download{ID: id, URL: "http://127.0.0.1:1/" + id, FileName: id, Queue: queue, Status: status, Size: -1}
	m.mu.Lock()
	m.list = append(m.list, d)
	m.byID[id] = d
	m.mu.Unlock()
	return d
}

func TestCheckScheduleStartsAndRemembers(t *testing.T) {
	m := bareManager(t)
	addFake(m, "a", "main", StPaused)
	now := time.Now()
	m.mu.Lock()
	q := m.queueByID("main")
	q.Sched = Schedule{Enabled: true, Start: now.Add(-2 * time.Hour).Format("15:04"), Days: allDays, WhenDone: "none"}
	occ, _ := q.Sched.lastStart(now)
	q.LastRun = occ.Add(-24 * time.Hour) // yesterday's ran; today's was missed
	m.mu.Unlock()

	m.checkSchedule(now)
	m.mu.Lock()
	running, bySched, last, dirty := m.qRun["main"], m.qBySched["main"], m.queueByID("main").LastRun, m.dirty
	notes := append([]Notice(nil), m.notices...)
	m.mu.Unlock()
	if !running || !bySched || !last.Equal(occ) || !dirty {
		t.Fatalf("running=%v bySched=%v lastRun=%v (want %v) dirty=%v", running, bySched, last, occ, dirty)
	}
	if len(notes) == 0 || notes[len(notes)-1].Kind != "sched" || !strings.Contains(notes[len(notes)-1].Text, "missed") {
		t.Fatalf("notices %+v", notes)
	}
	// the window saves the settings with an old copy: what the scheduler did stays
	m.mu.Lock()
	old := append([]Queue(nil), m.settings.Queues...)
	stale := append([]Queue(nil), m.settings.Queues...)
	stale[0].LastRun = occ.Add(-24 * time.Hour)
	stale[0].Sched.WhenDone = "sleep" // (not a timing change)
	m.settings.Queues = stale
	m.normalizeSettings()
	m.keepSchedState(old)
	kept := m.queueByID("main").LastRun
	stale = append([]Queue(nil), m.settings.Queues...)
	stale[0].Sched.Start = now.Add(-time.Hour).Format("15:04") // a new start time counts from now
	old = append([]Queue(nil), m.settings.Queues...)
	m.settings.Queues = stale
	m.normalizeSettings()
	m.keepSchedState(old)
	moved := m.queueByID("main").LastRun
	m.mu.Unlock()
	if !kept.Equal(occ) || moved.Before(now) {
		t.Fatalf("kept %v (want %v), after a change %v", kept, occ, moved)
	}
}

// D2: the start acted on is saved, so a restart inside the window doesn't start the queue again.
func TestScheduleLastRunSurvivesRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "zlsched")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	m := NewManager(dir)
	m.mu.Lock()
	m.settings.DownloadDir = dir
	q := m.queueByID("main")
	now := time.Now()
	q.Sched = Schedule{Enabled: true, Start: now.Add(-time.Hour).Format("15:04"), Days: allDays, WhenDone: "none"}
	q.LastRun, _ = q.Sched.lastStart(now) // it ran an hour ago
	want := q.LastRun
	m.mu.Unlock()
	m.save()
	m.Quit()

	m2 := NewManager(dir)
	defer m2.Quit()
	<-m2.meterReady()
	time.Sleep(200 * time.Millisecond) // (the scheduler's first look)
	m2.checkSchedule(time.Now())
	m2.mu.Lock()
	got, running := m2.queueByID("main").LastRun, m2.qRun["main"]
	var sched int
	for _, n := range m2.notices {
		if n.Kind == "sched" || n.Kind == "queueDone" {
			sched++
		}
	}
	m2.mu.Unlock()
	if !got.Equal(want) || running || sched != 0 {
		t.Fatalf("after a restart: lastRun %v (want %v), running %v, notices %d", got, want, running, sched)
	}
}

// D4: a failed download in a running queue tries again later, at the end of the queue; the
// queue isn't finished meanwhile; permanent errors and used-up tries aren't retried.
func TestQueueRetries(t *testing.T) {
	m := bareManager(t)
	a := addFake(m, "a", "main", StQueued)
	b := addFake(m, "b", "main", StQueued)
	m.StartQueue("main", true)
	fail := func(d *Download, err error) {
		m.mu.Lock()
		d.Status, d.Error = StError, "failed"
		m.noteFailure(d, err)
		m.mu.Unlock()
		m.kick()
	}
	for _, c := range []struct {
		err   error
		retry bool
	}{
		{permanentErr{errors.New("disk full")}, false},
		{statusErr{code: 404, status: "404 Not Found"}, false},
		{statusErr{code: 401, status: "401"}, false},
		{statusErr{code: 503, status: "503"}, true},
		{statusErr{code: 429, status: "429"}, true},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{io.ErrUnexpectedEOF, true},
	} {
		if permanentFailure(c.err) == c.retry {
			t.Errorf("%v: retry %v", c.err, !c.retry)
		}
	}

	fail(a, statusErr{code: 503, status: "503"})
	m.mu.Lock()
	waiting, at, running := m.retryWaiting(a), m.retryAt(a), m.qRun["main"]
	m.mu.Unlock()
	if !waiting || at < time.Now().Add(4*time.Minute).UnixMilli() || !running {
		t.Fatalf("after a failure: waiting %v at %v running %v", waiting, at, running)
	}
	m.retryTick(time.Now().Add(6 * time.Minute))
	m.mu.Lock()
	last := m.list[len(m.list)-1]
	st, er := a.Status, a.Error
	m.mu.Unlock()
	if st != StQueued || er != "" || last != a {
		t.Fatalf("retried: status %s error %q, last in queue %s", st, er, last.ID)
	}
	// the other one is done; a uses up its tries: then the queue finishes, with a failure
	m.mu.Lock()
	b.Status = StCompleted
	m.queueByID("main").Sched.WhenDone = "shutdown"
	m.mu.Unlock()
	for i := 0; i < 2; i++ {
		fail(a, errors.New("connection reset"))
		m.retryTick(time.Now().Add(time.Duration(6*(i+2)) * time.Minute))
	}
	fail(a, errors.New("connection reset"))
	m.mu.Lock()
	waiting, running = m.retryWaiting(a), m.qRun["main"]
	pend := m.sx.pend
	m.mu.Unlock()
	if waiting || running || pend != nil {
		t.Fatalf("tries used up: waiting %v, queue running %v, action %+v", waiting, running, pend)
	}
	// started by hand: the count starts over
	m.mu.Lock()
	m.sx.retry[a.ID].tries = 3
	m.settings.MaxActive = 0 // (it waits for a slot rather than really starting)
	m.startByHand(a)
	_, kept := m.sx.retry[a.ID]
	m.mu.Unlock()
	if kept {
		t.Fatal("tries not reset when started by hand")
	}
}

// D4 end to end: a download that can't connect fails, waits, then tries again and finishes.
func TestQueueRetryEndToEnd(t *testing.T) {
	if !online() {
		t.Skip("no network adapter here: refused connections don't use up tries while offline")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens yet: connections are refused
	m := newTestManager(t)
	defer m.Quit()
	d := m.Add(AddReq{URL: "http://" + addr + "/f.bin", QueueID: "main"})
	m.StartQueue("main", false)
	deadline := time.Now().Add(40 * time.Second)
	for {
		m.mu.Lock()
		st, waiting, running := d.Status, m.retryWaiting(d), m.qRun["main"]
		m.mu.Unlock()
		if st == StError {
			if !waiting || !running {
				t.Fatalf("failed: waiting %v, queue running %v", waiting, running)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("didn't fail (status %s)", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	data := randData(100 << 10)
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Skip("port taken meanwhile:", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.bin", time.Time{}, strings.NewReader(string(data)))
	})}
	go srv.Serve(ln)
	defer srv.Close()
	m.mu.Lock()
	m.sx.retry[d.ID].at = time.Now() // (rather than wait five minutes)
	m.mu.Unlock()
	for i := 0; ; i++ { // (it's still failed until the next retry tick)
		m.mu.Lock()
		st := d.Status
		m.mu.Unlock()
		if st == StCompleted {
			break
		}
		if i == 400 {
			t.Fatalf("not downloaded on the next try (status %s)", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	fileIs(t, d, data)
	for i := 0; i < 40; i++ {
		m.mu.Lock()
		running, left := m.qRun["main"], len(m.sx.retry)
		m.mu.Unlock()
		if !running && left == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("queue still running, or the tries not forgotten")
}

type powerCalls struct{ sleep, hibernate, shutdown atomic.Int32 }

func fakePower(t *testing.T) *powerCalls {
	c := &powerCalls{}
	s, h, d := sleepHook, hibernateHook, shutdownHook
	sleepHook = func() { c.sleep.Add(1) }
	hibernateHook = func() { c.hibernate.Add(1) }
	shutdownHook = func() { c.shutdown.Add(1) }
	t.Cleanup(func() { sleepHook, hibernateHook, shutdownHook = s, h, d })
	return c
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(what)
}

// D5: an action waits for other downloads, counts down a minute, then runs; Cancel stops it.
func TestWhenFinishedCountdown(t *testing.T) {
	calls := fakePower(t)
	m := bareManager(t)
	other := addFake(m, "manual", "", StDownloading)
	m.mu.Lock()
	other.running = true
	q := m.queueByID("main")
	q.Sched.WhenDone = "shutdown"
	m.mu.Unlock()
	m.StartQueue("main", true) // empty: finishes at once, by its schedule
	t0 := time.Now()
	m.powerTick(t0)
	m.mu.Lock()
	v := m.powerView()
	m.mu.Unlock()
	if v.Pending == nil || v.Pending.Action != "shutdown" || v.Pending.At != 0 {
		t.Fatalf("should wait for the other download: %+v", v.Pending)
	}
	m.powerTick(t0.Add(2 * time.Minute))
	if calls.shutdown.Load() != 0 {
		t.Fatal("turned off while another download runs")
	}
	m.mu.Lock()
	other.running, other.Status = false, StCompleted
	m.mu.Unlock()
	t1 := t0.Add(3 * time.Minute)
	m.powerTick(t1)
	m.mu.Lock()
	v = m.powerView()
	var noted bool
	for _, n := range m.notices {
		noted = noted || n.Kind == "power"
	}
	m.mu.Unlock()
	if v.Pending == nil || v.Pending.At != t1.Add(actionDelay).UnixMilli() || !noted {
		t.Fatalf("countdown: %+v noted %v", v.Pending, noted)
	}
	m.powerTick(t1.Add(30 * time.Second))
	if calls.shutdown.Load() != 0 {
		t.Fatal("ran before the countdown ended")
	}
	m.powerTick(t1.Add(actionDelay))
	eventually(t, "didn't turn off after the countdown", func() bool { return calls.shutdown.Load() == 1 })

	// cancelled during the countdown
	m.mu.Lock()
	m.requestPower("sleep", "test")
	m.mu.Unlock()
	m.powerTick(t1)
	if err := m.powerControl("cancel", ""); err != nil {
		t.Fatal(err)
	}
	m.powerTick(t1.Add(2 * actionDelay))
	time.Sleep(50 * time.Millisecond)
	if calls.sleep.Load() != 0 {
		t.Fatal("slept after Cancel")
	}
	// "Do it now"
	m.mu.Lock()
	m.requestPower("hibernate", "test")
	m.mu.Unlock()
	m.powerTick(t1)
	m.powerControl("now", "")
	eventually(t, "Do it now", func() bool { return calls.hibernate.Load() == 1 })
	// exit
	m.mu.Lock()
	m.requestPower("quit", "test")
	m.mu.Unlock()
	m.powerTick(t1)
	m.powerTick(t1.Add(actionDelay))
	select {
	case <-m.quit:
	default:
		t.Fatal("didn't exit")
	}
}

// D5: "When all downloads finish" fires once everything is done, then resets itself; it
// waits for something to run when set while idle; a stronger action wins.
func TestWhenAllDownloadsFinish(t *testing.T) {
	calls := fakePower(t)
	m := bareManager(t)
	d := addFake(m, "x", "", StDownloading)
	if err := m.powerControl("whenDone", "bogus"); err == nil {
		t.Fatal("accepted an unknown action")
	}
	m.powerControl("whenDone", "sleep") // idle: waits for something to run
	t0 := time.Now()
	m.powerTick(t0)
	m.mu.Lock()
	pend := m.sx.pend
	d.running = true
	m.mu.Unlock()
	if pend != nil {
		t.Fatal("fired with nothing downloaded")
	}
	m.powerTick(t0)
	m.mu.Lock()
	d.running, d.Status = false, StCompleted
	m.mu.Unlock()
	m.powerTick(t0.Add(time.Second))
	m.mu.Lock()
	v := m.powerView()
	m.mu.Unlock()
	if v.WhenAllDone != "" || v.Pending == nil || v.Pending.Action != "sleep" || v.Pending.At == 0 {
		t.Fatalf("after everything finished: %+v", v)
	}
	// a queue asking to turn off meanwhile wins over sleep
	m.mu.Lock()
	m.requestPower("shutdown", "Main queue finished")
	m.requestPower("quit", "Other queue finished")
	act := m.sx.pend.action
	m.mu.Unlock()
	if act != "shutdown" {
		t.Fatalf("action %s", act)
	}
	m.powerTick(t0.Add(time.Second))
	m.powerTick(t0.Add(time.Second + actionDelay))
	eventually(t, "the action didn't run", func() bool { return calls.shutdown.Load() == 1 })
	if calls.sleep.Load() != 0 {
		t.Fatal("ran two actions")
	}
	// Cancel also clears the one-shot choice
	m.powerControl("whenDone", "hibernate")
	m.mu.Lock()
	m.requestPower("sleep", "test")
	m.mu.Unlock()
	m.powerControl("cancel", "")
	m.mu.Lock()
	v = m.powerView()
	m.mu.Unlock()
	if v.WhenAllDone != "" || v.Pending != nil {
		t.Fatalf("after Cancel: %+v", v)
	}
}

// D5: a queue that had failures never runs its action; one started by hand doesn't either.
func TestWhenFinishedOnlyAfterCleanScheduledRun(t *testing.T) {
	m := bareManager(t)
	m.mu.Lock()
	m.queueByID("main").Sched.WhenDone = "sleep"
	m.queueByID("main").Retries = new(int) // (no retries)
	m.mu.Unlock()
	bad := addFake(m, "bad", "main", StError)
	m.StartQueue("main", true)
	m.mu.Lock()
	bad.Status = StError // (StartQueue queued it again: it failed again)
	m.mu.Unlock()
	m.kick()
	m.mu.Lock()
	running, pend := m.qRun["main"], m.sx.pend
	bad.Status, bad.Queue = StCompleted, ""
	m.mu.Unlock()
	if running || pend != nil {
		t.Fatalf("after a failure: running %v, action %+v", running, pend)
	}
	m.StartQueue("main", false) // empty, started by hand: finishes at once
	m.mu.Lock()
	running, pend = m.qRun["main"], m.sx.pend
	m.mu.Unlock()
	if running || pend != nil {
		t.Fatalf("started by hand: running %v, action %+v", running, pend)
	}
	m.StartQueue("main", true)
	m.mu.Lock()
	pend = m.sx.pend
	m.mu.Unlock()
	if pend == nil || pend.action != "sleep" || pend.why != iso("Main queue")+" finished" {
		t.Fatalf("by its schedule: %+v", pend)
	}
}

// D3: the computer is kept awake while downloads or queues run, not while only sharing, and
// let go a few seconds after.
func TestKeepAwakeFollowsActivity(t *testing.T) {
	var mu sync.Mutex
	var calls []bool
	old := keepAwakeHook
	keepAwakeHook = func(on bool) error { mu.Lock(); calls = append(calls, on); mu.Unlock(); return nil }
	defer func() { keepAwakeHook = old }()
	got := func() string {
		mu.Lock()
		defer mu.Unlock()
		s := ""
		for _, c := range calls {
			s += map[bool]string{true: "+", false: "-"}[c]
		}
		return s
	}
	m := bareManager(t)
	d := addFake(m, "t", "", StCompleted)
	t0 := time.Now()
	m.mu.Lock()
	d.Kind = "torrent"
	m.torrents[d.ID] = &torrent{} // sharing a finished torrent
	m.mu.Unlock()
	m.awakeTick(t0)
	if got() != "" {
		t.Fatalf("held awake while only sharing: %s", got())
	}
	m.mu.Lock()
	d.running, d.Status = true, StDownloading
	m.mu.Unlock()
	m.awakeTick(t0.Add(time.Second))
	m.awakeTick(t0.Add(2 * time.Second))
	if got() != "+" {
		t.Fatalf("downloading: %s", got())
	}
	m.mu.Lock()
	d.running = false
	m.qRun["main"] = true // a queue waiting (for another try, a metered connection)
	m.mu.Unlock()
	m.awakeTick(t0.Add(10 * time.Second))
	m.mu.Lock()
	m.qRun["main"] = false
	m.mu.Unlock()
	m.awakeTick(t0.Add(11 * time.Second))
	if got() != "+" {
		t.Fatalf("let go too soon: %s", got())
	}
	m.awakeTick(t0.Add(11*time.Second + awakeGrace))
	if got() != "+-" {
		t.Fatalf("not let go when idle: %s", got())
	}
	// switched off in Options
	m.mu.Lock()
	m.settings.KeepAwake = false
	d.running = true
	m.mu.Unlock()
	m.awakeTick(t0.Add(time.Minute))
	m.awakeRelease()
	if got() != "+-" {
		t.Fatalf("held although switched off: %s", got())
	}
}

// D3: a queue that may wake the computer keeps the wake timer set for its next start.
func TestWakeTimerFollowsSchedule(t *testing.T) {
	var mu sync.Mutex
	var set []time.Time
	old := wakeHook
	wakeHook = func(at time.Time) error { mu.Lock(); set = append(set, at); mu.Unlock(); return nil }
	defer func() { wakeHook = old }()
	m := bareManager(t)
	now := time.Now()
	m.mu.Lock()
	q := m.queueByID("main")
	q.Sched = Schedule{Enabled: true, Start: now.Add(2 * time.Hour).Format("15:04"), Days: allDays, Wake: true}
	want, _ := q.Sched.nextStart(now)
	m.mu.Unlock()
	m.armWake(now)
	m.armWake(now.Add(time.Second)) // unchanged: not set again
	m.mu.Lock()
	m.queueByID("main").Sched.Wake = false
	m.mu.Unlock()
	m.armWake(now.Add(2 * time.Second))
	mu.Lock()
	defer mu.Unlock()
	if len(set) != 2 || !set[0].Equal(want) || !set[1].IsZero() {
		t.Fatalf("wake timer: %v (want %v, then none)", set, want)
	}
}

// d9Server serves a file slowly, so a download is still running when Zipline "exits".
func d9Server(t *testing.T) (string, []byte) {
	data := randData(6 << 20)
	srv := testServer(data, 128<<10, true)
	t.Cleanup(srv.Close)
	return srv.URL, data
}

// d9Manager is a manager in its own folder, stopped and removed when the test ends.
func d9Manager(t *testing.T, dir string) *Manager {
	m := NewManager(dir)
	m.mu.Lock()
	m.settings.DownloadDir = dir
	m.mu.Unlock()
	t.Cleanup(func() {
		m.mu.Lock()
		var ids []string
		for _, d := range m.list {
			ids = append(ids, d.ID)
		}
		m.settings.Plan.Metered = false
		m.mu.Unlock()
		m.Pause(ids)
		m.Quit()
	})
	return m
}

func tempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "zlstart")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 40; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
}

// exitLike runs main's exit sequence (clean) or just saves while running (a crash), then copies
// the saved state to a new folder for the next start.
func exitLike(t *testing.T, m *Manager, clean bool) string {
	if clean {
		m.dropAllEarly()
		m.rememberRunning()
		m.Pause(allIDs(m))
		for i := 0; i < 100 && len(allIDs(m)) > 0; i++ {
			time.Sleep(20 * time.Millisecond)
		}
	}
	m.save()
	next := tempDir(t)
	files := []string{"settings.json", "downloads.json"}
	if !clean {
		files = append(files, "running") // (the marker an unclean exit leaves)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(m.dataDir, f))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(next, f), b, 0o600)
	}
	if !clean { // the crashed copy stops
		m.mu.Lock()
		var ids []string
		for _, d := range m.list {
			ids = append(ids, d.ID)
		}
		m.qRun = map[string]bool{}
		m.mu.Unlock()
		m.Pause(ids)
	}
	return next
}

func shortResume(t *testing.T) {
	old := resumeDelay
	resumeDelay = 300 * time.Millisecond
	t.Cleanup(func() { resumeDelay = old })
}

func byURL(m *Manager, u string) *Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.list {
		if d.URL == u {
			return d
		}
	}
	return nil
}

// D9: downloads and queues that were running carry on after a restart (a clean exit or a
// crash), after a short countdown the window can cancel.
func TestResumeAfterRestart(t *testing.T) {
	shortResume(t)
	base, data := d9Server(t)
	for _, clean := range []bool{true, false} {
		m := d9Manager(t, tempDir(t))
		a := m.Add(AddReq{URL: base + "/a.bin", Start: true})
		m.Add(AddReq{URL: base + "/q.bin", QueueID: "main"})
		m.Add(AddReq{URL: base + "/idle.bin"})
		m.StartQueue("main", true)
		waitFor(t, m, a, StDownloading, 10*time.Second)
		dir := exitLike(t, m, clean)

		m2 := d9Manager(t, dir)
		eventually(t, "no countdown shown", func() bool {
			m2.mu.Lock()
			defer m2.mu.Unlock()
			v := m2.powerView()
			return v.Resume != nil && v.Resume.Downloads == 1 && len(v.Resume.Queues) == 1
		})
		a2, q2, idle := byURL(m2, base+"/a.bin"), byURL(m2, base+"/q.bin"), byURL(m2, base+"/idle.bin")
		waitFor(t, m2, a2, StDownloading, 10*time.Second)
		waitFor(t, m2, q2, StDownloading, 10*time.Second)
		m2.mu.Lock()
		running, bySched, idleSt := m2.qRun["main"], m2.qBySched["main"], idle.Status
		m2.mu.Unlock()
		if !running || !bySched || idleSt != StPaused {
			t.Fatalf("clean=%v: queue running %v (by schedule %v), the other download %s", clean, running, bySched, idleSt)
		}
		if clean {
			waitFor(t, m2, a2, StCompleted, 120*time.Second)
			fileIs(t, a2, data)
		}
	}
}

// D9: "Start with everything paused" doesn't carry on; Cancel in the countdown doesn't either.
func TestResumeStartPausedAndCancel(t *testing.T) {
	shortResume(t)
	base, _ := d9Server(t)
	for _, how := range []string{"paused", "cancel"} {
		m := d9Manager(t, tempDir(t))
		a := m.Add(AddReq{URL: base + "/a.bin", Start: true})
		waitFor(t, m, a, StDownloading, 10*time.Second)
		if how == "paused" {
			m.mu.Lock()
			m.settings.StartPaused = true
			m.mu.Unlock()
		} else {
			old := resumeDelay
			resumeDelay = 3 * time.Second
			defer func() { resumeDelay = old }()
		}
		dir := exitLike(t, m, true)
		m2 := d9Manager(t, dir)
		if how == "cancel" {
			eventually(t, "no countdown", func() bool {
				m2.mu.Lock()
				defer m2.mu.Unlock()
				return m2.powerView().Resume != nil
			})
			m2.powerControl("resumeCancel", "")
		}
		time.Sleep(time.Second)
		a2 := byURL(m2, base+"/a.bin")
		m2.mu.Lock()
		st, v := a2.Status, m2.powerView()
		m2.mu.Unlock()
		if st != StPaused || v.Resume != nil {
			t.Fatalf("%s: status %s, countdown %+v", how, st, v.Resume)
		}
		m2.save() // nothing to carry on with any more
		b, _ := os.ReadFile(filepath.Join(dir, "downloads.json"))
		if strings.Contains(string(b), `"resume": true`) {
			t.Fatalf("%s: still marked to carry on", how)
		}
	}
}

// D9: on a metered connection the downloads wait for the normal one, as if it had paused them.
func TestResumeWaitsForMeteredConnection(t *testing.T) {
	shortResume(t)
	var metered atomic.Bool
	t.Cleanup(setMeteredCheck(func() (bool, bool) { return metered.Load(), true })) // (after the managers below have stopped)
	base, _ := d9Server(t)
	m := d9Manager(t, tempDir(t))
	a := m.Add(AddReq{URL: base + "/a.bin", Start: true})
	waitFor(t, m, a, StDownloading, 10*time.Second)
	m.mu.Lock()
	m.settings.Plan.Metered = true // (the option: pause on a metered connection)
	m.mu.Unlock()
	dir := exitLike(t, m, true)

	metered.Store(true) // started again on a phone hotspot
	m2 := d9Manager(t, dir)
	a2 := byURL(m2, base+"/a.bin")
	eventually(t, "not held for the metered connection", func() bool {
		m2.mu.Lock()
		defer m2.mu.Unlock()
		return m2.meterPaused[a2.ID] && m2.sx.resume == nil
	})
	m2.mu.Lock()
	st := a2.Status
	m2.mu.Unlock()
	if st != StPaused {
		t.Fatalf("started on a metered connection: %s", st)
	}
	metered.Store(false) // back on the normal connection
	m2.meteredTick()
	waitFor(t, m2, a2, StDownloading, 10*time.Second)
}
