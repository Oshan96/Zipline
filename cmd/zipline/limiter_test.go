package main

import (
	"math/rand"
	"sync/atomic"
	"testing"
	"time"
)

// A paused or removed download must not leave the speed limiter in debt (it used to keep
// draining buffered data after it was stopped, so the next download sat at 0 bytes).
func TestSpeedLimitAfterRemove(t *testing.T) {
	data := make([]byte, 30<<20)
	rand.Read(data)
	srv := testServer(data, 2<<20, true)
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.SpeedLimitOn, m.settings.SpeedLimitKB = true, 400
	m.applyLimit()
	m.mu.Unlock()
	for round := 0; round < 3; round++ {
		d := m.Add(AddReq{URL: srv.URL + "/f.bin", Start: true})
		time.Sleep(1500 * time.Millisecond)
		got := atomic.LoadInt64(&d.done)
		m.limiter.mu.Lock()
		av := m.limiter.avail
		m.limiter.mu.Unlock()
		t.Logf("round %d: %d KB in 1.5s, limiter avail %.0f KB", round, got>>10, av/1024)
		if round > 0 && got < 64<<10 { // the bug left it at 0 bytes
			t.Fatalf("a download started after another was removed is starved by the speed limiter")
		}
		m.Remove([]string{d.ID}, true)
		time.Sleep(200 * time.Millisecond)
	}
}
