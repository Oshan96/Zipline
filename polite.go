package main

import (
	"context"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Polite mode. Zipline watches the delay on the line while it downloads or shares. When something
// else needs the connection (a video call, a game, a TV streaming in another room) the delay goes
// up because the router's queue fills; Zipline then slows its downloads and uploads until the
// delay drops, and speeds back up once the line is quiet. Unlike a fixed speed limit, nothing is
// lost when nobody else is using the internet.
//
// The delay is measured as TCP connect times to the server being downloaded from, or, when only
// torrents run, to a few well-known internet addresses (each against its own baseline, keeping
// the lowest extra delay, so one slow reflector doesn't look like a busy line). The baseline is
// the lowest delay of the last 10 minutes; past 30 ms of extra delay Zipline eases off gently,
// and hard from 60 ms (LEDBAT++'s target, used by Windows Update). With several internet
// connections at once each has its own measurements and limits, so a slower line doesn't look
// congested next to a faster one.

const (
	politeHigh   = 60 * time.Millisecond // extra delay where Zipline eases off hard (LEDBAT++'s target)
	politeLow    = 30 * time.Millisecond // from here up it eases off gently; below, the line is free again
	politeMin    = 64 << 10              // downloads never slower than this (bytes/s)
	politeUpMin  = 16 << 10              // uploads never slower than this
	politeWindow = 10 * time.Minute      // how far back the "empty line" delay is remembered (as LEDBAT)
)

// PoliteView: is polite mode holding Zipline back right now, and why.
type PoliteView struct {
	On      bool             `json:"on"`
	Rate    float64          `json:"rate"`    // current download limit, bytes/s (0 = full speed)
	UpRate  float64          `json:"upRate"`  // current upload limit (0 = full speed)
	QueueMs int64            `json:"queueMs"` // extra delay measured on the line
	Lines   []PoliteLineView `json:"lines,omitempty"`
}

// PoliteLineView is one internet connection's state when several are used at once.
type PoliteLineView struct {
	Name    string  `json:"name"`
	Rate    float64 `json:"rate"`
	UpRate  float64 `json:"upRate"`
	QueueMs int64   `json:"queueMs"`
}

type rttSample struct {
	at  time.Time
	rtt time.Duration
}

// politeCtl decides the speed limits from delay measurements. It holds no timers, so it can be
// tested with a simulated connection.
type politeCtl struct {
	rate    float64 // current download limit in bytes/s; 0 = no limit
	peak    float64 // highest recent download speed, to know when we're back to full speed
	upRate  float64 // the same for uploads
	upPeak  float64
	samples []rttSample             // the delay to the transfer's own server
	others  map[string]*[]rttSample // …and to reflectors, each separately
}

// baseOf is the lowest delay of the last politeWindow (the line with an empty queue), dropping
// older measurements.
func baseOf(h *[]rttSample, now time.Time) time.Duration {
	keep := (*h)[:0]
	var b time.Duration = -1
	for _, s := range *h {
		if now.Sub(s.at) > politeWindow {
			continue
		}
		keep = append(keep, s)
		if b < 0 || s.rtt < b {
			b = s.rtt
		}
	}
	*h = keep
	return b
}

func (c *politeCtl) base(now time.Time) time.Duration { return baseOf(&c.samples, now) }

func (c *politeCtl) history(target string) *[]rttSample {
	if target == "" {
		return &c.samples
	}
	if c.others == nil {
		c.others = map[string]*[]rttSample{}
	}
	h := c.others[target]
	if h == nil {
		h = &[]rttSample{}
		c.others[target] = h
	}
	return h
}

// note keeps a measurement to target ("" = the transfer's own server) without acting on it.
func (c *politeCtl) note(now time.Time, target string, rtt time.Duration) {
	h := c.history(target)
	*h = append(*h, rttSample{now, rtt})
}

// queueOf is the extra delay the measurements show: each target against its own baseline, the
// lowest of them (a hiccup at one reflector isn't a busy line).
func (c *politeCtl) queueOf(now time.Time, rtts map[string]time.Duration) time.Duration {
	q := time.Duration(-1)
	for target, rtt := range rtts {
		c.note(now, target, rtt)
		x := rtt - baseOf(c.history(target), now)
		if q < 0 || x < q {
			q = x
		}
	}
	return max(q, 0)
}

// observe takes one delay measurement and the current download speed and returns the limit.
func (c *politeCtl) observe(now time.Time, rtt time.Duration, throughput float64) (limit float64, queue time.Duration) {
	return c.observeAll(now, map[string]time.Duration{"": rtt}, throughput)
}

func (c *politeCtl) observeAll(now time.Time, rtts map[string]time.Duration, throughput float64) (float64, time.Duration) {
	queue := c.queueOf(now, rtts)
	c.rate, c.peak = politeAdjust(c.rate, c.peak, throughput, queue, politeMin)
	return c.rate, queue
}

// observeUp slows uploads by the same rule, for the extra delay observe just measured.
func (c *politeCtl) observeUp(queue time.Duration, up float64) float64 {
	c.upRate, c.upPeak = politeAdjust(c.upRate, c.upPeak, up, queue, politeUpMin)
	return c.upRate
}

// backoff is how much to slow down for so much extra delay: nothing up to politeLow, then more
// and more, down to ×0.7 from politeHigh on.
func backoff(queue time.Duration) float64 {
	switch {
	case queue <= politeLow:
		return 1
	case queue >= politeHigh:
		return 0.7
	}
	return 1 - 0.3*float64(queue-politeLow)/float64(politeHigh-politeLow)
}

// politeAdjust moves one limit (downloads or uploads) for the delay just measured: down when the
// line is busy, back up by 15% a step once it's free, and off once well past the peak speed.
func politeAdjust(rate, peak, thr float64, queue time.Duration, floor float64) (float64, float64) {
	if thr > peak {
		peak = thr
	} else {
		peak *= 0.995
	}
	switch {
	case queue > politeLow:
		cur := rate
		if cur == 0 || cur > thr*1.2 && thr > 0 {
			cur = thr
		}
		if cur > 0 { // (nothing moving this way: nothing to slow)
			rate = max(cur*backoff(queue), floor)
		}
	case rate > 0:
		rate *= 1.15
		if peak > 0 && rate > peak*1.25 {
			rate = 0 // back to full speed
		}
	}
	return rate, peak
}

// rttProbe measures the round trip to a server with a TCP handshake (replaced in tests).
var rttProbe = func(ctx context.Context, addr string) (time.Duration, error) {
	t0 := time.Now()
	c, err := dialNet(ctx, "tcp", addr)
	if err != nil {
		return 0, err
	}
	d := time.Since(t0)
	c.Close()
	return d, nil
}

// rttProbeVia measures it through one network adapter (several connections at once).
var rttProbeVia = func(ctx context.Context, addr string, i netIf) (time.Duration, error) {
	t0 := time.Now()
	c, err := dialVia(ctx, "tcp", addr, i)
	if err != nil {
		return 0, err
	}
	d := time.Since(t0)
	c.Close()
	return d, nil
}

var politeInterval = time.Second

// politeReflector is measured when only torrents run (no single server to watch), together with
// a couple of alternatives.
var (
	politeReflector  = "1.1.1.1:443"
	politeReflectors = []string{politeReflector, "8.8.8.8:443", "9.9.9.9:443"}
)

// politeTarget picks the server to measure: the busiest running download's (or its proxy's).
// Torrents have no one server, so when only torrents run a well-known public address stands in.
// Caller holds m.mu.
func (m *Manager) politeTarget() (addr string, throughput float64) {
	var best *url.URL
	var bestSpeed float64
	running := false
	for _, d := range m.list {
		if !d.running || d.Provisional {
			continue
		}
		running = true
		throughput += d.speed
		raw := d.FinalURL
		if raw == "" {
			raw = d.URL
		}
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" && d.Torrent == nil && (best == nil || d.speed > bestSpeed) {
			best, bestSpeed = u, d.speed
		}
	}
	if !running {
		return "", 0
	}
	if best == nil {
		return politeReflector, throughput
	}
	u := best
	if p, err := proxyFor(u); err == nil && p != nil {
		u = p
	}
	port := u.Port()
	if port == "" {
		port = defaultPort(u)
	}
	return net.JoinHostPort(u.Hostname(), port), throughput
}

// uploadSpeed is how fast torrents are uploading. Caller holds m.mu.
func (m *Manager) uploadSpeed() float64 {
	var up float64
	for _, t := range m.torrents {
		if l := t.live.Load(); l != nil {
			up += l.Up
		}
	}
	return up
}

// politeLine is polite mode for one internet connection: the usual one, or one adapter when
// several are used at once.
type politeLine struct {
	name      string
	via       *netIf // nil: the usual connection
	ctl       politeCtl
	down, up  *Limiter
	lastQuiet time.Time
	queue     time.Duration
	in, out   int64 // the adapter's byte counters at the last look
	at        time.Time
}

// measure takes the round trip to addr on this line; to the reflectors when addr is the
// stand-in, each separately.
func (l *politeLine) measure(ctx context.Context, addr string) map[string]time.Duration {
	probe := func(a string) (time.Duration, error) {
		if l.via != nil {
			return rttProbeVia(ctx, a, *l.via)
		}
		return rttProbe(ctx, a)
	}
	out := map[string]time.Duration{}
	if addr != politeReflector {
		if rtt, err := probe(addr); err == nil {
			out[""] = rtt
		}
		return out
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, a := range politeReflectors {
		wg.Add(1)
		go func(a string) {
			defer wg.Done()
			if rtt, err := probe(a); err == nil {
				mu.Lock()
				out[a] = rtt
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()
	return out
}

// step runs one round on a line: now and then a measurement with the line nearly empty (so the
// baseline stays honest), then one at the current speeds, and the limits that follow from it.
func (l *politeLine) step(addr string, down, up float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	now := time.Now()
	if now.Sub(l.lastQuiet) > 45*time.Second && (down > 0 || up > 0) {
		l.lastQuiet = now
		if down > 0 {
			l.down.SetRate(max(int64(down/12), politeMin))
		}
		if up > 0 {
			l.up.SetRate(max(int64(up/12), politeUpMin))
		}
		time.Sleep(900 * time.Millisecond)
		for target, rtt := range l.measure(ctx, addr) {
			l.ctl.note(time.Now(), target, rtt)
		}
		l.down.SetRate(int64(l.ctl.rate))
		l.up.SetRate(int64(l.ctl.upRate))
	}
	rtts := l.measure(ctx, addr)
	if len(rtts) == 0 {
		return
	}
	before, upBefore := l.ctl.rate, l.ctl.upRate
	limit, queue := l.ctl.observeAll(time.Now(), rtts, down)
	upLimit := l.ctl.observeUp(queue, up)
	if moved(limit, before) {
		l.down.SetRate(int64(limit))
	}
	if moved(upLimit, upBefore) {
		l.up.SetRate(int64(upLimit))
	}
	l.queue = queue
}

// moved: a limit changed enough to set the limiter again (setting it starts its allowance afresh).
func moved(now, before float64) bool {
	return now == 0 && before != 0 || before == 0 && now != 0 || before > 0 && (now/before > 1.03 || before/now > 1.03)
}

// off lifts a line's limits.
func (l *politeLine) off() {
	l.ctl.rate, l.ctl.upRate, l.queue = 0, 0, 0
	if l.down.Rate() != 0 {
		l.down.SetRate(0)
	}
	if l.up.Rate() != 0 {
		l.up.SetRate(0)
	}
}

// politeAdapters lists the adapters to watch one by one: those in use when several internet
// connections are used at once, else none (the usual connection).
func politeAdapters() []netIf {
	mdial.mu.Lock()
	defer mdial.mu.Unlock()
	if !mdial.on {
		return nil
	}
	if ifs := mdial.adapters(); len(ifs) >= 2 {
		return ifs
	}
	return nil
}

// politeLoop runs polite mode while it's switched on and something is downloading or uploading.
func (m *Manager) politeLoop() {
	t := time.NewTicker(politeInterval)
	defer t.Stop()
	upLim := &Limiter{}
	if m.gate != nil {
		upLim = m.gate.politeUp
	}
	main := &politeLine{down: m.politeLim, up: upLim}
	lines := map[string]*politeLine{}
	for {
		select {
		case <-m.quit:
			return
		case <-t.C:
		}
		m.mu.Lock()
		on := m.settings.Polite
		addr, thr := m.politeTarget()
		up := m.uploadSpeed()
		m.mu.Unlock()
		ifs := politeAdapters()
		if !on || addr == "" && up == 0 {
			main.off()
			for _, l := range lines {
				l.off()
			}
			m.publishPolite(main, nil)
			continue
		}
		if addr == "" {
			addr = politeReflector
		}
		if len(ifs) == 0 {
			for _, l := range lines {
				l.off()
			}
			main.step(addr, thr, up)
			m.publishPolite(main, nil)
			continue
		}
		main.off() // each adapter has its own limits now
		var ls []*politeLine
		seen := map[string]bool{}
		for _, i := range ifs {
			l := lines[i.Name]
			if l == nil {
				lim := mdial.limits(i.Name)
				l = &politeLine{name: i.Name, down: &lim.down, up: &lim.up}
				lines[i.Name] = l
			}
			i := i
			l.via, seen[i.Name] = &i, true
			in, out := mdial.traffic(i.Name)
			now := time.Now()
			var din, dout float64
			if dt := now.Sub(l.at).Seconds(); !l.at.IsZero() && dt > 0 {
				din, dout = float64(in-l.in)/dt, float64(out-l.out)/dt
			}
			l.in, l.out, l.at = in, out, now
			l.step(addr, max(din, 0), max(dout, 0))
			ls = append(ls, l)
		}
		for name, l := range lines {
			if !seen[name] {
				l.off()
			}
		}
		m.publishPolite(main, ls)
	}
}

// publishPolite puts polite mode's state where the window reads it.
func (m *Manager) publishPolite(main *politeLine, lines []*politeLine) {
	rate, up, queue := main.ctl.rate, main.ctl.upRate, main.queue
	var views []PoliteLineView
	for _, l := range lines {
		views = append(views, PoliteLineView{Name: l.name, Rate: l.ctl.rate, UpRate: l.ctl.upRate, QueueMs: l.queue.Milliseconds()})
		rate += l.ctl.rate
		up += l.ctl.upRate
		queue = max(queue, l.queue)
	}
	m.mu.Lock()
	m.politeRate, m.politeQueue = rate, queue
	if m.gate != nil {
		m.gate.politeUpRate, m.gate.politeLines = up, views
	}
	m.mu.Unlock()
}

// adapterLims are polite mode's limits for one adapter, applied on its connections.
type adapterLims struct{ down, up Limiter }

// limits returns an adapter's polite limits (made on first use).
func (md *multiDialer) limits(name string) *adapterLims {
	md.mu.Lock()
	defer md.mu.Unlock()
	if md.lims == nil {
		md.lims = map[string]*adapterLims{}
	}
	l := md.lims[name]
	if l == nil {
		l = &adapterLims{}
		md.lims[name] = l
	}
	return l
}

// traffic returns how much an adapter has received and sent.
func (md *multiDialer) traffic(name string) (in, out int64) {
	md.mu.Lock()
	defer md.mu.Unlock()
	if p := md.bytes[name]; p != nil {
		in = atomic.LoadInt64(p)
	}
	if p := md.sent[name]; p != nil {
		out = atomic.LoadInt64(p)
	}
	return in, out
}
