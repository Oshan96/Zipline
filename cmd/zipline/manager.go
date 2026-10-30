package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Category struct {
	Name string   `json:"name"`
	Exts []string `json:"exts"`
}

type Schedule struct {
	Enabled       bool   `json:"enabled"`
	Start         string `json:"start"` // "HH:MM"
	StopEnabled   bool   `json:"stopEnabled"`
	Stop          string `json:"stop"`
	Days          []int  `json:"days"`     // 0=Sun..6=Sat
	WhenDone      string `json:"whenDone"` // none|quit|sleep|hibernate|shutdown
	RepeatEnabled bool   `json:"repeatEnabled"`
	RepeatMins    int    `json:"repeatMins"`     // start again every N minutes
	Wake          bool   `json:"wake,omitempty"` // wake the computer for the start (Windows)
}

// Queue: a named list of downloads with its own concurrency, schedule and, for
// synchronization queues, periodic re-checking of finished files.
type Queue struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	MaxConcurrent int       `json:"maxConcurrent"`
	Sync          bool      `json:"sync"`
	Sched         Schedule  `json:"schedule"`
	LastRun       time.Time `json:"lastRun,omitempty"`   // the scheduled start last acted on (sched.go)
	Retries       *int      `json:"retries,omitempty"`   // try a failed download again this often (nil: 3)
	RetryMins     int       `json:"retryMins,omitempty"` // minutes between tries
}

type Settings struct {
	DownloadDir        string          `json:"downloadDir"`
	CategoryFolders    bool            `json:"categoryFolders"`
	MaxConcurrent      int             `json:"maxConcurrent"`
	MaxActive          int             `json:"maxActive"` // downloads started by hand that run at once (queues have their own)
	Conns              int             `json:"conns"`
	SpeedLimitOn       bool            `json:"speedLimitOn"`
	SpeedLimitKB       int             `json:"speedLimitKB"`
	ClipboardMonitor   bool            `json:"clipboardMonitor"`
	ShowCompleteDialog bool            `json:"showCompleteDialog"`
	ShowProgressDialog bool            `json:"showProgressDialog"`
	Categories         []Category      `json:"categories"`
	Sched              Schedule        `json:"schedule"` // (older settings files: became the main queue's schedule)
	Queues             []Queue         `json:"queues"`
	Proxy              ProxySettings   `json:"proxy"`
	Cap                DataCap         `json:"cap"`
	Scan               ScanSettings    `json:"scan"`
	Tray               bool            `json:"tray"`
	Sites              []SiteRule      `json:"sites"`
	SkipSites          []string        `json:"skipSites"` // the browser extension never takes downloads from these
	Keys               KeySettings     `json:"keys"`
	Sounds             SoundSettings   `json:"sounds"`
	EarlyStart         bool            `json:"earlyStart"` // start downloading while the Add dialog is open
	Plan               PlanSettings    `json:"plan"`
	Polite             bool            `json:"polite"`      // slow down when other apps or devices need the line
	Multi              MultiNet        `json:"multi"`       // use several internet connections at once
	Checksums          bool            `json:"checksums"`   // look for checksums sites publish; check files as they finish
	AutoRefresh        bool            `json:"autoRefresh"` // when a link expires, get a fresh one from the page it came from
	Phone              PhoneSettings   `json:"phone"`       // control from a phone on the same network
	Torrent            TorrentSettings `json:"torrent"`
	KeepAwake          bool            `json:"keepAwake"`        // the computer doesn't sleep while downloading
	StartPaused        bool            `json:"startPaused"`      // at start-up, don't carry on with what was running
	Resume             *ResumeState    `json:"resume,omitempty"` // what was running (startup.go)
	Traffic            TrafficSettings `json:"traffic"`          // traffic profiles and their schedule
	Quota              QuotaSettings   `json:"quota"`            // daily and monthly data quotas
	Browser            BrowserSettings `json:"browser"`          // the browser extension's (Options → Browser), sent with each ping (extension.go)
	Theme              string          `json:"theme,omitempty"`  // the window's theme ("", "light", "dark"): the extension's popup follows it
}

func defaultSettings() Settings {
	home, _ := os.UserHomeDir()
	return Settings{
		DownloadDir:        filepath.Join(home, "Downloads"),
		CategoryFolders:    true,
		MaxConcurrent:      3,
		MaxActive:          6,
		Conns:              16,
		SpeedLimitKB:       1024,
		ClipboardMonitor:   true,
		ShowCompleteDialog: true,
		ShowProgressDialog: true,
		Categories: []Category{
			{"Compressed", strings.Fields("zip rar 7z tar gz tgz bz2 xz lz zst cab arj")},
			{"Documents", strings.Fields("pdf doc docx xls xlsx ppt pptx txt rtf odt ods odp epub mobi csv md")},
			{"Music", strings.Fields("mp3 wav flac aac m4a ogg oga wma opus aif aiff mid")},
			{"Programs", strings.Fields("exe msi msix apk xapk dmg pkg deb rpm appimage jar bin run iso img")},
			{"Video", strings.Fields("mp4 mkv avi mov wmv webm flv m4v mpg mpeg 3gp ts m2ts vob ogv")},
			{"Images", strings.Fields("jpg jpeg png gif webp bmp tif tiff svg heic psd raw")},
		},
		Sched:       defaultSchedule(),
		Proxy:       ProxySettings{Mode: "system"},
		Cap:         DataCap{MB: 500, Hours: 1},
		Tray:        true,
		Keys:        KeySettings{Zipline: "alt", Browser: "ctrlshift"},
		Sounds:      SoundSettings{Done: true, Failed: true, Queue: true},
		EarlyStart:  true,
		Plan:        defaultPlan(),
		Checksums:   true,
		AutoRefresh: true,
		Torrent:     defaultTorrentSettings(),
		KeepAwake:   true,
		Browser:     defaultBrowser(),
	}
}

func defaultSchedule() Schedule {
	return Schedule{Start: "02:00", Stop: "07:00", Days: []int{0, 1, 2, 3, 4, 5, 6}, WhenDone: "none", RepeatMins: 120}
}

type Prompt struct {
	ID      string            `json:"id"`
	URL     string            `json:"url"`
	Name    string            `json:"name,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Source  string            `json:"source"`
	Title   string            `json:"title,omitempty"` // page title, used to name video streams
	Kind    string            `json:"kind,omitempty"`  // "" = one link, "links" = page links to pick from
	JobID   string            `json:"jobId,omitempty"`
	Dir     string            `json:"dir,omitempty"` // preset save folder (command line)
	QueueID string            `json:"queueId,omitempty"`
	// a download taken over from the browser (extension.go): its request, what the reply said, and
	// whether closing the dialog hands it back to the browser
	Method  string     `json:"method,omitempty"`
	Body    []byte     `json:"body,omitempty"`
	Known   *knownFile `json:"known,omitempty"`
	Taken   bool       `json:"taken,omitempty"`
	Private bool       `json:"private,omitempty"` // from a private window
}

type Notice struct {
	Seq   int64  `json:"seq"`
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Text  string `json:"text,omitempty"`
	Queue bool   `json:"queue,omitempty"` // complete: it finished in a queue
}

type Manager struct {
	mu         sync.Mutex
	list       []*Download
	byID       map[string]*Download
	settings   Settings
	dataDir    string
	dirty      bool
	qRun       map[string]bool
	qBySched   map[string]bool
	qSyncing   map[string]bool
	qLastStart map[string]time.Time
	qDone      map[string]int // downloads each queue finished since it last finished itself (its tray balloon)
	limiter    *Limiter
	capCfg     atomic.Value // DataCap
	usage      *usageMeter
	scanSem    chan struct{}
	grab       *Grabber
	prompts    []Prompt
	notices    []Notice
	seq        int64
	quit       chan struct{}

	planIn      bool // inside the free data hours (Options → Data plan)
	planSaved   time.Time
	metered     bool // on a metered connection, and the option is on
	meterKnown  bool
	meterPaused map[string]bool // downloads paused because of it

	politeLim   *Limiter // polite mode's current limit
	politeRate  float64
	politeQueue time.Duration

	refresh map[string]*refreshJob // expired links the browser extension is asked to renew
	extSeen time.Time              // the browser extension last checked in
	ext     *extJobs               // jobs for the extension (extension.go)
	phone   *phoneServer           // the listener for phones (Options → Phone)

	torrents     map[string]*torrent // running or sharing, by download
	dht          *DHT
	tlisten      net.Listener
	tport        int
	tnet         *torrentNet // the port's UDP side (uTP, the DHT) and its router mapping (tnet.go)
	upLim        *Limiter
	upRate       int64                   // the upload limit's current rate (bytes/s)
	alert        string                  // shown once in the window: a damaged file was recovered at start
	metaSessions map[string]*metaSession // magnet links whose file list the Add dialog is waiting for

	hosts  *hostState  // connections per server, shared fairly between downloads
	gate   *gate       // the rules every start and every byte go through (gate.go)
	saveMu sync.Mutex  // one save at a time
	saving atomic.Bool // the loop's background save is still going

	vault *vault // seals the secrets in the saved files (vault.go)

	sx schedState // the scheduler, "when finished" actions, retries and the start-up resume (sched.go)
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func NewManager(dataDir string) *Manager {
	makePrivateDir(dataDir)
	tightenDataDir(dataDir)
	m := &Manager{byID: map[string]*Download{}, dataDir: dataDir, limiter: &Limiter{}, quit: make(chan struct{}),
		qRun: map[string]bool{}, qBySched: map[string]bool{}, qSyncing: map[string]bool{}, qLastStart: map[string]time.Time{}, qDone: map[string]int{},
		meterPaused: map[string]bool{}, politeLim: &Limiter{}, refresh: map[string]*refreshJob{}, torrents: map[string]*torrent{}, metaSessions: map[string]*metaSession{}}
	m.vault = newVault(dataDir)
	var notes []string
	var note string
	m.settings, note = loadState(filepath.Join(dataDir, "settings.json"), defaultSettings(), "settings")
	notes = append(notes, note)
	m.vault.openSettings(&m.settings)
	ensureSiteIDs(m.settings.Sites)
	m.hosts = newHostState()
	m.gate = newGate(dataDir)
	m.normalizeSettings()
	list, note := loadState[dlList](filepath.Join(dataDir, "downloads.json"), nil, "download list")
	notes = append(notes, note)
	m.alert = strings.TrimSpace(strings.Join(notes, " "))
	// Zipline didn't get to close properly last time (a crash, a power cut): the newest writes
	// of what was downloading may not have reached the disk.
	marker := filepath.Join(dataDir, "running")
	_, err := os.Stat(marker)
	unclean := err == nil
	for _, d := range list {
		if d == nil || d.ID == "" {
			continue
		}
		m.vault.openDownload(d)
		switch d.Status {
		case StDownloading, StConnecting:
			if unclean {
				d.rollBack(rollBackBy)
			}
			d.Status = StPaused
		}
		if d.Waiting { // waited for a free slot: after a restart it's started by hand again
			d.Waiting = false
			if d.Status == StQueued && d.Queue == "" {
				d.Status = StPaused
			}
		}
		var sum int64
		for _, s := range d.Segments {
			sum += s.Pos - s.Start
		}
		if d.Status == StCompleted {
			sum = d.Size
		}
		d.done = sum
		if d.ScanState == "scanning" || d.ScanState == "waiting" {
			d.ScanState, d.ScanNote = "", ""
		}
		if d.LegacyQ && d.Queue == "" {
			d.Queue = "main"
		}
		d.LegacyQ = false
		if m.queueByID(d.Queue) == nil {
			d.Queue = ""
		}
		m.list = append(m.list, d)
		m.byID[d.ID] = d
	}
	m.applyLimit()
	setProxySettings(m.settings.Proxy)
	m.grab = newGrabber(m)
	if n := m.vault.openNote(); n != "" {
		m.alert = strings.TrimSpace(m.alert + " " + n)
	}
	m.usage = newUsageMeter(filepath.Join(dataDir, "usage.json"))
	m.scanSem = make(chan struct{}, 1)
	os.WriteFile(marker, []byte(strconv.Itoa(os.Getpid())), privateFile) // removed on a clean exit (closeClean)
	startNetWatch()
	go m.loop()
	go m.meteredLoop()
	go m.politeLoop()
	m.startScheduler()
	go m.gateLoop()
	return m
}

// rollBackBy is how far back progress goes after an unclean exit: disks can lose their last
// writes even after a sync on some hardware (aria2 does the same).
const rollBackBy = 1 << 20

// rollBack moves every unfinished segment back by up to n bytes (never before its start), so
// those bytes are downloaded again.
func (d *Download) rollBack(n int64) {
	for _, s := range d.Segments {
		if s.Pos <= s.End {
			s.Pos = max(s.Pos-n, s.Start)
		}
	}
}

// closeClean marks a clean exit (after the final save): the next start trusts the saved progress.
func (m *Manager) closeClean() {
	os.Remove(filepath.Join(m.dataDir, "running"))
}

// dlList is the saved download list. Since format 2 it's {"format": 2, "downloads": [...]};
// older versions saved a bare array, which still loads.
type dlList []*Download

const listFormat = 2

func (l *dlList) UnmarshalJSON(b []byte) error {
	if b = bytes.TrimSpace(b); len(b) > 0 && b[0] == '[' {
		return json.Unmarshal(b, (*[]*Download)(l))
	}
	var v struct {
		Format    int         `json:"format"`
		Downloads []*Download `json:"downloads"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*l = v.Downloads
	return nil
}

func (m *Manager) normalizeSettings() {
	s := &m.settings
	if p := s.Phone.Port; p != 0 && (p < 1024 || p > 65535 || p == 17890) {
		s.Phone.Port = phonePort
	}
	if s.Phone.Port == 0 {
		s.Phone.Port = phonePort
	}
	if tp := &s.Torrent; true {
		if tp.Port != 0 && (tp.Port < 1024 || tp.Port > 65535 || tp.Port == 17890 || tp.Port == s.Phone.Port) {
			tp.Port = 0 // pick a free one
		}
		if tp.MaxPeers <= 0 {
			tp.MaxPeers = 60
		}
		tp.MaxPeers = min(max(tp.MaxPeers, 5), 100) // (and maxConns for all torrents together)
		tp.UpKB = max(tp.UpKB, 0)
		tp.SeedRatio = max(tp.SeedRatio, 0)
		tp.SeedMins = max(tp.SeedMins, 0)
	}
	if s.MaxConcurrent < 1 {
		s.MaxConcurrent = 1
	}
	if s.MaxConcurrent > 20 {
		s.MaxConcurrent = 20
	}
	if s.MaxActive <= 0 { // (lists from before the setting)
		s.MaxActive = defaultSettings().MaxActive
	}
	s.MaxActive = min(s.MaxActive, 50)
	if s.Conns < 1 {
		s.Conns = 1
	}
	if s.Conns > 32 {
		s.Conns = 32
	}
	if s.DownloadDir == "" {
		s.DownloadDir = defaultSettings().DownloadDir
	}
	rules := s.Sites[:0]
	for _, r := range s.Sites {
		if h, _, _ := splitSite(r.Site); h != "" {
			r.Site = strings.TrimSpace(r.Site)
			r.User = strings.TrimSpace(r.User)
			r.MaxConns = min(max(r.MaxConns, 0), 32)
			rules = append(rules, r)
		}
	}
	s.Sites = rules
	skip := s.SkipSites[:0]
	for _, x := range s.SkipSites {
		if x = strings.TrimSpace(x); x != "" {
			skip = append(skip, x)
		}
	}
	s.SkipSites = skip
	s.Browser.normalize()
	if _, ok := clockMinutes(s.Plan.Start); !ok {
		s.Plan.Start = "00:00"
	}
	if _, ok := clockMinutes(s.Plan.End); !ok {
		s.Plan.End = "07:00"
	}
	if s.Plan.Days == nil {
		s.Plan.Days = []int{0, 1, 2, 3, 4, 5, 6}
	}
	s.Plan.Margin = min(max(s.Plan.Margin, 0), 60)
	m.ensurePlanQueue()
	keyOK := map[string]bool{"alt": true, "ctrlalt": true, "ctrlshift": true, "shiftalt": true, "off": true}
	if !keyOK[s.Keys.Zipline] {
		s.Keys.Zipline = "alt"
	}
	if !keyOK[s.Keys.Browser] {
		s.Keys.Browser = "ctrlshift"
	}
	if s.Keys.Browser == s.Keys.Zipline && s.Keys.Browser != "off" {
		s.Keys.Browser = "off"
	}
	if len(s.Categories) == 0 {
		s.Categories = defaultSettings().Categories
	}
	if s.Sched.WhenDone == "" {
		s.Sched.WhenDone = "none"
	}
	if s.Proxy.Mode == "" {
		s.Proxy.Mode = "system"
	}
	if s.Cap.MB < 1 {
		s.Cap.MB = 500
	}
	if s.Cap.Hours < 1 {
		s.Cap.Hours = 1
	}
	if s.Cap.Hours > 24 {
		s.Cap.Hours = 24
	}
	// queues: "main" always exists and comes first
	if len(s.Queues) == 0 {
		s.Queues = []Queue{{ID: "main", Name: "Main queue", MaxConcurrent: s.MaxConcurrent, Sched: s.Sched}}
	}
	seen := map[string]bool{}
	var qs []Queue
	hasMain := false
	for _, q := range s.Queues {
		if q.ID == "" {
			q.ID = newID()
		}
		if seen[q.ID] {
			continue
		}
		seen[q.ID] = true
		q.Name = strings.TrimSpace(q.Name)
		if q.Name == "" {
			q.Name = "Queue"
		}
		if q.MaxConcurrent < 1 {
			q.MaxConcurrent = 1
		}
		if q.MaxConcurrent > 20 {
			q.MaxConcurrent = 20
		}
		if q.Sched.WhenDone == "" {
			q.Sched.WhenDone = "none"
		}
		if q.Sched.Start == "" {
			q.Sched.Start = "02:00"
		}
		if q.Sched.Stop == "" {
			q.Sched.Stop = "07:00"
		}
		if q.Sched.RepeatMins < 1 {
			q.Sched.RepeatMins = 120
		}
		if q.ID == "main" {
			hasMain = true
			qs = append([]Queue{q}, qs...)
		} else {
			qs = append(qs, q)
		}
	}
	if !hasMain {
		qs = append([]Queue{{ID: "main", Name: "Main queue", MaxConcurrent: 3, Sched: defaultSchedule()}}, qs...)
	}
	s.Queues = qs
	s.MaxConcurrent = qs[0].MaxConcurrent
	m.normalizeSched()
	normalizeGate(s)
}

// queueByID returns the queue settings (caller holds m.mu, or is in setup).
func (m *Manager) queueByID(id string) *Queue {
	if id == "" {
		return nil
	}
	for i := range m.settings.Queues {
		if m.settings.Queues[i].ID == id {
			return &m.settings.Queues[i]
		}
	}
	return nil
}

func (m *Manager) applyLimit() {
	m.capCfg.Store(m.settings.Cap)
	m.applyTraffic() // the overall limit (Speed Limiter and traffic profile), connections, quotas
	traySetEnabled(m.settings.Tray)
	m.applySites()
	mdial.configure(m.settings.Multi)
	m.applyPhone()
}

func (m *Manager) categoryFor(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	for _, c := range m.settings.Categories {
		for _, e := range c.Exts {
			if strings.EqualFold(e, ext) {
				return c.Name
			}
		}
	}
	return "General"
}

func (m *Manager) dirFor(cat string) string {
	if m.settings.CategoryFolders && cat != "General" {
		return filepath.Join(m.settings.DownloadDir, cat)
	}
	return m.settings.DownloadDir
}

// uniqueName avoids clashes with files on disk and other entries in the list. Caller holds m.mu.
func (m *Manager) uniqueName(dir, name string, self *Download) string {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	cand := name
	for i := 1; ; i++ {
		clash := false
		p := filepath.Join(dir, cand)
		if _, err := os.Stat(p); err == nil {
			clash = true
		} else if _, err := os.Stat(p + partSuffix); err == nil && !(self != nil && strings.EqualFold(self.Dir, dir) && strings.EqualFold(self.FileName, cand)) {
			clash = true // another download's unfinished part file
		}
		if !clash {
			for _, d := range m.list {
				if d != self && d.Status != StCompleted && strings.EqualFold(d.Dir, dir) && strings.EqualFold(d.FileName, cand) {
					clash = true
					break
				}
			}
		}
		if !clash {
			return cand
		}
		cand = base + " (" + itoa(i) + ")" + ext
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

type AddReq struct {
	URL      string            `json:"url"`
	Name     string            `json:"name"`
	Dir      string            `json:"dir"`
	Start    bool              `json:"start"`
	Queue    bool              `json:"queue"`   // legacy: true = main queue
	QueueID  string            `json:"queueId"` // queue to add to when not starting now
	Headers  map[string]string `json:"headers"`
	Desc     string            `json:"desc"`
	Variant  int               `json:"variant"`  // HLS quality (bandwidth), 0 = best
	Size     int64             `json:"size"`     // size the Add dialog already read (for estimates before it starts)
	Mirrors  []string          `json:"mirrors"`  // other addresses for the same file
	Checksum string            `json:"checksum"` // a checksum to check the file against (as pasted)
	Meta     bool              `json:"meta"`     // URL is a metalink
	MetaFile int               `json:"metaFile"` // which of its files (-1 = all)

	LiveFrom  string `json:"liveFrom"`  // a live event stream: "start" records it from its beginning, else from now
	StopAfter int    `json:"stopAfter"` // a live stream: stop recording after this many seconds (0 = when it ends)
	// a stream: the quality's details (Variant alone can't tell H.264 and HEVC apart) and its tracks
	VarRes    string  `json:"varRes"`
	VarCodecs string  `json:"varCodecs"`
	VarFPS    float64 `json:"varFps"`
	StreamPick
	Method  string `json:"method,omitempty"` // the browser's own request: "POST" (a form's download) is sent with Body
	Body    []byte `json:"body,omitempty"`
	Private bool   `json:"private,omitempty"` // from a private window: kept out of the saved list

	check  *Checksum
	pieces *PieceHashes
	note   string // shown with it in the list
}

// cleanMirrors keeps the valid, distinct addresses (at most 10), leaving out the main one.
func cleanMirrors(list []string, main string) []string {
	var out []string
	seen := map[string]bool{strings.TrimSpace(main): true}
	for _, u := range list {
		u = strings.TrimSpace(u)
		pu, err := url.Parse(u)
		if err != nil || pu.Host == "" || !(pu.Scheme == "http" || pu.Scheme == "https" || pu.Scheme == "ftp" || pu.Scheme == "ftps") || seen[u] {
			continue
		}
		seen[u] = true
		if out = append(out, u); len(out) == 10 {
			break
		}
	}
	return out
}

func (m *Manager) Add(r AddReq) *Download {
	m.mu.Lock()
	d := &Download{
		ID: newID(), URL: strings.TrimSpace(r.URL), Size: -1, Added: time.Now(),
		Conns: m.settings.Conns, Headers: r.Headers, Desc: r.Desc, Status: StPaused, Variant: r.Variant,
	}
	if r.Size > 0 {
		d.Size = r.Size
	}
	d.VarRes, d.VarCodecs, d.VarFPS, d.StreamPick = r.VarRes, r.VarCodecs, r.VarFPS, r.StreamPick
	if r.Name != "" {
		d.FileName = sanitizeName(r.Name)
		d.Category = m.categoryFor(d.FileName)
	}
	d.Mirrors = cleanMirrors(r.Mirrors, d.URL)
	d.Check, d.Pieces = r.check, r.pieces
	d.LiveFrom, d.StopAfter = r.LiveFrom, max(r.StopAfter, 0)
	if c, err := parseChecksum(r.Checksum, d.FileName); err == nil && c != nil {
		d.Check = c
	}
	if r.Method == "POST" {
		d.Method, d.Body = r.Method, r.Body
	}
	d.Private, d.Note = r.Private, r.note
	d.Dir = r.Dir
	if d.Dir == "" && d.Category != "" {
		d.Dir = m.dirFor(d.Category)
	}
	if d.FileName != "" && d.Dir != "" {
		d.FileName = m.uniqueName(d.Dir, d.FileName, d)
	}
	q := r.QueueID
	if q == "" && r.Queue {
		q = "main"
	}
	if q != "" && !r.Start && m.queueByID(q) != nil {
		d.Queue = q
		d.Status = StQueued
	}
	m.list = append(m.list, d)
	m.byID[d.ID] = d
	m.dirty = true
	if r.Start {
		m.startByHand(d)
	}
	m.mu.Unlock()
	m.kick()
	return d
}

// AddMetalink adds a metalink's files (one, or all with MetaFile -1), each with its mirrors
// and checksums, in the folders the metalink names. Files a torrent holds go to the torrent
// engine. err says why nothing could be added.
func (m *Manager) AddMetalink(r AddReq, files []mlFile) ([]*Download, error) {
	pick := files
	if r.MetaFile >= 0 && r.MetaFile < len(files) {
		pick = files[r.MetaFile : r.MetaFile+1]
	}
	out, rest, err := m.metalinkTorrents(r, pick) // (mltorrent.go)
	mlHost := hostOf(r.URL)
	for _, f := range rest {
		rr := r
		rr.Meta = false
		rr.URL = f.URLs[0].URL
		rr.Mirrors = nil
		for _, u := range f.URLs[1:] {
			rr.Mirrors = append(rr.Mirrors, u.URL)
		}
		if len(pick) > 1 || rr.Name == "" {
			rr.Name = f.path()
		}
		rr.Dir, rr.Name = m.metalinkPlace(rr.Dir, rr.Name)
		rr.Size = f.Size
		if len(pick) > 1 {
			rr.Checksum = ""
		}
		rr.check, rr.pieces = f.Hash, f.Pieces
		// the metalink site's sign-in stays with that site
		rr.Headers = map[string]string{}
		for k, v := range r.Headers {
			if (strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Authorization")) && !sameSite(mlHost, hostOf(rr.URL)) {
				continue
			}
			rr.Headers[k] = v
		}
		out = append(out, m.Add(rr))
	}
	if len(out) == 0 {
		if err == nil {
			err = errNotMetalink
		}
		return nil, err
	}
	return out, nil
}

func (m *Manager) startLocked(d *Download) {
	if d.running || d.Status == StCompleted || moveActive(d.ID) { // (a move starts it again when it's done)
		return
	}
	if d.Link != "refreshing" {
		d.Link = "" // started by hand: try the link as it is
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.running = true
	d.cancel = cancel
	d.noSeed = false // (a torrent started again shares when it finishes)
	d.Status = StConnecting
	d.Error = ""
	d.Waiting, d.Challenge, d.RetryAt = false, false, time.Time{}
	d.LastTry = time.Now()
	d.shareNote = nil
	m.dirty = true
	logf("starting %s", strings.TrimSpace(d.FileName+" ("+d.URL+")"))
	go m.run(ctx, cancel, d)
}

// startByHand starts a download the user asked for, unless as many as "downloads at once" allows
// already run: then it waits (shown as queued) and kick starts it when one ends. Caller holds m.mu.
func (m *Manager) startByHand(d *Download) {
	if d.running || d.Status == StCompleted {
		return
	}
	m.forgetRetry(d.ID) // a queue's tries start over
	if m.byHand() >= m.maxActive() {
		d.Waiting, d.Status = true, StQueued
		m.dirty = true
		return
	}
	m.startLocked(d)
}

// byHand counts the downloads running because someone started them: not those a running queue
// runs (queues have their own limit), nor an Add dialog's early start. Caller holds m.mu.
func (m *Manager) byHand() int {
	n := 0
	for _, d := range m.list {
		if d.running && !d.Provisional && !(d.Queue != "" && m.qRun[d.Queue]) && !m.slowTorrent(d) {
			n++
		}
	}
	return n
}

func (m *Manager) Start(ids []string) {
	var again []string
	m.mu.Lock()
	for _, id := range ids {
		if d := m.byID[id]; d != nil {
			if d.Status == StQueued {
				d.Status = StPaused
			}
			if m.metered {
				d.meteredOK = true // started by hand on a metered connection: leave it running
				delete(m.meterPaused, d.ID)
			}
			if d.Status == StCompleted && d.Torrent != nil {
				again = append(again, id) // a finished torrent shares again
				continue
			}
			m.startByHand(d)
		}
	}
	m.mu.Unlock()
	for _, id := range again {
		m.ShareAgain(id)
	}
}

func (m *Manager) Pause(ids []string) {
	m.mu.Lock()
	for _, id := range ids {
		if d := m.byID[id]; d != nil && d.Status != StCompleted {
			d.Status = StPaused
			d.Waiting = false
			if d.cancel != nil {
				d.cancel()
			}
		}
	}
	m.dirty = true
	m.mu.Unlock()
}

func (m *Manager) Remove(ids []string, withFile bool) {
	m.mu.Lock()
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
		if d := m.byID[id]; d != nil {
			d.Waiting = false
			if d.cancel != nil {
				d.cancel()
			}
			if t := m.torrents[id]; t != nil {
				go t.stop("stopped")
			}
			if d.Kind == "torrent" {
				go m.removeTorrentFiles(d, withFile)
				delete(m.byID, id)
				continue
			}
			go func(d *Download) {
				// wait for the worker to let go of the file
				for i := 0; i < 50; i++ {
					m.mu.Lock()
					r := d.running
					m.mu.Unlock()
					if !r {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				d.removeParts()
				if withFile && d.Status == StCompleted {
					os.Remove(d.path())
				}
			}(d)
			delete(m.byID, id)
		}
	}
	out := m.list[:0]
	for _, d := range m.list {
		if !set[d.ID] {
			out = append(out, d)
		}
	}
	m.list = out
	m.dirty = true
	m.mu.Unlock()
}

func (m *Manager) Redownload(ids []string) {
	m.mu.Lock()
	var check []*torrent
	for _, id := range ids {
		d := m.byID[id]
		if d != nil && d.Torrent != nil { // a torrent checks its files instead: the good pieces stay (trecheck.go)
			if t := m.checkTorrentLocked(d); t != nil {
				check = append(check, t)
			}
			continue
		}
		if d == nil || d.running {
			continue
		}
		d.removeParts()
		d.Segments = nil
		d.done = 0
		d.Replace = false
		d.Parts, d.PartsDone, d.PartBytes = 0, 0, 0
		d.AParts, d.ADone, d.ABytes, d.Packaged = 0, 0, 0, false
		d.resetStream()
		d.Extra, d.Marks = nil, nil
		d.Status = StPaused
		d.Error = ""
		d.Link, d.linkTries, d.repairs, d.distrust = "", 0, 0, nil
		if c := d.Check; c != nil {
			if c.State == "bad" {
				os.Remove(d.path()) // the damaged copy makes way for the new one
			}
			if c.Want == "" {
				d.Check = nil
			} else {
				c.State, c.Got = "", ""
			}
		}
		if _, err := os.Stat(d.path()); err == nil {
			d.FileName = m.uniqueName(d.Dir, d.FileName, d)
		}
		m.startByHand(d)
	}
	m.mu.Unlock()
	for _, t := range check {
		go t.checkAll()
	}
}

func (m *Manager) SetQueue(ids []string, qid string) {
	m.mu.Lock()
	if qid != "" && m.queueByID(qid) == nil {
		m.mu.Unlock()
		return
	}
	for _, id := range ids {
		d := m.byID[id]
		if d == nil {
			continue
		}
		q := m.queueByID(qid)
		if d.Status == StCompleted && (q == nil || !q.Sync) && qid != "" {
			continue // finished files only belong in synchronization queues
		}
		d.Queue = qid
		d.Waiting = false
		if qid != "" && !d.running && d.Status != StError && d.Status != StCompleted {
			d.Status = StQueued
		} else if qid == "" && d.Status == StQueued {
			d.Status = StPaused
		}
	}
	m.dirty = true
	m.mu.Unlock()
	m.kick()
}

func (m *Manager) Move(ids []string, delta int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	if delta < 0 {
		for i := 1; i < len(m.list); i++ {
			if set[m.list[i].ID] && !set[m.list[i-1].ID] {
				m.list[i], m.list[i-1] = m.list[i-1], m.list[i]
			}
		}
	} else {
		for i := len(m.list) - 2; i >= 0; i-- {
			if set[m.list[i].ID] && !set[m.list[i+1].ID] {
				m.list[i], m.list[i+1] = m.list[i+1], m.list[i]
			}
		}
	}
	m.dirty = true
}

func (m *Manager) StartQueue(qid string, bySched bool) {
	m.mu.Lock()
	q := m.queueByID(qid)
	if q == nil {
		m.mu.Unlock()
		return
	}
	if m.qRun[qid] {
		m.mu.Unlock()
		return
	}
	m.qRun[qid] = true
	m.qBySched[qid] = bySched
	m.qLastStart[qid] = time.Now()
	for _, d := range m.list {
		if d.Queue == qid && !d.running && (d.Status == StPaused || d.Status == StError) {
			d.Status = StQueued
		}
	}
	sync := q.Sync
	if sync {
		m.qSyncing[qid] = true
	}
	m.mu.Unlock()
	if sync {
		go m.syncQueue(qid)
		return
	}
	m.kick()
}

func (m *Manager) StopQueue(qid string) {
	m.mu.Lock()
	m.qRun[qid] = false
	for _, d := range m.list {
		if d.Queue == qid && d.running && d.cancel != nil {
			d.Status = StQueued
			d.cancel()
		}
	}
	m.mu.Unlock()
}

// kick starts queued downloads while each running queue has free slots.
// shutdownHook turns the computer off ("when finished"); tests replace it.
var shutdownHook = shutdownComputer

func (m *Manager) kick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	defer m.startWaiting()
	for qi := range m.settings.Queues {
		q := &m.settings.Queues[qi]
		if !m.qRun[q.ID] {
			continue
		}
		running, pending, failed, slow := 0, 0, 0, 0
		for _, d := range m.list {
			if d.Queue != q.ID {
				continue
			}
			switch {
			case d.running:
				running++
				if m.slowTorrent(d) {
					slow++ // a stalled torrent keeps trying but doesn't hold a place
				}
			case m.meterPaused[d.ID]:
				pending++ // paused for a metered connection: carries on later, so the queue isn't done
			case m.retryWaiting(d):
				pending++ // failed, and the queue tries it again later
			case d.Status == StError:
				failed++
			}
		}
		for _, d := range m.list {
			if d.Queue == q.ID && d.Status == StQueued && !d.running {
				if running-slow < q.MaxConcurrent && m.queueMayStart(q.ID) { // queues wait out a metered connection, a used-up quota, the free hours
					m.startLocked(d)
					running++
				} else {
					pending++
				}
			}
		}
		if pending == 0 && running == 0 && !m.qSyncing[q.ID] {
			m.qRun[q.ID] = false
			if failed > 0 {
				m.notify("queueDone", q.ID, fmt.Sprintf("%s finished, but %s couldn't be downloaded", iso(q.Name), plural(failed, "file")))
			} else {
				m.notify("queueDone", q.ID, iso(q.Name)+" finished")
			}
			if n := m.qDone[q.ID]; n > 0 || failed > 0 { // (its downloads had no balloon each; a check that found nothing new needs none)
				trayNotify(queueBalloon(q.Name, n, failed))
			}
			delete(m.qDone, q.ID)
			m.playEvent("queue")
			// the "when finished" action runs only when everything downloaded: a failure (a network
			// outage at night, say) must not turn the computer off with work left undone
			if m.qBySched[q.ID] && failed == 0 {
				m.requestPower(q.Sched.WhenDone, iso(q.Name)+" finished")
			}
		}
	}
}

// startWaiting starts downloads started by hand that wait for a free slot, in list order, while
// "downloads at once" allows. Caller holds m.mu.
func (m *Manager) startWaiting() {
	n := m.byHand()
	for _, d := range m.list {
		if n >= m.maxActive() {
			return
		}
		if d.Waiting && !d.running {
			m.startLocked(d)
			n++
		}
	}
}

// syncQueue re-checks finished files in a synchronization queue and re-downloads
// the ones that changed on the server (size, ETag or Last-Modified) or went missing.
func (m *Manager) syncQueue(qid string) {
	m.mu.Lock()
	var check []*Download
	for _, d := range m.list {
		if d.Queue == qid && d.Status == StCompleted && !d.running {
			check = append(check, d)
		}
	}
	m.mu.Unlock()

	var updated, failed int32
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, d := range check {
		wg.Add(1)
		sem <- struct{}{}
		go func(d *Download) {
			defer wg.Done()
			defer func() { <-sem }()
			m.netGateWait(context.Background()) // not on a metered connection or past the data cap
			changed, note := m.remoteChanged(d)
			m.mu.Lock()
			d.Checked = time.Now()
			switch {
			case note != "":
				d.SyncNote = note
				atomic.AddInt32(&failed, 1)
			case changed:
				d.SyncNote = "Changed on server — updating"
				d.Status = StQueued
				d.Segments = nil
				atomic.StoreInt64(&d.done, 0)
				d.Replace = true
				atomic.AddInt32(&updated, 1)
			default:
				d.SyncNote = "Up to date"
			}
			m.dirty = true
			m.mu.Unlock()
		}(d)
	}
	wg.Wait()
	m.mu.Lock()
	m.qSyncing[qid] = false
	name := qid
	if q := m.queueByID(qid); q != nil {
		name = q.Name
	}
	text := fmt.Sprintf("%s: checked %s, %d changed", iso(name), plural(len(check), "file"), updated)
	if failed > 0 {
		text += fmt.Sprintf(", %d couldn't be checked", failed)
	}
	m.notify("syncDone", qid, text)
	m.mu.Unlock()
	m.kick()
}

func (m *Manager) remoteChanged(d *Download) (bool, string) {
	if _, err := os.Stat(d.path()); err != nil {
		return true, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	switch {
	case d.Torrent != nil:
		return false, "" // a torrent's content is fixed by its hash: nothing to compare
	case d.Kind == "hls" || d.Kind == "dash":
		// a stream's file is built from its playlist, so the playlist is what can change
		body, _, err := m.fetchText(ctx, d, d.URL)
		if err != nil {
			return false, "Check failed: " + err.Error()
		}
		sig := playlistSig(body)
		m.mu.Lock()
		prev := d.SyncSig
		d.SyncSig = sig
		m.mu.Unlock()
		return prev != "" && prev != sig, ""
	}
	pr, err := probe(ctx, d, d.URL, 0, -1)
	if err != nil {
		return false, "Check failed: " + err.Error()
	}
	pr.body.Close()
	if pr.size > 0 && d.Size > 0 && pr.size != d.Size {
		return true, ""
	}
	if pr.etag != "" && d.ETag != "" {
		return pr.etag != d.ETag, ""
	}
	if pr.lastMod != "" && d.LastMod != "" {
		return pr.lastMod != d.LastMod, ""
	}
	return false, ""
}

// playlistSig fingerprints a stream's playlist, ignoring the query strings (sign-in tokens that
// change on every request) so only a real change of content counts.
func playlistSig(body string) string {
	h := sha256.New()
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") && !strings.HasPrefix(line, "<") {
			line, _, _ = strings.Cut(line, "?")
		}
		line = queryAttr.ReplaceAllString(line, "$1")
		h.Write([]byte(line + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// queryAttr drops the query string inside URI="…" attributes and MPD media="…" templates.
var queryAttr = regexp.MustCompile(`((?:URI|media|initialization|sourceURL)="[^"?]*)\?[^"]*`)

func (m *Manager) notify(kind, id string, text ...string) {
	m.seq++
	n := Notice{Seq: m.seq, Kind: kind, ID: id}
	if len(text) > 0 {
		n.Text = text[0]
	}
	m.notices = append(m.notices, n)
	if len(m.notices) > 50 {
		m.notices = m.notices[len(m.notices)-50:]
	}
}

func (m *Manager) AddPrompt(p Prompt) string {
	m.mu.Lock()
	p.ID = newID()
	m.prompts = append(m.prompts, p)
	if len(m.prompts) > 20 {
		m.prompts = m.prompts[1:]
	}
	m.mu.Unlock()
	return p.ID
}

func (m *Manager) DismissPrompt(id string) {
	m.mu.Lock()
	out := m.prompts[:0]
	for _, p := range m.prompts {
		if p.ID != id {
			out = append(out, p)
		}
	}
	m.prompts = out
	m.mu.Unlock()
}

// loop: speed sampling, persistence, scheduler.
func (m *Manager) loop() {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	lastSave := time.Now()
	planEvery := 0
	for range tick.C {
		m.dropStaleEarly()
		if planEvery++; planEvery%4 == 0 { // every 2 s
			m.planTick(time.Now())
			m.refreshTick(time.Now())
		}
		m.mu.Lock()
		for _, d := range m.list {
			cur := atomic.LoadInt64(&d.done)
			if d.running {
				inst := float64(cur-d.lastDone) * 2
				if inst < 0 {
					inst = 0
				}
				if d.speed == 0 {
					d.speed = inst
				} else {
					d.speed = d.speed*0.6 + inst*0.4
				}
			} else {
				d.speed = 0
			}
			d.lastDone = cur
		}
		dirty := m.dirty
		save := dirty || time.Since(lastSave) > 3*time.Second
		var running bool
		for _, d := range m.list {
			running = running || d.running
		}
		m.mu.Unlock()
		// in the background: syncing part files to disk can take a while, and the speeds
		// shouldn't stall meanwhile
		if save && (dirty || running) && m.saving.CompareAndSwap(false, true) {
			go func() {
				defer m.saving.Store(false)
				m.save()
			}()
			lastSave = time.Now()
		}
	}
}

// save writes the download list and settings. Each download is captured under its own lock (one
// at a time, so connections aren't held up), then the part files of running downloads are synced
// to disk before the list is written: the saved progress never claims more than the disk has.
func (m *Manager) save() {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	m.dirty = false
	keep := make([]*Download, 0, len(m.list))
	for _, d := range m.list {
		if !d.Provisional && !d.Private { // still waiting in the Add dialog: not part of the list yet (a private one never is)
			keep = append(keep, d)
		}
	}
	st := m.settings
	st.Resume = m.resumeState(time.Now())
	m.vault.sealSettings(&st)
	sb, _ := json.MarshalIndent(st, "", " ")
	m.mu.Unlock()

	list := make([]json.RawMessage, 0, len(keep))
	var files []*os.File
	for _, d := range keep {
		m.mu.Lock() // (most of a download's fields are guarded by m.mu, its segments by d.mu)
		m.noteResume(d)
		d.mu.Lock()
		b, err := json.Marshal(m.vault.wire(d))
		if d.file != nil {
			files = append(files, d.file)
		}
		d.mu.Unlock()
		m.mu.Unlock()
		if err != nil {
			return
		}
		list = append(list, b)
	}
	for _, f := range files {
		f.Sync() // (a download that just finished closed it: nothing to do then)
	}
	b, err := json.MarshalIndent(struct {
		Format    int               `json:"format"`
		Downloads []json.RawMessage `json:"downloads"`
	}{listFormat, list}, "", " ")
	if err != nil {
		return
	}
	writeAtomic(filepath.Join(m.dataDir, "downloads.json"), b)
	writeAtomicPerm(filepath.Join(m.dataDir, "settings.json"), sb, privateFile) // holds saved logins
	m.usage.save(filepath.Join(m.dataDir, "usage.json"))
	m.gate.save()
	m.grab.flush() // (site scans, saved so one can be resumed)
	m.noteVaultSave()
}

func writeAtomic(p string, b []byte) { writeAtomicPerm(p, b, privateFile) }

// writeAtomicPerm replaces a file through a temporary copy, keeping the previous version as
// p.bak, so a crash or a damaged write never costs more than the last change. The copy reaches
// the disk before it takes the file's place.
func writeAtomicPerm(p string, b []byte, perm os.FileMode) {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return
	}
	os.Chmod(tmp, perm) // an older file keeps its mode
	if _, err := os.Stat(p); err == nil {
		os.Rename(p, p+".bak")
	}
	os.Rename(tmp, p)
	syncDir(filepath.Dir(p))
}

// syncDir makes renames in dir durable (Unix; Windows can't open a folder for that).
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if f, err := os.Open(dir); err == nil {
		f.Sync()
		f.Close()
	}
}

// loadState reads a saved JSON file. If it's missing or damaged it falls back to the backup
// (p.bak) and then to an unfinished save (p.tmp). A damaged file is moved aside, never written
// over, and the note says what happened.
func loadState[T any](p string, def T, what string) (T, string) {
	try := func(f string) (T, bool) {
		v := def
		b, err := os.ReadFile(f)
		return v, err == nil && json.Unmarshal(b, &v) == nil
	}
	if v, ok := try(p); ok {
		return v, ""
	}
	damaged := ""
	if _, err := os.Stat(p); err == nil {
		damaged = p + ".damaged-" + time.Now().Format("20060102-150405")
		os.Rename(p, damaged)
	}
	for _, alt := range []string{p + ".bak", p + ".tmp"} {
		if v, ok := try(alt); ok {
			if damaged == "" {
				return v, "" // a save was interrupted: the previous copy is fine
			}
			when := ""
			if st, err := os.Stat(alt); err == nil {
				when = " from " + st.ModTime().Format("Jan 2, 15:04")
			}
			return v, fmt.Sprintf("Zipline's %s file was damaged, so the backup%s was loaded. The damaged copy was kept as %s.", what, when, filepath.Base(damaged))
		}
	}
	if damaged != "" {
		return def, fmt.Sprintf("Zipline's %s file was damaged and there was no backup, so it starts empty. The damaged copy was kept as %s.", what, filepath.Base(damaged))
	}
	return def, ""
}

// ---------------------------------------------------------------- snapshot

type DLView struct {
	ID        string       `json:"id"`
	URL       string       `json:"url"`
	Name      string       `json:"name"`
	Dir       string       `json:"dir"`
	Category  string       `json:"category"`
	Size      int64        `json:"size"`
	Done      int64        `json:"done"`
	Speed     float64      `json:"speed"`
	Status    string       `json:"status"`
	Error     string       `json:"error,omitempty"`
	Resumable bool         `json:"resumable"`
	Added     time.Time    `json:"added"`
	Finished  time.Time    `json:"finished"`
	LastTry   time.Time    `json:"lastTry"`
	InQueue   bool         `json:"inQueue"`
	Queue     string       `json:"queue"`
	LimitKB   int          `json:"limitKB"`
	Scan      string       `json:"scan,omitempty"`
	ScanState string       `json:"scanState,omitempty"`
	ScanNote  string       `json:"scanNote,omitempty"`
	SyncNote  string       `json:"syncNote,omitempty"`
	Metered   bool         `json:"metered,omitempty"` // paused because of a metered connection
	Checked   time.Time    `json:"checked"`
	Kind      string       `json:"kind,omitempty"`
	Parts     int          `json:"parts,omitempty"`
	PartsDone int          `json:"partsDone,omitempty"`
	Est       int64        `json:"est,omitempty"` // HLS: estimated final size
	AParts    int          `json:"aParts,omitempty"`
	ADone     int          `json:"aDone,omitempty"`
	Prog      float64      `json:"prog,omitempty"` // HLS: 0…1 across video and audio
	Note      string       `json:"note,omitempty"`
	Stage     string       `json:"stage,omitempty"`
	StagePct  int          `json:"stagePct,omitempty"`
	Workers   int32        `json:"workers"`
	Conns     int          `json:"conns"`
	Desc      string       `json:"desc,omitempty"`
	Segs      [][4]any     `json:"segs,omitempty"`
	Link      string       `json:"link,omitempty"`    // the link needs renewing: "expired", "refreshing", "waiting"
	Page      string       `json:"page,omitempty"`    // the web page it came from
	Expires   int64        `json:"expires,omitempty"` // the signed link runs out then (Unix seconds)
	Mirrors   []string     `json:"mirrors,omitempty"`
	Check     *Checksum    `json:"check,omitempty"`
	Pieces    int          `json:"pieces,omitempty"`  // piece checksums from a metalink
	Sources   []SourceView `json:"sources,omitempty"` // addresses in use (progress window)
	Torrent   *TorrentView `json:"torrent,omitempty"`
	Waiting   bool         `json:"waiting,omitempty"`   // started by hand, waits for a free slot ("downloads at once")
	Challenge bool         `json:"challenge,omitempty"` // stopped by bot protection: offer "Open in browser"
	Retrying  bool         `json:"retrying,omitempty"`  // every connection lost its server and is trying again
	RetryAt   int64        `json:"retryAt,omitempty"`   // every connection was lost: tries again then (Unix ms)
	TryAgain  int64        `json:"tryAgain,omitempty"`  // failed: its queue tries it again then (Unix ms)
	Move      *MoveView    `json:"move,omitempty"`      // being moved to another folder

	Live bool    `json:"live,omitempty"` // a live stream being recorded
	Rec  float64 `json:"rec,omitempty"`  // seconds recorded so far

	// a metalink's piece checks while it downloads: pieces that matched, damaged ones fetched again
	PiecesOK  int `json:"piecesOk,omitempty"`
	PiecesBad int `json:"piecesBad,omitempty"`

	Private bool `json:"private,omitempty"` // from a private window: leaves the list once finished
}

type QView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Syncing bool   `json:"syncing"`
	Sync    bool   `json:"sync"`
	Max     int    `json:"max"`
}

type Snapshot struct {
	Downloads    []DLView    `json:"downloads"`
	Queues       []QView     `json:"queues"`
	QueueRunning bool        `json:"queueRunning"`
	TotalSpeed   float64     `json:"totalSpeed"`
	Prompts      []Prompt    `json:"prompts"`
	Early        []DLView    `json:"early,omitempty"` // downloads started while their Add dialog is open
	Plan         PlanView    `json:"plan"`
	Polite       PoliteView  `json:"polite"`
	Net          []NetIfView `json:"net,omitempty"` // adapters in use (several connections at once)
	Notices      []Notice    `json:"notices"`
	SpeedLimitOn bool        `json:"speedLimitOn"`
	SpeedLimitKB int         `json:"speedLimitKB"`
	Cap          CapView     `json:"cap"`
	Profile      ProfileView `json:"profile"` // the traffic profile in use
	Quota        QuotasView  `json:"quota"`
	OS           string      `json:"os"`
	Alert        string      `json:"alert,omitempty"`
	Power        PowerView   `json:"power"`
}

func (m *Manager) Snapshot(withSegs map[string]bool) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Prompts: append([]Prompt{}, m.prompts...), Notices: append([]Notice{}, m.notices...),
		SpeedLimitOn: m.settings.SpeedLimitOn, SpeedLimitKB: m.settings.SpeedLimitKB, Cap: m.capView(), OS: runtime.GOOS, Alert: m.alert,
		Plan: m.planView(time.Now()), Polite: PoliteView{On: m.settings.Polite, Rate: m.politeRate, QueueMs: m.politeQueue.Milliseconds()}}
	if m.settings.Multi.On {
		s.Net = netStats(m.settings.Multi)
	}
	s.Power = m.powerView()
	m.trafficView(&s)
	for _, q := range m.settings.Queues {
		s.Queues = append(s.Queues, QView{ID: q.ID, Name: q.Name, Running: m.qRun[q.ID], Syncing: m.qSyncing[q.ID], Sync: q.Sync, Max: q.MaxConcurrent})
		s.QueueRunning = s.QueueRunning || m.qRun[q.ID]
	}
	s.Downloads = make([]DLView, 0, len(m.list))
	for _, d := range m.list {
		v := DLView{ID: d.ID, URL: stripURLPass(d.URL), Name: d.FileName, Dir: d.Dir, Category: d.Category, Size: d.Size,
			Done: atomic.LoadInt64(&d.done), Speed: d.speed, Status: d.Status, Error: d.Error, Resumable: d.Resumable,
			Added: d.Added, Finished: d.Finished, LastTry: d.LastTry, InQueue: d.Queue != "", Queue: d.Queue, LimitKB: d.LimitKB,
			SyncNote: d.SyncNote, Metered: m.meterPaused[d.ID], Checked: d.Checked, Scan: d.Scan, ScanState: d.ScanState, ScanNote: d.ScanNote, Workers: atomic.LoadInt32(&d.workers), Conns: d.Conns, Desc: d.Desc,
			Kind: d.Kind, Parts: d.Parts, PartsDone: d.PartsDone, Note: d.Note, Stage: d.stage, StagePct: d.stagePct,
			Link: d.Link, Page: pageOf(d), Mirrors: stripURLPasses(d.Mirrors), Waiting: d.Waiting, Challenge: d.Challenge}
		if !d.RetryAt.IsZero() {
			v.RetryAt = d.RetryAt.UnixMilli()
		}
		v.TryAgain = m.retryAt(d)
		v.Private = d.Private
		v.Move = moveView(d.ID)
		if d.running && d.Status == StDownloading && d.Kind == "" {
			v.Retrying = d.reconnecting()
		}
		if d.Check != nil {
			c := *d.Check
			v.Check = &c
		}
		if d.Pieces != nil {
			v.Pieces = len(d.Pieces.Hashes)
			v.PiecesOK, v.PiecesBad = d.pieceCounts()
		}
		if d.Status != StCompleted {
			for _, u := range []string{d.FinalURL, d.URL} {
				if t := cachedExpiry(u); !t.IsZero() {
					v.Expires = t.Unix()
					break
				}
			}
		}
		if d.Torrent != nil {
			v.Torrent = m.torrentView(d, withSegs[d.ID])
		}
		if d.running && d.srcs != nil && (withSegs[d.ID] || withSegs["*"]) && (d.srcs.count() > 1 || d.Pieces != nil) {
			v.Sources = d.srcs.view()
		}
		if d.Kind == "hls" && d.Status != StCompleted && d.Live {
			v.Live, v.Rec = true, d.WantSecs
		} else if d.Kind == "hls" && d.Status != StCompleted && d.Parts > 0 {
			v.AParts, v.ADone = d.AParts, d.ADone
			for _, x := range d.Extra { // more audio languages: their pieces count too
				v.AParts, v.ADone = v.AParts+x.Parts, v.ADone+x.Done
			}
			v.Prog, v.Est = hlsProgress(d)
		}
		if v.Category == "" {
			v.Category = "General"
		}
		if d.stage != "" { // converting: nothing is being transferred
			v.Speed = 0
		}
		if withSegs[d.ID] || withSegs["*"] {
			d.mu.Lock()
			segs := append([]*Segment{}, d.Segments...)
			sort.Slice(segs, func(i, j int) bool { return segs[i].Start < segs[j].Start })
			for _, sg := range segs {
				st := sg.state
				if sg.Pos > sg.End {
					st = "done"
				} else if !sg.active && (st == "" || !d.running && st != "failed") {
					st = "idle" // (a paused download's pieces aren't receiving, whatever they last did)
				}
				v.Segs = append(v.Segs, [4]any{sg.Start, sg.End, sg.Pos, st})
			}
			d.mu.Unlock()
		}
		s.TotalSpeed += v.Speed
		if d.Provisional {
			s.Early = append(s.Early, v)
			continue
		}
		s.Downloads = append(s.Downloads, v)
	}
	return s
}

// hlsProgress estimates how far a stream download is (0…1) and its final size. With a separate
// audio track the two parts are weighted by their (estimated) sizes.
func hlsProgress(d *Download) (float64, int64) {
	vFrac := float64(d.PartsDone) / float64(d.Parts)
	var vEst, aEst int64
	if d.PartsDone > 0 {
		vEst = d.PartBytes * int64(d.Parts) / int64(d.PartsDone)
	}
	aParts, aDone, aBytes := d.AParts, d.ADone, d.ABytes
	for _, x := range d.Extra { // more audio languages count as audio
		aParts, aDone, aBytes = aParts+x.Parts, aDone+x.Done, aBytes+x.Bytes
	}
	if aParts == 0 {
		return vFrac, vEst
	}
	aFrac := float64(aDone) / float64(aParts)
	if aDone > 0 {
		aEst = aBytes * int64(aParts) / int64(aDone)
	}
	wA := 0.08 // typical share of audio until both sizes are known
	switch {
	case vEst > 0 && aEst > 0:
		wA = float64(aEst) / float64(aEst+vEst)
	case aEst > 0:
		vEst = int64(float64(aEst) * (1 - wA) / wA)
	}
	return (1-wA)*vFrac + wA*aFrac, vEst + aEst
}
