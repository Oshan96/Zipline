//go:build !windows

package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Fetching and running PAC scripts on macOS and Linux (Windows has WinHTTP do it).

// wpadURL is where auto-detect looks: the "wpad" host in the local DNS domain.
var wpadURL = "http://wpad/wpad.dat"

const (
	pacMaxSize    = 1 << 20
	pacScriptTTL  = 30 * time.Minute
	pacRetryAfter = time.Minute // after a failed fetch
	pacResultTTL  = 5 * time.Minute
)

// pacClient fetches scripts directly: asking the proxy settings how to reach the file
// that defines them would go round in circles.
var pacClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second},
}

var pacCache struct {
	mu      sync.Mutex
	scripts map[string]*pacScript // by PAC URL
	results map[string]pacResult  // by PAC URL + " " + scheme://host
}

type pacScript struct {
	text string
	at   time.Time     // of the last fetch
	ok   bool          // whether it worked
	done chan struct{} // set while a fetch runs; closed when it ends
}

type pacResult struct {
	list string
	ok   bool
	at   time.Time
}

func pacReset() {
	pacCache.mu.Lock()
	pacCache.scripts, pacCache.results = nil, nil
	pacCache.mu.Unlock()
}

// pacFor runs the PAC script at pacURL for u. ok is false when the script can't be had or
// fails, and the caller falls back to the static settings. Answers are kept per host.
func pacFor(pacURL string, u *url.URL, wait bool) (list string, ok bool) {
	key := pacURL + " " + u.Scheme + "://" + u.Host
	pacCache.mu.Lock()
	r, hit := pacCache.results[key]
	pacCache.mu.Unlock()
	if hit && time.Since(r.at) < pacResultTTL {
		return r.list, r.ok
	}
	script := pacScriptFor(pacURL, wait)
	if script == "" {
		return "", false
	}
	t := *u
	t.User, t.Fragment, t.RawFragment = nil, "", ""
	list, err := evalPAC(script, t.String(), strings.ToLower(u.Hostname()))
	r = pacResult{list, err == nil, time.Now()}
	pacCache.mu.Lock()
	if pacCache.results == nil || len(pacCache.results) >= 1024 {
		pacCache.results = map[string]pacResult{}
	}
	pacCache.results[key] = r
	pacCache.mu.Unlock()
	return r.list, r.ok
}

// pacScriptFor returns the script at pacURL ("" if there's none yet). A stale copy is
// used while a fresh one downloads in the background; with nothing in hand, wait says
// whether to wait for the download.
func pacScriptFor(pacURL string, wait bool) string {
	pacCache.mu.Lock()
	if pacCache.scripts == nil {
		pacCache.scripts = map[string]*pacScript{}
	}
	s := pacCache.scripts[pacURL]
	if s == nil {
		s = &pacScript{}
		pacCache.scripts[pacURL] = s
	}
	ttl := pacScriptTTL
	if !s.ok {
		ttl = pacRetryAfter
	}
	if s.done == nil && time.Since(s.at) > ttl {
		s.done = make(chan struct{})
		go pacRefresh(pacURL, s)
	}
	text, done := s.text, s.done
	pacCache.mu.Unlock()
	if text == "" && done != nil && wait {
		<-done
		pacCache.mu.Lock()
		text = s.text
		pacCache.mu.Unlock()
	}
	return text
}

func pacRefresh(pacURL string, s *pacScript) {
	text, err := fetchPAC(pacURL)
	pacCache.mu.Lock()
	defer pacCache.mu.Unlock()
	s.at, s.ok = time.Now(), err == nil
	if err == nil && text != s.text { // (a failed refresh keeps the old script)
		s.text = text
		pacCache.results = nil // answers from the old script are out of date
	}
	close(s.done)
	s.done = nil
}

// fetchPAC downloads a PAC script from an http(s):// or file:// URL.
func fetchPAC(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	var body io.Reader
	switch strings.ToLower(u.Scheme) {
	case "file":
		f, err := os.Open(u.Path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		body = f
	case "http", "https":
		resp, err := pacClient.Get(raw)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("PAC file: %s", resp.Status)
		}
		body = resp.Body
	default:
		return "", fmt.Errorf("unsupported PAC address %q", raw)
	}
	b, err := io.ReadAll(io.LimitReader(body, pacMaxSize+1))
	if err != nil {
		return "", err
	}
	if len(b) > pacMaxSize {
		return "", errors.New("PAC file is larger than 1 MB")
	}
	return string(b), nil
}
