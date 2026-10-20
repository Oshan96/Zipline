package main

import (
	"strconv"
	"strings"
	"time"
)

// Data plans with free hours (for example SLT-Mobitel's free data from midnight to 7am, or a
// night-time package): downloads in the "Free data hours" queue run only inside that window,
// stop a few minutes before it ends so nothing spills into paid data, and carry on the next
// time. Zipline learns the night-time speed to estimate how many nights a backlog takes.
//
// Separately, downloads can pause while the computer is on a metered connection such as a
// phone hotspot, and carry on once it's back on the normal connection.

const planQueueID = "freehours"

type PlanSettings struct {
	On      bool   `json:"on"`
	Start   string `json:"start"`   // "00:00"
	End     string `json:"end"`     // "07:00"; earlier than Start means it runs past midnight
	Days    []int  `json:"days"`    // days a free window starts on (0 = Sunday)
	Margin  int    `json:"margin"`  // stop this many minutes before the end
	Metered bool   `json:"metered"` // pause downloads on a metered connection
	UpPaid  bool   `json:"upPaid"`  // uploads count even in free hours (off: they're free too)
	AvgKBs  int    `json:"avgKBs,omitempty"`
}

func defaultPlan() PlanSettings {
	return PlanSettings{Start: "00:00", End: "07:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, Margin: 5}
}

func clockMinutes(s string) (int, bool) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, false
	}
	return hh*60 + mm, true
}

// span returns the window's start (minutes after midnight) and usable length in minutes,
// with the safety margin taken off the end. Equal start and end means all day.
func (p PlanSettings) span() (start, length int) {
	s, ok1 := clockMinutes(p.Start)
	e, ok2 := clockMinutes(p.End)
	if !ok1 || !ok2 {
		return 0, 0
	}
	l := ((e-s)%1440 + 1440) % 1440
	if l == 0 {
		l = 1440
	}
	return s, max(l-max(p.Margin, 0), 1)
}

func (p PlanSettings) startsOn(d time.Weekday) bool {
	for _, x := range p.Days {
		if x == int(d) {
			return true
		}
	}
	return false
}

func dayStart(t time.Time, back int) time.Time {
	y, mo, d := t.Date()
	return time.Date(y, mo, d-back, 0, 0, 0, 0, t.Location())
}

// windowAt returns the free window that contains t, if any.
func (p PlanSettings) windowAt(t time.Time) (from, to time.Time, ok bool) {
	start, l := p.span()
	if l == 0 {
		return
	}
	for back := 0; back <= 1; back++ {
		day := dayStart(t, back)
		if !p.startsOn(day.Weekday()) {
			continue
		}
		f := day.Add(time.Duration(start) * time.Minute)
		e := f.Add(time.Duration(l) * time.Minute)
		if !t.Before(f) && t.Before(e) {
			return f, e, true
		}
	}
	return
}

// nextWindow returns the next free window that starts after t.
func (p PlanSettings) nextWindow(t time.Time) (from, to time.Time, ok bool) {
	start, l := p.span()
	if l == 0 {
		return
	}
	for i := 0; i <= 8; i++ {
		day := dayStart(t, -i)
		f := day.Add(time.Duration(start) * time.Minute)
		if p.startsOn(day.Weekday()) && f.After(t) {
			return f, f.Add(time.Duration(l) * time.Minute), true
		}
	}
	return
}

// estimateFinish works out when remaining bytes will be done at speed bytes/s, counting only
// free hours. It returns the finish time and how many windows (nights) it takes.
func (p PlanSettings) estimateFinish(now time.Time, remaining, speed int64) (time.Time, int, bool) {
	if remaining <= 0 || speed <= 0 {
		return time.Time{}, 0, false
	}
	need := float64(remaining) / float64(speed) // seconds of free time needed
	t := now
	for n := 1; n <= 60; n++ {
		from, to, in := p.windowAt(t)
		if !in {
			var ok bool
			if from, to, ok = p.nextWindow(t); !ok {
				return time.Time{}, 0, false
			}
		}
		if from.Before(t) {
			from = t
		}
		avail := to.Sub(from).Seconds()
		if need <= avail {
			return from.Add(time.Duration(need * float64(time.Second))), n, true
		}
		need -= avail
		t = to
	}
	return time.Time{}, 0, false
}

// PlanView is what the window shows about the plan.
type PlanView struct {
	On        bool      `json:"on"`
	In        bool      `json:"in"`   // inside the free hours now
	From      time.Time `json:"from"` // the current or next free window
	To        time.Time `json:"to"`
	Items     int       `json:"items"`     // unfinished downloads in the Free data hours queue
	Remaining int64     `json:"remaining"` // their bytes still to fetch (known sizes)
	Finish    time.Time `json:"finish"`    // estimated finish (zero = unknown)
	Nights    int       `json:"nights"`
	UsedFree  int64     `json:"usedFree"` // downloaded in the current (or last) free window
	UsedPaid  int64     `json:"usedPaid"` // downloaded since the last free window ended
	Metered   bool      `json:"metered"`  // on a metered connection now
	MeterKnow bool      `json:"meterKnown"`
	Paused    int       `json:"paused"` // downloads paused because of the metered connection
}

// ensurePlanQueue creates the Free data hours queue when the plan is switched on. Caller holds m.mu.
func (m *Manager) ensurePlanQueue() {
	if !m.settings.Plan.On || m.queueByID(planQueueID) != nil {
		return
	}
	m.settings.Queues = append(m.settings.Queues, Queue{ID: planQueueID, Name: "Free data hours", MaxConcurrent: 3, Sched: defaultSchedule()})
}

// planTick starts and stops the Free data hours queue with the window. Called from the loop.
func (m *Manager) planTick(now time.Time) {
	m.mu.Lock()
	p := m.settings.Plan
	q := m.queueByID(planQueueID)
	in := false
	if p.On && q != nil {
		_, _, in = p.windowAt(now)
	}
	was := m.planIn
	m.planIn = in
	running := m.qRun[planQueueID]
	queued, speed := 0, 0.0
	for _, d := range m.list {
		if d.Queue == planQueueID {
			if d.Status == StQueued && !d.running {
				queued++
			}
			if d.running {
				speed += d.speed
			}
		}
	}
	// learn the free-hours speed (about a two-minute average) for the estimates
	if in && speed > 0 {
		avg := float64(p.AvgKBs) * 1024
		if avg == 0 {
			avg = speed
		}
		avg = avg*0.98 + speed*0.02
		if kb := int(avg / 1024); kb != m.settings.Plan.AvgKBs {
			m.settings.Plan.AvgKBs = kb
			if now.Sub(m.planSaved) > time.Minute {
				m.planSaved, m.dirty = now, true
			}
		}
	}
	m.mu.Unlock()
	switch {
	case in && !was:
		m.StartQueue(planQueueID, true)
		m.mu.Lock()
		m.notify("planStart", planQueueID, "Free data hours started")
		m.mu.Unlock()
	case !in && was:
		n := m.countRunning(planQueueID)
		m.StopQueue(planQueueID)
		if n > 0 {
			m.mu.Lock()
			m.notify("planEnd", planQueueID, "Free data hours are ending — "+plural(n, "download")+" will carry on next time")
			m.mu.Unlock()
		}
	case in && !running && queued > 0: // added during the window, after the queue finished
		m.StartQueue(planQueueID, true)
	}
}

// saverMode: the user is saving data right now (outside the free hours of their plan, or on a
// metered connection they asked Zipline to spare), so nothing starts unless they say so. The
// Add dialog doesn't start early then. Caller holds m.mu.
func (m *Manager) saverMode(now time.Time) bool {
	p := m.settings.Plan
	if p.Metered && m.metered {
		return true
	}
	if p.On && m.queueByID(planQueueID) != nil {
		_, _, in := p.windowAt(now)
		return !in
	}
	return false
}

func (m *Manager) countRunning(qid string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, d := range m.list {
		if d.Queue == qid && d.running {
			n++
		}
	}
	return n
}

// iso sets a name (a file, a queue, a phone) apart from the sentence around it with Unicode's isolate marks, so
// a name in a right-to-left script can't scramble the words next to it. Windows and browsers draw the marks as nothing.
func iso(name string) string { return "\u2068" + name + "\u2069" }

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// planView summarises the plan for the window. Caller holds m.mu.
func (m *Manager) planView(now time.Time) PlanView {
	p := m.settings.Plan
	v := PlanView{On: p.On, Metered: m.metered, MeterKnow: m.meterKnown, Paused: len(m.meterPaused)}
	if !p.On {
		return v
	}
	from, to, in := p.windowAt(now)
	if !in {
		from, to, _ = p.nextWindow(now)
	}
	v.In, v.From, v.To = in, from, to
	for _, d := range m.list {
		if d.Queue != planQueueID || d.Status == StCompleted {
			continue
		}
		v.Items++
		if d.Size > 0 {
			v.Remaining += max(d.Size-d.done, 0)
		} else if d.Kind == "hls" && d.Parts > 0 && d.PartsDone > 0 {
			_, est := hlsProgress(d)
			v.Remaining += max(est-d.done, 0)
		}
	}
	if fin, n, ok := p.estimateFinish(now, v.Remaining, int64(p.AvgKBs)*1024); ok {
		v.Finish, v.Nights = fin, n
	}
	// usage: this (or the last) free window, and paid data since it ended
	if in {
		v.UsedFree = m.usage.between(from, now)
	} else if pf, pt := p.previousWindow(now); !pf.IsZero() {
		v.UsedFree = m.usage.between(pf, pt)
		v.UsedPaid = m.usage.between(pt, now)
	}
	return v
}

// previousWindow returns the most recent free window that ended before t.
func (p PlanSettings) previousWindow(t time.Time) (from, to time.Time) {
	start, l := p.span()
	if l == 0 {
		return
	}
	for back := 0; back <= 8; back++ {
		day := dayStart(t, back)
		f := day.Add(time.Duration(start) * time.Minute)
		e := f.Add(time.Duration(l) * time.Minute)
		if p.startsOn(day.Weekday()) && !e.After(t) {
			return f, e
		}
	}
	return
}
