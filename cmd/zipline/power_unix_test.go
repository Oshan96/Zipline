//go:build darwin || linux

package main

import (
	"syscall"
	"testing"
	"time"
)

// D3: the helper that holds sleep off runs once while held and ends when let go.
func TestHoldAwakeHelper(t *testing.T) {
	if err := holdAwake("sleep", "30"); err != nil {
		t.Fatal(err)
	}
	awakeCmd.Lock()
	c := awakeCmd.cmd
	awakeCmd.Unlock()
	if c == nil {
		t.Fatal("helper not running")
	}
	holdAwake("sleep", "30") // already held: no second one
	awakeCmd.Lock()
	same := awakeCmd.cmd == c
	awakeCmd.Unlock()
	if !same {
		t.Fatal("started a second helper")
	}
	pid := c.Process.Pid
	releaseAwake()
	for i := 0; syscall.Kill(pid, 0) == nil; i++ {
		if i == 100 {
			t.Fatal("helper still running after being let go")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
