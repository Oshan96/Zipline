package main

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// "When finished" actions: exit Zipline, sleep, hibernate or turn the computer off, asked for
// by a queue its schedule started, or once by "When all downloads finish" (Tasks menu). An
// action first waits until nothing else downloads (another queue, a download started by hand),
// then counts down for a minute so whoever is at the computer can cancel it: a dialog in the
// window, a notice on the phone, a balloon from the tray icon. With nobody there it runs when
// the countdown ends.

// pendingAction is an action waiting for other downloads, or counting down.
type pendingAction struct {
	action string    // quit, sleep, hibernate, shutdown
	why    string    // what finished
	at     time.Time // the countdown ends then; zero = waiting for other downloads
}

// actionDelay is the countdown before an action runs (tests shorten it).
var actionDelay = 60 * time.Second

// Hooks for the actions, so tests never power anything off.
var (
	sleepHook     = sleepComputer
	hibernateHook = hibernateComputer
)

// actionRank: when several actions are asked for, the stronger one wins.
var actionRank = map[string]int{"quit": 1, "sleep": 2, "hibernate": 3, "shutdown": 4}

func actionWords(a string) string {
	switch a {
	case "quit":
		return "Exiting Zipline"
	case "sleep":
		return "Putting the computer to sleep"
	case "hibernate":
		return "Hibernating the computer"
	case "shutdown":
		return "Turning off the computer"
	}
	return ""
}

// requestPower asks for an action once nothing else downloads. Caller holds m.mu.
func (m *Manager) requestPower(action, why string) {
	if actionRank[action] == 0 {
		return
	}
	if p := m.sx.pend; p != nil && actionRank[p.action] >= actionRank[action] {
		return
	}
	m.sx.pend = &pendingAction{action: action, why: why}
	logf("when finished: %s (%s)", action, why)
}

// busy: something downloads or is about to (a running queue, a download waiting for a slot or
// for another try, the start-up resume). Caller holds m.mu.
func (m *Manager) busy() bool {
	for _, d := range m.list {
		if d.running || d.Waiting {
			return true
		}
	}
	for _, q := range m.settings.Queues {
		if m.qRun[q.ID] || m.qSyncing[q.ID] {
			return true
		}
	}
	return m.sx.resume != nil
}

// powerLoop runs the retries and the "when finished" actions.
func (m *Manager) powerLoop() {
	powerActionsInit()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.quit:
			return
		case now := <-t.C:
			m.retryTick(now)
			m.powerTick(now)
		}
	}
}

// powerTick fires "When all downloads finish", and starts, pauses or ends the countdown.
func (m *Manager) powerTick(now time.Time) {
	m.mu.Lock()
	busy := m.busy()
	if m.sx.allDone != "" {
		if busy {
			m.sx.allBusy = true
		} else if m.sx.allBusy {
			m.requestPower(m.sx.allDone, "All downloads finished")
			m.sx.allDone, m.sx.allBusy = "", false // one-shot
		}
	}
	run, balloon := "", ""
	switch p := m.sx.pend; {
	case p == nil:
	case busy:
		p.at = time.Time{} // something started again: wait for it to finish
	case p.at.IsZero():
		p.at = now.Add(actionDelay)
		balloon = fmt.Sprintf("%s in %d seconds. %s.", actionWords(p.action), int(actionDelay.Seconds()), p.why)
		m.notify("power", p.action, balloon)
	case !now.Before(p.at):
		run = p.action
		m.sx.pend = nil
	}
	m.mu.Unlock()
	if balloon != "" {
		trayNotify("Zipline", balloon+" Right-click the Zipline icon to cancel.", true)
	}
	if run != "" {
		m.doPower(run)
	}
}

func (m *Manager) doPower(a string) {
	logf("when finished: %s now", a)
	switch a {
	case "quit":
		m.Quit()
	case "sleep":
		go sleepHook()
	case "hibernate":
		go hibernateHook()
	case "shutdown":
		go shutdownHook()
	}
}

// powerControl carries out the window's and the phone's choices: "whenDone" sets the one-shot
// "When all downloads finish" (value: none, quit, sleep, hibernate, shutdown), "cancel" and
// "now" answer the countdown, "resumeCancel" and "resumeNow" the start-up resume.
func (m *Manager) powerControl(action, value string) error {
	m.mu.Lock()
	run := ""
	switch action {
	case "whenDone":
		if value != "none" && actionRank[value] == 0 {
			m.mu.Unlock()
			return errors.New("unknown action")
		}
		m.sx.allDone, m.sx.allBusy = value, m.busy()
		if value == "none" {
			m.sx.allDone = ""
		}
	case "cancel":
		if p := m.sx.pend; p != nil {
			logf("when finished: %s cancelled", p.action)
		}
		m.sx.pend, m.sx.allDone, m.sx.allBusy = nil, "", false
	case "now":
		if p := m.sx.pend; p != nil {
			run, m.sx.pend = p.action, nil
		}
	case "resumeCancel":
		m.sx.resume = nil
	case "resumeNow":
		if p := m.sx.resume; p != nil {
			p.at = time.Now()
		}
	default:
		m.mu.Unlock()
		return errors.New("unknown action")
	}
	m.mu.Unlock()
	if run != "" {
		m.doPower(run)
	}
	return nil
}

// PowerView is what the window and the phone show about power actions and the start-up resume.
type PowerView struct {
	WhenAllDone string       `json:"whenAllDone,omitempty"` // the one-shot "When all downloads finish"
	Can         []string     `json:"can"`                   // actions this computer can take
	Pending     *PendingView `json:"pending,omitempty"`
	Resume      *ResumeView  `json:"resume,omitempty"`
}

type PendingView struct {
	Action string `json:"action"`
	Text   string `json:"text"` // "Turning off the computer"
	Why    string `json:"why"`
	At     int64  `json:"at,omitempty"` // the countdown ends then (Unix ms); 0 = waiting for other downloads
}

type ResumeView struct {
	Downloads int      `json:"downloads"`
	Sharing   int      `json:"sharing,omitempty"` // finished torrents that carry on sharing
	Queues    []string `json:"queues,omitempty"`
	At        int64    `json:"at"` // carries on then (Unix ms)
}

// powerView: caller holds m.mu.
func (m *Manager) powerView() PowerView {
	v := PowerView{WhenAllDone: m.sx.allDone, Can: powerActions()}
	if p := m.sx.pend; p != nil {
		v.Pending = &PendingView{Action: p.action, Text: actionWords(p.action), Why: p.why}
		if !p.at.IsZero() {
			v.Pending.At = p.at.UnixMilli()
		}
	}
	if p := m.sx.resume; p != nil && !p.at.IsZero() {
		v.Resume = &ResumeView{Downloads: len(p.ids), Sharing: len(p.shares), At: p.at.UnixMilli()}
		for _, rq := range p.queues {
			if q := m.queueByID(rq.ID); q != nil {
				v.Resume.Queues = append(v.Resume.Queues, q.Name)
			}
		}
	}
	return v
}

// phonePower is the phone's notice: an action counting down or waiting, or the start-up resume.
func (m *Manager) phonePower() map[string]any {
	m.mu.Lock()
	v := m.powerView()
	m.mu.Unlock()
	out := map[string]any{}
	if p := v.Pending; p != nil {
		out["pending"] = map[string]any{"text": p.Text, "why": p.Why, "at": p.At}
	}
	if r := v.Resume; r != nil {
		var what []string
		if r.Downloads > 0 {
			what = append(what, plural(r.Downloads, "download"))
		}
		if r.Sharing > 0 {
			what = append(what, "sharing "+plural(r.Sharing, "torrent"))
		}
		for _, q := range r.Queues {
			what = append(what, iso(q))
		}
		out["resume"] = map[string]any{"text": "Resuming " + andList(what), "at": r.At}
	}
	return out
}

// andList: "a", "a and b", "a, b and c".
func andList(s []string) string {
	if len(s) < 2 {
		return strings.Join(s, "")
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

// powerH: the window's "When all downloads finish" choice and its countdown buttons.
func (s *Server) powerH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		Value  string `json:"value"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.m.powerControl(req.Action, req.Value); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// cancelLabel is the tray menu's item for cancelling an action ("" when there's none).
func (m *Manager) cancelLabel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.sx.pend; p != nil {
		t := actionWords(p.action)
		return "Cancel " + strings.ToLower(t[:1]) + t[1:]
	}
	return ""
}
