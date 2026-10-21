package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// Keeping the computer awake: while anything downloads (or a queue runs, or a torrent
// downloads — sharing a finished one doesn't count), the computer isn't allowed to fall asleep
// by itself (Options → General). The display may still turn off. A few seconds after the last
// download ends, sleep is allowed again.

// Hooks so tests never touch the real computer: keepAwakeHook holds sleep off (true) or lets
// it go (false); wakeHook sets the wake timer (zero time: clears it).
var (
	keepAwakeHook = setKeepAwake
	wakeHook      = setWakeTimer
)

// awakeGrace: idle this long before sleep is allowed again (between two downloads of a queue
// there's no gap, but a retry or a new link can take a moment).
const awakeGrace = 3 * time.Second

// keepAwakeLoop is started by main (only the app holds the computer awake, never a test).
func (m *Manager) keepAwakeLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.quit:
			m.awakeRelease()
			return
		case now := <-t.C:
			m.awakeTick(now)
		}
	}
}

// downloading: a download runs or a queue does. Caller holds m.mu.
func (m *Manager) downloading() bool {
	for _, d := range m.list {
		if d.running {
			return true
		}
	}
	for _, q := range m.settings.Queues {
		if m.qRun[q.ID] || m.qSyncing[q.ID] {
			return true
		}
	}
	return false
}

// awakeTick holds sleep off while downloading and lets it go once idle for awakeGrace.
func (m *Manager) awakeTick(now time.Time) {
	m.mu.Lock()
	want := m.settings.KeepAwake && m.downloading()
	if want || m.sx.idleSince.IsZero() {
		m.sx.idleSince = now
	}
	hold := want && !m.sx.awake
	release := !want && m.sx.awake && now.Sub(m.sx.idleSince) >= awakeGrace
	if hold || release {
		m.sx.awake = hold
	}
	m.mu.Unlock()
	switch {
	case hold:
		if err := keepAwakeHook(true); err != nil {
			logf("keep awake: %v", err)
		}
	case release:
		keepAwakeHook(false)
	}
}

func (m *Manager) awakeRelease() {
	m.mu.Lock()
	held := m.sx.awake
	m.sx.awake = false
	m.mu.Unlock()
	if held {
		keepAwakeHook(false)
	}
}

// Which "when finished" actions this computer can take: looked up once in the background
// (on Linux it asks systemd, which can take a moment).
var (
	powerCan     atomic.Pointer[[]string]
	powerCanOnce sync.Once
)

func powerActionsInit() {
	powerCanOnce.Do(func() {
		can := systemPowerActions()
		powerCan.Store(&can)
	})
}

func powerActions() []string {
	if p := powerCan.Load(); p != nil {
		return *p
	}
	return []string{"quit", "shutdown"}
}
