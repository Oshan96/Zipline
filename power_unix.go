//go:build darwin || linux

package main

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// macOS and Linux keep the computer awake with a helper program that holds sleep off while it
// runs (caffeinate, systemd-inhibit): started when downloads begin, stopped when they end.

var awakeCmd struct {
	sync.Mutex
	cmd *exec.Cmd
}

// holdAwake runs the helper unless it already runs.
func holdAwake(name string, args ...string) error {
	awakeCmd.Lock()
	defer awakeCmd.Unlock()
	if awakeCmd.cmd != nil {
		return nil
	}
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = helperAttr()
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	if err := cmd.Start(); err != nil {
		return err
	}
	awakeCmd.cmd = cmd
	go func() {
		err := cmd.Wait()
		awakeCmd.Lock()
		gone := awakeCmd.cmd == cmd // it ended by itself (no permission, no system bus)
		if gone {
			awakeCmd.cmd = nil
		}
		awakeCmd.Unlock()
		if gone {
			logf("keep awake: %s ended: %v %s", filepath.Base(name), err, strings.TrimSpace(errOut.String()))
		}
	}()
	return nil
}

func releaseAwake() {
	awakeCmd.Lock()
	defer awakeCmd.Unlock()
	if c := awakeCmd.cmd; c != nil {
		c.Process.Signal(syscall.SIGTERM)
		awakeCmd.cmd = nil
	}
}

// Waking a Mac or a Linux computer for a schedule needs administrator setup (pmset, rtcwake),
// so Zipline doesn't offer it there.
func setWakeTimer(time.Time) error { return nil }
