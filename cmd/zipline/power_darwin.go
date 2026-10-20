//go:build darwin

package main

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// caffeinate -i keeps the Mac from idle sleep (the display may still sleep); -w ends it when
// Zipline ends, even after a crash.
func setKeepAwake(on bool) error {
	if !on {
		releaseAwake()
		return nil
	}
	return holdAwake("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
}

func helperAttr() *syscall.SysProcAttr { return nil }

func sleepComputer()     { exec.Command("pmset", "sleepnow").Run() }
func hibernateComputer() {} // (not offered: a Mac's hibernation is a system-wide setting)

func systemPowerActions() []string { return []string{"quit", "sleep", "shutdown"} }
