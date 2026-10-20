//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// On Linux desktops, magnet links and .torrent files open with the app whose .desktop file
// is the default for their MIME types (xdg-mime).

const assocDesktop = "zipline-torrent.desktop"

var assocTypes = []string{"x-scheme-handler/magnet", "application/x-bittorrent"}

func assocDesktopPath() string {
	dir := os.Getenv("XDG_DATA_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(dir, "applications", assocDesktop)
}

func torrentAssociation() (supported, on bool) {
	if _, err := exec.LookPath("xdg-mime"); err != nil {
		return false, false
	}
	out, err := exec.Command("xdg-mime", "query", "default", assocTypes[0]).Output()
	return true, err == nil && strings.TrimSpace(string(out)) == assocDesktop
}

func setTorrentAssociation(on bool) error {
	if _, err := exec.LookPath("xdg-mime"); err != nil {
		return errors.New("xdg-mime isn't installed")
	}
	p := assocDesktopPath()
	if !on {
		os.Remove(p)
		exec.Command("update-desktop-database", filepath.Dir(p)).Run()
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	entry := "[Desktop Entry]\nType=Application\nName=Zipline\nComment=Open magnet links and torrent files with Zipline\n" +
		"Exec=\"" + strings.ReplaceAll(exe, `"`, `\"`) + "\" %u\nMimeType=" + strings.Join(assocTypes, ";") + ";\nNoDisplay=true\nTerminal=false\n"
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(entry), 0o644); err != nil {
		return err
	}
	exec.Command("update-desktop-database", filepath.Dir(p)).Run()
	for _, t := range assocTypes {
		if err := exec.Command("xdg-mime", "default", assocDesktop, t).Run(); err != nil {
			return err
		}
	}
	return nil
}
