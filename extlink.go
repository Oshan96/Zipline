package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The browser extension's link to the app (the browser's side is extension/link.js, the
// native-messaging host is nativehost.go). Two ways in:
//
//   - Native messaging, preferred: the browser starts Zipline's host program for this extension only
//     and vouches for both sides. The host reaches the app with run.json's secret (X-Token) and says
//     so (X-Zipline-Via: native).
//   - HTTP, paired: an extension without the host asks to pair (/api/ext/pair) and the window asks
//     "A browser extension wants to connect". Once allowed, the extension holds a key and signs each
//     request with it (HMAC-SHA256 over the method, path, time, a nonce and the body; each nonce
//     once, within 90 seconds). The app answers the nonce with the same key (X-Zipline-Proof), so a
//     program that takes the port while Zipline is away can neither use the key nor learn it.
//
// Anything else gets only the read-only ping: the app's version and that pairing is needed.
//
// Version handshake: every reply carries the app's version and the oldest extension it works with
// (X-Zipline-Version, X-Zipline-Min-Ext); the extension sends its own version and the oldest app it
// works with (X-Zipline-Ext-Version, X-Zipline-Min-App). Options → Browser shows the result.

const (
	minExtVersion = "1.0.0" // the first extension with the link (native host, pairing, handshake)
	pairAskLife   = 2 * time.Minute
	extRecent     = 75 * time.Second // the extension checks in every 30 seconds
)

// extPair is a browser paired over HTTP.
type extPair struct {
	ID      string    `json:"id"`
	Key     string    `json:"key"` // protected like saved passwords
	Origin  string    `json:"origin"`
	Browser string    `json:"browser"`
	Added   time.Time `json:"added"`
	Seen    time.Time `json:"seen,omitempty"`
}

// pairAsk is an extension's request to pair, waiting for the window's answer.
type pairAsk struct {
	ID, Origin, Browser, Version string
	Made                         time.Time
	State                        string // pending, allowed, denied
	Pair                         *extPair
}

// extLinkState lives in serverSec (guarded by its mu).
type extLinkState struct {
	loaded   bool
	pairs    []*extPair
	asks     map[string]*pairAsk
	nonces   map[string]time.Time
	deniedAt time.Time
	// what the extension said last
	via, version, minApp, origin string
	seen, oldSeen, pairSeen      time.Time // linked, an extension without the link, an unpaired one
	waiting                      int       // native hosts waiting for a wake-up
	announced                    map[string]bool
}

func (l *extLinkState) init() {
	l.asks = map[string]*pairAsk{}
	l.nonces = map[string]time.Time{}
	l.announced = map[string]bool{}
}

func extOrigin(o string) bool {
	return strings.HasPrefix(o, "chrome-extension://") || strings.HasPrefix(o, "moz-extension://") || strings.HasPrefix(o, "extension://")
}

// ---------------------------------------------------------------- paired browsers, on disk

func (s *Server) pairFile() string {
	if s.m == nil || s.m.dataDir == "" {
		return ""
	}
	return filepath.Join(s.m.dataDir, "extpair.json")
}

// pairsLocked reads extpair.json the first time. Caller holds sec.mu.
func (s *Server) pairsLocked() []*extPair {
	l := &s.sec.link
	if !l.loaded {
		l.loaded = true
		if p := s.pairFile(); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				var list []*extPair
				if json.Unmarshal(b, &list) == nil {
					for _, x := range list {
						if x != nil && x.ID != "" {
							if x.Key = usableSecret(s.m.vault.reveal(x.Key)); x.Key != "" { // (a key that can't be opened: that browser pairs again)
								l.pairs = append(l.pairs, x)
							}
						}
					}
				}
			}
		}
	}
	return l.pairs
}

// savePairsLocked writes the paired browsers (keys protected, file for this user only). Caller holds sec.mu.
func (s *Server) savePairsLocked() {
	p := s.pairFile()
	if p == "" {
		return
	}
	out := make([]extPair, 0, len(s.sec.link.pairs))
	for _, x := range s.sec.link.pairs {
		c := *x
		c.Key = s.m.vault.protect(c.Key)
		out = append(out, c)
	}
	b, _ := json.MarshalIndent(out, "", " ")
	os.Remove(p + ".tmp")
	if os.WriteFile(p+".tmp", b, 0o600) == nil {
		os.Rename(p+".tmp", p)
	}
}

// ---------------------------------------------------------------- the gate for /api/ext/*

// extSig is what a paired extension sends in X-Zipline-Sig.
func extSig(key, method, uri, ts, nonce string, body []byte) string {
	bh := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(key))
	io.WriteString(mac, method+"\n"+uri+"\n"+ts+"\n"+nonce+"\n"+hex.EncodeToString(bh[:]))
	return hex.EncodeToString(mac.Sum(nil))
}

// extGate checks an /api/ext/* request: an extension's origin (or none: a program), then the secret
// or a paired extension's signature. The read-only ping is answered here for everyone else.
func (s *Server) extGate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	o := r.Header.Get("Origin")
	if o != "" && !extOrigin(o) {
		http.Error(w, "forbidden", 403)
		return nil, false
	}
	h := w.Header()
	if o != "" {
		h.Set("Access-Control-Allow-Origin", o)
		h.Set("Access-Control-Allow-Headers", "Content-Type, X-Zipline-Pair, X-Zipline-Time, X-Zipline-Nonce, X-Zipline-Sig, X-Zipline-Ext-Version, X-Zipline-Min-App")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Expose-Headers", "X-Zipline-Version, X-Zipline-Min-Ext, X-Zipline-Proof")
		h.Set("Vary", "Origin")
	}
	h.Set("X-Zipline-Version", appVersion)
	h.Set("X-Zipline-Min-Ext", minExtVersion)
	if r.Method == "OPTIONS" {
		return nil, false
	}
	nonce := r.Header.Get("X-Zipline-Nonce")
	kind, key, ok := s.extAuth(w, r)
	if !ok {
		return nil, false
	}
	if kind == "" {
		if nonce != "" && s.token != "" { // (the command line and the host check they reached Zipline)
			h.Set("X-Zipline-Proof", linkProof(s.token, nonce))
		}
		s.noteUnlinked(r)
		if r.URL.Path == "/api/ext/ping" {
			writeJSON(w, map[string]any{"app": "zipline", "version": appVersion, "minExt": minExtVersion, "pair": true})
			return nil, false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not paired", "pair": true})
		return nil, false
	}
	if nonce != "" {
		h.Set("X-Zipline-Proof", linkProof(key, nonce))
	}
	s.noteLinked(r, kind)
	return withAuth(r, kind), true
}

// extAuth: the secret (programs, the native host) or a paired extension's signature. kind is ""
// with ok for a request without either; ok is false when a reply (401) has been written.
func (s *Server) extAuth(w http.ResponseWriter, r *http.Request) (kind, key string, ok bool) {
	if t := r.Header.Get("X-Token"); t != "" && s.token != "" && hmac.Equal([]byte(t), []byte(s.token)) {
		return authSecret, s.token, true
	}
	id := r.Header.Get("X-Zipline-Pair")
	if id == "" {
		return "", "", true
	}
	bad := func() (string, string, bool) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not paired", "pair": true})
		return "", "", false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		return bad()
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	ts, nonce, sig := r.Header.Get("X-Zipline-Time"), r.Header.Get("X-Zipline-Nonce"), r.Header.Get("X-Zipline-Sig")
	ms, err := strconv.ParseInt(ts, 10, 64)
	sec := s.security()
	sec.mu.Lock()
	defer sec.mu.Unlock()
	now := sec.now()
	if err != nil || len(nonce) < 16 || len(nonce) > 64 {
		return bad()
	}
	if d := now.Sub(time.UnixMilli(ms)); d > 90*time.Second || d < -90*time.Second {
		return bad()
	}
	var p *extPair
	for _, x := range s.pairsLocked() {
		if x.ID == id {
			p = x
		}
	}
	if p == nil || !hmac.Equal([]byte(strings.ToLower(sig)), []byte(extSig(p.Key, r.Method, r.URL.RequestURI(), ts, nonce, body))) {
		return bad()
	}
	l := &sec.link
	for n, t := range l.nonces {
		if now.Sub(t) > 3*time.Minute {
			delete(l.nonces, n)
		}
	}
	if _, used := l.nonces[nonce]; used {
		return bad()
	}
	l.nonces[nonce] = now
	p.Seen = now
	return authPaired, p.Key, true
}

// noteLinked remembers what a linked extension said (Options → Browser).
func (s *Server) noteLinked(r *http.Request, kind string) {
	native := r.Header.Get("X-Zipline-Via") == "native"
	if kind != authPaired && !native { // (the command line)
		return
	}
	sec := s.security()
	sec.mu.Lock()
	defer sec.mu.Unlock()
	l := &sec.link
	l.via, l.seen = "http", sec.now()
	l.origin = r.Header.Get("Origin")
	if native {
		l.via, l.origin = "native", r.Header.Get("X-Zipline-Ext-Origin")
	}
	l.version, l.minApp = clip(r.Header.Get("X-Zipline-Ext-Version"), 20), clip(r.Header.Get("X-Zipline-Min-App"), 20)
}

// noteUnlinked: an extension knocked without a key; one without a version is older than the link.
func (s *Server) noteUnlinked(r *http.Request) {
	if !extOrigin(r.Header.Get("Origin")) {
		return
	}
	sec := s.security()
	sec.mu.Lock()
	defer sec.mu.Unlock()
	if v := r.Header.Get("X-Zipline-Ext-Version"); v == "" {
		sec.link.oldSeen = sec.now()
	} else {
		sec.link.pairSeen = sec.now()
		sec.link.version, sec.link.minApp = clip(v, 20), clip(r.Header.Get("X-Zipline-Min-App"), 20)
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---------------------------------------------------------------- pairing

// extPairGate lets in extensions only (pairing is for browsers), without a key.
func (s *Server) extPairGate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		o := r.Header.Get("Origin")
		if !extOrigin(o) {
			http.Error(w, "forbidden", 403)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", o)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Zipline-Ext-Version, X-Zipline-Min-App")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Expose-Headers", "X-Zipline-Version, X-Zipline-Min-Ext")
		w.Header().Set("Vary", "Origin")
		w.Header().Set("X-Zipline-Version", appVersion)
		w.Header().Set("X-Zipline-Min-Ext", minExtVersion)
		if r.Method == "OPTIONS" {
			return
		}
		h(w, r)
	}
}

// extPair: {version, browser} asks to pair (the window asks you); {id} asks how it went. The key is
// handed out once, to the asker (only it knows the request's id).
func (s *Server) extPair(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Browser string `json:"browser"`
	}
	if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	origin := r.Header.Get("Origin")
	sec := s.security()
	sec.mu.Lock()
	l := &sec.link
	now := sec.now()
	for id, a := range l.asks {
		if now.Sub(a.Made) > pairAskLife {
			delete(l.asks, id)
		}
	}
	if req.ID != "" {
		a := l.asks[req.ID]
		res := map[string]any{"state": "expired"}
		if a != nil && a.Origin == origin {
			res["state"] = a.State
			switch a.State {
			case "allowed":
				res["pair"], res["key"] = a.Pair.ID, a.Pair.Key
				delete(l.asks, req.ID)
			case "denied":
				delete(l.asks, req.ID)
			}
		}
		sec.mu.Unlock()
		writeJSON(w, res)
		return
	}
	if now.Sub(l.deniedAt) < 30*time.Second {
		sec.mu.Unlock()
		writeJSON(w, map[string]any{"state": "denied"})
		return
	}
	for _, a := range l.asks {
		if a.State == "pending" { // one at a time (the asker keeps its id; nobody else gets it)
			sec.mu.Unlock()
			writeJSON(w, map[string]any{"state": "busy"})
			return
		}
	}
	a := &pairAsk{ID: randHex(16), Origin: origin, Browser: clip(req.Browser, 60), Version: clip(req.Version, 20), Made: now, State: "pending"}
	l.asks[a.ID] = a
	sec.mu.Unlock()
	if s.m != nil {
		s.m.AddPrompt(Prompt{Kind: "pair", JobID: a.ID, URL: origin, Title: a.Browser, Name: a.Version, Source: "browser"})
		go bringToFront()
	}
	writeJSON(w, map[string]any{"id": a.ID, "state": "pending"})
}

// pairingH is the window's side: GET lists the paired browsers; POST {id, allow} answers a request,
// {remove: id} forgets a browser ("all" forgets every one).
func (s *Server) pairingH(w http.ResponseWriter, r *http.Request) {
	sec := s.security()
	if r.Method == "POST" {
		var req struct {
			ID     string `json:"id"`
			Allow  bool   `json:"allow"`
			Remove string `json:"remove"`
		}
		if readJSON(r, &req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		sec.mu.Lock()
		l := &sec.link
		pairs := s.pairsLocked()
		switch {
		case req.Remove != "":
			keep := pairs[:0]
			for _, p := range pairs {
				if req.Remove != "all" && p.ID != req.Remove {
					keep = append(keep, p)
				}
			}
			l.pairs = keep
			s.savePairsLocked()
		case req.ID != "":
			a := l.asks[req.ID]
			if a == nil || a.State != "pending" || sec.now().Sub(a.Made) > pairAskLife {
				sec.mu.Unlock()
				writeJSON(w, map[string]any{"error": "that request has expired; ask again from the browser"})
				return
			}
			if req.Allow {
				a.State = "allowed"
				a.Pair = &extPair{ID: randHex(8), Key: randHex(32), Origin: a.Origin, Browser: a.Browser, Added: sec.now()}
				l.pairs = append(pairs, a.Pair)
				s.savePairsLocked()
			} else {
				a.State, l.deniedAt = "denied", sec.now()
			}
		}
		sec.mu.Unlock()
	}
	writeJSON(w, map[string]any{"ok": true, "pairs": s.pairList()})
}

func (s *Server) pairList() []map[string]any {
	sec := s.security()
	sec.mu.Lock()
	defer sec.mu.Unlock()
	out := []map[string]any{}
	for _, p := range s.pairsLocked() {
		x := map[string]any{"id": p.ID, "browser": p.Browser, "origin": p.Origin, "added": p.Added}
		if !p.Seen.IsZero() {
			x["seen"] = p.Seen
		}
		out = append(out, x)
	}
	return out
}

// ---------------------------------------------------------------- status for Options → Browser

func (s *Server) linkStatusH(w http.ResponseWriter, r *http.Request) {
	sec := s.security()
	sec.mu.Lock()
	l := &sec.link
	now := sec.now()
	recent := func(t time.Time) bool { return !t.IsZero() && now.Sub(t) < extRecent }
	state := "none"
	switch {
	case l.waiting > 0 || l.via == "native" && recent(l.seen):
		state = "native"
	case l.via == "http" && recent(l.seen):
		state = "http"
	case recent(l.oldSeen):
		state = "old"
	case recent(l.pairSeen):
		state = "unpaired"
	}
	ext := map[string]any{"version": l.version, "minApp": l.minApp, "via": l.via, "origin": l.origin}
	if !l.seen.IsZero() {
		ext["seen"] = l.seen
	}
	extOld := state == "old" || l.version != "" && versionLess(l.version, minExtVersion)
	appOld := l.minApp != "" && versionLess(appVersion, l.minApp)
	sec.mu.Unlock()
	// port: where Zipline listens; usual: where it wanted to (another program had it when they differ)
	writeJSON(w, map[string]any{"state": state, "port": s.port, "usual": appPort, "app": appVersion, "minExt": minExtVersion,
		"ext": ext, "extOld": extOld, "appOld": appOld, "pairs": s.pairList()})
}

// versionLess compares dotted versions by number ("1.6.2" < "1.10"); missing parts count as 0.
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(strings.TrimSpace(as[i]))
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(strings.TrimSpace(bs[i]))
		}
		if x != y {
			return x < y
		}
	}
	return false
}

// ---------------------------------------------------------------- wake-ups for the native host

// The native host long-polls /api/ext/wait and tells the extension to check in at once when the app
// has something for it (pages to open for a fresh link or a scan, new settings), instead of at its
// next 30-second ping. Each wake-up has a number; the host sends the last one it saw, so one that
// came between two polls still reaches it.
var wakeHub = struct {
	sync.Mutex
	ch  chan struct{}
	gen int64
}{ch: make(chan struct{})}

// wakeExtension tells waiting hosts that the extension should check in now.
func wakeExtension() {
	wakeHub.Lock()
	close(wakeHub.ch)
	wakeHub.ch = make(chan struct{})
	wakeHub.gen++
	wakeHub.Unlock()
}

func wakeState() (<-chan struct{}, int64) {
	wakeHub.Lock()
	defer wakeHub.Unlock()
	return wakeHub.ch, wakeHub.gen
}

// extPending lists what waits for the extension's ping: pages for a fresh link, scan pages to collect.
func (s *Server) extPending() map[string]bool {
	out := map[string]bool{}
	m := s.m
	if m == nil {
		return out
	}
	m.mu.Lock()
	for id, j := range m.refresh {
		if j.taken.IsZero() {
			out["r"+id] = true
		}
	}
	m.mu.Unlock()
	if g := m.grab; g != nil {
		g.mu.Lock()
		for _, j := range g.jobs {
			j.mu.Lock()
			for _, c := range j.scan.collects {
				if c.State == "waiting" && c.taken.IsZero() {
					out["g"+c.ID] = true
				}
			}
			j.mu.Unlock()
		}
		g.mu.Unlock()
	}
	return out
}

// notePending wakes the extension when something it hasn't been told about waits for it.
func (s *Server) notePending() {
	now := s.extPending()
	sec := s.security()
	sec.mu.Lock()
	l := &sec.link
	fresh := false
	for k := range now {
		if !l.announced[k] {
			fresh = true
		}
	}
	l.announced = now
	sec.mu.Unlock()
	if fresh {
		wakeExtension()
	}
}

// extWait answers {"wake": true, "gen"} as soon as there's something new for the extension (or a
// wake-up after gen), else {"wake": false, "gen"} after t seconds (25 by default).
func (s *Server) extWait(w http.ResponseWriter, r *http.Request) {
	t, _ := strconv.Atoi(r.URL.Query().Get("t"))
	if t <= 0 || t > 55 {
		t = 25
	}
	since, err := strconv.ParseInt(r.URL.Query().Get("gen"), 10, 64)
	if err != nil {
		since = -1
	}
	if r.Header.Get("X-Zipline-Via") == "native" {
		sec := s.security()
		sec.mu.Lock()
		sec.link.waiting++
		sec.mu.Unlock()
		defer func() {
			sec.mu.Lock()
			sec.link.waiting--
			sec.link.seen = sec.now() // (connected until now)
			sec.mu.Unlock()
		}()
	}
	ch, gen := wakeState()
	if since >= 0 && since != gen {
		writeJSON(w, map[string]any{"wake": true, "gen": gen})
		return
	}
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	end := time.NewTimer(time.Duration(t) * time.Second)
	defer end.Stop()
	for {
		select {
		case <-ch:
			_, gen = wakeState()
			writeJSON(w, map[string]any{"wake": true, "gen": gen})
			return
		case <-tick.C:
			s.notePending()
		case <-end.C:
			writeJSON(w, map[string]any{"wake": false, "gen": gen})
			return
		case <-r.Context().Done():
			return
		}
	}
}
