package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web/index.html
var indexHTML string

//go:embed web/app.js
var appJS string

// i18n.js is the words and number formats both pages share (see i18n.go for how the phone page gets it).
//
//go:embed web/i18n.js
var i18nJS string

type Server struct {
	m       *Manager
	token   string   // run.json's secret (session.go): programs on this computer send it as X-Token
	streams sync.Map // the windows' live-update streams by their client name (events.go)
	port    int      // where it listens (0 in tests: any port)
	secOnce sync.Once
	sec     *serverSec // window sessions and the extension link (session.go, extlink.go)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(strings.Replace(appJS, "__ZL_VERSION__", appVersion, 1))) // (the About box shows it)
	})
	mux.HandleFunc("/i18n.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(i18nJS))
	})
	mux.HandleFunc("/api/events", s.auth(s.events))
	mux.HandleFunc("/api/events/view", s.auth(s.eventsView))
	mux.HandleFunc("/api/probe", s.auth(s.probe))
	mux.HandleFunc("/api/add", s.auth(s.add))
	mux.HandleFunc("/api/action", s.auth(s.action))
	mux.HandleFunc("/api/queue", s.auth(s.queue))
	mux.HandleFunc("/api/power", s.auth(s.powerH))
	mux.HandleFunc("/api/settings", s.auth(s.guardSettings(s.settingsH)))
	mux.HandleFunc("/api/props", s.auth(s.props))
	mux.HandleFunc("/api/limit", s.auth(s.limit))
	mux.HandleFunc("/api/profile", s.auth(s.profileH))
	mux.HandleFunc("/api/quota", s.auth(s.quotaH))
	mux.HandleFunc("/api/grab/start", s.auth(s.grabStart))
	mux.HandleFunc("/api/grab/status", s.auth(s.grabStatus))
	mux.HandleFunc("/api/grab/stop", s.auth(s.grabStop))
	mux.HandleFunc("/api/grab/add", s.auth(s.grabAdd))
	mux.HandleFunc("/api/grab/projects", s.auth(s.grabProjects))
	mux.HandleFunc("/api/grab/project", s.auth(s.grabProjectSave))
	mux.HandleFunc("/api/grab/project/delete", s.auth(s.grabProjectDelete))
	mux.HandleFunc("/api/grab/project/run", s.auth(s.grabProjectRun))
	mux.HandleFunc("/api/grab/scans", s.auth(s.grabScans))
	mux.HandleFunc("/api/grab/resume", s.auth(s.grabResume))
	mux.HandleFunc("/api/grab/scan/delete", s.auth(s.grabScanDelete))
	mux.HandleFunc("/api/grab/browser", s.auth(s.grabBrowser))
	mux.HandleFunc("/api/proxytest", s.auth(s.proxyTest))
	mux.HandleFunc("/api/prompt/dismiss", s.auth(s.dismiss))
	mux.HandleFunc("/api/pickdir", s.auth(s.pickdir))
	mux.HandleFunc("/api/clipboard", s.auth(s.clip))
	mux.HandleFunc("/api/quit", s.auth(s.quitH))
	mux.HandleFunc("/api/early", s.auth(s.early))
	mux.HandleFunc("/api/netifs", s.auth(s.netifs))
	mux.HandleFunc("/api/refreshlink", s.auth(s.refreshLink))
	mux.HandleFunc("/api/phone", s.auth(s.phoneH))
	mux.HandleFunc("/api/torrent/open", s.auth(s.torrentOpen))
	mux.HandleFunc("/api/torrent/meta", s.auth(s.torrentMeta))
	mux.HandleFunc("/api/torrent/files", s.auth(s.torrentFiles))
	mux.HandleFunc("/api/torrent/add", s.auth(s.torrentAdd))
	mux.HandleFunc("/api/torrent/detail", s.auth(s.torrentDetail))
	mux.HandleFunc("/api/torrent/net", s.auth(s.torrentNet))
	s.torrentRoutes(mux)
	s.linkRoutes(mux)
	s.phoneUIRoutes(mux)
	s.extRoutes(mux)
	mux.HandleFunc("/api/alert/seen", s.auth(func(w http.ResponseWriter, r *http.Request) {
		s.m.mu.Lock()
		s.m.alert = ""
		s.m.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("/api/torrent/assoc", s.auth(s.torrentAssoc))
	mux.HandleFunc("/api/renew", s.auth(s.renew))
	mux.HandleFunc("/api/checksum", s.auth(s.checksum))
	mux.HandleFunc("/api/ext/refresh", s.ext(s.extRefresh))
	mux.HandleFunc("/api/sound", s.auth(s.sound))
	mux.HandleFunc("/api/ext/ping", s.ext(func(w http.ResponseWriter, r *http.Request) {
		s.m.mu.Lock()
		s.m.extSeen = time.Now()
		cfg := map[string]any{"skipSites": s.m.settings.SkipSites, "keys": s.m.settings.Keys, "torrents": !s.m.settings.Torrent.NoBrowser,
			"protected": protectedSites, "protectedMsg": protectedMsg, "browser": s.m.settings.Browser, "theme": s.m.settings.Theme}
		jobs := s.m.takeRefreshJobs() // pages to open for a fresh link
		s.m.mu.Unlock()
		res := map[string]any{"app": "zipline", "ok": true, "config": cfg, "refresh": jobs, "grab": s.m.grab.takeAsks()}
		s.m.jobsReply(res) // downloads to hand back, cookies to send (extension.go)
		writeJSON(w, res)
	}))
	mux.HandleFunc("/api/ext/add", s.ext(s.extAdd))
	mux.HandleFunc("/api/ext/check", s.ext(s.extCheck))
	mux.HandleFunc("/api/ext/links", s.ext(s.extLinks))
	mux.HandleFunc("/api/ext/grab", s.ext(s.extGrab))
	mux.HandleFunc("/api/ext/scan", s.ext(s.extScan))
	mux.HandleFunc("/api/ext/cli", s.ext(s.secretOnly(s.extCLI)))
	mux.HandleFunc("/api/export", s.auth(s.export))
	mux.HandleFunc("/api/cliusage", s.auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(cliUsage))
	}))
	mux.HandleFunc("/api/import", s.auth(s.importH))
	mux.HandleFunc("/api/openurl", s.auth(func(w http.ResponseWriter, r *http.Request) { // in the normal browser
		var req struct {
			URL string `json:"url"`
		}
		if readJSON(r, &req) != nil || !(strings.HasPrefix(req.URL, "http://") || strings.HasPrefix(req.URL, "https://")) {
			http.Error(w, "bad request", 400)
			return
		}
		openPath(req.URL)
		writeJSON(w, map[string]any{"ok": true})
	}))
	mux.HandleFunc("/api/diagnostics", s.auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" { // show the log file in the file manager
			revealPath(filepath.Join(s.m.dataDir, "zipline.log"))
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(diagnosticsText(s.m)))
	}))
	mux.HandleFunc("/api/ext/show", s.ext(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Page string `json:"page"` // "browser": open Options → Browser
		}
		readJSON(r, &req)
		if req.Page == "browser" {
			s.m.uiCommand("browser")
		}
		go bringToFront()
		writeJSON(w, map[string]any{"ok": true})
	}))
	return s.hostGuard(mux)
}

// hostGuard blocks DNS-rebinding attacks: only Zipline's own loopback address is served.
func (s *Server) hostGuard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostOK(r.Host) {
			http.Error(w, "forbidden", 403)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// auth lets in the window's session or a program with the secret (session.go).
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind, ok := s.authorize(w, r)
		if !ok {
			return
		}
		h(w, withAuth(r, kind))
	}
}

// ext allows calls only from the browser extension, paired or through its native host, and from
// programs with the secret (extlink.go).
func (s *Server) ext(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r, ok := s.extGate(w, r)
		if !ok {
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.windowSession(r) { // only a window Zipline opened (login?c=…) gets the app
		s.lockedPage(w, 200)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	pageHeaders(w.Header())
	w.Write([]byte(indexHTML))
}

func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Title   string            `json:"title"`
		Early   bool              `json:"early"` // the Add dialog: may start downloading right away
		// a download taken over from the browser: its request, and what the browser's reply said
		// (then the server isn't asked again before the download itself starts)
		Method  string     `json:"method"`
		Body    []byte     `json:"body"`
		Known   *knownFile `json:"known"`
		Private bool       `json:"private"`
	}
	if readJSON(r, &req) != nil || req.URL == "" {
		http.Error(w, "bad request", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	d := &Download{URL: strings.TrimSpace(req.URL), Headers: req.Headers}
	if strings.EqualFold(req.Method, "POST") {
		d.Method, d.Body = "POST", req.Body
	}
	if protectedLink(d.URL, d.Headers) {
		writeJSON(w, map[string]any{"error": protectedMsg, "protected": true})
		return
	}
	if isMagnet(d.URL) || strings.HasPrefix(d.URL, "torrent:") {
		s.probeTorrent(w, d.URL)
		return
	}
	if looksLikeHLS(d.URL) {
		s.probeHLS(w, ctx, d, req.Title)
		return
	}
	pr, err := probeKnown(ctx, d, req.Known)
	if errors.Is(err, errCloudFolder) { // a shared folder: the dialog lists its files (cloudfolder.go)
		s.probeCloudFolder(w, ctx, d)
		return
	}
	if err != nil {
		writeJSON(w, map[string]any{"error": downloadErrText(err, d.URL), "challenge": errors.Is(err, errChallenge)})
		return
	}
	if looksLikeTorrent(pr.mime, pr.name, pr.size) {
		mi, terr := s.m.torrentFromBody(pr.body)
		pr.body.Close()
		if terr == nil {
			res := s.m.torrentInfo(mi.InfoHash, mi.Name, mi, nil)
			res["url"] = torrentRef(mi.InfoHash)
			writeJSON(w, res)
			return
		}
		writeJSON(w, map[string]any{"error": "the link gave a torrent file that can't be read: " + terr.Error()})
		return
	}
	if isMetalinkMime(pr.mime, d.URL) || isMetalinkMime(pr.mime, pr.finalURL) {
		files, merr := parseMetalink(pr.body)
		pr.body.Close()
		if merr == nil {
			s.probeMetalink(w, d.URL, files)
			return
		}
	}
	pr.body.Close()
	if isHLSMime(pr.mime) {
		s.probeHLS(w, ctx, d, req.Title)
		return
	}
	base, _ := url.Parse(pr.finalURL)
	nMirrors := len(mirrorsFromLinks(parseLinks(pr.hdrs, base), d.URL, pr.finalURL))
	sum := headerChecksum(pr.hdrs)
	s.m.mu.Lock()
	cat := s.m.categoryFor(pr.name)
	dir := s.m.dirFor(cat)
	name := s.m.uniqueName(dir, pr.name, nil)
	match := s.m.waitingMatch(pr, d.URL)
	early := s.m.settings.EarlyStart && req.Early && pr.resumable && pr.size > 0 && !s.m.saverMode(time.Now()) && match == nil
	s.m.mu.Unlock()
	res := map[string]any{"name": name, "size": pr.size, "resumable": pr.resumable, "category": cat, "dir": dir, "mime": pr.mime, "finalUrl": pr.finalURL}
	if nMirrors > 0 && pr.resumable {
		res["serverMirrors"] = nMirrors
	}
	if svc := cloudService(d.URL); svc != "" {
		res["service"] = svc
	}
	if sum != nil {
		res["checksum"] = map[string]string{"algo": sum.Algo, "from": "server"}
	}
	if match != nil {
		res["matches"] = map[string]any{"id": match.ID, "name": match.FileName, "done": atomic.LoadInt64(&match.done), "size": match.Size}
	}
	if early { // start now; the dialog confirms or cancels it
		e := s.m.startEarly(d.URL, req.Headers, name, dir, pr.size, pr.finalURL, pr.etag, pr.lastMod, func(e *Download) { e.Method, e.Body, e.Private = d.Method, d.Body, req.Private })
		res["early"], res["name"] = e.ID, e.FileName
	}
	writeJSON(w, res)
}

// early handles a download started while its Add dialog is open: keep (the dialog is still
// there), commit (Start / Download Later) or cancel.
func (s *Server) early(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		earlyCommit
	}
	if readJSON(r, &req) != nil || req.ID == "" {
		http.Error(w, "bad request", 400)
		return
	}
	switch req.Action {
	case "keep":
		writeJSON(w, map[string]any{"ok": s.m.keepEarly(req.ID)})
	case "cancel":
		s.m.mu.Lock()
		d := s.m.byID[req.ID]
		ok := d != nil && d.Provisional
		s.m.mu.Unlock()
		if ok {
			s.m.Remove([]string{req.ID}, true)
		}
		writeJSON(w, map[string]any{"ok": ok})
	case "commit":
		if _, err := parseChecksum(req.Checksum, req.Name); err != nil {
			writeJSON(w, map[string]any{"error": err.Error(), "field": "checksum"})
			return
		}
		d, err := s.m.commitEarly(req.earlyCommit)
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"id": d.ID})
	default:
		http.Error(w, "bad request", 400)
	}
}

// sound serves a chime (GET, for the window to play) or plays it on this computer (POST).
func (s *Server) sound(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	b := soundWAV(name)
	if b == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == "POST" {
		writeJSON(w, map[string]any{"played": playSound(name)})
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(b)
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	var req AddReq
	if readJSON(r, &req) != nil || req.URL == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if protectedLink(req.URL, req.Headers) {
		writeJSON(w, map[string]any{"error": protectedMsg, "protected": true})
		return
	}
	if _, err := parseChecksum(req.Checksum, req.Name); err != nil {
		writeJSON(w, map[string]any{"error": err.Error(), "field": "checksum"})
		return
	}
	if req.Meta && s.addCloudFolder(w, r, req) { // a shared folder's files (cloudfolder.go)
		return
	}
	if req.Meta {
		files := cachedMetalink(req.URL)
		if files == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			defer cancel()
			var err error
			if files, err = fetchMetalink(ctx, &Download{URL: req.URL, Headers: req.Headers}, req.URL); err != nil {
				writeJSON(w, map[string]any{"error": "couldn't read the metalink: " + err.Error()})
				return
			}
		}
		ds, err := s.m.AddMetalink(req, files)
		if err != nil {
			writeJSON(w, map[string]any{"error": "couldn't add the metalink's files: " + err.Error()})
			return
		}
		var ids []string
		for _, d := range ds {
			ids = append(ids, d.ID)
		}
		writeJSON(w, map[string]any{"id": ids[0], "ids": ids})
		return
	}
	d := s.m.Add(req)
	writeJSON(w, map[string]any{"id": d.ID})
}

// probeMetalink describes a metalink's files for the Add dialog.
func (s *Server) probeMetalink(w http.ResponseWriter, u string, files []mlFile) {
	cacheMetalink(u, files)
	type fv struct {
		Name      string `json:"name"` // with the folders it goes in
		Size      int64  `json:"size"`
		Algo      string `json:"algo,omitempty"`
		Mirrors   int    `json:"mirrors"`
		Pieces    bool   `json:"pieces"`
		PieceAlgo string `json:"pieceAlgo,omitempty"`
		Torrent   bool   `json:"torrent,omitempty"` // downloaded with BitTorrent, the mirrors as web seeds
	}
	var list []fv
	var total int64
	for _, f := range files {
		v := fv{Name: f.path(), Size: f.Size, Mirrors: len(f.URLs), Pieces: f.Pieces != nil, Torrent: f.Torrent != ""}
		if f.Hash != nil {
			v.Algo = f.Hash.Algo
		}
		if f.Pieces != nil {
			v.PieceAlgo = f.Pieces.Algo
		}
		list = append(list, v)
		if f.Size > 0 {
			total += f.Size
		}
	}
	s.m.mu.Lock()
	cat := s.m.categoryFor(files[0].Name)
	dir := s.m.dirFor(cat)
	name := s.m.metalinkName(dir, files[0])
	s.m.mu.Unlock()
	writeJSON(w, map[string]any{"name": name, "size": files[0].Size, "resumable": true, "category": cat, "dir": dir,
		"mime": "application/metalink4+xml", "metalink": map[string]any{"files": list, "total": total}})
}

// refreshLink is "Refresh download link".
func (s *Server) refreshLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	writeJSON(w, map[string]any{"result": s.m.RefreshLink(req.ID)})
}

// renew gives a download a new link, after checking it's the same file.
func (s *Server) renew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string            `json:"id"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	if readJSON(r, &req) != nil || req.URL == "" {
		http.Error(w, "bad request", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	u := strings.TrimSpace(req.URL)
	if s.renewStream(w, ctx, req.ID, u, req.Headers) {
		return
	}
	pr, err := probe(ctx, &Download{URL: u, Headers: req.Headers}, u, 0, 0)
	if err != nil {
		writeJSON(w, map[string]any{"error": downloadErrText(err, u)})
		return
	}
	pr.body.Close()
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.byID[req.ID]
	switch {
	case d == nil:
		writeJSON(w, map[string]any{"error": "that download is gone"})
	case d.running:
		writeJSON(w, map[string]any{"error": "pause the download first"})
	case !sameDownload(d, pr, u):
		writeJSON(w, map[string]any{"error": fmt.Sprintf("that link is for a different file (%s, %s)", pr.name, humanSize(pr.size))})
	default:
		h := map[string]string{}
		for k, v := range req.Headers {
			h[k] = v
		}
		m.renewLink(d, u, h, pr)
		writeJSON(w, map[string]any{"ok": true})
	}
}

// renewStream is renew for a stream download: the new link must serve the same stream (same
// quality and pieces). False when id isn't a stream download.
func (s *Server) renewStream(w http.ResponseWriter, ctx context.Context, id, u string, headers map[string]string) bool {
	m := s.m
	m.mu.Lock()
	d := m.byID[id]
	stream := d != nil && d.Kind == "hls"
	m.mu.Unlock()
	if !stream {
		return false
	}
	same := m.sameStream(ctx, d, headers, u)
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case d.running:
		writeJSON(w, map[string]any{"error": "pause the download first"})
	case !same:
		writeJSON(w, map[string]any{"error": "that link is for a different video (or another quality of it)"})
	default:
		m.renewStream(d, u, headers)
		writeJSON(w, map[string]any{"ok": true})
	}
	return true
}

// checksum sets the checksum a download should match, or checks/calculates a finished file's.
func (s *Server) checksum(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Want string `json:"want"`
		Calc bool   `json:"calc"` // no checksum to compare: just show the file's SHA-256
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	m := s.m
	m.mu.Lock()
	d := m.byID[req.ID]
	if d == nil {
		m.mu.Unlock()
		http.Error(w, "not found", 404)
		return
	}
	var c *Checksum
	if strings.TrimSpace(req.Want) != "" {
		var err error
		if c, err = parseChecksum(req.Want, d.FileName); err != nil {
			m.mu.Unlock()
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
	} else if req.Calc {
		c = &Checksum{Algo: "sha256", From: "calculated"}
	} else { // clear it
		d.Check = nil
		m.dirty = true
		m.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	d.Check = c
	m.dirty = true
	done := d.Status == StCompleted && d.stage == ""
	m.mu.Unlock()
	if done {
		go m.checkFinished(d)
	}
	writeJSON(w, map[string]any{"ok": true, "algo": c.Algo, "checking": done})
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string   `json:"action"`
		IDs    []string `json:"ids"`
		Queue  string   `json:"queue"`
		Sure   bool     `json:"sure"` // asked about using normal data outside free hours
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	m := s.m
	switch req.Action {
	case "start":
		if err := m.StartChecked(req.IDs, req.Sure); err != nil {
			writeJSON(w, map[string]any{"error": err.Error(), "ask": "freeHours"})
			return
		}
	case "pause":
		m.Pause(req.IDs)
	case "remove":
		m.Remove(req.IDs, false)
	case "removeFile":
		m.Remove(req.IDs, true)
	case "redownload":
		m.Redownload(req.IDs)
	case "recheck": // torrents: Check files
		m.CheckTorrents(req.IDs)
	case "queueAdd":
		q := req.Queue
		if q == "" {
			q = "main"
		}
		m.SetQueue(req.IDs, q)
	case "queueRemove":
		m.SetQueue(req.IDs, "")
	case "up":
		m.Move(req.IDs, -1)
	case "down":
		m.Move(req.IDs, 1)
	case "scan":
		m.ScanNow(req.IDs)
	case "stopSharing":
		for _, id := range req.IDs {
			m.StopSharing(id)
		}
	case "browse": // the link in the normal browser: a site's "are you human" check, a sign-in page
		m.mu.Lock()
		var links []string
		for _, id := range req.IDs {
			if d := m.byID[id]; d != nil && (strings.HasPrefix(d.URL, "http://") || strings.HasPrefix(d.URL, "https://")) {
				links = append(links, d.URL)
			}
		}
		m.mu.Unlock()
		for _, u := range links {
			openPath(u)
		}
	case "open", "folder":
		m.mu.Lock()
		var paths []string
		for _, id := range req.IDs {
			if d := m.byID[id]; d != nil {
				p := d.path()
				if d.Status != StCompleted && d.Kind != "torrent" { // (torrents write under their own names)
					p = d.partPath()
				}
				paths = append(paths, p)
			}
		}
		m.mu.Unlock()
		for _, p := range paths {
			if req.Action == "open" {
				openPath(p)
			} else {
				revealPath(p)
			}
		}
	default:
		http.Error(w, "unknown action", 400)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) queue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
		ID     string `json:"id"`
		Sure   bool   `json:"sure"` // asked about using normal data outside free hours
	}
	readJSON(r, &req)
	if req.ID == "" {
		req.ID = "main"
	}
	if req.Action == "start" {
		if err := s.m.StartQueueChecked(req.ID, req.Sure); err != nil {
			writeJSON(w, map[string]any{"error": err.Error(), "ask": "freeHours"})
			return
		}
	} else {
		s.m.StopQueue(req.ID)
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) settingsH(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var st Settings
		if err := readJSON(r, &st); err != nil || strings.TrimSpace(st.DownloadDir) == "" {
			http.Error(w, "bad request", 400)
			return
		}
		if st.Proxy.Mode == "manual" && strings.TrimSpace(st.Proxy.URL) != "" {
			if _, err := parseProxyURL(st.Proxy.URL); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		s.m.mu.Lock()
		st.Phone.Devices = s.m.settings.Phone.Devices // phones are paired and removed through /api/phone
		st.Phone.Allow = s.m.settings.Phone.Allow     // so are the networks allowed anyway
		s.m.mergeSecrets(&st)                         // passwords the window left untouched stay
		old := s.m.settings.Queues
		// a profile chosen by hand over the schedule is switched through /api/profile
		st.Traffic.Override, st.Traffic.Until = s.m.settings.Traffic.Override, s.m.settings.Traffic.Until
		s.m.settings = st
		s.m.normalizeSettings()
		s.m.keepSchedState(old) // (the window's copy of what the scheduler did may be out of date)
		s.m.applyLimit()
		// downloads in deleted queues drop out of queueing
		for _, d := range s.m.list {
			if d.Queue != "" && s.m.queueByID(d.Queue) == nil {
				d.Queue = ""
				if d.Status == StQueued {
					d.Status = StPaused
				}
			}
		}
		s.m.dirty = true
		proxy := s.m.settings.Proxy
		s.m.mu.Unlock()
		setProxySettings(proxy)
		s.m.restartTorrentListener()
		s.m.torrentNetChanged()
		s.m.kick()
	}
	writeJSON(w, s.m.settingsView()) // (never a password: vault_settings.go)
}

func (s *Server) props(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string    `json:"id"`
		URL     string    `json:"url"`
		Name    string    `json:"name"`
		Desc    string    `json:"desc"`
		Scan    string    `json:"scan"`    // "" = unchanged, "default", "on", "off"
		Mirrors *[]string `json:"mirrors"` // nil = unchanged
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	m := s.m
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.byID[req.ID]
	if d == nil {
		http.Error(w, "not found", 404)
		return
	}
	if d.Torrent != nil { // a torrent's address and file names come from the torrent itself
		req.URL, req.Name, req.Mirrors = "", "", nil
	}
	req.URL = restoreURLPass(strings.TrimSpace(req.URL), d.URL) // (the window was shown it without its password)
	if req.Mirrors != nil {
		ms := restoreURLPasses(*req.Mirrors, d.Mirrors)
		req.Mirrors = &ms
	}
	if req.URL != "" && req.URL != d.URL {
		d.URL = strings.TrimSpace(req.URL)
		d.FinalURL = ""
		if d.Link != "" { // a new link for an expired one: Resume carries on
			d.Link, d.linkTries = "", 0
			if d.Status == StError {
				d.Error = "New link saved — press Resume to carry on"
			}
		}
	}
	if req.Mirrors != nil {
		d.Mirrors = cleanMirrors(*req.Mirrors, d.URL)
	}
	d.Desc = req.Desc
	switch req.Scan {
	case "default":
		d.Scan = ""
	case "on", "off":
		d.Scan = req.Scan
	}
	if n := sanitizeName(req.Name); req.Name != "" && n != d.FileName {
		if d.running {
			writeJSON(w, map[string]any{"error": "Pause the download before renaming"})
			return
		}
		if moveActive(d.ID) {
			writeJSON(w, map[string]any{"error": "Wait until it has been moved before renaming"})
			return
		}
		if d.Status == StCompleted {
			nn := m.uniqueName(d.Dir, n, d)
			if err := os.Rename(d.path(), filepath.Join(d.Dir, nn)); err != nil {
				writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			d.FileName = nn
		} else {
			old, oldA, oldX := d.partPath(), d.audioPartPath(), d.extraPartPaths()
			d.FileName = m.uniqueName(d.Dir, n, d)
			os.Rename(old, d.partPath())
			os.Rename(oldA, d.audioPartPath())
			moveExtras(d, oldX)
		}
		d.Category = m.categoryFor(d.FileName)
	}
	m.dirty = true
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) dismiss(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readJSON(r, &req)
	s.m.DismissPrompt(req.ID)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) pickdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Start string `json:"start"`
	}
	readJSON(r, &req)
	dir, err := pickDirectory(req.Start)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"dir": dir})
}

func (s *Server) clip(w http.ResponseWriter, r *http.Request) {
	t, _ := readClipboard()
	writeJSON(w, map[string]any{"text": t})
}

func (s *Server) quitH(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.m.Quit()
	}()
}

func (s *Server) extAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL       string `json:"url"`
		Referer   string `json:"referer"`
		Origin    string `json:"origin"` // of the player's own request (streams)
		Cookie    string `json:"cookie"`
		UserAgent string `json:"userAgent"`
		Name      string `json:"filename"`
		Title     string `json:"title"`
		// a download taken over from the browser (extension.go): the browser's own request, what its
		// reply said, and the window it came from (handed back as it was if the Add dialog is closed)
		Headers map[string]string `json:"headers"`
		Method  string            `json:"method"`
		Body    []byte            `json:"body"`
		Known   *knownFile        `json:"known"`
		Taken   bool              `json:"taken"`
		Private bool              `json:"private"`
		Ctx     json.RawMessage   `json:"ctx"`
	}
	if readJSON(r, &req) != nil || !(strings.HasPrefix(req.URL, "http://") || strings.HasPrefix(req.URL, "https://") || isFTP(req.URL) || isMagnet(req.URL)) || len(req.Body) > 1<<20 || len(req.Ctx) > 4<<10 {
		http.Error(w, "bad request", 400)
		return
	}
	if protectedLink(req.URL, map[string]string{"Referer": req.Referer}) {
		writeJSON(w, map[string]any{"ok": false, "error": protectedMsg, "protected": true})
		return
	}
	if isMagnet(req.URL) { // a magnet link clicked in the browser: the Add dialog shows its files
		if _, err := parseMagnet(req.URL); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		s.m.AddPrompt(Prompt{URL: strings.TrimSpace(req.URL), Source: "browser", Private: req.Private})
		bringToFront()
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	h := cleanBrowserHeaders(req.Headers)
	if req.Referer != "" {
		h["Referer"] = req.Referer
	}
	if req.Origin != "" {
		h["Origin"] = req.Origin
	}
	if req.Cookie != "" {
		h["Cookie"] = req.Cookie
	}
	if req.UserAgent != "" {
		h["User-Agent"] = req.UserAgent
	}
	title := strings.TrimSpace(req.Title)
	if r := []rune(title); len(r) > 120 { // (cut between letters: a title can be in any script)
		title = string(r[:120])
	}
	name := ""
	if req.Name != "" { // (Base("") is "."; that would stand in for the name the Add dialog works out itself)
		name = filepath.Base(req.Name)
	}
	p := Prompt{URL: req.URL, Name: name, Headers: h, Source: "browser", Title: title, Known: req.Known, Taken: req.Taken, Private: req.Private}
	if strings.EqualFold(req.Method, "POST") {
		p.Method, p.Body = "POST", req.Body
	}
	prompt := func() {
		id := s.m.AddPrompt(p)
		if p.Taken { // closing its Add dialog hands it back to the browser
			back := map[string]string{}
			for k, v := range h {
				if k != "Cookie" && k != "Authorization" { // (the browser sends its own)
					back[k] = v
				}
			}
			s.m.noteTaken(id, giveBackJob{URL: p.URL, Method: p.Method, Body: p.Body, Headers: back, Filename: name, Ctx: req.Ctx})
		}
		bringToFront()
	}
	if p.Method == "" && s.m.hasWaitingLinks() { // maybe the link a stopped download is waiting for
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if s.m.tryLinks(ctx, []linkCand{{URL: req.URL, Referer: req.Referer, Cookie: h["Cookie"]}}, h["User-Agent"], "") != nil {
				return
			}
			prompt()
		}()
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	prompt()
	writeJSON(w, map[string]any{"ok": true})
}

// extRefresh receives links from the page an expired download came from.
func (s *Server) extRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID        string     `json:"id"`
		Links     []linkCand `json:"links"`
		UserAgent string     `json:"userAgent"`
		Done      bool       `json:"done"` // the extension has given up
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	m := s.m
	m.mu.Lock()
	d := m.byID[req.ID]
	waiting := d != nil && d.Link == "refreshing"
	m.mu.Unlock()
	if !waiting {
		writeJSON(w, map[string]any{"ok": true, "matched": d != nil && d.Link == "", "gone": true})
		return
	}
	if len(req.Links) > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second) // (the extension waits 24 s: an extension's request may not take 30)
		defer cancel()
		if m.tryLinks(ctx, req.Links, req.UserAgent, req.ID) != nil {
			writeJSON(w, map[string]any{"ok": true, "matched": true})
			return
		}
	}
	if req.Done {
		m.mu.Lock()
		if d.Link == "refreshing" {
			m.linkNeeded(d)
		}
		m.mu.Unlock()
	}
	writeJSON(w, map[string]any{"ok": true, "matched": false})
}

// extCheck lets the browser extension confirm Zipline can fetch the same file the
// browser is downloading before it takes the download over.
func (s *Server) extCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL       string `json:"url"`
		Referer   string `json:"referer"`
		Cookie    string `json:"cookie"`
		UserAgent string `json:"userAgent"`
	}
	if readJSON(r, &req) != nil || !(strings.HasPrefix(req.URL, "http://") || strings.HasPrefix(req.URL, "https://")) {
		http.Error(w, "bad request", 400)
		return
	}
	if protectedLink(req.URL, map[string]string{"Referer": req.Referer}) { // (the browser keeps those downloads)
		writeJSON(w, map[string]any{"ok": false, "error": protectedMsg, "protected": true})
		return
	}
	h := map[string]string{}
	if req.Referer != "" {
		h["Referer"] = req.Referer
	}
	if req.Cookie != "" {
		h["Cookie"] = req.Cookie
	}
	if req.UserAgent != "" {
		h["User-Agent"] = req.UserAgent
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	pr, err := probe(ctx, &Download{URL: req.URL, Headers: h}, req.URL, 0, -1)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	pr.body.Close()
	writeJSON(w, map[string]any{"ok": true, "size": pr.size, "name": pr.name, "mime": pr.mime, "resumable": pr.resumable})
}

// limit sets a per-download speed limit (KB/s, 0 = none); applies immediately.
func (s *Server) limit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
		KB int    `json:"kb"`
	}
	if readJSON(r, &req) != nil || req.KB < 0 {
		http.Error(w, "bad request", 400)
		return
	}
	s.m.mu.Lock()
	d := s.m.byID[req.ID]
	if d != nil {
		d.LimitKB = req.KB
		d.lim.SetRate(int64(req.KB) * 1024)
		s.m.dirty = true
	}
	s.m.mu.Unlock()
	if d == nil {
		http.Error(w, "not found", 404)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// proxyTest tries a small request with the given (unsaved) proxy settings.
func (s *Server) proxyTest(w http.ResponseWriter, r *http.Request) {
	var p ProxySettings
	if readJSON(r, &p) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	s.m.fillProxyPass(&p)
	target, _ := url.Parse("https://www.gstatic.com/generate_204")
	used := "direct connection"
	pu, err := proxyForWith(p, target)
	if err == nil {
		if pu != nil {
			used = "proxy " + pu.Host
		}
		// a throwaway client so active downloads keep their current proxy
		tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return pu, nil }, TLSHandshakeTimeout: 10 * time.Second}
		defer tr.CloseIdleConnections()
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		var req *http.Request
		if req, err = http.NewRequestWithContext(ctx, "GET", target.String(), nil); err == nil {
			var resp *http.Response
			if resp, err = (&http.Client{Transport: tr}).Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusProxyAuthRequired {
					err = fmt.Errorf("the proxy asked for a user name and password")
				} else if resp.StatusCode >= 400 {
					err = fmt.Errorf("got %s", resp.Status)
				}
			}
		}
	}
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": friendlyNetErr(err, pu), "via": used})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "via": used})
}

// friendlyNetErr turns Go network errors into one short sentence.
func friendlyNetErr(err error, proxy *url.URL) string {
	m := err.Error()
	where := "the internet"
	if proxy != nil {
		where = "the proxy at " + proxy.Host
	}
	switch {
	case strings.Contains(m, "connection refused"):
		return "couldn't reach " + where + " (connection refused)"
	case strings.Contains(m, "no such host"):
		return "couldn't find " + where + " (name not found)"
	case strings.Contains(m, "timeout") || strings.Contains(m, "deadline exceeded"):
		return "no answer from " + where + " (timed out)"
	case strings.Contains(m, "certificate"):
		return "secure connection failed (certificate problem)"
	}
	if i := strings.LastIndex(m, ": "); i > 0 && len(m)-i < 80 {
		return m[i+2:]
	}
	return m
}

func (s *Server) probeHLS(w http.ResponseWriter, ctx context.Context, d *Download, title string) {
	info, err := s.m.resolveHLS(ctx, d, d.URL, 0)
	if err != nil {
		writeJSON(w, map[string]any{"error": downloadErrText(err, d.URL)})
		return
	}
	s.m.mu.Lock()
	dir := s.m.dirFor("Video")
	name := s.m.uniqueName(dir, hlsName(d.URL, title, info.Ext), nil)
	s.m.mu.Unlock()
	res := map[string]any{"kind": "hls", "name": name, "size": -1, "resumable": true, "category": "Video", "dir": dir,
		"mime": "video stream (" + info.Format + ")", "variants": info.Variants, "segments": info.Segments, "duration": info.Duration,
		"fmp4": info.FMP4, "separateAudio": info.Audio != nil, "format": info.Format, "live": info.Live, "event": info.Event}
	streamChoices(res, info)
	switch {
	case info.DRM:
		res["error"] = "this video is DRM-protected, so it can't be downloaded"
	case info.Live && info.Format == "DASH":
		res["error"] = "this is a live DASH stream; Zipline records live HLS streams only"
	case streamRefused(info) != nil:
		res["error"] = streamRefused(info).Error()
	}
	if match := s.m.waitingStream(ctx, d.URL, d.Headers); match != nil && !info.Live { // the same stream as one whose link expired
		s.m.mu.Lock()
		done, size := match.streamProgress()
		res["matches"] = map[string]any{"id": match.ID, "name": match.FileName, "done": done, "size": size}
		s.m.mu.Unlock()
	}
	writeJSON(w, res)
}

// ---------------------------------------------------------------- site grabber

func (s *Server) grabStart(w http.ResponseWriter, r *http.Request) {
	var o GrabOptions
	if readJSON(r, &o) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	j, err := s.m.grab.Start(o, "")
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"id": j.ID})
}

func (s *Server) grabStatus(w http.ResponseWriter, r *http.Request) {
	j := s.m.grab.Job(r.URL.Query().Get("id"))
	if j == nil {
		http.Error(w, "not found", 404)
		return
	}
	writeJSON(w, j.view())
}

func (s *Server) grabStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readJSON(r, &req)
	if j := s.m.grab.Job(req.ID); j != nil {
		j.stop()
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) grabAdd(w http.ResponseWriter, r *http.Request) {
	var req GrabAddReq
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	note := s.browserCookies(r.Context(), &req) // links from the browser: its cookies for the chosen files' sites only
	n, err := s.m.grab.AddFiles(req)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	res := map[string]any{"added": n}
	if note != "" {
		res["note"] = note
	}
	writeJSON(w, res)
}

func (s *Server) grabProjects(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.m.grab.Projects())
}

func (s *Server) grabProjectSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Project GrabProject `json:"project"`
		JobID   string      `json:"jobId"`
	}
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	p, err := s.m.grab.SaveProject(req.Project, req.JobID)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"project": p})
}

func (s *Server) grabProjectDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readJSON(r, &req)
	s.m.grab.DeleteProject(req.ID)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) grabProjectRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readJSON(r, &req)
	j, err := s.m.grab.RunProject(req.ID, false)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"id": j.ID})
}

func (s *Server) grabScans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.m.grab.Scans())
}

func (s *Server) grabResume(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readJSON(r, &req)
	j, err := s.m.grab.Resume(req.ID)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"id": j.ID})
}

func (s *Server) grabScanDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	readJSON(r, &req)
	s.m.grab.DeleteScan(req.ID)
	writeJSON(w, map[string]any{"ok": true})
}

// grabBrowser asks the browser extension to open a scan's script-built pages and send their links.
func (s *Server) grabBrowser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Page string `json:"page"`
	}
	readJSON(r, &req)
	if err := s.m.grab.AskBrowser(req.ID, req.Page); err != nil {
		res := map[string]any{"error": err.Error()}
		if err == errNoBrowser {
			res["code"] = "noext"
		}
		writeJSON(w, res)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---------------------------------------------------------------- page links, command line, export/import

func (s *Server) extLinks(w http.ResponseWriter, r *http.Request) {
	var p PageLinks
	if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&p) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	j, err := s.m.grab.FromPage(p)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	title := strings.TrimSpace(p.Title)
	if len(title) > 120 {
		title = title[:120]
	}
	s.m.pickerOpen(j.ID) // (its picker may ask the browser for cookies)
	s.m.AddPrompt(Prompt{URL: p.Page, Title: title, Kind: "links", JobID: j.ID, Source: "browser", Private: p.Private})
	bringToFront()
	writeJSON(w, map[string]any{"ok": true, "count": len(j.Files)})
}

// extGrab takes the links the browser found on a scan's script-built page (see grabjs.go).
func (s *Server) extGrab(w http.ResponseWriter, r *http.Request) {
	var b BrowserLinks
	if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&b) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := s.m.grab.FeedLinks(b); err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.m.pickerOpen(b.JobID) // (the scan's files may want the browser's cookies when you pick them)
	writeJSON(w, map[string]any{"ok": true})
}

// extScan is "Scan this site" in the browser: the window opens the grabber on the page, and the
// scan it starts uses the browser's sign-in for that site.
func (s *Server) extScan(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Page    string            `json:"page"`
		Title   string            `json:"title"`
		Cookies map[string]string `json:"cookies"`
	}
	if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if u, err := url.Parse(p.Page); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		writeJSON(w, map[string]any{"ok": false, "error": "not a web page"})
		return
	}
	id := s.m.grab.NewSession(p.Page, p.Cookies)
	s.m.AddPrompt(Prompt{URL: p.Page, Title: strings.TrimSpace(p.Title), Kind: "scan", JobID: id, Source: "browser"}) // (JobID: the sign-in's id)
	bringToFront()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) extCLI(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" || r.Header.Get("Origin") != "" { // local programs only, never a web page or extension
		http.Error(w, "forbidden", 403)
		return
	}
	var req CLIRequest
	if readJSON(r, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	note, err := s.m.ApplyCLI(&req)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "note": note})
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var ids []string
	if v := q.Get("ids"); v != "" {
		ids = strings.Split(v, ",")
	}
	b, name := s.m.Export(q.Get("format"), ids, q.Get("secrets") == "1")
	if q.Get("format") == "txt" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("X-Filename", name)
	if q.Get("save") == "1" { // fallback: write into the download folder and show it
		s.m.mu.Lock()
		dir := s.m.settings.DownloadDir
		s.m.mu.Unlock()
		p := uniquePath(filepath.Join(dir, name))
		perm := os.FileMode(0o644)
		if q.Get("secrets") == "1" { // it holds logins and cookies
			perm = privateFile
		}
		if err := os.WriteFile(p, b, perm); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		revealPath(p)
		writeJSON(w, map[string]any{"path": p})
		return
	}
	w.Write(b)
}

func (s *Server) importH(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content string `json:"content"`
		QueueID string `json:"queueId"`
		Start   bool   `json:"start"`
		SkipDup bool   `json:"skipDup"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	res, err := s.m.Import(req.Content, req.QueueID, req.Start, req.SkipDup)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, res)
}

// netifs lists the network adapters (GET), or tests one (POST {"name"}).
func (s *Server) netifs(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var req struct {
			Name string `json:"name"`
		}
		if readJSON(r, &req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		rtt, err := testNetIf(ctx, req.Name, "1.1.1.1:443")
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": friendlyNetErr(err, nil)})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "ms": rtt.Milliseconds()})
		return
	}
	s.m.mu.Lock()
	c := s.m.settings.Multi
	s.m.mu.Unlock()
	writeJSON(w, map[string]any{"adapters": netStats(c)})
}
