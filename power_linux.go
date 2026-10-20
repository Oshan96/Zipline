//go:build linux

package main

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// systemd-inhibit holds a "sleep" lock while it runs. It's stopped when Zipline ends, even after
// a crash (Pdeathsig), and it stops its own "sleep infinity" with it.
func setKeepAwake(on bool) error {
	if !on {
		releaseAwake()
		return nil
	}
	p, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		return nil // (no systemd: nothing to ask)
	}
	return holdAwake(p, "--what=sleep", "--who=Zipline", "--why=Downloading", "--mode=block", "sleep", "infinity")
}

func helperAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM} }

func sleepComputer()     { exec.Command("systemctl", "suspend").Run() }
func hibernateComputer() { exec.Command("systemctl", "hibernate").Run() }

// systemPowerActions asks systemd-logind whether this user may suspend or hibernate without a
// password ("yes"; "challenge" would ask for one, which nobody may be there to type).
func systemPowerActions() []string {
	can := []string{"quit"}
	ask := func(method string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "busctl", "call", "org.freedesktop.login1", "/org/freedesktop/login1",
			"org.freedesktop.login1.Manager", method).Output()
		return err == nil && strings.TrimSpace(string(out)) == `s "yes"`
	}
	if ask("CanSuspend") {
		can = append(can, "sleep")
	}
	if ask("CanHibernate") {
		can = append(can, "hibernate")
	}
	return append(can, "shutdown")
}
