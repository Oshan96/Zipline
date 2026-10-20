package main

import (
	"sync/atomic"
	"time"
)

// Metered connections: when the computer moves to a phone hotspot (or a Wi-Fi marked as
// metered), running downloads pause and queues hold off; they carry on when it's back on an
// unmetered connection. A download started by hand while metered is left alone. Torrents that
// are sharing keep going but upload nothing (gate.go); a start-up that finds the connection
// metered waits for the normal one before they share again.

// meteredCheck asks the system whether the current connection is metered.
func meteredCheck() (metered, known bool) {
	if f := meteredHook.Load(); f != nil {
		return (*f)()
	}
	return systemMetered()
}

// meteredHook stands in for the system in tests (setMeteredCheck): swapped atomically, as a
// manager from an earlier test may still be checking.
var meteredHook atomic.Pointer[func() (bool, bool)]

// setMeteredCheck makes meteredCheck answer with f, and returns what puts the old one back.
func setMeteredCheck(f func() (bool, bool)) (restore func()) {
	old := meteredHook.Swap(&f)
	return func() { meteredHook.Store(old) }
}

// meteredTick checks the connection every few seconds. Called from the loop.
func (m *Manager) meteredTick() {
	m.mu.Lock()
	on := m.settings.Plan.Metered
	m.mu.Unlock()
	metered, known := false, false
	if on {
		metered, known = m.isMetered()
	}
	m.mu.Lock()
	was := m.metered
	m.metered, m.meterKnown = on && known && metered, known
	switch {
	case m.metered && !was:
		var ids []string
		for _, d := range m.list {
			if d.running && !d.meteredOK && !d.Provisional {
				ids = append(ids, d.ID)
				m.meterPaused[d.ID] = true
			}
		}
		m.mu.Unlock()
		m.Pause(ids)
		m.mu.Lock()
		if len(ids) > 0 {
			m.notify("metered", "", "Metered connection: paused "+plural(len(ids), "download")+" until you're back on your normal connection")
		}
	case !m.metered && was:
		var ids []string
		for id := range m.meterPaused {
			if d := m.byID[id]; d != nil && !d.running && (d.Status == StPaused || d.Status == StCompleted && d.Torrent != nil) { // (a finished torrent that was to share)
				ids = append(ids, id)
			}
		}
		m.meterPaused = map[string]bool{}
		for _, d := range m.list {
			d.meteredOK = false
		}
		m.mu.Unlock()
		m.Start(ids)
		m.kick()
		m.mu.Lock()
		if len(ids) > 0 {
			m.notify("metered", "", "Back on an unmetered connection: resumed "+plural(len(ids), "download"))
		}
	}
	m.mu.Unlock()
}

// meteredLoop polls the connection type while the option is on.
func (m *Manager) meteredLoop() {
	m.meteredTick() // the first check right away: scheduled starts and the start-up resume wait for it
	close(m.meterReady())
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.quit:
			return
		case <-t.C:
			m.meteredTick()
		}
	}
}
