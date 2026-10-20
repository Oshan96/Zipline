//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

func makeSparse(f *os.File) {} // ext4/APFS/btrfs truncate already creates sparse files

func clipboardSeq() uint32 { return 0 }

func readClipboard() (string, error) {
	var cmds [][]string
	if runtime.GOOS == "darwin" {
		cmds = [][]string{{"pbpaste"}}
	} else {
		cmds = [][]string{{"wl-paste", "-n"}, {"xclip", "-o", "-selection", "clipboard"}, {"xsel", "-ob"}}
	}
	for _, c := range cmds {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		out, err := exec.Command(c[0], c[1:]...).Output()
		if err == nil {
			return string(out), nil
		}
	}
	return "", errors.New("no clipboard tool")
}

func openPath(p string) {
	if runtime.GOOS == "darwin" {
		exec.Command("open", p).Start()
	} else {
		exec.Command("xdg-open", p).Start()
	}
}

func revealPath(p string) {
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat(p); err == nil {
			exec.Command("open", "-R", p).Start()
			return
		}
		exec.Command("open", filepath.Dir(p)).Start()
		return
	}
	exec.Command("xdg-open", filepath.Dir(p)).Start()
}

func openWindow(url string) {
	if runtime.GOOS == "darwin" {
		for _, app := range []string{"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium"} {
			if _, err := os.Stat("/Applications/" + app + ".app"); err == nil {
				exec.Command("open", append([]string{"-na", app, "--args"}, windowArgs(url)...)...).Start()
				return
			}
		}
		exec.Command("open", url).Start()
		return
	}
	for _, b := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
		if p, err := exec.LookPath(b); err == nil {
			exec.Command(p, windowArgs(url)...).Start()
			return
		}
	}
	exec.Command("xdg-open", url).Start()
}

func pickDirectory(start string) (string, error) {
	if runtime.GOOS == "darwin" {
		// Run the picker from System Events and activate it so it opens in front of Zipline.
		out, err := exec.Command("osascript", "-e", `tell application "System Events"
activate
set f to choose folder with prompt "Choose download folder"
end tell
POSIX path of f`).Output()
		if err != nil {
			return "", nil // user cancelled
		}
		return strings.TrimRight(strings.TrimSpace(string(out)), "/"), nil
	}
	for _, c := range [][]string{{"zenity", "--file-selection", "--directory", "--filename=" + start + "/"}, {"kdialog", "--getexistingdirectory", start}} {
		if _, err := exec.LookPath(c[0]); err == nil {
			out, err := exec.Command(c[0], c[1:]...).Output()
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(out)), nil
		}
	}
	return "", errors.New("no folder picker available (install zenity) — type the path instead")
}

func focusAppWindow() bool { return false }

func shutdownComputer() {
	if runtime.GOOS == "darwin" {
		exec.Command("osascript", "-e", `tell app "System Events" to shut down`).Run()
	} else {
		exec.Command("systemctl", "poweroff").Run()
	}
}

func hideConsole(c *exec.Cmd) {}

// detach lets a started process outlive this one (the command line starting Zipline in the background).
func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// autoScanner: ClamAV if installed (exit 0 = clean, 1 = infected).
func autoScanner(file string) (string, []string, string) {
	for _, n := range []string{"clamdscan", "clamscan"} {
		if p, err := exec.LookPath(n); err == nil {
			args := []string{"--no-summary", file}
			if st, err := os.Stat(file); err == nil && st.IsDir() && n == "clamscan" { // a torrent's folder
				args = []string{"--no-summary", "--recursive", file}
			}
			if n == "clamdscan" {
				args = []string{"--no-summary", "--fdpass", file}
			}
			return p, args, "ClamAV"
		}
	}
	return "", nil, ""
}

func cliPrint(s string) { fmt.Println(s) }

func cliMessage(s string, isErr bool) {
	if isErr {
		fmt.Fprintln(os.Stderr, s)
		return
	}
	fmt.Println(s)
}
