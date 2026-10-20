package main

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// D6: the schedule picks the right profile at the right time (2026-10-12 is a Monday).
func TestProfileSchedulePicks(t *testing.T) {
	ts := TrafficSettings{Profiles: defaultProfiles(), Default: "normal", Sched: true, Rules: []ProfileRule{
		{Profile: "light", From: "09:00", To: "18:00", Days: []int{1, 2, 3, 4, 5}},
		{Profile: "night", From: "23:00", To: "07:00", Days: []int{0, 1, 2, 3, 4, 5, 6}},
	}}
	normalizeTraffic(&ts)
	for when, want := range map[string]string{
		"2026-10-12 08:59": "normal", "2026-10-12 09:00": "light", "2026-10-12 17:59": "light", "2026-10-12 18:00": "normal",
		"2026-10-12 23:30": "night", "2026-10-13 06:59": "night", "2026-10-13 07:00": "normal",
		"2026-10-17 12:00": "normal", // Saturday: no Light
	} {
		if got := ts.scheduled(at(when)); got != want {
			t.Errorf("%s: %s, want %s", when, got, want)
		}
	}
	if next := ts.nextChange(at("2026-10-12 10:15")); !next.Equal(at("2026-10-12 18:00")) {
		t.Fatalf("next change %v", next)
	}
	ts.Sched = false
	if got := ts.scheduled(at("2026-10-12 10:00")); got != "normal" {
		t.Fatalf("schedule off: %s", got)
	}
	// chosen by hand: until then
	ts.Sched, ts.Override, ts.Until = true, "night", at("2026-10-12 18:00")
	if id, byHand := ts.active(at("2026-10-12 12:00")); id != "night" || !byHand {
		t.Fatalf("by hand: %s %v", id, byHand)
	}
	if id, byHand := ts.active(at("2026-10-12 18:00")); id != "normal" || byHand {
		t.Fatalf("after: %s %v", id, byHand)
	}
}

// D6: switching applies the limits at once (on top of the Speed Limiter), and setting the same
// profile again doesn't start the limiters' allowance afresh.
func TestProfileSwitchAppliesLimits(t *testing.T) {
	m := newTestManager(t)
	if err := m.SetProfile("light"); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	down, atOnce, conns, up := m.limiter.Rate(), m.maxActive(), m.connsWanted(), m.upRateWanted()
	def := m.hosts.def.Load()
	m.mu.Unlock()
	if down != 512<<10 || atOnce != 2 || conns != 4 || int(def) != 4 || up != 64<<10 || m.upLimiter().Rate() != 64<<10 {
		t.Fatalf("Light: down %d, at once %d, conns %d (hosts %d), up %d", down, atOnce, conns, def, up)
	}
	if v := m.Snapshot(nil).Profile; v.ID != "light" || v.Name != "Light" || len(v.List) != 3 {
		t.Fatalf("view %+v", v)
	}
	// the Speed Limiter stays an overall limit on top
	m.mu.Lock()
	m.settings.SpeedLimitOn, m.settings.SpeedLimitKB = true, 300
	m.settings.Torrent.UpKB = 20
	m.applyLimit()
	down, up = m.limiter.Rate(), m.upRateWanted()
	last := m.limiter.last
	m.applyLimit() // nothing changed: the limiter keeps its allowance
	same := m.limiter.last.Equal(last)
	m.mu.Unlock()
	if down != 300<<10 || up != 20<<10 || !same {
		t.Fatalf("with the Speed Limiter: down %d, up %d, limiter reset %v", down, up, !same)
	}
	m.SetProfile("normal")
	m.mu.Lock()
	m.settings.SpeedLimitOn = false
	m.applyLimit()
	down, atOnce, conns = m.limiter.Rate(), m.maxActive(), m.connsWanted()
	m.mu.Unlock()
	if down != 0 || atOnce != m.settings.MaxActive || conns != m.settings.Conns {
		t.Fatalf("Normal: down %d, at once %d, conns %d", down, atOnce, conns)
	}
	if err := m.SetProfile("nope"); err == nil {
		t.Fatal("an unknown profile")
	}
}

// D6: a profile with fewer downloads at once sends the extra ones back to wait their turn; they
// carry on when it's over.
func TestProfileDownloadsAtOnce(t *testing.T) {
	data := make([]byte, 32<<20)
	rand.Read(data)
	srv := testServer(data, 256<<10, true)
	defer srv.Close()
	m := newTestManager(t)
	var ds []*Download
	for i := 0; i < 4; i++ {
		ds = append(ds, m.Add(AddReq{URL: srv.URL + "/f" + itoa(i) + ".bin", Start: true}))
	}
	for _, d := range ds {
		waitFor(t, m, d, StDownloading, 10*time.Second)
	}
	m.SetProfile("light") // 2 at once
	count := func() (running, waiting int) {
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, d := range ds {
			if d.running {
				running++
			}
			if d.Waiting {
				waiting++
			}
		}
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for r, w := count(); (r != 2 || w != 2) && time.Now().Before(deadline); r, w = count() {
		time.Sleep(50 * time.Millisecond)
	}
	if r, w := count(); r != 2 || w != 2 {
		t.Fatalf("Light: %d running, %d waiting", r, w)
	}
	m.SetProfile("night") // 8 at once
	deadline = time.Now().Add(5 * time.Second)
	for r, _ := count(); r != 4 && time.Now().Before(deadline); r, _ = count() {
		time.Sleep(50 * time.Millisecond)
	}
	if r, w := count(); r != 4 || w != 0 {
		t.Fatalf("Night: %d running, %d waiting", r, w)
	}
	var ids []string
	for _, d := range ds {
		ids = append(ids, d.ID)
	}
	m.Pause(ids)
}

// D6: profiles and their schedule go through the settings API and survive a restart; a choice
// made by hand isn't lost when Options is saved.
func TestProfileSettingsRoundTrip(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	st := apiGet(t, h, "settings")
	tr := st["traffic"].(map[string]any)
	if len(tr["profiles"].([]any)) != 3 || tr["default"] != "normal" {
		t.Fatalf("defaults: %v", tr)
	}
	profiles := append(tr["profiles"].([]any), map[string]any{"name": "  Weekend  ", "downKB": 2048, "upKB": 0, "atOnce": 3, "conns": 8, "noSeed": false})
	profiles = append(profiles, map[string]any{"id": "x", "name": "weekend"}) // the same name again
	tr["profiles"], tr["sched"] = profiles, true
	tr["rules"] = []any{map[string]any{"profile": "light", "from": "09:00", "to": "18:00", "days": []int{1, 2, 3, 4, 5}},
		map[string]any{"profile": "gone", "from": "01:00", "to": "02:00", "days": []int{1}}}
	st["quota"] = map[string]any{"count": "down", "warn": 90, "rules": []any{map[string]any{"on": true, "period": "month", "resetDay": 15, "mb": 102400, "netName": "Home"}}}
	st["torrent"].(map[string]any)["maxSeeds"] = 3
	st["torrent"].(map[string]any)["metaMins"] = 30
	apiCall(t, h, "settings", st)
	m.mu.Lock()
	got := m.settings
	m.mu.Unlock()
	ts := got.Traffic
	if len(ts.Profiles) != 5 || ts.Profiles[3].Name != "Weekend" || ts.Profiles[3].ID == "" || ts.Profiles[4].Name != "weekend 2" || len(ts.Rules) != 1 || !ts.Sched {
		t.Fatalf("traffic: %+v", ts)
	}
	q := got.Quota
	if q.Count != "down" || q.Warn != 90 || len(q.Rules) != 1 || q.Rules[0].ID == "" || q.Rules[0].Period != "month" || q.Rules[0].ResetDay != 15 {
		t.Fatalf("quota: %+v", q)
	}
	if got.Torrent.MaxSeeds != 3 || got.Torrent.MetaMins != 30 {
		t.Fatalf("torrents: %+v", got.Torrent)
	}
	// chosen by hand from the status bar while Options is open: saving Options keeps it
	weekend := ts.Profiles[3].ID
	stale := apiGet(t, h, "settings")
	r := apiCall(t, h, "profile", map[string]any{"id": weekend})
	if r["id"] != weekend {
		t.Fatalf("switch: %v", r)
	}
	apiCall(t, h, "settings", stale)
	m.mu.Lock()
	id, _ := m.settings.Traffic.active(time.Now())
	m.mu.Unlock()
	if id != weekend {
		t.Fatalf("after saving Options: %s", id)
	}
	m.save()
	again := NewManager(m.dataDir)
	defer again.Quit()
	again.mu.Lock()
	defer again.mu.Unlock()
	if len(again.settings.Traffic.Profiles) != 5 || len(again.settings.Quota.Rules) != 1 || again.gate.prof.Load().ID != weekend {
		t.Fatalf("after a restart: %+v / %+v", again.settings.Traffic, again.gate.prof.Load())
	}
}

// D6: switching from the phone, and the phone page shows the profile in use.
func TestProfileFromPhone(t *testing.T) {
	m, pc, _ := phoneManager(t)
	pc.do("POST", "hello", map[string]any{"name": "Test phone"}, nil)
	if code, body := pc.do("POST", "profile", map[string]any{"id": "night"}, nil); code != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("switch: %d %s", code, body)
	}
	_, body := pc.do("GET", "state", nil, nil)
	var st struct {
		Profile struct {
			ID   string           `json:"id"`
			Name string           `json:"name"`
			List []map[string]any `json:"list"`
		} `json:"profile"`
	}
	json.Unmarshal([]byte(body), &st)
	if st.Profile.ID != "night" || st.Profile.Name != "Night" || len(st.Profile.List) != 3 {
		t.Fatalf("phone state: %s", body)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.maxActive() != 8 {
		t.Fatal("not applied")
	}
}

// D6: with the schedule on, the gate switches when a rule starts, and a choice made by hand
// lasts until the schedule's next change.
func TestProfileScheduleSwitches(t *testing.T) {
	m := newTestManager(t)
	now := time.Now()
	m.mu.Lock()
	m.settings.Traffic.Sched = true
	m.settings.Traffic.Rules = []ProfileRule{{Profile: "light", From: now.Add(-time.Hour).Format("15:04"), To: now.Add(time.Hour).Format("15:04"), Days: []int{0, 1, 2, 3, 4, 5, 6}}}
	m.normalizeSettings()
	m.mu.Unlock()
	m.gateTick(time.Now())
	if p := m.gate.prof.Load(); p.ID != "light" {
		t.Fatalf("the rule's profile: %s", p.ID)
	}
	m.mu.Lock()
	told := false
	for _, n := range m.notices {
		told = told || n.Kind == "profile" && n.ID == "light"
	}
	m.mu.Unlock()
	if !told {
		t.Fatal("the window wasn't told")
	}
	m.SetProfile("night")
	v := m.Snapshot(nil).Profile
	if v.ID != "night" || !v.ByHand || v.Until.Sub(now) > time.Hour+time.Minute || v.Until.Before(now) {
		t.Fatalf("by hand: %+v", v)
	}
	m.mu.Lock()
	m.settings.Traffic.Until = time.Now().Add(-time.Second) // the rule ended
	m.mu.Unlock()
	m.gateTick(time.Now())
	if p := m.gate.prof.Load(); p.ID != "light" || m.Snapshot(nil).Profile.ByHand {
		t.Fatalf("after the hand choice ran out: %s", p.ID)
	}
}

func apiGet(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/"+path, nil)
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}
