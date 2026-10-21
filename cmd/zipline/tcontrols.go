package main

import (
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// A torrent's own settings: its trackers (ttrackers.go), an upload limit, and how long it keeps
// sharing. They override Options → Torrents for this torrent only. The download limit is the
// ordinary per-download one (Download.LimitKB, /api/limit), which a torrent's peers already keep to.

// torrentExtras is embedded in TorrentState, so these save and load with the torrent.
type torrentExtras struct {
	Tiers     [][]string `json:"tiers,omitempty"`     // the trackers by tier, in the order they're tried
	Key       uint32     `json:"key,omitempty"`       // sent to every tracker (BEP 3): the same one each time
	UpKB      int        `json:"upKB,omitempty"`      // this torrent's own upload limit, KB/s (0 = none)
	Seed      string     `json:"seed,omitempty"`      // "custom": share for the ratio and time below, not Options → Torrents'
	SeedRatio float64    `json:"seedRatio,omitempty"` // …until uploaded/size reaches this
	SeedMins  int        `json:"seedMins,omitempty"`  // …or for this long (either 0: don't share after finishing)
}

// seedCustom is TorrentState.Seed when the torrent has a ratio and time of its own.
const seedCustom = "custom"

// seedTarget is how long a torrent keeps sharing once it has finished: its own settings, or Options → Torrents'.
func seedTarget(s TorrentSettings, ts *TorrentState) (ratio float64, mins int) {
	if ts != nil && ts.Seed == seedCustom {
		return ts.SeedRatio, ts.SeedMins
	}
	return s.SeedRatio, s.SeedMins
}

// tctl is what a running torrent keeps for the above (and for moving it).
type tctl struct {
	cmu    sync.Mutex
	ownUp  atomic.Pointer[Limiter] // this torrent's own upload limit (nil = none)
	moving atomic.Bool             // being moved to another folder: its files are left alone
}

// setUpKB applies the torrent's own upload limit, at once. A limiter's rate is only set when it
// changes: setting it resets its balance.
func (t *torrent) setUpKB(kb int) {
	t.cmu.Lock()
	defer t.cmu.Unlock()
	if kb <= 0 {
		t.ownUp.Store(nil)
		return
	}
	l := t.ownUp.Load()
	if l == nil {
		l = &Limiter{}
	}
	if r := int64(kb) * 1024; l.Rate() != r {
		l.SetRate(r)
	}
	t.ownUp.Store(l)
}

// upWait waits until n bytes may be uploaded: this torrent's own limit, then the shared ones.
func (t *torrent) upWait(n int) {
	if l := t.ownUp.Load(); l != nil {
		l.Wait(t.ctx, n)
	}
	t.m.upWait(t.ctx, n)
}

// torrentCtlView is what the window is told about the above (embedded in TorrentView).
type torrentCtlView struct {
	Tiers     [][]string `json:"tiers,omitempty"` // (only in the details)
	UpKB      int        `json:"upKB,omitempty"`
	Seed      string     `json:"seed,omitempty"`
	SeedRatio float64    `json:"seedRatio,omitempty"`
	SeedMins  int        `json:"seedMins,omitempty"`
}

func ctlView(ts *TorrentState, detail bool) torrentCtlView {
	v := torrentCtlView{UpKB: ts.UpKB, Seed: ts.Seed, SeedRatio: ts.SeedRatio, SeedMins: ts.SeedMins}
	if detail {
		v.Tiers = cloneTiers(ts.Tiers)
	}
	return v
}

// SetTorrentLimits sets a torrent's own upload limit and sharing time; a running torrent keeps to them from now on.
func (m *Manager) SetTorrentLimits(id string, upKB int, seed string, ratio float64, mins int) error {
	if upKB < 0 || upKB > 1<<24 || ratio < 0 || ratio > 1e6 || mins < 0 || mins > 1<<24 || ratio != ratio || seed != "" && seed != seedCustom {
		return errors.New("those limits aren't valid")
	}
	m.mu.Lock()
	d, t := m.byID[id], m.torrents[id]
	if d == nil || d.Torrent == nil {
		m.mu.Unlock()
		return errors.New("not a torrent")
	}
	ts := d.Torrent
	ts.UpKB, ts.Seed, ts.SeedRatio, ts.SeedMins = upKB, seed, ratio, mins
	if seed == "" { // (nothing to remember)
		ts.SeedRatio, ts.SeedMins = 0, 0
	}
	m.dirty = true
	m.mu.Unlock()
	if t != nil {
		t.setUpKB(upKB) // (the sharing time is read again every second)
	}
	return nil
}

// seedNow is how long to share now, as the settings stand (they can change while a torrent shares).
func (t *torrent) seedNow() (float64, time.Duration) {
	t.m.mu.Lock()
	ratio, mins := seedTarget(t.m.settings.Torrent, t.d.Torrent)
	t.m.mu.Unlock()
	return ratio, time.Duration(mins) * time.Minute
}

// ---------------------------------------------------------------- the window's calls

func (s *Server) torrentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/torrent/limits", s.auth(s.torrentLimits))
	mux.HandleFunc("/api/torrent/trackers", s.auth(s.torrentTrackers))
	mux.HandleFunc("/api/move", s.auth(s.moveH))
}

func (s *Server) torrentLimits(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID        string  `json:"id"`
		UpKB      int     `json:"upKB"`
		Seed      string  `json:"seed"`
		SeedRatio float64 `json:"seedRatio"`
		SeedMins  int     `json:"seedMins"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.m.SetTorrentLimits(req.ID, req.UpKB, req.Seed, req.SeedRatio, req.SeedMins); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) torrentTrackers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID    string     `json:"id"`
		Tiers [][]string `json:"tiers"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	tiers, err := s.m.SetTorrentTrackers(req.ID, req.Tiers)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "tiers": tiers})
}
