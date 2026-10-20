//go:build !windows

package main

import (
	"os/exec"
	"runtime"
	"strings"
)

// Linux: NetworkManager knows whether the connection is metered (it guesses for phone
// hotspots and you can set it per network). macOS doesn't tell apps, so it's unknown there.
func systemMetered() (metered, known bool) {
	if runtime.GOOS != "linux" {
		return false, false
	}
	bin, err := exec.LookPath("busctl")
	if err != nil {
		return false, false
	}
	out, err := exec.Command(bin, "get-property", "org.freedesktop.NetworkManager", "/org/freedesktop/NetworkManager",
		"org.freedesktop.NetworkManager", "Metered").Output()
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "u")) {
	case "1", "3": // yes, guessed yes
		return true, true
	case "2", "4": // no, guessed no
		return false, true
	}
	return false, false
}
