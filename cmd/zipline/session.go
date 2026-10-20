package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zipline/web"
)

// The window's sign-in. The page on 127.0.0.1 used to carry the token for anyone who asked; now
// Zipline opens its window with a one-time code (login?c=…, good for 60 seconds, once) that the
// server swaps for a session cookie: HttpOnly (scripts can't read it), SameSite=Strict (other sites
// can't send it). Without the cookie the page only says to open Zipline from its icon or menu.
// A request with the cookie must also come from the window's own origin, and anything that changes
// something carries the X-Zipline header, which a form on another site can't send.
//
// Programs on this computer (the command line, the native-messaging host, tests) use the secret from
// run.json instead (X-Token). The browser extension has its own paths (extlink.go).

var lockedHTML = web.LockedHTML

var extLinkJS = web.ExtLinkJS

const (
	sessionCookie = "zl_session"
	loginCodeLife = 60 * time.Second

	authWindow = "window" // the window's session cookie
	authSecret = "secret" // run.json's secret: the command line, the native host, tests
	authPaired = "paired" // a paired browser extension over HTTP (extlink.go)
)

type authKey struct{}

// authOf says how a request was let in.
func authOf(r *http.Request) string {
	k, _ := r.Context().Value(authKey{}).(string)
	return k
}

func withAuth(r *http.Request, kind string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authKey{}, kind))
}

// serverSec is the server's sign-in state: window codes and sessions, and the extension link.
type serverSec struct {
	mu       sync.Mutex
	now      func() time.Time
	codes    map[string]time.Time // one-time window codes → when they stop working
	sessions map[string]time.Time // window sessions → when they started
	link     extLinkState         // pairing and what the extension last said (extlink.go)
}

func (s *Server) security() *serverSec {
	s.secOnce.Do(func() {
		s.sec = &serverSec{now: time.Now, codes: map[string]time.Time{}, sessions: map[string]time.Time{}}
		s.sec.link.init()
	})
	return s.sec
}

// theServer is the app's server, for opening the window from the tray and a second launch.
var theServer *Server

// loginURL is a fresh one-time address that signs a window in.
func loginURL() string {
	if theServer == nil {
		return appURL
	}
	return theServer.loginURL()
}

func (s *Server) loginURL() string {
	sec := s.security()
	code := randHex(24)
	sec.mu.Lock()
	now := sec.now()
	for c, until := range sec.codes {
		if now.After(until) {
			delete(sec.codes, c)
		}
	}
	sec.codes[code] = now.Add(loginCodeLife)
	sec.mu.Unlock()
	return appURL + "login?c=" + code
}

// takeCode uses up a window code; false when it's unknown, used or too old.
func (s *Server) takeCode(code string) bool {
	if code == "" {
		return false
	}
	sec := s.security()
	sec.mu.Lock()
	defer sec.mu.Unlock()
	until, ok := sec.codes[code]
	delete(sec.codes, code)
	return ok && !sec.now().After(until)
}

func (s *Server) windowSession(r *http.Request) bool {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return false
	}
	sec := s.security()
	sec.mu.Lock()
	defer sec.mu.Unlock()
	_, ok := sec.sessions[c.Value]
	return ok
}

// cookieName is the session cookie's name: browsers keep cookies by host, not port, so a second
// Zipline on this computer (another user's, on another port) would otherwise replace this one's.
func (s *Server) cookieName() string {
	if s.port == 0 {
		return sessionCookie
	}
	return sessionCookie + "_" + strconv.Itoa(s.port)
}

// login swaps a window code for a session cookie and goes on to the window.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "GET only", 405)
		return
	}
	if s.takeCode(r.URL.Query().Get("c")) {
		id := randHex(32)
		sec := s.security()
		sec.mu.Lock()
		if len(sec.sessions) >= 64 { // (old windows' sessions; a window lasts until the app quits)
			oldest, when := "", time.Time{}
			for k, t := range sec.sessions {
				if oldest == "" || t.Before(when) {
					oldest, when = k, t
				}
			}
			delete(sec.sessions, oldest)
		}
		sec.sessions[id] = sec.now()
		sec.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if s.windowSession(r) { // a used code reloaded in a window that's signed in
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.lockedPage(w, http.StatusForbidden)
}

// lockedPage is all a browser gets without a session: how to open the window. No token, no data.
func (s *Server) lockedPage(w http.ResponseWriter, code int) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	pageHeaders(h)
	w.WriteHeader(code)
	io.WriteString(w, lockedPage)
}

var lockedPage = withColors(lockedHTML)

// pageHeaders keep the window's pages out of other sites' frames and their addresses out of Referers.
func pageHeaders(h http.Header) {
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("Referrer-Policy", "no-referrer")
}

// hostOK is the DNS-rebinding check: only Zipline's own loopback address and port (any port in tests,
// where the server doesn't know it).
func (s *Server) hostOK(hostport string) bool {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, ""
	}
	if host != "127.0.0.1" && host != "localhost" {
		return false
	}
	return s.port == 0 || port == strconv.Itoa(s.port)
}

// authorize lets in the secret (programs on this computer) or the window's session from its own page.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (string, bool) {
	t := r.Header.Get("X-Token")
	if t == "" {
		t = r.URL.Query().Get("token")
	}
	if t != "" && s.token != "" && hmac.Equal([]byte(t), []byte(s.token)) {
		if n := r.Header.Get("X-Zipline-Nonce"); n != "" { // (the program checks it reached Zipline)
			w.Header().Set("X-Zipline-Proof", linkProof(s.token, n))
		}
		return authSecret, true
	}
	if !s.windowSession(r) {
		http.Error(w, "unauthorized", 401)
		return "", false
	}
	if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
		http.Error(w, "forbidden", 403)
		return "", false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" || site == "same-site" {
		http.Error(w, "forbidden", 403)
		return "", false
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-Zipline") != "1" {
		http.Error(w, "forbidden", 403)
		return "", false
	}
	return authWindow, true
}

// windowOnly: for the window itself (letting a browser in, …), never a program with the secret.
func (s *Server) windowOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authOf(r) != authWindow {
			http.Error(w, "only in Zipline's window", 403)
			return
		}
		h(w, r)
	}
}

// secretOnly: for programs of this user (the command line), never the window or an extension.
func (s *Server) secretOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authOf(r) != authSecret || r.Header.Get("X-Zipline-Via") == "native" {
			http.Error(w, "forbidden", 403)
			return
		}
		h(w, r)
	}
}

// guardSettings: a change to the program Zipline runs on every finished download (the virus
// scanner and its arguments) is taken only from the window, which asks first, never from a program
// with the secret. After a save the extension is woken to pick up its settings.
func (s *Server) guardSettings(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			h(w, r)
			return
		}
		b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		var st struct {
			Scan ScanSettings `json:"scan"`
		}
		json.Unmarshal(b, &st)
		s.m.mu.Lock()
		cur := s.m.settings.Scan
		s.m.mu.Unlock()
		changed := strings.TrimSpace(st.Scan.Program) != strings.TrimSpace(cur.Program) || strings.TrimSpace(st.Scan.Args) != strings.TrimSpace(cur.Args)
		if changed && authOf(r) != authWindow {
			http.Error(w, "the virus-scan program can only be changed in Zipline's window", 403)
			return
		}
		h(w, r)
		wakeExtension()
	}
}

// newLogin is `zipline --login-url`: a fresh window address for a program with the secret.
func (s *Server) newLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	writeJSON(w, map[string]any{"url": s.loginURL()})
}

// linkRoutes adds the sign-in and the extension link's routes.
func (s *Server) linkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/login", s.login)
	mux.HandleFunc("/extlink.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, extLinkJS)
	})
	mux.HandleFunc("/api/login/new", s.auth(s.secretOnly(s.newLogin)))
	mux.HandleFunc("/api/pairing", s.auth(s.windowOnly(s.pairingH)))
	mux.HandleFunc("/api/linkstatus", s.auth(s.linkStatusH))
	mux.HandleFunc("/api/ext/pair", s.extPairGate(s.extPair))
	mux.HandleFunc("/api/ext/wait", s.ext(s.extWait))
}
