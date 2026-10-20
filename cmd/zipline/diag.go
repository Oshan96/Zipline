package main

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
)

// Diagnostics: a small rotating log, crash reports for the windowless Windows build, and the
// text Help → Copy diagnostics puts on the clipboard. None of them may hold a password,
// pairing code or signed link: people paste them into public bug reports.

const (
	logMax       = 1 << 20   // zipline.log moves to zipline.log.1 at this size
	crashMax     = 1 << 20   // crash.log is cut back when it's grown past this…
	crashKeep    = 256 << 10 // …to its last this many bytes
	diagLogLines = 200
	diagCrashMax = 16 << 10 // of the latest crash report
)

func versionText() string {
	return fmt.Sprintf("Zipline %s (%s/%s, %s)", appVersion, runtime.GOOS, runtime.GOARCH, runtime.Version())
}

// ---------------------------------------------------------------- log

type rotLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
	size int64
}

var appLog rotLog

// openLog starts writing logf's lines to dataDir/zipline.log. Until it's called logf does
// nothing, so tests and command-line runs leave no files behind.
func openLog(dataDir string) error {
	appLog.mu.Lock()
	if appLog.f != nil {
		appLog.f.Close()
		appLog.f = nil
	}
	err := appLog.open(filepath.Join(dataDir, "zipline.log"))
	appLog.mu.Unlock()
	if err == nil {
		logf("%s started", versionText())
	}
	return err
}

// closeLog stops logging (tests, and before quitting).
func closeLog() {
	appLog.mu.Lock()
	defer appLog.mu.Unlock()
	if appLog.f != nil {
		appLog.f.Close()
		appLog.f = nil
	}
}

// open starts appending to p. Caller holds l.mu.
func (l *rotLog) open(p string) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) // file names and sites are private
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.path, l.size = f, p, st.Size()
	return nil
}

// rotate keeps the full log as zipline.log.1 (replacing the older one) and starts a new one.
// Caller holds l.mu.
func (l *rotLog) rotate() {
	l.f.Close()
	l.f = nil
	if os.Rename(l.path, l.path+".1") != nil {
		os.Truncate(l.path, 0) // another program has it open: start over rather than grow
	}
	l.open(l.path)
}

// logf adds a timestamped line to the log. Web addresses in it lose their logins and
// secret-looking query values (see redactURL); don't pass it headers or cookies.
func logf(format string, args ...any) {
	appLog.mu.Lock()
	defer appLog.mu.Unlock()
	if appLog.f == nil {
		return
	}
	msg := redactText(strings.TrimRight(fmt.Sprintf(format, args...), "\r\n"))
	line := time.Now().Format("2006-01-02 15:04:05.000 ") + msg + "\n"
	if appLog.size > 0 && appLog.size+int64(len(line)) > logMax {
		appLog.rotate()
		if appLog.f == nil {
			return
		}
	}
	n, _ := appLog.f.WriteString(line)
	appLog.size += int64(n)
}

// ---------------------------------------------------------------- redaction

var urlInText = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)

// redactText applies redactURL to every web address in s.
func redactText(s string) string { return urlInText.ReplaceAllStringFunc(s, redactURL) }

// redactURL makes a link safe to log: "user:pass@" goes, and so do the values of query (and
// fragment) parameters that look like credentials, such as signed-link signatures
// (X-Amz-Signature, X-Goog-Signature, Azure's sig), tokens, keys and session IDs. The rest is
// left exactly as it was, so the log still shows which site and file it was.
func redactURL(raw string) string {
	rest, frag, hasFrag := strings.Cut(raw, "#")
	base, query, hasQuery := strings.Cut(rest, "?")
	if i := strings.Index(base, "://"); i >= 0 {
		auth := base[i+3:]
		if end := strings.IndexByte(auth, '/'); end >= 0 {
			auth = auth[:end]
		}
		if at := strings.LastIndexByte(auth, '@'); at >= 0 {
			base = base[:i+3] + base[i+3+at+1:]
		}
	}
	if hasQuery {
		base += "?" + redactParams(query)
	}
	if hasFrag {
		base += "#" + redactParams(frag)
	}
	return base
}

func redactParams(q string) string {
	parts := strings.Split(q, "&")
	for i, p := range parts {
		k, _, ok := strings.Cut(p, "=")
		if !ok {
			continue
		}
		if uk, err := url.QueryUnescape(k); err == nil {
			k = uk
		}
		if secretParam(k) {
			parts[i] = p[:strings.IndexByte(p, '=')] + "=REDACTED"
		}
	}
	return strings.Join(parts, "&")
}

// secretParam says whether a query parameter's value is a credential. Matching is loose on
// purpose: hiding a harmless value costs little, logging a token costs a lot.
func secretParam(key string) bool {
	k := strings.ToLower(key)
	switch k {
	case "sid", "code", "otp", "hdnts", "hdntl", "__gda__", "x-amz-date": // Akamai tokens; AWS date pairs with its signature
		return true
	}
	for _, w := range []string{"token", "sig", "key", "auth", "pass", "pwd", "session", "secret", "credential", "cookie", "jwt", "policy", "ticket"} {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- crashes

// crashMark starts the line each run writes to crash.log, so a report says which version
// crashed and roughly when (Go's report itself has no time).
const crashMark = "=== "

// setupCrashOutput sends Go's report of a fatal crash (an unrecovered panic or a fatal
// runtime error) to dataDir/crash.log as well as to standard error. The Windows build has
// no console (-H windowsgui), so without this a crash leaves no trace at all.
func setupCrashOutput(dataDir string) {
	p := filepath.Join(dataDir, "crash.log")
	trimFileTail(p, crashMax, crashKeep)
	dropIdleRun(p)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		logf("crash log: %v", err)
		return
	}
	defer f.Close() // SetCrashOutput keeps its own copy of the file
	fmt.Fprintf(f, "%s%s started %s\n", crashMark, versionText(), time.Now().Format("2006-01-02 15:04:05 -0700"))
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		logf("crash log: %v", err)
	}
}

// dropIdleRun removes the line the previous run wrote at its start when nothing followed it
// (it didn't crash), so crash.log only grows with crashes.
func dropIdleRun(p string) {
	st, err := os.Stat(p)
	if err != nil {
		return
	}
	b, err := readTail(p, 512)
	if err != nil {
		return
	}
	i := bytes.LastIndexByte(bytes.TrimRight(b, "\n"), '\n') + 1
	if bytes.HasPrefix(b[i:], []byte(crashMark)) && (i > 0 || int64(len(b)) == st.Size()) {
		os.Truncate(p, st.Size()-int64(len(b)-i))
	}
}

// trimFileTail cuts a file that has grown past limit back to its last keep bytes, from the
// start of a line.
func trimFileTail(p string, limit, keep int64) error {
	st, err := os.Stat(p)
	if err != nil || st.Size() <= limit {
		return err
	}
	b, err := readTail(p, keep)
	if err != nil {
		return err
	}
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[i+1:]
	}
	return os.WriteFile(p, append([]byte("[older crash reports removed]\n"), b...), 0o600)
}

// readTail reads the last n bytes of a file (all of it if it's shorter).
func readTail(p string, n int64) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if off := st.Size() - n; off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(f, n))
}

// latestCrash is the last run in crash.log that crashed: its "=== … started" line and Go's
// report after it (cut short past diagCrashMax: the panic and the crashing goroutine come
// first). "" if none did.
func latestCrash(log string) string {
	s := strings.TrimRight(log, "\n")
	for {
		i := strings.LastIndex(s, "\n"+crashMark) + 1 // 0: the text's start
		run := s[i:]
		if _, report, _ := strings.Cut(run, "\n"); strings.TrimSpace(report) != "" {
			if len(run) > diagCrashMax {
				run = strings.ToValidUTF8(run[:diagCrashMax], "") + "\n[…]"
			}
			return run
		}
		if i == 0 {
			return ""
		}
		s = s[:i-1]
	}
}

// ---------------------------------------------------------------- Help → Copy diagnostics

// diagnosticsText describes this copy of Zipline for a bug report: version, system, a
// summary of the settings without any login, password or pairing code, how many downloads
// are in each state, the end of the log and the latest crash report.
func diagnosticsText(m *Manager) string {
	m.mu.Lock()
	s := m.settings
	s.Sites = slices.Clone(s.Sites) // (phone devices and site rules can change in place)
	s.Phone.Devices = slices.Clone(s.Phone.Devices)
	dataDir, extSeen := m.dataDir, m.extSeen
	status, kinds := map[string]int{}, map[string]int{}
	total := 0
	for _, d := range m.list {
		if d.Provisional {
			continue
		}
		total++
		status[d.Status]++
		k := d.Kind
		if k == "" {
			k = "file"
		}
		kinds[k]++
	}
	m.mu.Unlock()

	var b strings.Builder
	b.WriteString(versionText() + "\n")
	fmt.Fprintf(&b, "Time: %s\n", time.Now().Format("2006-01-02 15:04:05 -0700"))
	fmt.Fprintf(&b, "Data folder: %s\n", dataDir)
	if extSeen.IsZero() {
		b.WriteString("Browser extension: not seen since Zipline started\n")
	} else {
		fmt.Fprintf(&b, "Browser extension: last seen %s ago\n", time.Since(extSeen).Round(time.Second))
	}

	b.WriteString("\nSettings\n")
	for _, l := range settingsSummary(s) {
		b.WriteString("  " + l + "\n")
	}

	fmt.Fprintf(&b, "\nDownloads: %d\n", total)
	if total > 0 {
		fmt.Fprintf(&b, "  by status: %s\n", countList(status, []string{StQueued, StConnecting, StDownloading, StPaused, StCompleted, StError}))
		fmt.Fprintf(&b, "  by kind: %s\n", countList(kinds, []string{"file", "hls", "dash", "torrent"}))
	}

	lines := logTail(dataDir, diagLogLines)
	fmt.Fprintf(&b, "\nLog (last %d lines)\n", len(lines))
	for _, l := range lines {
		b.WriteString("  " + l + "\n")
	}

	c, _ := readTail(filepath.Join(dataDir, "crash.log"), crashKeep)
	if crash := latestCrash(string(c)); crash != "" {
		b.WriteString("\nLatest crash (crash.log)\n" + redactText(crash) + "\n")
	} else {
		b.WriteString("\nNo crash reports.\n")
	}
	return b.String()
}

// settingsSummary lists the settings that explain how downloads behave. Logins, passwords,
// pairing codes and the proxy's user name are left out; only whether one is saved shows.
func settingsSummary(s Settings) []string {
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	folder := s.DownloadDir
	if free, fs, _, err := diskInfo(s.DownloadDir); err == nil {
		folder += fmt.Sprintf(" (%s, %s free)", fsDisplayName(fs), humanSize(free))
	}
	add("Download folder: %s; category folders %s", folder, onOff(s.CategoryFolders))
	add("Downloads at once (started by hand): %d; connections each: %d", s.MaxActive, s.Conns)
	add("Speed limit: %s (%d KB/s)", onOff(s.SpeedLimitOn), s.SpeedLimitKB)
	add("Traffic profiles: %d, default %s, schedule %s (%d times); data quotas: %d, counting %s", len(s.Traffic.Profiles), s.Traffic.profile(s.Traffic.Default).Name,
		onOff(s.Traffic.Sched), len(s.Traffic.Rules), len(s.Quota.Rules), cmp.Or(s.Quota.Count, "both"))
	add("Queues: %d", len(s.Queues))

	proxy := s.Proxy.Mode
	if proxy == "" {
		proxy = "system"
	}
	if s.Proxy.Mode == "manual" {
		if u, err := parseProxyURL(s.Proxy.URL); err == nil {
			proxy += " " + u.Scheme + "://" + u.Host // (never its user name or password)
		} else {
			proxy += " (address not valid)"
		}
		if s.Proxy.User != "" || s.Proxy.Pass != "" || strings.Contains(s.Proxy.URL, "@") {
			proxy += ", login saved"
		}
		if strings.TrimSpace(s.Proxy.Bypass) != "" {
			proxy += ", bypass list set"
		}
	}
	add("Proxy: %s", proxy)

	logins := 0
	for _, r := range s.Sites {
		if r.User != "" || r.Pass != "" {
			logins++
		}
	}
	add("Site rules: %d (%d with a saved login); sites the extension skips: %d", len(s.Sites), logins, len(s.SkipSites))

	paired := 0
	for _, d := range s.Phone.Devices {
		if !d.Pending {
			paired++
		}
	}
	add("Phone control: %s, port %d, %d paired", onOff(s.Phone.On), s.Phone.Port, paired)

	tp := s.Torrent
	port := "automatic"
	if tp.Port != 0 {
		port = fmt.Sprint(tp.Port)
	}
	up := "none"
	if tp.UpKB > 0 {
		up = fmt.Sprintf("%d KB/s", tp.UpKB)
	}
	add("Torrents: port %s, %d peers, upload limit %s, share ratio %g or %d min, DHT %s", port, tp.MaxPeers, up, tp.SeedRatio, tp.SeedMins, onOff(!tp.NoDHT))

	scan := onOff(s.Scan.On)
	if s.Scan.On {
		if s.Scan.Program == "" {
			scan += " (automatic)"
		} else {
			scan += " (" + filepath.Base(s.Scan.Program) + ")"
		}
	}
	add("Virus scan: %s", scan)
	add("Data cap: %s; data plan hours: %s; pause on metered: %s", onOff(s.Cap.On), onOff(s.Plan.On), onOff(s.Plan.Metered))
	add("Several connections: %s (%d chosen); polite mode: %s", onOff(s.Multi.On), len(s.Multi.Use), onOff(s.Polite))
	add("Clipboard: %s; start early: %s; checksums: %s; refresh expired links: %s; tray: %s; sounds: %s",
		onOff(s.ClipboardMonitor), onOff(s.EarlyStart), onOff(s.Checksums), onOff(s.AutoRefresh), onOff(s.Tray), onOff(s.Sounds.On))
	return out
}

// countList writes counts in the given order, then any others alphabetically: "completed 3, error 1".
func countList(n map[string]int, order []string) string {
	var keys []string
	for k := range n {
		if !slices.Contains(order, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range append(slices.Clone(order), keys...) {
		if n[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, n[k]))
		}
	}
	return strings.Join(parts, ", ")
}

// logTail is the last n lines of the log (with the previous file's end if needed), redacted.
func logTail(dataDir string, n int) []string {
	p := filepath.Join(dataDir, "zipline.log")
	appLog.mu.Lock() // so the log isn't moved aside halfway through
	old, _ := readTail(p+".1", logMax)
	cur, _ := readTail(p, logMax)
	appLog.mu.Unlock()
	lines := strings.Split(strings.TrimRight(string(old)+string(cur), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		lines[i] = redactText(strings.TrimRight(l, "\r"))
	}
	return lines
}
