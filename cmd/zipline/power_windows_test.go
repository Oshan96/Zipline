//go:build windows

package main

import (
	"testing"
	"time"
)

// D3: the Windows calls behind keeping awake, the wake timer and the available actions work.
func TestWinPowerCalls(t *testing.T) {
	if err := setKeepAwake(true); err != nil {
		t.Fatal(err)
	}
	t.Logf("power request: %v (otherwise SetThreadExecutionState)", !awakeReq.noReq)
	if err := setKeepAwake(true); err != nil { // (already held)
		t.Fatal(err)
	}
	if err := setKeepAwake(false); err != nil {
		t.Fatal(err)
	}
	if err := setKeepAwake(true); err != nil { // held again with the same request
		t.Fatal(err)
	}
	setKeepAwake(false)
	for _, at := range []time.Time{time.Now().Add(time.Hour), time.Now().Add(2 * time.Hour), {}} {
		if err := setWakeTimer(at); err != nil {
			t.Fatalf("wake timer %v: %v", at, err)
		}
	}
	can := systemPowerActions()
	if len(can) < 2 || can[0] != "quit" || can[len(can)-1] != "shutdown" {
		t.Fatalf("actions %v", can)
	}
	t.Logf("this computer can: %v", can)
}
