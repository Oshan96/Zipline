package main

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// Traffic profiles: named sets of limits — overall download and upload speed, downloads started
// by hand at once, connections per download, and whether finished torrents are shared. Switch
// by hand (Options → Connection, the status bar, the tray menu, the phone) or let a schedule pick
// one (Light 09:00–18:00 on weekdays, say); outside the schedule's times the default profile
// applies, and a choice made by hand while the schedule is on lasts until the schedule next
// changes. The Speed Limiter stays an extra overall limit on top of whichever profile is in use.

type TrafficProfile struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	DownKB int    `json:"downKB"` // overall download limit, KB/s (0 = none)
	UpKB   int    `json:"upKB"`   // upload limit, KB/s (0 = none)
	AtOnce int    `json:"atOnce"` // downloads started by hand that run at once (0 = as in Options)
	Conns  int    `json:"conns"`  // connections per download (0 = as in Options)
	NoSeed bool   `json:"noSeed"` // don't share finished torrents
}

type ProfileRule struct {
	Profile string `json:"profile"` // a profile's id
	From    string `json:"from"`    // "09:00"
	To      string `json:"to"`      // "18:00"; earlier than From means it runs past midnight
	Days    []int  `json:"days"`    // the days it starts on (0 = Sunday)
}

type TrafficSettings struct {
	Profiles []TrafficProfile `json:"profiles"`
	Default  string           `json:"default"` // the profile in use (outside the schedule's times)
	Sched    bool             `json:"sched"`   // switch profiles by the rules
	Rules    []ProfileRule    `json:"rules"`
	Override string           `json:"override,omitempty"` // chosen by hand while the schedule is on…
	Until    time.Time        `json:"until,omitempty"`    // …until the schedule next changes
}

func defaultProfiles() []TrafficProfile {
	return []TrafficProfile{
		{ID: "normal", Name: "Normal"},
		{ID: "light", Name: "Light", DownKB: 512, UpKB: 64, AtOnce: 2, Conns: 4, NoSeed: true},
		{ID: "night", Name: "Night", AtOnce: 8},
	}
}

// normalizeTraffic tidies the profiles: at least one, distinct ids and names, sensible numbers,
// rules and choices that point at profiles that exist.
func normalizeTraffic(ts *TrafficSettings) {
	if len(ts.Profiles) == 0 {
		ts.Profiles = defaultProfiles()
	}
	ids, names := map[string]bool{}, map[string]bool{}
	ps := ts.Profiles[:0]
	for _, p := range ts.Profiles {
		if p.ID == "" || ids[p.ID] {
			p.ID = newID()
		}
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			p.Name = "Profile"
		}
		for base, n := p.Name, 2; names[strings.ToLower(p.Name)]; n++ {
			p.Name = base + " " + itoa(n)
		}
		ids[p.ID], names[strings.ToLower(p.Name)] = true, true
		p.DownKB, p.UpKB = max(p.DownKB, 0), max(p.UpKB, 0)
		p.AtOnce, p.Conns = min(max(p.AtOnce, 0), 50), min(max(p.Conns, 0), 32)
		ps = append(ps, p)
	}
	ts.Profiles = ps
	if !ids[ts.Default] {
		ts.Default = ts.Profiles[0].ID
	}
	rules := ts.Rules[:0]
	for _, r := range ts.Rules {
		_, ok1 := clockMinutes(r.From)
		_, ok2 := clockMinutes(r.To)
		if ids[r.Profile] && ok1 && ok2 && len(r.Days) > 0 {
			rules = append(rules, r)
		}
	}
	ts.Rules = rules
	if !ids[ts.Override] || ts.Until.IsZero() {
		ts.Override, ts.Until = "", time.Time{}
	}
}

func (ts *TrafficSettings) profile(id string) TrafficProfile {
	for _, p := range ts.Profiles {
		if p.ID == id {
			return p
		}
	}
	if len(ts.Profiles) > 0 {
		return ts.Profiles[0]
	}
	return TrafficProfile{ID: "normal", Name: "Normal"}
}

// scheduled is the profile the schedule picks at t: the first rule whose times include t, or the
// default one.
func (ts *TrafficSettings) scheduled(t time.Time) string {
	if ts.Sched {
		for _, r := range ts.Rules {
			if _, _, in := (PlanSettings{Start: r.From, End: r.To, Days: r.Days}).windowAt(t); in {
				return r.Profile
			}
		}
	}
	return ts.Default
}

// active is the profile in use at t, and whether it was chosen by hand over the schedule.
func (ts *TrafficSettings) active(t time.Time) (string, bool) {
	if ts.Override != "" && t.Before(ts.Until) {
		return ts.Override, true
	}
	return ts.scheduled(t), false
}

// nextChange is when the schedule next picks another profile (zero: not within a week).
func (ts *TrafficSettings) nextChange(t time.Time) time.Time {
	cur := ts.scheduled(t)
	start := t.Truncate(time.Minute)
	for i := 1; i <= 8*24*60; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		if ts.scheduled(at) != cur {
			return at
		}
	}
	return time.Time{}
}

// applyTraffic puts the profile in use into effect: the overall download limit (with the Speed
// Limiter on top), connections per download, downloads at once (ones started by hand beyond the
// new number go back to waiting their turn). A limiter's rate is set only when it changes:
// setting it starts its allowance afresh. Caller holds m.mu.
func (m *Manager) applyTraffic() {
	g := m.gate
	now := time.Now()
	ts := &m.settings.Traffic
	if ts.Override != "" && !now.Before(ts.Until) { // the schedule takes over again
		ts.Override, ts.Until = "", time.Time{}
		m.dirty = true
	}
	id, _ := ts.active(now)
	p := ts.profile(id)
	old := g.prof.Swap(&p)
	rate := int64(0)
	if m.settings.SpeedLimitOn && m.settings.SpeedLimitKB > 0 {
		rate = int64(m.settings.SpeedLimitKB) * 1024
	}
	if r := int64(p.DownKB) * 1024; r > 0 && (rate == 0 || r < rate) {
		rate = r
	}
	if rate != g.downRate {
		g.downRate = rate
		m.limiter.SetRate(rate)
	}
	m.hosts.setDefault(m.connsWanted())
	if old != nil && m.maxActive() < m.byHand() {
		m.holdExtraByHand()
	}
	m.gateRecompute(now)
}

// maxActive is "downloads at once" for downloads started by hand: the profile's, else Options'.
// Caller holds m.mu.
func (m *Manager) maxActive() int {
	if g := m.gate; g != nil {
		if p := g.prof.Load(); p != nil && p.AtOnce > 0 {
			return p.AtOnce
		}
	}
	return m.settings.MaxActive
}

// connsWanted is connections per download: the profile's, else Options'. Caller holds m.mu.
func (m *Manager) connsWanted() int {
	if g := m.gate; g != nil {
		if p := g.prof.Load(); p != nil && p.Conns > 0 {
			return p.Conns
		}
	}
	return m.settings.Conns
}

// dlConns is how many connections a download may use: the profile's number when it sets one.
func (m *Manager) dlConns(d *Download) int {
	if g := m.gate; g != nil {
		if p := g.prof.Load(); p != nil && p.Conns > 0 {
			return p.Conns
		}
	}
	return d.Conns
}

// upRateWanted is the upload limit in bytes/s: Options → Torrents' and the profile's, whichever
// is lower (0 = none). Caller holds m.mu.
func (m *Manager) upRateWanted() int64 {
	r := int64(m.settings.Torrent.UpKB) * 1024
	if g := m.gate; g != nil {
		if p := g.prof.Load(); p != nil && p.UpKB > 0 && (r == 0 || int64(p.UpKB)*1024 < r) {
			r = int64(p.UpKB) * 1024
		}
	}
	return r
}

// holdExtraByHand sends the downloads started by hand beyond "downloads at once" back to waiting
// their turn, the most recently started first. Caller holds m.mu.
func (m *Manager) holdExtraByHand() {
	extra := m.byHand() - m.maxActive()
	for i := len(m.list) - 1; i >= 0 && extra > 0; i-- {
		d := m.list[i]
		if !d.running || d.Provisional || d.Queue != "" && m.qRun[d.Queue] || m.slowTorrent(d) || d.cancel == nil {
			continue
		}
		d.Waiting, d.Status = true, StQueued
		d.cancel()
		m.dirty = true
		extra--
	}
}

// SetProfile switches to a profile by hand. With the schedule on, the choice lasts until the
// schedule next changes; otherwise it's the profile from now on.
func (m *Manager) SetProfile(id string) error {
	m.mu.Lock()
	ts := &m.settings.Traffic
	found := false
	for _, p := range ts.Profiles {
		found = found || p.ID == id
	}
	if !found {
		m.mu.Unlock()
		return errors.New("that traffic profile isn't there any more")
	}
	now := time.Now()
	switch {
	case !ts.Sched || len(ts.Rules) == 0:
		ts.Default, ts.Override, ts.Until = id, "", time.Time{}
	case ts.scheduled(now) == id:
		ts.Override, ts.Until = "", time.Time{} // back to the schedule
	default:
		ts.Override, ts.Until = id, ts.nextChange(now)
		if ts.Until.IsZero() {
			ts.Until = now.AddDate(0, 0, 7)
		}
	}
	m.applyTraffic()
	m.dirty = true
	m.mu.Unlock()
	m.kick()
	return nil
}

// ProfileView is the traffic profile in use, for the window, the tray and the phone.
type ProfileView struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	ByHand bool             `json:"byHand"` // chosen by hand over the schedule…
	Until  time.Time        `json:"until"`  // …until then
	Sched  bool             `json:"sched"`  // the schedule is on
	List   []TrafficProfile `json:"list"`
}

// profileView: caller holds m.mu.
func (m *Manager) profileView(now time.Time) ProfileView {
	ts := &m.settings.Traffic
	v := ProfileView{Sched: ts.Sched && len(ts.Rules) > 0, List: append([]TrafficProfile{}, ts.Profiles...)}
	if p := m.gate.prof.Load(); p != nil {
		v.ID, v.Name = p.ID, p.Name
	}
	if _, byHand := ts.active(now); byHand {
		v.ByHand, v.Until = true, ts.Until
	}
	return v
}

// profileH is /api/profile: switch to a profile.
func (s *Server) profileH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.m.SetProfile(req.ID); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	s.m.mu.Lock()
	v := s.m.profileView(time.Now())
	s.m.mu.Unlock()
	writeJSON(w, v)
}

// quotaH is /api/quota: carry on anyway with a used-up quota until it starts again.
func (s *Server) quotaH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.m.QuotaCarryOn(req.ID); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// phoneProfile is the traffic profile for the phone page: the one in use and the choices.
func phoneProfile(v ProfileView) map[string]any {
	list := make([]map[string]any, 0, len(v.List))
	for _, p := range v.List {
		list = append(list, map[string]any{"id": p.ID, "name": p.Name})
	}
	out := map[string]any{"id": v.ID, "name": v.Name, "list": list}
	if v.ByHand {
		out["until"] = v.Until.UnixMilli()
	}
	return out
}

// phoneQuota is the quota the status bar shows, for the phone page (nil when none is on).
func phoneQuota(v QuotasView) map[string]any {
	q := v.Show
	if q == nil {
		return nil
	}
	return map[string]any{"period": q.Period, "used": q.Used, "limit": q.Limit, "over": q.Over && !q.CarryOn, "net": q.Net}
}
