package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ================================================================ hourly data cap

// DataCap: move at most MB megabytes in any rolling window of Hours hours (downloads, uploads or
// both: Options → Data plan's "what counts").
type DataCap struct {
	On    bool `json:"on"`
	MB    int  `json:"mb"`
	Hours int  `json:"hours"`
}

type CapView struct {
	On      bool  `json:"on"`
	Used    int64 `json:"used"`
	Limit   int64 `json:"limit"`
	Hours   int   `json:"hours"`
	Blocked bool  `json:"blocked"`
	FreeIn  int64 `json:"freeIn"` // seconds until some allowance frees up
}

var errCapReconnect = errors.New("paused by the data cap")

// usageMeter counts bytes per minute for the rolling cap window.
type usageMeter struct {
	mu      sync.Mutex
	buckets map[int64]int64 // unix minute → bytes
}

func newUsageMeter(file string) *usageMeter {
	u := &usageMeter{buckets: map[int64]int64{}}
	if b, err := os.ReadFile(file); err == nil {
		json.Unmarshal(b, &u.buckets)
	}
	return u
}

func (u *usageMeter) add(n int64) {
	if u == nil {
		return
	}
	min := time.Now().Unix() / 60
	u.mu.Lock()
	u.buckets[min] += n
	u.mu.Unlock()
}

// used returns bytes in the window and how long until the oldest counted minute drops out.
func (u *usageMeter) used(hours int) (int64, time.Duration) {
	if u == nil || hours < 1 {
		return 0, 0
	}
	now := time.Now().Unix() / 60
	from := now - int64(hours)*60 + 1
	u.mu.Lock()
	defer u.mu.Unlock()
	var sum int64
	oldest := int64(-1)
	for m, b := range u.buckets {
		if m < from-24*60 { // older than any possible window
			delete(u.buckets, m)
			continue
		}
		if m >= from {
			sum += b
			if oldest < 0 || m < oldest {
				oldest = m
			}
		}
	}
	var free time.Duration
	if oldest >= 0 {
		free = time.Until(time.Unix((oldest+int64(hours)*60)*60, 0))
		if free < 0 {
			free = 0
		}
	}
	return sum, free
}

// between returns the bytes downloaded from one time to another (to the minute; the last
// day or so is kept).
func (u *usageMeter) between(from, to time.Time) int64 {
	if u == nil || !to.After(from) {
		return 0
	}
	a, b := from.Unix()/60, to.Unix()/60
	u.mu.Lock()
	defer u.mu.Unlock()
	var sum int64
	for m, n := range u.buckets {
		if m >= a && m < b || (m == b && to.Unix()%60 > 0) {
			sum += n
		}
	}
	return sum
}

func (u *usageMeter) save(file string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	b, _ := json.Marshal(u.buckets)
	u.mu.Unlock()
	writeAtomic(file, b)
}

func (m *Manager) capConfig() DataCap {
	c, _ := m.capCfg.Load().(DataCap)
	return c
}

// capWait blocks while the data cap or a quota holds downloads back (see gate.go). Returns
// true if it waited long enough that the connection should be re-opened.
func (m *Manager) capWait(ctx context.Context) bool {
	start := time.Now()
	for m.gate.downHeld() {
		if !sleepCtx(ctx, time.Second) {
			break
		}
	}
	return time.Since(start) > 15*time.Second
}

// capView is called with m.mu held.
func (m *Manager) capView() CapView {
	c := m.capConfig()
	v := CapView{On: c.On, Hours: c.Hours, Limit: int64(c.MB) << 20}
	if !c.On {
		return v
	}
	_, free := m.usage.used(c.Hours)
	used := m.capUsed(c.Hours)
	v.Used = used
	v.Blocked = used >= v.Limit
	v.FreeIn = int64(free.Seconds())
	return v
}

// ================================================================ virus scan

type ScanSettings struct {
	On      bool   `json:"on"`
	Program string `json:"program"` // "" = automatic (Windows Defender, or ClamAV elsewhere)
	Args    string `json:"args"`    // %f = the downloaded file
}

// shouldScan: per-download choice first, then the global setting. Caller holds m.mu.
func (m *Manager) shouldScan(d *Download) bool {
	switch d.Scan {
	case "on":
		return true
	case "off":
		return false
	}
	return m.settings.Scan.On
}

// scanner returns the command for a file, or an error if no scanner is available.
func (m *Manager) scanner(file string) (*exec.Cmd, string, error) {
	m.mu.Lock()
	s := m.settings.Scan
	m.mu.Unlock()
	if strings.TrimSpace(s.Program) != "" {
		args := splitArgs(s.Args)
		hasFile := false
		for i, a := range args {
			if strings.Contains(a, "%f") {
				args[i] = strings.ReplaceAll(a, "%f", file)
				hasFile = true
			}
		}
		if !hasFile {
			args = append(args, file)
		}
		return exec.Command(strings.TrimSpace(s.Program), args...), filepath.Base(s.Program), nil
	}
	prog, args, name := autoScanner(file)
	if prog == "" {
		return nil, "", errors.New("no virus scanner found — choose one in Options → Virus scan")
	}
	return exec.Command(prog, args...), name, nil
}

// splitArgs splits a command line, keeping "quoted parts" together.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	inQ, has := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQ, has = !inQ, true
		case (r == ' ' || r == '\t') && !inQ:
			if has {
				out = append(out, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		out = append(out, cur.String())
	}
	return out
}

func (m *Manager) scanFile(d *Download) {
	m.scanSem <- struct{}{} // one scan at a time
	defer func() { <-m.scanSem }()
	m.mu.Lock()
	file := d.path()
	d.ScanState, d.ScanNote = "scanning", "Scanning for viruses…"
	m.dirty = true
	m.mu.Unlock()

	set := func(state, note string) {
		m.mu.Lock()
		d.ScanState, d.ScanNote = state, note
		m.dirty = true
		if state == "threat" {
			m.notify("threat", d.ID, iso(d.FileName)+": "+note)
		}
		m.mu.Unlock()
		if state == "threat" {
			trayNotify("Virus scan warning", d.FileName+" — "+note, true)
		}
	}
	if _, err := os.Stat(file); err != nil {
		set("failed", "Not scanned: the file is no longer there")
		return
	}
	cmd, name, err := m.scanner(file)
	if err != nil {
		set("failed", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd = exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
	hideConsole(cmd)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			set("failed", "Couldn't run "+name+": "+err.Error())
			return
		}
	}
	_, statErr := os.Stat(file)
	switch {
	case ctx.Err() != nil:
		set("failed", "Scan took too long and was stopped")
	case code == 0 && statErr == nil:
		set("clean", "No threats found ("+name+")")
	case statErr != nil:
		set("threat", "Threat found — "+name+" removed or quarantined the file")
	case !scanFoundThreat(name, code):
		// the scanner itself failed (ClamAV 2: no virus database, Defender switched off…)
		detail := strings.TrimSpace(lastLine(string(out)))
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", code)
		}
		set("failed", "Not scanned: "+name+" couldn't scan it ("+detail+")")
	default:
		detail := strings.TrimSpace(lastLine(string(out)))
		if detail == "" {
			detail = fmt.Sprintf("exit code %d", code)
		}
		set("threat", "Threat found by "+name+" ("+detail+")")
	}
}

// scanFoundThreat reads a scanner's exit code. ClamAV: 1 = found, 2 = error. Microsoft
// Defender's MpCmdRun: 2 = found, anything else is an error. Another program: anything but 0 is
// treated as a problem, as Options → Virus scan says.
func scanFoundThreat(name string, code int) bool {
	switch name {
	case "ClamAV":
		return code == 1
	case "Microsoft Defender":
		return code == 2
	}
	return code != 0
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 140 {
				l = l[:140] + "…"
			}
			return l
		}
	}
	return ""
}

// ScanNow scans finished downloads on demand.
func (m *Manager) ScanNow(ids []string) {
	m.mu.Lock()
	var todo []*Download
	for _, id := range ids {
		if d := m.byID[id]; d != nil && d.Status == StCompleted && d.ScanState != "scanning" && d.ScanState != "waiting" {
			d.ScanState, d.ScanNote = "waiting", "Waiting to scan"
			todo = append(todo, d)
		}
	}
	m.dirty = true
	m.mu.Unlock()
	for _, d := range todo {
		go m.scanFile(d)
	}
}

// ================================================================ export / import

type exportItem struct {
	URL       string            `json:"url"`
	Name      string            `json:"name,omitempty"`
	Dir       string            `json:"dir,omitempty"`
	Category  string            `json:"category,omitempty"`
	Size      int64             `json:"size"`
	Status    string            `json:"status"`
	Queue     string            `json:"queue,omitempty"`
	Desc      string            `json:"desc,omitempty"`
	Added     time.Time         `json:"added"`
	Finished  time.Time         `json:"finished,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	LimitKB   int               `json:"limitKB,omitempty"`
	Scan      string            `json:"scan,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Variant   int               `json:"variant,omitempty"`
	Resumable bool              `json:"resumable,omitempty"`
	ETag      string            `json:"etag,omitempty"`
	Segments  []*Segment        `json:"segments,omitempty"`
	Mirrors   []string          `json:"mirrors,omitempty"`
	Checksum  *Checksum         `json:"checksum,omitempty"`    // what to check the file against
	Skip      []bool            `json:"skip,omitempty"`        // a torrent's files not downloaded
	TorrentB  []byte            `json:"torrentFile,omitempty"` // the .torrent itself (base64 in JSON)
}

type exportFile struct {
	App       string       `json:"app"`
	Version   int          `json:"version"`
	Exported  time.Time    `json:"exported"`
	Queues    []Queue      `json:"queues"`
	Downloads []exportItem `json:"downloads"`
}

var secretHeaders = map[string]bool{"cookie": true, "authorization": true, "proxy-authorization": true}

// Export returns the list as Zipline JSON or as plain links. Cookies and logins are
// left out unless secrets is true.
func (m *Manager) Export(format string, ids []string, secrets bool) ([]byte, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pick := map[string]bool{}
	for _, id := range ids {
		pick[id] = true
	}
	var list []*Download
	for _, d := range m.list {
		if len(pick) == 0 && !d.Private || pick[d.ID] { // (private downloads only when picked)
			list = append(list, d)
		}
	}
	stamp := time.Now().Format("2006-01-02")
	if format == "txt" {
		var b strings.Builder
		for _, d := range list {
			b.WriteString(exportLink(d.URL, secrets) + "\r\n")
		}
		return []byte(b.String()), "zipline-links-" + stamp + ".txt"
	}
	f := exportFile{App: "Zipline", Version: 1, Exported: time.Now(), Queues: m.settings.Queues}
	for _, d := range list {
		h := map[string]string{}
		for k, v := range d.Headers {
			if secrets || !secretHeaders[strings.ToLower(k)] {
				h[k] = v
			}
		}
		if len(h) == 0 {
			h = nil
		}
		d.mu.Lock()
		segs := append([]*Segment{}, d.Segments...)
		d.mu.Unlock()
		it := exportItem{URL: exportLink(d.URL, secrets), Name: d.FileName, Dir: d.Dir, Category: d.Category, Size: d.Size,
			Status: d.Status, Queue: d.Queue, Desc: d.Desc, Added: d.Added, Finished: d.Finished, Headers: h, LimitKB: d.LimitKB,
			Scan: d.Scan, Kind: d.Kind, Variant: d.Variant, Resumable: d.Resumable, ETag: d.ETag, Segments: segs, Mirrors: exportLinks(d.Mirrors, secrets)}
		if c := d.Check; c != nil && c.Want != "" {
			it.Checksum = &Checksum{Algo: c.Algo, Want: c.Want, From: c.From}
		}
		if ts := d.Torrent; ts != nil {
			it.Segments, it.Skip = nil, ts.Skip
			if b, err := os.ReadFile(m.torrentPath(ts.InfoHash)); err == nil && len(b) < 10<<20 {
				it.TorrentB = b
			}
		}
		f.Downloads = append(f.Downloads, it)
	}
	b, _ := json.MarshalIndent(f, "", "  ")
	return b, "zipline-downloads-" + stamp + ".json"
}

type ImportResult struct {
	Added   int    `json:"added"`
	Skipped int    `json:"skipped"`
	Format  string `json:"format"`
}

// Import reads a Zipline JSON export, an IDM-style .ef2 export, or plain links (one per line).
func (m *Manager) Import(content, queueID string, start, skipDup bool) (ImportResult, error) {
	content = strings.TrimPrefix(content, "\uFEFF")
	trim := strings.TrimSpace(content)
	if strings.HasPrefix(trim, "{") {
		var f exportFile
		if err := json.Unmarshal([]byte(trim), &f); err != nil {
			return ImportResult{}, fmt.Errorf("that JSON file isn't a Zipline export: %v", err)
		}
		if f.App != "Zipline" {
			return ImportResult{}, errors.New("that JSON file isn't a Zipline export")
		}
		return m.importZipline(f, skipDup)
	}
	var items []exportItem
	format := "links"
	if strings.Contains(content, "<\r\n") || strings.HasPrefix(trim, "<") {
		format = "ef2"
		items = parseEF2(content)
	} else {
		sc := bufio.NewScanner(strings.NewReader(content))
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		for sc.Scan() {
			for _, f := range strings.Fields(sc.Text()) {
				if URLre(f) || isMagnet(f) {
					items = append(items, exportItem{URL: f})
				}
			}
		}
	}
	if len(items) == 0 {
		return ImportResult{Format: format}, errors.New("no download links found in that file")
	}
	res := ImportResult{Format: format}
	m.mu.Lock()
	have := m.urlSet()
	if queueID != "" && m.queueByID(queueID) == nil {
		queueID = ""
	}
	m.mu.Unlock()
	for _, it := range items {
		if skipDup && have[it.URL] {
			res.Skipped++
			continue
		}
		have[it.URL] = true
		if isMagnet(it.URL) {
			if _, err := m.AddTorrent(TorrentAdd{Magnet: it.URL, QueueID: queueID}); err != nil {
				res.Skipped++
				continue
			}
		} else {
			m.Add(AddReq{URL: it.URL, QueueID: queueID, Headers: it.Headers})
		}
		res.Added++
	}
	if start && queueID != "" && res.Added > 0 {
		m.StartQueueChecked(queueID, false)
	}
	return res, nil
}

func URLre(s string) bool {
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://") || isFTP(l)
}

// urlSet: caller holds m.mu.
func (m *Manager) urlSet() map[string]bool {
	have := map[string]bool{}
	for _, d := range m.list {
		have[d.URL] = true
	}
	return have
}

// parseEF2 reads blocks of the form
//
//	<
//	https://host/file
//	referer: …
//	cookie: …
//	>
func parseEF2(s string) []exportItem {
	var out []exportItem
	var cur *exportItem
	for _, line := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "<":
			cur = &exportItem{}
		case line == ">":
			if cur != nil && URLre(cur.URL) {
				out = append(out, *cur)
			}
			cur = nil
		case cur == nil || line == "":
		case cur.URL == "" && URLre(line):
			cur.URL = line
		default:
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			if cur.Headers == nil {
				cur.Headers = map[string]string{}
			}
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "referer":
				cur.Headers["Referer"] = v
			case "cookie":
				cur.Headers["Cookie"] = v
			case "user-agent":
				cur.Headers["User-Agent"] = v
			}
		}
	}
	return out
}

func (m *Manager) importZipline(f exportFile, skipDup bool) (res ImportResult, err error) {
	res = ImportResult{Format: "zipline"}
	var torrents []TorrentAdd
	defer func() { // runs last, after the list lock is released (adding a torrent takes it)
		for _, ta := range torrents {
			if _, err := m.AddTorrent(ta); err == nil {
				res.Added++
			} else {
				res.Skipped++
			}
		}
	}()
	m.mu.Lock()
	defer func() {
		m.dirty = true
		m.mu.Unlock()
		m.kick()
	}()
	// bring over queues that don't exist here yet
	for _, q := range f.Queues {
		if q.ID != "" && m.queueByID(q.ID) == nil {
			m.settings.Queues = append(m.settings.Queues, q)
		}
	}
	m.normalizeSettings()
	have := m.urlSet()
	for _, it := range f.Downloads {
		if !filepath.IsAbs(it.Dir) {
			it.Dir = "" // a folder from another computer that makes no sense here: use the usual one
		}
		if isMagnet(it.URL) || it.Kind == "torrent" {
			if skipDup && have[it.URL] {
				res.Skipped++
				continue
			}
			have[it.URL] = true
			ta := TorrentAdd{Magnet: it.URL, Dir: it.Dir, Desc: it.Desc, Skip: it.Skip}
			if q := m.queueByID(it.Queue); q != nil {
				ta.QueueID = q.ID
			}
			if len(it.TorrentB) > 0 {
				if mi, err := parseTorrent(it.TorrentB); err == nil {
					m.rememberTorrent(mi, it.TorrentB)
					ta.Magnet, ta.Hash = "", mi.HashHex()
				}
			}
			torrents = append(torrents, ta)
			continue
		}
		if !URLre(it.URL) {
			continue
		}
		if skipDup && have[it.URL] {
			res.Skipped++
			continue
		}
		have[it.URL] = true
		d := &Download{ID: newID(), URL: it.URL, FileName: it.Name, Dir: it.Dir, Category: it.Category, Size: it.Size,
			Status: it.Status, Queue: it.Queue, Desc: it.Desc, Added: it.Added, Finished: it.Finished, Headers: it.Headers,
			LimitKB: it.LimitKB, Scan: it.Scan, Kind: it.Kind, Variant: it.Variant, Conns: m.settings.Conns, Resumable: it.Resumable, ETag: it.ETag,
			Mirrors: cleanMirrors(it.Mirrors, it.URL)}
		if c := it.Checksum; c != nil && c.Want != "" {
			d.Check = &Checksum{Algo: c.Algo, Want: c.Want, From: c.From}
		}
		if d.Added.IsZero() {
			d.Added = time.Now()
		}
		if m.queueByID(d.Queue) == nil {
			d.Queue = ""
		}
		switch d.Status {
		case StCompleted:
			d.done = d.Size
		default:
			// keep progress only if the partial file is here too
			if _, err := os.Stat(d.partPath()); err == nil && it.Kind == "" && len(it.Segments) > 0 {
				d.Segments = it.Segments
				for _, s := range d.Segments {
					d.done += s.Pos - s.Start
				}
			} else {
				d.Size, d.Resumable = -1, false
			}
			d.Status = StPaused
			if d.Queue != "" {
				d.Status = StQueued
			}
		}
		if d.Dir != "" && d.FileName != "" && d.Status != StCompleted {
			d.FileName = m.uniqueName(d.Dir, d.FileName, d)
		}
		m.list = append(m.list, d)
		m.byID[d.ID] = d
		res.Added++
	}
	return res, nil
}

// ================================================================ command line

const cliUsage = `Zipline Download Manager — command line

  Zipline [options] [link ...]

  A link can be http(s)://, ftp(s)://, a magnet link, or a .torrent file on this computer.

Adding downloads
  -o, --output FOLDER    Save into FOLDER
  -n, --name NAME        File name (when adding one link)
  -q, --queue NAME       Add to this queue (created if it doesn't exist)
  -s, --start            Start downloading now
      --silent           Add without showing the "Download file info" window
      --referer URL      Send this Referer
      --cookie TEXT      Send these cookies
      --user-agent TEXT  Send this User-Agent
      --user NAME        Login name (HTTP or FTP)
      --password PASS    Login password (it shows in the process list: --password-stdin is safer)
      --password-stdin   Read the login password from standard input instead

Controlling Zipline
      --start-queue NAME Start a queue
      --stop-queue NAME  Stop a queue
      --import FILE      Import a download list (.json, .txt or .ef2)
      --export FILE      Export the download list (.json, or .txt for links)
      --nowindow         Start in the background, without a window (on Windows, in the tray)
      --quit             Exit a running Zipline
      --login-url        Print a one-time address that opens Zipline's window in a browser
  -v, --version          Show the version
  -h, --help             Show this help

Windows-style switches also work:
  /d LINK  /p FOLDER  /f NAME  /n (silent)  /a (add to queue)  /s (start queue)

Examples
  Zipline https://example.com/file.zip
  Zipline --silent --start -o "D:\ISO" https://example.com/disc.iso
  Zipline -q Night --silent https://a.example/1.zip https://a.example/2.zip
  Zipline "C:\Users\me\Downloads\holiday.torrent"
`

type CLIRequest struct {
	URLs       []string          `json:"urls"`
	Dir        string            `json:"dir"`
	Name       string            `json:"name"`
	Queue      string            `json:"queue"`
	Start      bool              `json:"start"`
	Silent     bool              `json:"silent"`
	Headers    map[string]string `json:"headers"`
	StartQueue string            `json:"startQueue"`
	StopQueue  string            `json:"stopQueue"`
	Import     string            `json:"import"`
	Export     string            `json:"export"`
	Quit       bool              `json:"quit"`
	NoWindow   bool              `json:"-"`
	Help       bool              `json:"-"`
	Version    bool              `json:"-"`
	user, pass string
}

func (r *CLIRequest) empty() bool {
	return len(r.URLs) == 0 && r.StartQueue == "" && r.StopQueue == "" && r.Import == "" && r.Export == "" && !r.Quit
}

func parseCLI(args []string, windows bool) (*CLIRequest, error) {
	r := &CLIRequest{Headers: map[string]string{}}
	stdinPass := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() (string, error) {
			if k, v, ok := strings.Cut(a, "="); ok && strings.HasPrefix(k, "--") {
				return v, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		name := a
		if k, _, ok := strings.Cut(a, "="); ok && strings.HasPrefix(a, "--") {
			name = k
		}
		// IDM-style /x switches (Windows only, so Unix paths aren't mistaken for them)
		if windows && len(a) == 2 && a[0] == '/' {
			name = map[byte]string{'d': "--url", 'p': "--output", 'f': "--name", 'n': "--silent", 'a': "--later", 's': "--start-queue-main", 'q': "--ignore", 'h': "--ignore"}[a[1]|0x20]
			if name == "" {
				return nil, fmt.Errorf("unknown switch %s", a)
			}
		}
		var err error
		var v string
		switch name {
		case "-h", "--help", "-?", "/?":
			r.Help = true
		case "-v", "--version":
			r.Version = true
		case "--url":
			v, err = val()
			r.URLs = append(r.URLs, v)
		case "-o", "--output", "--dir":
			r.Dir, err = val()
		case "-n", "--name":
			r.Name, err = val()
		case "-q", "--queue":
			r.Queue, err = val()
		case "--later":
			if r.Queue == "" {
				r.Queue = "main"
			}
		case "-s", "--start":
			r.Start = true
		case "--silent":
			r.Silent = true
		case "--referer":
			v, err = val()
			r.Headers["Referer"] = v
		case "--cookie":
			v, err = val()
			r.Headers["Cookie"] = v
		case "--user-agent":
			v, err = val()
			r.Headers["User-Agent"] = v
		case "--user":
			r.user, err = val()
		case "--password":
			r.pass, err = val()
		case "--password-stdin":
			stdinPass = true
		case "--start-queue":
			r.StartQueue, err = val()
		case "--start-queue-main":
			r.StartQueue = "main"
		case "--stop-queue":
			r.StopQueue, err = val()
		case "--import":
			r.Import, err = val()
		case "--export":
			r.Export, err = val()
		case "--nowindow", "-nowindow":
			r.NoWindow = true
		case "--quit":
			r.Quit = true
		case "--ignore":
		default:
			if strings.HasPrefix(a, "-") {
				return nil, fmt.Errorf("unknown option %s (try --help)", a)
			}
			if strings.HasPrefix(strings.ToLower(a), "file://") { // a file manager may pass file:///…/x.torrent
				if u, err := url.Parse(a); err == nil && u.Path != "" {
					pth := u.Path
					if len(pth) > 2 && pth[0] == '/' && pth[2] == ':' { // file:///C:/Users/…
						pth = pth[1:]
					}
					a = filepath.FromSlash(pth)
				}
			}
			if strings.HasSuffix(strings.ToLower(a), ".torrent") && !URLre(a) { // a torrent file on this computer
				abs, err := filepath.Abs(a)
				if err != nil {
					return nil, err
				}
				r.URLs = append(r.URLs, "torrentfile:"+abs)
				continue
			}
			if !URLre(a) && !isMagnet(a) {
				return nil, fmt.Errorf("%q isn't an http, https, ftp, ftps or magnet link, or a .torrent file", a)
			}
			r.URLs = append(r.URLs, a)
		}
		if err != nil {
			return nil, err
		}
	}
	if stdinPass && !r.Help && !r.Version {
		if r.user == "" || r.pass != "" {
			return nil, errors.New("--password-stdin goes with --user, and instead of --password")
		}
		var err error
		if r.pass, err = readPasswordLine(cliStdin); err != nil {
			return nil, err
		}
	}
	if r.user != "" {
		r.Headers["Authorization"] = basicAuth(r.user, r.pass)
	}
	if r.Name != "" && len(r.URLs) > 1 {
		return nil, errors.New("--name only works with a single link")
	}
	for _, p := range []*string{&r.Dir, &r.Import, &r.Export} {
		if *p != "" {
			if abs, err := filepath.Abs(*p); err == nil {
				*p = abs
			}
		}
	}
	return r, nil
}

// resolveQueue finds a queue by id or name; creates it by name when missing. Caller holds m.mu.
func (m *Manager) resolveQueue(name string) string {
	if name == "" {
		return ""
	}
	for _, q := range m.settings.Queues {
		if q.ID == name || strings.EqualFold(q.Name, name) {
			return q.ID
		}
	}
	q := Queue{ID: newID(), Name: name, MaxConcurrent: 3, Sched: defaultSchedule()}
	m.settings.Queues = append(m.settings.Queues, q)
	m.normalizeSettings()
	m.dirty = true
	return q.ID
}

// ApplyCLI carries out a command-line request in the running app.
func (m *Manager) ApplyCLI(r *CLIRequest) (string, error) {
	var notes []string
	if r.Import != "" {
		b, err := os.ReadFile(r.Import)
		if err != nil {
			return "", err
		}
		m.mu.Lock()
		q := m.resolveQueue(r.Queue)
		m.mu.Unlock()
		res, err := m.Import(string(b), q, r.Start, true)
		if err != nil {
			return "", err
		}
		notes = append(notes, fmt.Sprintf("imported %d (skipped %d already in the list)", res.Added, res.Skipped))
	}
	if r.Export != "" {
		format := "json"
		if strings.EqualFold(filepath.Ext(r.Export), ".txt") {
			format = "txt"
		}
		b, _ := m.Export(format, nil, false)
		if err := os.WriteFile(r.Export, b, 0o644); err != nil {
			return "", err
		}
		notes = append(notes, "exported to "+r.Export)
	}
	if len(r.URLs) > 0 {
		m.mu.Lock()
		q := m.resolveQueue(r.Queue)
		m.mu.Unlock()
		h := map[string]string{}
		for k, v := range r.Headers {
			h[k] = v
		}
		if len(h) == 0 {
			h = nil
		}
		skipped := 0 // links from protected sites (YouTube, DRM services)
		for _, u := range r.URLs {
			if protectedLink(u, h) {
				skipped++
				continue
			}
			if p, ok := strings.CutPrefix(u, "torrentfile:"); ok { // read it here: the window can't
				b, err := os.ReadFile(p)
				if err != nil {
					return "", err
				}
				mi, err := parseTorrent(b)
				if err != nil {
					return "", fmt.Errorf("%s: %w", filepath.Base(p), err)
				}
				m.mu.Lock()
				m.rememberTorrent(mi, b)
				m.mu.Unlock()
				u = torrentRef(mi.InfoHash)
			}
			if r.Silent && (isMagnet(u) || strings.HasPrefix(u, "torrent:")) {
				ta := TorrentAdd{Magnet: u, Dir: r.Dir, Start: r.Start || q == "", QueueID: q}
				if ih, ok := parseTorrentRef(u); ok {
					ta.Magnet, ta.Hash = "", hex.EncodeToString(ih[:])
				}
				if _, err := m.AddTorrent(ta); err != nil {
					return "", err
				}
				continue
			}
			if r.Silent {
				m.Add(AddReq{URL: u, Name: r.Name, Dir: r.Dir, Start: r.Start || q == "", QueueID: q, Headers: h})
			} else {
				m.AddPrompt(Prompt{URL: u, Name: r.Name, Dir: r.Dir, QueueID: q, Headers: h, Source: "cli"})
			}
		}
		if skipped == len(r.URLs) {
			return "", errors.New(protectedMsg)
		}
		if r.Silent && q != "" && r.Start {
			if err := m.StartQueueChecked(q, false); err != nil {
				notes = append(notes, err.Error())
			}
		}
		if !r.Silent && !r.NoWindow {
			go bringToFront()
		}
		notes = append(notes, "added "+plural(len(r.URLs)-skipped, "link"))
		if skipped > 0 {
			notes = append(notes, fmt.Sprintf("skipped %s: %s", plural(skipped, "link"), protectedMsg))
		}
	}
	if r.StartQueue != "" {
		m.mu.Lock()
		q := m.resolveQueue(r.StartQueue)
		m.mu.Unlock()
		if err := m.StartQueueChecked(q, false); err != nil {
			notes = append(notes, err.Error())
		}
	}
	if r.StopQueue != "" {
		m.mu.Lock()
		q := m.resolveQueue(r.StopQueue)
		m.mu.Unlock()
		m.StopQueue(q)
	}
	if r.Quit {
		go func() {
			time.Sleep(200 * time.Millisecond)
			m.Quit()
		}()
	}
	return strings.Join(notes, "; "), nil
}

// Quit asks the app to exit (safe to call more than once).
func (m *Manager) Quit() {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.quit:
	default:
		close(m.quit)
	}
}

// ================================================================ "download all links" from a page

type PageLinks struct {
	Page      string            `json:"page"`
	Title     string            `json:"title"`
	Links     []PageLink        `json:"links"`
	Cookies   map[string]string `json:"cookies"` // host → Cookie header (older extensions; now asked for when you pick files)
	UserAgent string            `json:"userAgent"`
	Ctx       json.RawMessage   `json:"ctx"`     // the window they came from, as the extension describes it
	Private   bool              `json:"private"` // a private window
}

type PageLink struct {
	URL  string `json:"url"`
	Text string `json:"text"`
}

// FromPage turns a page's links into a finished grab so the grabber window can show them.
func (g *Grabber) FromPage(p PageLinks) (*GrabJob, error) {
	if len(p.Links) == 0 {
		return nil, errors.New("no links on that page")
	}
	if len(p.Links) > 5000 {
		p.Links = p.Links[:5000]
	}
	h := map[string]string{}
	if p.UserAgent != "" {
		h["User-Agent"] = p.UserAgent
	}
	j := &GrabJob{ID: newID(), Opts: GrabOptions{URL: p.Page, Headers: h}, State: "done", Started: time.Now(), Finished: time.Now(),
		seen: map[string]bool{}, Pages: 1, Title: p.Title, Cookies: p.Cookies, FromPage: true, private: p.Private}
	if len(p.Ctx) <= 4<<10 {
		j.extCtx = p.Ctx
	}
	for _, l := range p.Links {
		u, err := url.Parse(strings.TrimSpace(l.URL))
		if err != nil || !URLre(u.String()) {
			continue
		}
		u.Fragment = ""
		name := path.Base(u.Path)
		if name == "" || name == "/" || name == "." {
			name = u.Host
		}
		if un, err := url.PathUnescape(name); err == nil {
			name = un
		}
		j.addFile(GrabFile{URL: u.String(), Name: name, Ext: extOf(u), Category: g.category(u), From: p.Page, Text: strings.TrimSpace(l.Text)})
	}
	sort.SliceStable(j.Files, func(a, b int) bool { return j.Files[a].Category < j.Files[b].Category })
	g.mu.Lock()
	g.jobs[j.ID] = j
	g.mu.Unlock()
	return j, nil
}

// silence "imported and not used" if a platform build drops a helper
var _ = atomic.AddInt64

func basicAuth(user, pass string) string {
	return "Basic " + base64Std(user+":"+pass)
}

func base64Std(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// uiCommand asks the window to do something (e.g. "add" opens the Add URL box). It's a
// prompt rather than a notice so a window that opens afterwards still sees it.
func (m *Manager) uiCommand(cmd string) {
	m.AddPrompt(Prompt{Kind: "ui:" + cmd, Source: "tray"})
}

// downloadErrText turns low-level network errors into one plain sentence.
func downloadErrText(err error, rawURL string) string {
	return friendlyError(err, rawURL)
}
