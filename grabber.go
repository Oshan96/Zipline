package main

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"math/rand"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Site grabber: crawls a site (or an FTP folder) to a chosen depth and collects links
// to files of the chosen types. Results are picked in the UI and added as downloads.
// Saved projects can re-run on a timer and queue only the files that are new.

type GrabOptions struct {
	URL       string            `json:"url"`
	Depth     int               `json:"depth"` // 0 = this page only
	Scope     string            `json:"scope"` // site | domain | folder
	Types     []string          `json:"types"` // category names
	Exts      string            `json:"exts"`  // extra extensions, space separated
	CrossSite bool              `json:"crossSite"`
	Robots    bool              `json:"robots"`
	MaxPages  int               `json:"maxPages"`
	Wait      int               `json:"wait,omitempty"`    // least milliseconds between requests to one server
	Sitemap   bool              `json:"sitemap,omitempty"` // also read the site's sitemaps
	Session   string            `json:"session,omitempty"` // a sign-in sent by the browser (see NewSession)
	Headers   map[string]string `json:"headers,omitempty"`
	Sites     []string          `json:"-"` // where the start page redirected (its login and cookies go there too)
}

type GrabFile struct {
	URL      string `json:"url"`
	Name     string `json:"name"`
	Ext      string `json:"ext"`
	Category string `json:"category"`
	From     string `json:"from"`
	Text     string `json:"text,omitempty"` // link text (page links)
}

type GrabJob struct {
	ID       string            `json:"id"`
	Opts     GrabOptions       `json:"opts"`
	State    string            `json:"state"` // running | done | stopped | error
	Error    string            `json:"error,omitempty"`
	Pages    int               `json:"pages"`
	Queued   int               `json:"queued"`
	Skipped  int               `json:"skipped"` // blocked by robots.txt / scope / limit
	Files    []GrabFile        `json:"files"`
	Started  time.Time         `json:"started"`
	Finished time.Time         `json:"finished"`
	Project  string            `json:"project,omitempty"`
	Title    string            `json:"title,omitempty"`
	FromPage bool              `json:"fromPage,omitempty"` // "download all links" from the browser
	Cookies  map[string]string `json:"-"`                  // host → Cookie header (page links)
	Failed   int               `json:"failed"`             // pages that could not be read
	Failures []GrabFailure     `json:"-"`
	SignedIn []string          `json:"-"` // sites the scan uses the browser's sign-in for

	extCtx  []byte // links from the browser: the window they came from, as the extension described it (its cookies are asked for there)
	private bool   // ...a private window: the files are private downloads

	mu     sync.Mutex
	cancel context.CancelFunc
	seen   map[string]bool
	scan   scanState
}

type GrabProject struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Opts     GrabOptions `json:"opts"`
	QueueID  string      `json:"queueId"`
	Dir      string      `json:"dir"`
	Keep     bool        `json:"keep"`     // keep the site's folder structure
	AutoMins int         `json:"autoMins"` // re-run every N minutes and queue new files (0 = off)
	LastRun  time.Time   `json:"lastRun"`
	Known    []string    `json:"known,omitempty"`
}

type Grabber struct {
	m        *Manager
	mu       sync.Mutex
	jobs     map[string]*GrabJob
	projects []*GrabProject
	file     string

	pace     *hostPace               // politeness, shared by all scans
	robots   *robotsStore            // robots.txt of the sites crawled lately
	sessions map[string]*grabSession // browser sign-ins waiting for their scan
	scanDir  string                  // where scans are saved
}

func newGrabber(m *Manager) *Grabber {
	g := &Grabber{m: m, jobs: map[string]*GrabJob{}, file: filepath.Join(m.dataDir, "grabber.json")}
	if b, err := os.ReadFile(g.file); err == nil {
		json.Unmarshal(b, &g.projects)
		for _, p := range g.projects {
			m.vault.revealHeaders(p.Opts.Headers)
		}
	}
	g.initScans()
	return g
}

func (g *Grabber) saveProjects() {
	g.mu.Lock()
	b, _ := json.MarshalIndent(g.sealedProjects(), "", " ")
	g.mu.Unlock()
	writeAtomic(g.file, b)
}

// ---------------------------------------------------------------- options

func (g *Grabber) wantedExts(o GrabOptions) map[string]bool {
	want := map[string]bool{}
	g.m.mu.Lock()
	for _, c := range g.m.settings.Categories {
		for _, t := range o.Types {
			if strings.EqualFold(t, c.Name) {
				for _, e := range c.Exts {
					want[strings.ToLower(e)] = true
				}
			}
		}
	}
	g.m.mu.Unlock()
	for _, e := range strings.FieldsFunc(strings.ToLower(o.Exts), func(r rune) bool { return r == ' ' || r == ',' || r == ';' || r == '.' }) {
		want[e] = true
	}
	return want
}

func normalizeOpts(o GrabOptions) (GrabOptions, error) {
	o.URL = strings.TrimSpace(o.URL)
	if !strings.Contains(o.URL, "://") {
		o.URL = "https://" + o.URL
	}
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ftp" && u.Scheme != "ftps") {
		return o, errors.New("enter an http, https or ftp link")
	}
	if o.Depth < 0 {
		o.Depth = 0
	}
	if o.Depth > 8 {
		o.Depth = 8
	}
	if o.MaxPages <= 0 {
		o.MaxPages = 300
	}
	if o.MaxPages > 5000 {
		o.MaxPages = 5000
	}
	if o.Scope == "" {
		o.Scope = "site"
	}
	switch {
	case o.Wait <= 0:
		o.Wait = int(grabWaitDefault / time.Millisecond)
	case time.Duration(o.Wait)*time.Millisecond < grabWaitFloor:
		o.Wait = int(grabWaitFloor / time.Millisecond)
	case o.Wait > 60000:
		o.Wait = 60000
	}
	return o, nil
}

// regDomain is a simple "registrable domain": the last two labels (three for x.co.uk style).
func regDomain(h string) string {
	parts := strings.Split(strings.ToLower(h), ".")
	if len(parts) <= 2 {
		return strings.Join(parts, ".")
	}
	if len(parts[len(parts)-2]) <= 3 && len(parts[len(parts)-1]) == 2 {
		return strings.Join(parts[len(parts)-3:], ".")
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// grabScope decides which links belong to "the site". It starts from the address you typed and
// also takes in where that address redirected to (example.com → www.example.com), and treats a
// host and its www. form as the same site.
type grabScope struct {
	mu      sync.Mutex
	mode    string // "site" (default), "domain" or "folder"
	hosts   map[string]bool
	domains map[string]bool
	dirs    map[string][]string // folder scope: host → allowed path prefixes
}

func hostKey(u *url.URL) string {
	return strings.TrimPrefix(strings.ToLower(u.Host), "www.")
}

func newGrabScope(start *url.URL, mode string) *grabScope {
	s := &grabScope{mode: mode, hosts: map[string]bool{}, domains: map[string]bool{}, dirs: map[string][]string{}}
	s.add(start)
	return s
}

// add takes in another address for the site (the start page's redirect target).
func (s *grabScope) add(u *url.URL) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := hostKey(u)
	s.hosts[k] = true
	s.domains[regDomain(u.Hostname())] = true
	dir := u.Path
	if !strings.HasSuffix(dir, "/") {
		dir = path.Dir(dir) + "/"
	}
	if !slices.Contains(s.dirs[k], dir) {
		s.dirs[k] = append(s.dirs[k], dir)
	}
}

func (s *grabScope) in(u *url.URL) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.mode {
	case "domain":
		return s.domains[regDomain(u.Hostname())]
	case "folder":
		for _, dir := range s.dirs[hostKey(u)] {
			if strings.HasPrefix(u.Path, dir) {
				return true
			}
		}
		return false
	default:
		return s.hosts[hostKey(u)]
	}
}

// inScope is the scope check for one start address (no redirects known).
func inScope(start, u *url.URL, scope string) bool { return newGrabScope(start, scope).in(u) }

func extOf(u *url.URL) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(u.Path), "."))
}

// ---------------------------------------------------------------- jobs

func (g *Grabber) Start(o GrabOptions, project string) (*GrabJob, error) {
	o, err := normalizeOpts(o)
	if err != nil {
		return nil, err
	}
	session := o.Session
	o.Session = ""
	ctx, cancel := context.WithCancel(context.Background())
	j := &GrabJob{ID: newID(), Opts: o, State: "running", Started: time.Now(), cancel: cancel, seen: map[string]bool{}, Project: project}
	if session != "" {
		g.signIn(j, session)
	}
	g.mu.Lock()
	g.jobs[j.ID] = j
	// keep only recent jobs
	if len(g.jobs) > 20 {
		var old []*GrabJob
		for _, x := range g.jobs {
			if x.State != "running" {
				old = append(old, x)
			}
		}
		sort.Slice(old, func(a, b int) bool { return old[a].Started.Before(old[b].Started) })
		for i := 0; i < len(old)-10; i++ {
			delete(g.jobs, old[i].ID)
			g.dropScanFile(old[i].ID)
		}
	}
	g.mu.Unlock()
	go g.run(ctx, cancel, j)
	return j, nil
}

func (g *Grabber) Job(id string) *GrabJob {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.jobs[id]
}

func (j *GrabJob) view() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := map[string]any{"id": j.ID, "state": j.State, "error": j.Error, "pages": j.Pages, "queued": j.Queued, "skipped": j.Skipped,
		"files": append([]GrabFile{}, j.Files...), "started": j.Started, "finished": j.Finished, "opts": j.Opts.withoutLogins(), "project": j.Project,
		"title": j.Title, "fromPage": j.FromPage}
	j.viewExtra(v)
	return v
}

func (j *GrabJob) addFile(f GrabFile) {
	j.mu.Lock()
	defer j.mu.Unlock()
	key := "f:" + dedupKeyOf(f.URL) // (the same file under another spelling is the same file)
	if j.seen[key] || len(j.Files) >= 10000 {
		return
	}
	j.seen[key] = true
	j.Files = append(j.Files, f)
}

var (
	attrRe   = regexp.MustCompile(`(?i)\b(?:href|src|data-src|data-original|data-lazy-src|data-href|poster|data-url)\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>]+))`)
	srcsetRe = regexp.MustCompile(`(?i)\bsrcset\s*=\s*("([^"]*)"|'([^']*)')`)
	baseRe   = regexp.MustCompile(`(?i)<base\s[^>]*href\s*=\s*["']([^"']+)["']`)
	cssURLRe = regexp.MustCompile(`(?i)url\(\s*['"]?([^'")\s]+)['"]?\s*\)`)
)

// extractLinks finds every URL-ish attribute in an HTML page (no full parser needed).
func extractLinks(page *url.URL, body string) []*url.URL {
	base := page
	if m := baseRe.FindStringSubmatch(body); m != nil {
		if b, err := page.Parse(html.UnescapeString(m[1])); err == nil {
			base = b
		}
	}
	var raw []string
	for _, m := range attrRe.FindAllStringSubmatch(body, -1) {
		raw = append(raw, m[2]+m[3]+m[4])
	}
	for _, m := range srcsetRe.FindAllStringSubmatch(body, -1) {
		for _, part := range strings.Split(m[2]+m[3], ",") {
			if f := strings.Fields(part); len(f) > 0 {
				raw = append(raw, f[0])
			}
		}
	}
	for _, m := range cssURLRe.FindAllStringSubmatch(body, -1) {
		raw = append(raw, m[1])
	}
	var out []*url.URL
	for _, r := range raw {
		r = strings.TrimSpace(html.UnescapeString(r))
		low := strings.ToLower(r)
		if r == "" || strings.HasPrefix(r, "#") || strings.HasPrefix(low, "javascript:") || strings.HasPrefix(low, "mailto:") ||
			strings.HasPrefix(low, "tel:") || strings.HasPrefix(low, "data:") || strings.HasPrefix(low, "blob:") {
			continue
		}
		u, err := base.Parse(r)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ftp" && u.Scheme != "ftps") {
			continue
		}
		u.Fragment = ""
		out = append(out, u)
	}
	return out
}

func (g *Grabber) category(u *url.URL) string {
	g.m.mu.Lock()
	defer g.m.mu.Unlock()
	return g.m.categoryFor(u.Path)
}

// ---------------------------------------------------------------- FTP folders

func (g *Grabber) crawlFTP(ctx context.Context, j *GrabJob) error {
	o := j.Opts
	start, _ := url.Parse(o.URL)
	want := g.wantedExts(o)
	d := &Download{Headers: o.Headers}
	wait := time.Duration(o.Wait) * time.Millisecond
	// the folders still to list, kept in the job so a stopped scan can be resumed
	j.mu.Lock()
	queue := j.scan.left
	if !j.scan.begun {
		dir := start.Path
		if dir == "" {
			dir = "/"
		}
		queue = []crawlItem{{URL: dir}}
	}
	j.scan.begun, j.scan.left = true, queue
	j.mu.Unlock()
	if !g.m.netGateWait(ctx) {
		return ctx.Err()
	}
	fc, err := ftpDial(ctx, start, d)
	if err != nil {
		return err
	}
	defer func() { fc.Close() }()
	for first := true; len(queue) > 0 && ctx.Err() == nil; first = false {
		it := queue[0]
		queue = queue[1:]
		if !first && !sleepCtx(ctx, gapFor(wait, 0, rand.Float64())) { // (one folder at a time, a pause between)
			break
		}
		if !g.m.netGateWait(ctx) {
			break
		}
		entries, err := fc.list(ctx, it.URL)
		if err != nil && ctx.Err() == nil { // the connection may have dropped: once more on a fresh one
			fc.Close()
			if fc, err = ftpDial(ctx, start, d); err == nil {
				entries, err = fc.list(ctx, it.URL)
			}
		}
		if ctx.Err() != nil {
			break
		}
		for _, e := range entries {
			g.m.countDown(int64(len(e.name) + 64)) // (about a listing line each)
		}
		j.mu.Lock()
		j.Pages++
		if err != nil && it.Depth > 0 {
			j.Failed++
			if len(j.Failures) < maxFailures {
				j.Failures = append(j.Failures, GrabFailure{URL: ftpURL(start, it.URL), Kind: "net", Detail: friendlyError(err, o.URL)})
			}
		}
		j.mu.Unlock()
		if err != nil {
			if it.Depth == 0 {
				return err
			}
			continue
		}
		for _, e := range entries {
			p := path.Join(it.URL, e.name)
			u := *start
			u.Path = p
			if e.dir {
				if it.Depth < o.Depth {
					j.mu.Lock()
					over := j.Pages+len(queue) >= o.MaxPages
					if over {
						j.Skipped++
					}
					j.mu.Unlock()
					if !over {
						queue = append(queue, crawlItem{URL: p + "/", Depth: it.Depth + 1})
					}
				}
				continue
			}
			if ext := extOf(&u); want[ext] {
				j.addFile(GrabFile{URL: u.String(), Name: e.name, Ext: ext, Category: g.category(&u), From: start.String()})
			}
		}
		j.mu.Lock()
		j.scan.left = queue
		j.mu.Unlock()
	}
	return nil // (left holds the folders not done: a stopped scan resumes from them)
}

func ftpURL(start *url.URL, dir string) string {
	u := *start
	u.Path = dir
	return u.String()
}

type ftpEntry struct {
	name string
	dir  bool
}

// list uses MLSD (machine-readable) and falls back to NLST names.
func (fc *ftpConn) list(ctx context.Context, dir string) ([]ftpEntry, error) {
	read := func(cmd string) ([]string, int, error) {
		data, err := fc.passive(ctx)
		if err != nil {
			return nil, 0, err
		}
		defer data.Close()
		code, msg, err := fc.cmd(cmd)
		if err != nil {
			return nil, 0, err
		}
		if code != 150 && code != 125 {
			return nil, code, ftpErr(code, msg)
		}
		b, _ := io.ReadAll(io.LimitReader(data, 8<<20))
		data.Close()
		fc.read() // 226 transfer complete
		return strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n"), code, nil
	}
	if lines, _, err := read("MLSD " + dir); err == nil {
		var out []ftpEntry
		for _, l := range lines {
			facts, name, ok := strings.Cut(l, " ")
			if !ok || name == "" {
				continue
			}
			t := ""
			for _, f := range strings.Split(facts, ";") {
				if k, v, ok := strings.Cut(f, "="); ok && strings.EqualFold(k, "type") {
					t = strings.ToLower(v)
				}
			}
			if t == "cdir" || t == "pdir" || name == "." || name == ".." {
				continue
			}
			out = append(out, ftpEntry{name: name, dir: t == "dir"})
		}
		return out, nil
	}
	lines, _, err := read("NLST " + dir)
	if err != nil {
		return nil, err
	}
	var out []ftpEntry
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || l == "." || l == ".." {
			continue
		}
		n := path.Base(l)
		out = append(out, ftpEntry{name: n, dir: !strings.Contains(n, ".")})
	}
	return out, nil
}

// ---------------------------------------------------------------- adding results

type GrabAddReq struct {
	JobID   string   `json:"id"`
	URLs    []string `json:"urls"`
	Dir     string   `json:"dir"`
	Keep    bool     `json:"keep"`
	QueueID string   `json:"queueId"`
	Start   bool     `json:"start"`

	cookies map[string]string // the browser's, for the chosen files' sites (extension.go)
	note    string            // for the files, when the browser didn't give them
}

func (g *Grabber) AddFiles(r GrabAddReq) (int, error) {
	j := g.Job(r.JobID)
	if j == nil {
		return 0, errors.New("that grab has expired; run it again")
	}
	j.mu.Lock()
	byURL := map[string]GrabFile{}
	for _, f := range j.Files {
		byURL[f.URL] = f
	}
	opts := j.Opts
	j.mu.Unlock()
	var files []GrabFile
	for _, u := range r.URLs {
		if f, ok := byURL[u]; ok {
			files = append(files, f)
		}
	}
	ck := j.fileCookies(files)
	for h, c := range r.cookies { // (asked for now, for these files only)
		if ck == nil {
			ck = map[string]string{}
		}
		if ck[h] == "" {
			ck[h] = c
		}
	}
	return g.addDownloadsX(files, opts, r.Dir, r.Keep, r.QueueID, r.Start, grabExtra{cookies: ck, note: r.note, private: j.private}), nil
}

// grabExtra is what files from a grab may carry besides their address: cookies by host, a note
// and whether they're private.
type grabExtra struct {
	cookies map[string]string
	note    string
	private bool
}

// grabSite: does a file's host belong to the site the grab started at (or was redirected to)?
func grabSite(start string, more []string, host string) bool {
	if sameSite(start, host) {
		return true
	}
	for _, s := range more {
		if sameSite(s, host) {
			return true
		}
	}
	return false
}

func (g *Grabber) addDownloads(files []GrabFile, opts GrabOptions, dir string, keep bool, queueID string, start bool) int {
	return g.addDownloadsWith(files, opts, dir, keep, queueID, start, nil)
}

func (g *Grabber) addDownloadsWith(files []GrabFile, opts GrabOptions, dir string, keep bool, queueID string, start bool, cookies map[string]string) int {
	return g.addDownloadsX(files, opts, dir, keep, queueID, start, grabExtra{cookies: cookies})
}

func (g *Grabber) addDownloadsX(files []GrabFile, opts GrabOptions, dir string, keep bool, queueID string, start bool, x grabExtra) int {
	cookies := x.cookies
	if queueID == "" {
		queueID = "main"
	}
	n := 0
	startHost := ""
	if su, err := url.Parse(opts.URL); err == nil {
		startHost = su.Hostname()
	}
	for _, f := range files {
		if protectedURL(f.URL) {
			continue
		}
		h := map[string]string{}
		fu, _ := url.Parse(f.URL)
		for k, v := range opts.Headers {
			// a login or cookie typed for the site goes only to that site's files
			if (strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Authorization")) && (fu == nil || !grabSite(startHost, opts.Sites, fu.Hostname())) {
				continue
			}
			h[k] = v
		}
		if u, err := url.Parse(f.URL); err == nil && cookies[u.Hostname()] != "" {
			h["Cookie"] = cookies[u.Hostname()]
		}
		if f.From != "" && !isFTP(f.From) {
			h["Referer"] = f.From
		}
		target := dir
		if target != "" && keep {
			if u, err := url.Parse(f.URL); err == nil {
				sub := path.Dir(u.Path)
				var parts []string
				for _, p := range strings.Split(sub, "/") {
					if p != "" && p != "." && p != ".." {
						parts = append(parts, sanitizeName(p))
					}
				}
				target = filepath.Join(append([]string{dir, sanitizeName(u.Hostname())}, parts...)...)
			}
		}
		note := ""
		if h["Cookie"] == "" {
			note = x.note
		}
		g.m.Add(AddReq{URL: f.URL, Name: f.Name, Dir: target, QueueID: queueID, Headers: h, Private: x.private, note: note})
		n++
	}
	if start && n > 0 { // (the Free data hours queue starts by itself in the free hours)
		g.m.StartQueueChecked(queueID, false)
	}
	return n
}

// ---------------------------------------------------------------- projects

func (g *Grabber) Projects() []*GrabProject {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]*GrabProject, len(g.projects))
	for i, p := range g.projects {
		c := *p
		c.Known, c.Opts = nil, c.Opts.withoutLogins()
		out[i] = &c
	}
	return out
}

func (g *Grabber) SaveProject(p GrabProject, jobID string) (*GrabProject, error) {
	o, err := normalizeOpts(p.Opts)
	if err != nil {
		return nil, err
	}
	p.Opts = o
	if strings.TrimSpace(p.Name) == "" {
		if u, err := url.Parse(o.URL); err == nil {
			p.Name = u.Host
		}
	}
	g.mu.Lock()
	var cur *GrabProject
	for _, x := range g.projects {
		if x.ID == p.ID && p.ID != "" {
			cur = x
		}
	}
	if cur == nil {
		p.ID = newID()
		cur = &GrabProject{ID: p.ID}
		g.projects = append(g.projects, cur)
	}
	known := cur.Known
	*cur = p
	cur.Known = known
	g.mu.Unlock()
	// files from the run being saved count as already handled
	if j := g.Job(jobID); j != nil {
		j.mu.Lock()
		for _, f := range j.Files {
			cur.Known = append(cur.Known, f.URL)
		}
		j.mu.Unlock()
		g.mu.Lock()
		cur.LastRun = time.Now()
		g.mu.Unlock()
	}
	g.saveProjects()
	return g.forWindow(cur), nil
}

// forWindow copies a project without its list of known files and without its logins, for the window.
func (g *Grabber) forWindow(p *GrabProject) *GrabProject {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := *p
	c.Known, c.Opts = nil, c.Opts.withoutLogins()
	return &c
}

func (g *Grabber) DeleteProject(id string) {
	g.mu.Lock()
	out := g.projects[:0]
	for _, p := range g.projects {
		if p.ID != id {
			out = append(out, p)
		}
	}
	g.projects = out
	g.mu.Unlock()
	g.saveProjects()
}

func (g *Grabber) project(id string) *GrabProject {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.projects {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// projectFinished queues files a scheduled project run found for the first time.
func (g *Grabber) projectFinished(id string, j *GrabJob) {
	p := g.project(id)
	if p == nil || j.State != "done" {
		return
	}
	g.mu.Lock()
	known := map[string]bool{}
	for _, u := range p.Known {
		known[u] = true
	}
	opts, dir, keep, q := p.Opts, p.Dir, p.Keep, p.QueueID
	name := p.Name
	g.mu.Unlock()
	j.mu.Lock()
	var fresh []GrabFile
	for _, f := range j.Files {
		if !known[f.URL] {
			fresh = append(fresh, f)
		}
	}
	j.mu.Unlock()
	if dir == "" {
		dir = g.m.settings.DownloadDir
	}
	n := g.addDownloads(fresh, opts, dir, keep, q, true)
	g.mu.Lock()
	for _, f := range fresh {
		p.Known = append(p.Known, f.URL)
	}
	if len(p.Known) > 50000 {
		p.Known = p.Known[len(p.Known)-50000:]
	}
	p.LastRun = time.Now()
	g.mu.Unlock()
	g.saveProjects()
	g.m.mu.Lock()
	g.m.notify("grabDone", id, sprintfNew(iso(name), n))
	g.m.mu.Unlock()
}

func sprintfNew(name string, n int) string {
	switch n {
	case 0:
		return name + ": no new files"
	case 1:
		return name + ": 1 new file queued"
	}
	return name + ": " + itoa(n) + " new files queued"
}

// RunProject starts a project run. Scheduled runs queue new files automatically.
func (g *Grabber) RunProject(id string, scheduled bool) (*GrabJob, error) {
	p := g.project(id)
	if p == nil {
		return nil, errors.New("project not found")
	}
	g.mu.Lock()
	opts := p.Opts
	p.LastRun = time.Now()
	g.mu.Unlock()
	pid := ""
	if scheduled {
		pid = id
	}
	return g.Start(opts, pid)
}

// tick runs projects whose timer is due (called from the manager loop).
func (g *Grabber) tick(now time.Time) {
	g.mu.Lock()
	var due []string
	for _, p := range g.projects {
		if p.AutoMins > 0 && now.Sub(p.LastRun) >= time.Duration(p.AutoMins)*time.Minute {
			running := false
			for _, j := range g.jobs {
				if j.Project == p.ID && j.State == "running" {
					running = true
				}
			}
			if !running {
				due = append(due, p.ID)
			}
		}
	}
	g.mu.Unlock()
	for _, id := range due {
		g.RunProject(id, true)
	}
}
