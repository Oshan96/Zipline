package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const appTitle = "Zipline Download Manager"

// appPort is where the window and the browser extension reach the app. ZL_PORT changes it, for
// running a second copy while developing (the extension's HTTP fallback only knows 17890). When
// another program has it, Zipline takes a free port and says which in run.json (runfile.go).
var appPort = func() int {
	if p, err := strconv.Atoi(os.Getenv("ZL_PORT")); err == nil && p > 1024 && p < 65536 {
		return p
	}
	return 17890
}()

// windowProfile is a private browser profile for the app window, so the user's normal
// browser (its downloads tray, extensions, sign-in prompts) never shows up inside Zipline.
var windowProfile string

func windowArgs(url string) []string {
	args := []string{"--app=" + url, "--window-size=1180,720", "--no-first-run", "--no-default-browser-check", "--disable-sync", "--disable-extensions"}
	if windowProfile != "" {
		args = append([]string{"--user-data-dir=" + windowProfile}, args...)
	}
	return args
}

var (
	appURL     = fmt.Sprintf("http://127.0.0.1:%d/", appPort)
	sseClients int32
	theManager *Manager
)

func bringToFront() {
	if atomic.LoadInt32(&sseClients) == 0 {
		openWindow(loginURL()) // a fresh one-time sign-in for the window (session.go)
		return
	}
	focusAppWindow()
}

func main() {
	if code, ok := specialMode(os.Args[1:]); ok { // the browser's native-messaging host, --login-url
		os.Exit(code)
	}
	req, err := parseCLI(os.Args[1:], runtime.GOOS == "windows")
	if err != nil {
		cliMessage("Zipline: "+err.Error()+"\n\n"+cliUsage, true)
		os.Exit(2)
	}
	if req.Help {
		cliMessage(cliUsage, false)
		return
	}
	if req.Version {
		cliMessage(versionText(), false)
		return
	}

	// One Zipline per user: the lock file in the data folder (instance.go), not the port.
	dataDir := appDataDir()
	makePrivateDir(dataDir) // (0700 on macOS and Linux: perms_unix.go)
	lock, err := lockInstance(dataDir)
	if err == errLocked {
		// Already running: hand the request to it.
		os.Exit(sendToRunning(req))
	}
	var ln net.Listener
	port := appPort
	if lock == nil { // (no lock file possible here: the port is the lock, as it used to be)
		if ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", appPort)); err != nil {
			os.Exit(sendToRunning(req))
		}
	} else if ln, port, err = listenApp(); err != nil {
		cliMessage("Zipline: "+err.Error(), true)
		os.Exit(1)
	}
	if !req.empty() || req.Quit {
		// A command-line job while Zipline isn't running: answer it and return, instead of this
		// process turning into the app (scripts would wait for ever).
		ln.Close()
		lock.Release()
		os.Exit(runWithoutApp(req))
	}
	appURL = fmt.Sprintf("http://127.0.0.1:%d/", port)

	windowProfile = filepath.Join(dataDir, "window")
	openLog(dataDir)          // zipline.log: what happened, for Help → Copy diagnostics
	setupCrashOutput(dataDir) // the Windows build has no console: a crash would otherwise vanish

	m := NewManager(dataDir)
	theManager = m
	tb := make([]byte, 32)
	rand.Read(tb)
	srv := &Server{m: m, token: hex.EncodeToString(tb), port: port}
	theServer = srv
	hs := &http.Server{Handler: srv.routes(), ReadHeaderTimeout: 10 * time.Second}
	go hs.Serve(ln)
	// how the command line, the native host and --login-url find this Zipline and prove themselves
	if err := writeRunFile(dataDir, runInfo{Port: port, PID: os.Getpid(), Version: appVersion, Secret: srv.token}); err != nil {
		logf("couldn't write run.json: %v", err)
	}
	go registerNativeHost(dataDir) // the browsers' native-messaging host is this program (nmregister.go)
	startTray(m)
	go m.keepAwakeLoop() // (started here, not by the manager: tests never keep the computer awake)

	silentOnly := !req.empty() && (req.Silent || len(req.URLs) == 0)
	if !req.empty() {
		if _, err := m.ApplyCLI(req); err != nil {
			cliMessage(cliPrefix(err.Error()), true)
		}
	}
	go clipboardMonitor(m)

	// Silent command-line jobs and --nowindow start in the background (tray icon only).
	if !req.NoWindow && !silentOnly && atomic.LoadInt32(&sseClients) == 0 {
		openWindow(srv.loginURL())
	}
	<-m.quit
	m.dropAllEarly() // downloads still waiting in an Add dialog are discarded
	// what runs now carries on next time (recorded before it's all paused)
	m.rememberRunning()
	m.Pause(allIDs(m))
	m.closeTorrentNet() // the router's port mappings come off (portmap.go)
	time.Sleep(400 * time.Millisecond)
	m.save()
	m.closeClean()
	removeRunFile(dataDir, os.Getpid())
	logf("Zipline stopped")
	closeLog()
	lock.Release()
}

func allIDs(m *Manager) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, d := range m.list {
		if d.running {
			ids = append(ids, d.ID)
		}
	}
	return ids
}

var urlRe = regexp.MustCompile(`^https?://\S+$`)

func clipboardMonitor(m *Manager) {
	var last string
	lastSeq := clipboardSeq()
	armed := false
	for {
		time.Sleep(900 * time.Millisecond)
		m.mu.Lock()
		on := m.settings.ClipboardMonitor
		m.mu.Unlock()
		if !on { // switched off: don't touch the clipboard at all (on Mac and Linux each read runs a program)
			armed = false
			continue
		}
		if !armed { // just switched on: what's already on the clipboard isn't new
			last, _ = readClipboard()
			lastSeq = clipboardSeq()
			armed = true
			continue
		}
		if seq := clipboardSeq(); seq != 0 {
			if seq == lastSeq {
				continue
			}
			lastSeq = seq
		}
		t, err := readClipboard()
		if err != nil || t == last {
			continue
		}
		last = t
		if m.offerClipboard(strings.TrimSpace(t)) {
			bringToFront()
		}
	}
}

// offerClipboard asks (an Add prompt) about a link that was just copied, when it looks like
// something to download. YouTube and the other protected services get no prompt at all.
func (m *Manager) offerClipboard(t string) bool {
	if isMagnet(t) && !strings.ContainsAny(t, " \n") {
		m.AddPrompt(Prompt{URL: t, Source: "clipboard"})
		return true
	}
	if !urlRe.MatchString(t) || protectedURL(t) {
		return false
	}
	u, err := url.Parse(t)
	if err != nil {
		return false
	}
	m.mu.Lock()
	cat := m.categoryFor(u.Path)
	m.mu.Unlock()
	if cat == "General" && cloudService(t) == "" {
		return false // only catch links that look like files (or cloud share links)
	}
	m.AddPrompt(Prompt{URL: t, Source: "clipboard"})
	return true
}

// cliPrefix puts "Zipline: " before a message, unless it already starts with the name.
func cliPrefix(msg string) string {
	if strings.HasPrefix(msg, "Zipline ") {
		return msg
	}
	return "Zipline: " + msg
}

// sendToRunning passes a command-line request to the Zipline that's already running.
// runWithoutApp carries out a command-line request when Zipline isn't running: quitting and
// stopping a queue need nothing, an export reads the saved list, and anything else starts
// Zipline in the background and hands the request to it.
func runWithoutApp(req *CLIRequest) int {
	onlyQuit := req.Quit && len(req.URLs) == 0 && req.StartQueue == "" && req.StopQueue == "" && req.Import == "" && req.Export == ""
	if onlyQuit {
		cliPrint("Zipline isn't running.")
		return 0
	}
	if len(req.URLs) == 0 && req.StartQueue == "" && req.Import == "" { // export and/or stop: no app needed
		if req.Export != "" {
			m := offlineManager()
			b, _ := m.Export(exportFormat(req.Export), nil, false)
			if err := os.WriteFile(req.Export, b, 0o644); err != nil {
				cliMessage("Zipline: "+err.Error(), true)
				return 1
			}
			cliPrint(fmt.Sprintf("Zipline: exported %s to %s", plural(len(m.list), "download"), req.Export))
		}
		if req.StopQueue != "" {
			cliPrint("Zipline isn't running, so no queue is running.")
		}
		return 0
	}
	exe, err := os.Executable()
	if err != nil {
		cliMessage("Zipline: "+err.Error(), true)
		return 1
	}
	cmd := exec.Command(exe, "--nowindow")
	detach(cmd)
	if err := cmd.Start(); err != nil {
		cliMessage("Zipline: couldn't start: "+err.Error(), true)
		return 1
	}
	cmd.Process.Release()
	if _, err := waitApp(appDataDir(), 15*time.Second); err != nil { // up to 15 s for it to start listening
		cliMessage("Zipline: started, but it didn't answer in time", true)
		return 1
	}
	req.Quit = false
	return sendToRunning(req)
}

// offlineManager reads the saved list and settings without starting anything.
func offlineManager() *Manager {
	dir := appDataDir()
	st, _ := loadState(filepath.Join(dir, "settings.json"), defaultSettings(), "settings")
	list, _ := loadState[dlList](filepath.Join(dir, "downloads.json"), nil, "download list")
	m := &Manager{settings: st, byID: map[string]*Download{}, vault: newVault(dir)}
	for _, d := range list {
		if d != nil && d.ID != "" {
			m.vault.openDownload(d)
			m.list = append(m.list, d)
		}
	}
	return m
}

func exportFormat(p string) string {
	if strings.EqualFold(filepath.Ext(p), ".txt") {
		return "txt"
	}
	return "json"
}

// sendToRunning finds the running Zipline through run.json (it may still be starting) and hands it the
// request with the secret; without arguments it brings the window up (a fresh sign-in).
func sendToRunning(req *CLIRequest) int {
	a, err := waitApp(appDataDir(), 10*time.Second)
	if err != nil {
		cliMessage("Zipline: couldn't reach the running app: "+err.Error(), true)
		return 1
	}
	if req.empty() {
		if err := a.post("api/ext/show", map[string]any{}, nil); err != nil {
			cliMessage("Zipline: couldn't reach the running app: "+err.Error(), true)
			return 1
		}
		return 0
	}
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Note  string `json:"note"`
	}
	if err := a.post("api/ext/cli", req, &res); err != nil {
		cliMessage("Zipline: couldn't reach the running app: "+err.Error(), true)
		return 1
	}
	if !res.OK {
		cliMessage(cliPrefix(res.Error), true)
		return 1
	}
	if res.Note != "" {
		cliPrint("Zipline: " + res.Note)
	}
	return 0
}
