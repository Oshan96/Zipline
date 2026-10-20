//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// startFromHost starts Zipline from the native host outside the browser's job object
// (CREATE_BREAKAWAY_FROM_JOB), so it keeps running when the browser closes the host. A job that
// doesn't allow breaking away refuses that; Zipline then starts the plain detached way.
func startFromHost(cmd *exec.Cmd) error {
	flags := uint32(0x00000008 | 0x00000200) // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags | 0x01000000}
	if err := cmd.Start(); err != nil {
		retry := exec.Command(cmd.Path, cmd.Args[1:]...)
		retry.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		if err := retry.Start(); err != nil {
			return err
		}
		return retry.Process.Release()
	}
	return cmd.Process.Release()
}
