//go:build !windows

package main

import "os/exec"

// startFromHost starts Zipline from the native host in a session of its own, so it keeps running when
// the browser closes the host.
func startFromHost(cmd *exec.Cmd) error {
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
