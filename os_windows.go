//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	user32          = syscall.NewLazyDLL("user32.dll")
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	pOpenClipboard  = user32.NewProc("OpenClipboard")
	pCloseClipboard = user32.NewProc("CloseClipboard")
	pGetClipboard   = user32.NewProc("GetClipboardData")
	pClipSeq        = user32.NewProc("GetClipboardSequenceNumber")
	pGlobalLock     = kernel32.NewProc("GlobalLock")
	pGlobalUnlock   = kernel32.NewProc("GlobalUnlock")
	pLstrlenW       = kernel32.NewProc("lstrlenW")
	pMoveMemory     = kernel32.NewProc("RtlMoveMemory")
)

func hidden(c *exec.Cmd) *exec.Cmd {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return c
}

func clipboardSeq() uint32 {
	r, _, _ := pClipSeq.Call()
	return uint32(r)
}

func readClipboard() (string, error) {
	if r, _, _ := pOpenClipboard.Call(0); r == 0 {
		return "", errors.New("clipboard busy")
	}
	defer pCloseClipboard.Call()
	h, _, _ := pGetClipboard.Call(13) // CF_UNICODETEXT
	if h == 0 {
		return "", nil
	}
	p, _, _ := pGlobalLock.Call(h)
	if p == 0 {
		return "", nil
	}
	defer pGlobalUnlock.Call(h)
	n, _, _ := pLstrlenW.Call(p)
	if n == 0 || n > 1<<20 {
		return "", nil
	}
	u := make([]uint16, n)
	pMoveMemory.Call(uintptr(unsafe.Pointer(&u[0])), p, n*2)
	return syscall.UTF16ToString(u), nil
}

func makeSparse(f *os.File) {
	// FSCTL_SET_SPARSE: avoids NTFS zero-filling the gap when segments write far ahead.
	var ret uint32
	syscall.DeviceIoControl(syscall.Handle(f.Fd()), 0x000900c4, nil, 0, nil, 0, &ret, nil)
}

func openPath(p string) {
	hidden(exec.Command("rundll32", "url.dll,FileProtocolHandler", p)).Start()
}

func revealPath(p string) {
	if _, err := os.Stat(p); err == nil {
		exec.Command("explorer", "/select,", p).Start()
		return
	}
	exec.Command("explorer", filepath.Dir(p)).Start()
}

func browserCandidates() []string {
	var out []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		out = append(out,
			filepath.Join(base, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(base, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(base, `BraveSoftware\Brave-Browser\Application\brave.exe`),
		)
	}
	return out
}

func openWindow(url string) {
	for _, b := range browserCandidates() {
		if _, err := os.Stat(b); err == nil {
			if exec.Command(b, windowArgs(url)...).Start() == nil {
				return
			}
		}
	}
	hidden(exec.Command("rundll32", "url.dll,FileProtocolHandler", url)).Start()
}

func pickDirectory(start string) (string, error) {
	pickMu.Lock()
	defer pickMu.Unlock()
	dir, err := pickFolderNative(start)
	if err == nil || err == errPickCancelled {
		if err == errPickCancelled {
			return "", nil
		}
		return dir, nil
	}
	// Fallback: classic dialog via PowerShell, owned by a visible top-most form so it can't hide.
	script := `Add-Type -AssemblyName System.Windows.Forms; $w = New-Object System.Windows.Forms.Form -Property @{TopMost=$true; ShowInTaskbar=$false; Width=1; Height=1; StartPosition='CenterScreen'; Opacity=0}; $w.Show(); $w.Activate(); ` +
		`$f = New-Object System.Windows.Forms.FolderBrowserDialog; $f.SelectedPath = '` + strings.ReplaceAll(start, "'", "''") + `'; $f.ShowNewFolderButton = $true; ` +
		`if ($f.ShowDialog($w) -eq 'OK') { [Console]::OutputEncoding = [Text.Encoding]::UTF8; Write-Output $f.SelectedPath }; $w.Close()`
	out, err := hidden(exec.Command("powershell", "-NoProfile", "-STA", "-Command", script)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// shutdownComputer runs after Zipline's own one-minute countdown (whendone.go), so Windows
// doesn't add another; a timeout above 0 still closes apps that would hold it up, as before.
func shutdownComputer() { exec.Command("shutdown", "/s", "/t", "1").Run() }

// hideConsole keeps helper tools (ffmpeg) from flashing a console window.
func hideConsole(c *exec.Cmd) { hidden(c) }

// detach lets a started process outlive this one (the command line starting Zipline in the background).
func detach(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
}

// autoScanner: Microsoft Defender's command-line scanner. Exit code 2 = threat found;
// Defender then handles the file as it normally would (quarantine).
func autoScanner(file string) (string, []string, string) {
	var best string
	var bestTime int64
	if pd := os.Getenv("ProgramData"); pd != "" {
		dirs, _ := filepath.Glob(filepath.Join(pd, `Microsoft\Windows Defender\Platform\*\MpCmdRun.exe`))
		for _, p := range dirs {
			if st, err := os.Stat(p); err == nil && st.ModTime().Unix() > bestTime {
				best, bestTime = p, st.ModTime().Unix()
			}
		}
	}
	if best == "" {
		for _, env := range []string{"ProgramFiles", "ProgramW6432"} {
			if pf := os.Getenv(env); pf != "" {
				p := filepath.Join(pf, `Windows Defender\MpCmdRun.exe`)
				if _, err := os.Stat(p); err == nil {
					best = p
					break
				}
			}
		}
	}
	if best == "" {
		return "", nil, ""
	}
	return best, []string{"-Scan", "-ScanType", "3", "-File", file}, "Microsoft Defender"
}

var (
	pAttachConsole = kernel32.NewProc("AttachConsole")
	pMessageBoxW   = user32.NewProc("MessageBoxW")
	consoleOut     *os.File
	consoleTried   bool
)

// console attaches to the command prompt Zipline was started from (it's a GUI app,
// so it has no console of its own). Returns nil when started from Explorer.
func console() *os.File {
	if !consoleTried {
		consoleTried = true
		if r, _, _ := pAttachConsole.Call(^uintptr(0)); r != 0 { // ATTACH_PARENT_PROCESS
			if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
				consoleOut = f
			}
		}
	}
	return consoleOut
}

func cliPrint(s string) {
	if c := console(); c != nil {
		c.WriteString("\r\n" + strings.ReplaceAll(s, "\n", "\r\n") + "\r\n")
	}
}

func cliMessage(s string, isErr bool) {
	if c := console(); c != nil {
		cliPrint(s)
		return
	}
	title, _ := syscall.UTF16PtrFromString("Zipline")
	text, _ := syscall.UTF16PtrFromString(s)
	flags := uintptr(0x40) // MB_ICONINFORMATION
	if isErr {
		flags = 0x30 // MB_ICONWARNING
	}
	pMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), flags)
}
