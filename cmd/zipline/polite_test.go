package main

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// D8: the baseline is the lowest delay of the last 10 minutes.
func TestPoliteBaselineTenMinutes(t *testing.T) {
	c := &politeCtl{}
	t0 := time.Now()
	c.observe(t0, 20*time.Millisecond, 1e6)
	if _, q := c.observe(t0.Add(9*time.Minute), 40*time.Millisecond, 1e6); q != 20*time.Millisecond {
		t.Fatalf("9 minutes on, the old low still counts: extra delay %v", q)
	}
	// 11 minutes after the low one, the lowest left is the 40 ms one
	if _, q := c.observe(t0.Add(11*time.Minute), 45*time.Millisecond, 1e6); q != 5*time.Millisecond {
		t.Fatalf("after 10 minutes the baseline moves: extra delay %v", q)
	}
}

// D8: nothing happens below 30 ms of extra delay, a gentle slowdown from there, and a hard one
// from 60 ms (LEDBAT++'s target).
func TestPoliteTargetBand(t *testing.T) {
	step := func(extra time.Duration) float64 {
		c := &politeCtl{}
		now := time.Now()
		c.observe(now, 20*time.Millisecond, 4e6) // the empty line
		limit, _ := c.observe(now.Add(time.Second), 20*time.Millisecond+extra, 4e6)
		return limit
	}
	if l := step(25 * time.Millisecond); l != 0 {
		t.Fatalf("25 ms: limited to %.0f", l)
	}
	gentle := step(45 * time.Millisecond)
	if gentle <= 0.7*4e6 || gentle >= 4e6 {
		t.Fatalf("45 ms: %.0f, want between 70%% and 100%%", gentle)
	}
	if l := step(80 * time.Millisecond); l != 0.7*4e6 {
		t.Fatalf("80 ms: %.0f, want 70%%", l)
	}
	if backoff(30*time.Millisecond) != 1 || backoff(60*time.Millisecond) != 0.7 {
		t.Fatal("band edges")
	}
}

// D8: uploads slow down on a busy line too, and speed back up once it's free.
func TestPoliteSlowsUploads(t *testing.T) {
	c := &politeCtl{}
	now := time.Now()
	_, q := c.observe(now, 20*time.Millisecond, 0)
	if up := c.observeUp(q, 500e3); up != 0 {
		t.Fatalf("quiet line: upload limit %.0f", up)
	}
	_, q = c.observe(now.Add(time.Second), 150*time.Millisecond, 0)
	up := c.observeUp(q, 500e3)
	if up != 0.7*500e3 {
		t.Fatalf("busy line: upload limit %.0f", up)
	}
	if c.rate != 0 {
		t.Fatal("nothing downloading: no download limit")
	}
	for i := 2; i < 40 && c.upRate != 0; i++ { // peers take up to 500 KB/s
		_, q = c.observe(now.Add(time.Duration(i)*time.Second), 20*time.Millisecond, 0)
		c.observeUp(q, min(c.upRate, 500e3))
	}
	if c.upRate != 0 {
		t.Fatalf("free line again: upload limit %.0f", c.upRate)
	}
}

// D8: reflectors are each measured against their own baseline; the lowest extra delay counts,
// so a far reflector isn't mistaken for a busy line.
func TestPoliteReflectorsSeparately(t *testing.T) {
	c := &politeCtl{}
	now := time.Now()
	c.observeAll(now, map[string]time.Duration{"a": 10 * time.Millisecond, "b": 90 * time.Millisecond}, 1e6)
	limit, q := c.observeAll(now.Add(time.Second), map[string]time.Duration{"b": 95 * time.Millisecond}, 1e6) // a didn't answer
	if q != 5*time.Millisecond || limit != 0 {
		t.Fatalf("far reflector alone: extra delay %v, limit %.0f", q, limit)
	}
	_, q = c.observeAll(now.Add(2*time.Second), map[string]time.Duration{"a": 70 * time.Millisecond, "b": 160 * time.Millisecond}, 1e6)
	if q != 60*time.Millisecond {
		t.Fatalf("both slower: extra delay %v", q)
	}
}

// D8: with several internet connections, each line keeps its own baseline and limits: a slower
// line doesn't look congested next to a faster one, and a busy line slows only itself.
func TestPolitePerAdapter(t *testing.T) {
	old := rttProbeVia
	var busyB atomic.Bool
	rttProbeVia = func(_ context.Context, _ string, i netIf) (time.Duration, error) {
		switch i.Name {
		case "fast":
			return 10 * time.Millisecond, nil
		case "slow":
			if busyB.Load() {
				return 250 * time.Millisecond, nil
			}
			return 90 * time.Millisecond, nil
		}
		return 0, errors.New("no such adapter")
	}
	defer func() { rttProbeVia = old }()
	a := &politeLine{name: "fast", via: &netIf{Name: "fast"}, down: &Limiter{}, up: &Limiter{}, lastQuiet: time.Now()}
	b := &politeLine{name: "slow", via: &netIf{Name: "slow"}, down: &Limiter{}, up: &Limiter{}, lastQuiet: time.Now()}
	for i := 0; i < 5; i++ {
		a.step("server:443", 2e6, 100e3)
		b.step("server:443", 1e6, 50e3)
	}
	if a.down.Rate() != 0 || b.down.Rate() != 0 || b.queue != 0 {
		t.Fatalf("both lines quiet: limits %d / %d, extra delay on the slow one %v", a.down.Rate(), b.down.Rate(), b.queue)
	}
	// (the old way: one baseline for both lines saw the slower line as 80 ms of queue)
	shared := &politeCtl{}
	now := time.Now()
	shared.observe(now, 10*time.Millisecond, 3e6)
	if limit, _ := shared.observe(now.Add(time.Second), 90*time.Millisecond, 3e6); limit == 0 {
		t.Fatal("a shared baseline should have mistaken the slower line for a busy one")
	}
	busyB.Store(true) // someone needs the slow line
	b.step("server:443", 1e6, 50e3)
	a.step("server:443", 2e6, 100e3)
	if b.down.Rate() == 0 || b.up.Rate() == 0 || a.down.Rate() != 0 || a.up.Rate() != 0 {
		t.Fatalf("the busy line eases off (down %d, up %d), the other doesn't (down %d, up %d)", b.down.Rate(), b.up.Rate(), a.down.Rate(), a.up.Rate())
	}
}

// D8: the polite loop slows torrent uploads (the upload limiter) when the line is busy. (It runs
// at its usual pace: other tests' managers read the interval.)
func TestPoliteLoopSlowsUploads(t *testing.T) {
	oldProbe := rttProbe
	var calls int32
	rttProbe = func(context.Context, string) (time.Duration, error) {
		if atomic.AddInt32(&calls, 1) <= 6 { // the first round: the quiet line, then the line in use
			return 15 * time.Millisecond, nil
		}
		return 250 * time.Millisecond, nil
	}
	defer func() { rttProbe = oldProbe }()
	m := newTestManager(t)
	// a torrent sharing at 400 KB/s (just what polite mode reads of it)
	tr := &torrent{m: m, d: &Download{ID: "sharing"}, peers: map[netip.AddrPort]*tpeer{}}
	tr.live.Store(&TorrentLive{Seeding: true, Up: 400e3})
	m.mu.Lock()
	m.torrents["sharing"] = tr
	m.settings.Polite = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.torrents, "sharing")
		m.mu.Unlock()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for m.Snapshot(nil).Polite.UpRate == 0 && time.Now().Before(deadline) { // (the quiet probe also limits it briefly)
		time.Sleep(20 * time.Millisecond)
	}
	if v := m.Snapshot(nil).Polite; v.UpRate == 0 || v.UpRate > 400e3 || v.QueueMs < 100 {
		t.Fatalf("view %+v", v)
	}
	if r := m.gate.politeUp.Rate(); r == 0 || r > 400e3 {
		t.Fatalf("upload limit %d", r)
	}
	m.mu.Lock()
	m.settings.Polite = false
	m.mu.Unlock()
	deadline = time.Now().Add(5 * time.Second)
	for m.gate.politeUp.Rate() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m.gate.politeUp.Rate() != 0 {
		t.Fatal("switching polite mode off should lift the upload limit")
	}
}
