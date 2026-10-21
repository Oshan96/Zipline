//go:build !windows && !darwin && !linux

package main

import "time"

// Other systems: no keeping awake, waking, sleeping or hibernating.
func setKeepAwake(on bool) error   { return nil }
func setWakeTimer(time.Time) error { return nil }
func sleepComputer()               {}
func hibernateComputer()           {}
func systemPowerActions() []string { return []string{"quit", "shutdown"} }
