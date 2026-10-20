//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// On Linux the key goes to the Secret Service when secret-tool is installed and a desktop
// session is running, and to a private file in the data folder otherwise.
func platformSealer(dir string) sealer {
	l := linuxKeys{file: fileKeys{filepath.Join(dir, "vault.key")}}
	if systemKeyStore && secretServiceHere() {
		l.ss = &secretToolKeys{run: runCommand}
	}
	return newGCMSealer(l)
}

func secretServiceHere() bool {
	if _, err := exec.LookPath("secret-tool"); err != nil {
		return false
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return true
	}
	run := os.Getenv("XDG_RUNTIME_DIR")
	if run == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(run, "bus"))
	return err == nil
}
