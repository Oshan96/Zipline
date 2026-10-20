package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSeedTarget(t *testing.T) {
	g := TorrentSettings{SeedRatio: 1.5, SeedMins: 90}
	for _, c := range []struct {
		ts    *TorrentState
		ratio float64
		mins  int
	}{
		{nil, 1.5, 90},
		{&TorrentState{}, 1.5, 90},
		{&TorrentState{torrentExtras: torrentExtras{SeedRatio: 9, SeedMins: 9}}, 1.5, 90}, // (not switched on)
		{&TorrentState{torrentExtras: torrentExtras{Seed: seedCustom, SeedRatio: 3, SeedMins: 600}}, 3, 600},
		{&TorrentState{torrentExtras: torrentExtras{Seed: seedCustom}}, 0, 0}, // "don't share" is a choice too
	} {
		if r, m := seedTarget(g, c.ts); r != c.ratio || m != c.mins {
			t.Errorf("%+v: %v %v, want %v %v", c.ts, r, m, c.ratio, c.mins)
		}
	}
}

// The torrent's own settings are saved with it, and only when it has any.
func TestTorrentExtrasSaved(t *testing.T) {
	b, _ := json.Marshal(&TorrentState{InfoHash: "ab", torrentExtras: torrentExtras{UpKB: 50, Seed: seedCustom, SeedRatio: 2.5, SeedMins: 30, Key: 7,
		Tiers: [][]string{{"http://a/x"}}}})
	for _, want := range []string{`"upKB":50`, `"seed":"custom"`, `"seedRatio":2.5`, `"seedMins":30`, `"key":7`, `"tiers":[["http://a/x"]]`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s isn't in %s", want, b)
		}
	}
	var back TorrentState
	if err := json.Unmarshal(b, &back); err != nil || back.UpKB != 50 || back.Seed != seedCustom || back.SeedRatio != 2.5 || back.Key != 7 || len(back.Tiers) != 1 {
		t.Fatalf("%+v %v", back, err)
	}
	b, _ = json.Marshal(&TorrentState{InfoHash: "ab"})
	if string(b) != `{"infoHash":"ab"}` {
		t.Errorf("nothing to remember, yet: %s", b)
	}
}

func TestTorrentLimitsAPI(t *testing.T) {
	m := torrentManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	d := addStopped(t, m, idleTorrent(t), nil)
	for _, bad := range []map[string]any{
		{"upKB": -1}, {"seed": "forever"}, {"seed": "custom", "seedRatio": -2}, {"seed": "custom", "seedMins": -5}, {"upKB": 1 << 30},
	} {
		bad["id"] = d.ID
		if r := apiCall(t, h, "torrent/limits", bad); r["error"] == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if r := apiCall(t, h, "torrent/limits", map[string]any{"id": "nope"}); r["error"] == nil {
		t.Error("an unknown download")
	}
	r := apiCall(t, h, "torrent/limits", map[string]any{"id": d.ID, "upKB": 80, "seed": "custom", "seedRatio": 2.5, "seedMins": 45})
	if r["ok"] != true {
		t.Fatal(r)
	}
	m.mu.Lock()
	ts := *d.Torrent
	m.mu.Unlock()
	if ts.UpKB != 80 || ts.Seed != seedCustom || ts.SeedRatio != 2.5 || ts.SeedMins != 45 {
		t.Fatalf("%+v", ts)
	}
	var v *TorrentView
	for _, x := range m.Snapshot(nil).Downloads {
		if x.ID == d.ID {
			v = x.Torrent
		}
	}
	if v == nil || v.UpKB != 80 || v.Seed != seedCustom || v.SeedRatio != 2.5 || v.SeedMins != 45 {
		t.Fatalf("the list shows %+v", v)
	}
	// back to Options → Torrents' settings: nothing is kept
	apiCall(t, h, "torrent/limits", map[string]any{"id": d.ID, "upKB": 0, "seed": "", "seedRatio": 7, "seedMins": 7})
	m.mu.Lock()
	ts = *d.Torrent
	m.mu.Unlock()
	if ts.UpKB != 0 || ts.Seed != "" || ts.SeedRatio != 0 || ts.SeedMins != 0 {
		t.Fatalf("%+v", ts)
	}
}

// A torrent's own upload limit holds at once, and is lifted at once.
func TestTorrentOwnUploadLimit(t *testing.T) {
	tor, _, _ := webSeedTorrent(t)
	m := torrentManager(t)
	d, _ := addTorrentBytes(t, m, tor, nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	tr := runningTorrent(t, m, d.ID) // sharing
	send := func(n int) time.Duration {
		start := time.Now()
		for range n {
			tr.upWait(16 << 10)
		}
		return time.Since(start)
	}
	if took := send(8); took > 200*time.Millisecond {
		t.Fatalf("no limit yet, but %v", took)
	}
	if err := m.SetTorrentLimits(d.ID, 64, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	// 4 blocks of 16 KiB at 64 KB/s: a second (the first block waits for its share too)
	if took := send(4); took < 700*time.Millisecond || took > 3*time.Second {
		t.Fatalf("64 KB/s: 64 KB took %v", took)
	}
	if err := m.SetTorrentLimits(d.ID, 0, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if took := send(8); took > 200*time.Millisecond {
		t.Fatalf("lifted, but %v", took)
	}
	tr.setUpKB(32)
	if l := tr.ownUp.Load(); l == nil || l.Rate() != 32<<10 {
		t.Fatalf("limiter: %v", l)
	}
	tr.setUpKB(32) // (the same rate again keeps the limiter as it is)
	l := tr.ownUp.Load()
	tr.setUpKB(32)
	if tr.ownUp.Load() != l {
		t.Fatal("the limiter was replaced")
	}
}

// A torrent's own ratio and time decide how long it shares, whatever Options → Torrents says.
func TestTorrentOwnSeedLimits(t *testing.T) {
	tor, _, _ := webSeedTorrent(t)

	// Options say "share"; this one says "don't": it stops as soon as it has finished
	m := torrentManager(t)
	d := addStopped(t, m, tor, nil)
	if err := m.SetTorrentLimits(d.ID, 0, seedCustom, 0, 0); err != nil {
		t.Fatal(err)
	}
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	waitCond(t, 10*time.Second, "it stops sharing at once", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.torrents[d.ID] == nil
	})

	// Options say "don't"; this one says "share": it does
	m2 := torrentManager(t)
	m2.mu.Lock()
	m2.settings.Torrent.SeedRatio = 0
	m2.mu.Unlock()
	d2 := addStopped(t, m2, tor, nil)
	if err := m2.SetTorrentLimits(d2.ID, 0, seedCustom, 5, 600); err != nil {
		t.Fatal(err)
	}
	m2.Start([]string{d2.ID})
	waitFor(t, m2, d2, StCompleted, 60*time.Second)
	tr := runningTorrent(t, m2, d2.ID)
	time.Sleep(2500 * time.Millisecond)
	tr.mu.Lock()
	seeding := tr.seeding
	tr.mu.Unlock()
	if !seeding {
		t.Fatal("it should share: its own settings say so")
	}

	// changed while it shares: back to Options' ("don't") and it stops
	if err := m2.SetTorrentLimits(d2.ID, 0, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	waitCond(t, 10*time.Second, "it stops when told to follow Options", func() bool {
		m2.mu.Lock()
		defer m2.mu.Unlock()
		return m2.torrents[d2.ID] == nil
	})
}
