package main

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Data quotas that match real plans: a daily or monthly allowance (a monthly one starts again on
// the plan's reset day), counting downloads, uploads or both, for one network or all of them.
// Zipline keeps a total per day for each network in usage-days.json (13 months of it), separate
// from the per-minute meter behind the rolling data cap. Bytes moved during free data hours don't
// count. At the warning level the window says so once; at 100% downloads and uploads wait until
// the quota starts again, unless you carry on anyway for the rest of that period.

type QuotaSettings struct {
	Count string  `json:"count"` // what counts towards quotas and the data cap: "both" (default), "down" or "up"
	Warn  int     `json:"warn"`  // say so when this much of a quota is used (percent)
	Rules []Quota `json:"rules"`
}

type Quota struct {
	ID       string `json:"id"`
	On       bool   `json:"on"`
	Network  string `json:"network"`  // the network it's for ("" = every network)
	NetName  string `json:"netName"`  // that network's name, as shown
	Period   string `json:"period"`   // "day" or "month"
	ResetDay int    `json:"resetDay"` // a monthly quota starts again on this day of the month
	MB       int64  `json:"mb"`       // the allowance
}

func countsDown(count string) bool { return count != "up" }
func countsUp(count string) bool   { return count != "down" }

// normalizeQuotas tidies the quota settings. Caller holds m.mu.
func normalizeQuotas(q *QuotaSettings) {
	if q.Count != "down" && q.Count != "up" {
		q.Count = "both"
	}
	if q.Warn < 50 || q.Warn > 99 {
		q.Warn = 80
	}
	seen := map[string]bool{}
	rules := q.Rules[:0]
	for _, r := range q.Rules {
		if r.MB <= 0 {
			continue
		}
		if r.ID == "" || seen[r.ID] {
			r.ID = newID()
		}
		seen[r.ID] = true
		if r.Period != "month" {
			r.Period = "day"
		}
		r.ResetDay = min(max(r.ResetDay, 1), 31)
		r.NetName = strings.TrimSpace(r.NetName)
		rules = append(rules, r)
	}
	q.Rules = rules
}

func daysIn(y int, mo time.Month) int { return time.Date(y, mo+1, 0, 0, 0, 0, 0, time.UTC).Day() }

// period returns the quota's current period: when it started and when it starts again. A monthly
// quota whose reset day is past the end of a short month starts again on that month's last day.
func (q Quota) period(now time.Time) (from, to time.Time) {
	day := dayStart(now, 0)
	if q.Period != "month" {
		return day, day.AddDate(0, 0, 1)
	}
	at := func(y int, mo time.Month) time.Time {
		for mo < 1 {
			y, mo = y-1, mo+12
		}
		for mo > 12 {
			y, mo = y+1, mo-12
		}
		return time.Date(y, mo, min(max(q.ResetDay, 1), daysIn(y, mo)), 0, 0, 0, 0, now.Location())
	}
	y, mo := now.Year(), now.Month()
	from = at(y, mo)
	if day.Before(from) {
		from = at(y, mo-1)
		return from, at(y, mo)
	}
	return from, at(y, mo+1)
}

// periodKey names a quota's current period, for "carry on anyway" and warnings said once.
func (q Quota) periodKey(now time.Time) string {
	from, _ := q.period(now)
	return q.Period + ":" + from.Format("2006-01-02")
}

// ---------------------------------------------------------------- daily totals per network

type dayUse struct {
	Down     int64 `json:"down,omitempty"`
	Up       int64 `json:"up,omitempty"`
	FreeDown int64 `json:"freeDown,omitempty"` // during free data hours
	FreeUp   int64 `json:"freeUp,omitempty"`
}

type netUse struct {
	Name string             `json:"name,omitempty"`
	Days map[string]*dayUse `json:"days"` // "2006-01-02" (local time)
}

// dayMeter keeps the bytes moved per day and network, and what was said about each quota.
type dayMeter struct {
	mu      sync.Mutex
	Nets    map[string]*netUse `json:"networks"`
	CarryOn map[string]string  `json:"carryOn,omitempty"` // quota → the period it carries on regardless
	Told    map[string]string  `json:"told,omitempty"`    // quota → "warn:…"/"stop:…" already said this period
	dirty   bool
}

const keepDays = 400 // a little over 13 months

func newDayMeter(file string) *dayMeter {
	dm := &dayMeter{}
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, dm)
	} else if b, err := os.ReadFile(file + ".bak"); err == nil {
		json.Unmarshal(b, dm)
	}
	if dm.Nets == nil {
		dm.Nets = map[string]*netUse{}
	}
	if dm.CarryOn == nil {
		dm.CarryOn = map[string]string{}
	}
	if dm.Told == nil {
		dm.Told = map[string]string{}
	}
	for k, n := range dm.Nets {
		if n == nil {
			delete(dm.Nets, k)
		} else if n.Days == nil {
			n.Days = map[string]*dayUse{}
		}
	}
	return dm
}

// add counts n bytes moved now on a network; free: inside free data hours.
func (dm *dayMeter) add(net, name string, now time.Time, n int64, up, free bool) {
	if dm == nil || n <= 0 {
		return
	}
	key := now.Format("2006-01-02")
	dm.mu.Lock()
	nu := dm.Nets[net]
	if nu == nil {
		nu = &netUse{Days: map[string]*dayUse{}}
		dm.Nets[net] = nu
	}
	if name != "" {
		nu.Name = name
	}
	d := nu.Days[key]
	if d == nil {
		d = &dayUse{}
		nu.Days[key] = d
	}
	switch {
	case up && free:
		d.Up += n
		d.FreeUp += n
	case up:
		d.Up += n
	case free:
		d.Down += n
		d.FreeDown += n
	default:
		d.Down += n
	}
	dm.dirty = true
	dm.mu.Unlock()
}

// used adds up what counts towards a quota in its current period: downloads and/or uploads on its
// network (or all of them), less what was free. upPaid: uploads count even in free hours.
func (dm *dayMeter) used(q Quota, count string, upPaid bool, now time.Time) int64 {
	if dm == nil {
		return 0
	}
	from, to := q.period(now)
	a, b := from.Format("2006-01-02"), to.Format("2006-01-02")
	dm.mu.Lock()
	defer dm.mu.Unlock()
	var sum int64
	for k, nu := range dm.Nets {
		if q.Network != "" && k != q.Network {
			continue
		}
		for day, d := range nu.Days {
			if day < a || day >= b {
				continue
			}
			if countsDown(count) {
				sum += d.Down - d.FreeDown
			}
			if countsUp(count) {
				sum += d.Up
				if !upPaid {
					sum -= d.FreeUp
				}
			}
		}
	}
	return sum
}

// day returns one day's totals on a network ("" = all networks added up).
func (dm *dayMeter) day(net string, t time.Time) dayUse {
	var out dayUse
	if dm == nil {
		return out
	}
	key := t.Format("2006-01-02")
	dm.mu.Lock()
	defer dm.mu.Unlock()
	for k, nu := range dm.Nets {
		if d := nu.Days[key]; d != nil && (net == "" || k == net) {
			out.Down += d.Down
			out.Up += d.Up
			out.FreeDown += d.FreeDown
			out.FreeUp += d.FreeUp
		}
	}
	return out
}

// networks lists the networks seen, most recently used first.
func (dm *dayMeter) networks() []NetSeen {
	if dm == nil {
		return nil
	}
	dm.mu.Lock()
	defer dm.mu.Unlock()
	var out []NetSeen
	for k, nu := range dm.Nets {
		last := ""
		for day := range nu.Days {
			if day > last {
				last = day
			}
		}
		out = append(out, NetSeen{Key: k, Name: nu.Name, Last: last})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Last != out[j].Last {
			return out[i].Last > out[j].Last
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// NetSeen is a network Zipline has moved data on (Options → Data plan offers them for quotas).
type NetSeen struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Last string `json:"last"` // the last day it was used
}

// prune forgets days older than keepDays. Caller holds dm.mu.
func (dm *dayMeter) prune(now time.Time) {
	cut := now.AddDate(0, 0, -keepDays).Format("2006-01-02")
	for k, nu := range dm.Nets {
		for day := range nu.Days {
			if day < cut {
				delete(nu.Days, day)
				dm.dirty = true
			}
		}
		if len(nu.Days) == 0 {
			delete(dm.Nets, k)
		}
	}
}

func (dm *dayMeter) save(file string) {
	if dm == nil {
		return
	}
	dm.mu.Lock()
	if !dm.dirty {
		dm.mu.Unlock()
		return
	}
	dm.prune(time.Now())
	b, err := json.Marshal(dm)
	dm.dirty = false
	dm.mu.Unlock()
	if err == nil {
		writeAtomic(file, b)
	}
}

// said reports whether something was already said about a quota this period, and remembers it.
func (dm *dayMeter) said(id, what string) bool {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if dm.Told[id] == what {
		return true
	}
	dm.Told[id] = what
	dm.dirty = true
	return false
}

func (dm *dayMeter) carryingOn(id, period string) bool {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	return dm.CarryOn[id] == period
}

func (dm *dayMeter) setCarryOn(id, period string) {
	dm.mu.Lock()
	dm.CarryOn[id] = period
	dm.dirty = true
	dm.mu.Unlock()
}

// ---------------------------------------------------------------- what the window shows

// QuotaView is one quota's state.
type QuotaView struct {
	ID      string    `json:"id"`
	Period  string    `json:"period"` // "day" or "month"
	Net     string    `json:"net"`    // the network's name ("" = every network)
	Used    int64     `json:"used"`
	Limit   int64     `json:"limit"`
	Resets  time.Time `json:"resets"`
	Applies bool      `json:"applies"` // this computer is on its network now
	Warn    bool      `json:"warn"`    // past the warning level
	Over    bool      `json:"over"`    // used up
	CarryOn bool      `json:"carryOn"` // used up, but carrying on until it starts again
}

// QuotasView is the quotas' state for the window: Show is the one the status bar shows.
type QuotasView struct {
	Show    *QuotaView  `json:"show,omitempty"`
	All     []QuotaView `json:"all,omitempty"`
	NetKey  string      `json:"netKey"` // the network this computer is on now
	NetName string      `json:"netName"`
	Free    bool        `json:"free"`  // inside free data hours: nothing counts now
	Count   string      `json:"count"` // "both", "down" or "up"
	Today   dayUse      `json:"today"` // moved today on this network
	Nets    []NetSeen   `json:"nets,omitempty"`
}
