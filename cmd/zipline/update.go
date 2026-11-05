package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Checking for a newer Zipline (asked once in the window, then Options → General): once a day Zipline
// asks GitHub for its latest release, and says when there's a newer one. Nothing about this computer
// goes with the request. An installed Windows copy updates itself from the window (Update now): the
// installer is downloaded from the release, checked against the SHA-256 GitHub gives for it, and run
// silently, and it starts the new Zipline. Any other copy downloads its package into the list.

type UpdateSettings struct {
	Check string `json:"check"` // "": not asked yet, "on", "off"
}

var (
	updateAPI  = "https://api.github.com/repos/Oshan96/Zipline/releases/latest" // replaced in tests
	updateFrom = "https://github.com/Oshan96/Zipline/releases/download/"        // updates come from here only; replaced in tests
	updatePage = "https://github.com/Oshan96/Zipline/releases/"                 // a release's page
)

// UpdateView is what the window shows: whether there's a newer version and how to get it.
type UpdateView struct {
	Current string   `json:"current"`
	Version string   `json:"version,omitempty"` // a newer version ("" when this one is the newest)
	Notes   []string `json:"notes,omitempty"`   // what changed in it, one line each
	Page    string   `json:"page,omitempty"`    // its release page
	Asset   string   `json:"asset,omitempty"`   // the file for this copy ("" none: the page has the others)
	URL     string   `json:"url,omitempty"`
	Install bool     `json:"install,omitempty"` // this copy installs it itself
	Checked int64    `json:"checked,omitempty"` // when it last asked (Unix ms)
	Error   string   `json:"error,omitempty"`   // why that check failed
	State   string   `json:"state,omitempty"`   // updating: "downloading", "installing", "failed"
	Pct     int      `json:"pct,omitempty"`

	digest string // "sha256:…", as GitHub gives it
	size   int64
}

type updater struct {
	view UpdateView
	told string // the version a notice was given for
	busy bool
}

// updateLoop checks a minute after Zipline starts, then once a day, while checking is on.
func (m *Manager) updateLoop() {
	os.RemoveAll(filepath.Join(m.dataDir, "update")) // (an installer that has done its work)
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.quit:
			return
		case <-t.C:
		}
		m.mu.Lock()
		due := m.settings.Updates.Check == "on" && time.Since(time.UnixMilli(m.upd.view.Checked)) > 23*time.Hour
		m.mu.Unlock()
		if due {
			m.CheckUpdate(context.Background())
		}
		t.Reset(time.Hour)
	}
}

// updateView is the window's copy. Caller holds m.mu.
func (m *Manager) updateView() *UpdateView {
	if m.upd.view.Checked == 0 && m.upd.view.State == "" {
		return nil
	}
	v := m.upd.view
	return &v
}

// CheckUpdate asks GitHub now (Check now, Help → Check for updates, and the daily check).
func (m *Manager) CheckUpdate(ctx context.Context) UpdateView {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	v, err := fetchUpdate(ctx)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.upd.view.Current, m.upd.view.Checked, m.upd.view.Error = appVersion, time.Now().UnixMilli(), err.Error()
		return m.upd.view
	}
	if m.upd.busy { // (an update under way keeps showing)
		v.State, v.Pct = m.upd.view.State, m.upd.view.Pct
	}
	m.upd.view = v
	if v.Version != "" && m.upd.told != v.Version {
		m.upd.told = v.Version
		m.notify("update", "", "Zipline "+v.Version+" is available")
	}
	return v
}

type ghRelease struct {
	Tag    string `json:"tag_name"`
	Page   string `json:"html_url"`
	Body   string `json:"body"`
	Draft  bool   `json:"draft"`
	Pre    bool   `json:"prerelease"`
	Assets []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

var releaseTag = regexp.MustCompile(`^v?(\d+(?:\.\d+){1,3})$`)

func fetchUpdate(ctx context.Context) (UpdateView, error) {
	v := UpdateView{Current: appVersion, Checked: time.Now().UnixMilli()}
	req, err := http.NewRequestWithContext(ctx, "GET", updateAPI, nil)
	if err != nil {
		return v, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Zipline") // (GitHub wants one; nothing more is sent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return v, fmt.Errorf("couldn't reach GitHub: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return v, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var r ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return v, fmt.Errorf("GitHub's answer couldn't be read: %v", err)
	}
	tag := releaseTag.FindStringSubmatch(r.Tag)
	if tag == nil || r.Draft || r.Pre || !versionLess(appVersion, tag[1]) {
		return v, nil // the newest already
	}
	v.Version, v.Notes = tag[1], releaseNotes(r.Body)
	if strings.HasPrefix(r.Page, updatePage) {
		v.Page = r.Page
	}
	name, install := updateAsset()
	for _, a := range r.Assets {
		if name != "" && a.Name == name && a.URL == updateFrom+r.Tag+"/"+name {
			v.Asset, v.URL, v.size, v.digest = a.Name, a.URL, a.Size, a.Digest
			v.Install = install && strings.HasPrefix(a.Digest, "sha256:") // (never one that can't be checked)
		}
	}
	return v, nil
}

var noteBy = regexp.MustCompile(`\s+by @[\w-]+ in https?://\S+$`)

// releaseNotes reads a release's "What's Changed" list: one line per change, without its author and link.
func releaseNotes(body string) []string {
	var out []string
	in := false
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(l, "## "):
			in = strings.EqualFold(l, "## What's Changed")
		case in && (strings.HasPrefix(l, "* ") || strings.HasPrefix(l, "- ")) && len(out) < 12:
			n := noteBy.ReplaceAllString(strings.TrimSpace(l[2:]), "")
			if len(n) > 200 {
				n = n[:200] + "…"
			}
			out = append(out, n)
		}
	}
	return out
}

// updateAsset is the release file for this copy of Zipline, and whether it installs itself.
var updateAsset = func() (name string, install bool) {
	exe, _ := os.Executable()
	switch runtime.GOOS {
	case "windows":
		if _, err := os.Stat(filepath.Join(filepath.Dir(exe), "Uninstall.exe")); err == nil { // (the installer's copy)
			return "Zipline-Windows-Setup.exe", true
		}
		if runtime.GOARCH == "arm64" {
			return "Zipline-Windows-arm64.zip", false
		}
		return "Zipline-Windows-x64.zip", false
	case "darwin":
		return "Zipline-macOS.dmg", false
	case "linux":
		if runtime.GOARCH != "amd64" {
			return "", false
		}
		if exe == "/usr/bin/zipline" { // installed from a package: the same kind
			rel, _ := os.ReadFile("/etc/os-release")
			s := strings.ToLower(string(rel))
			switch {
			case strings.Contains(s, "debian") || strings.Contains(s, "ubuntu"):
				return "Zipline-Linux-x64.deb", false
			case strings.Contains(s, "fedora") || strings.Contains(s, "rhel") || strings.Contains(s, "suse"):
				return "Zipline-Linux-x64.rpm", false
			}
		}
		return "Zipline-Linux-x64.tar.gz", false
	}
	return "", false
}

// InstallUpdate downloads the installer, checks it and runs it, then quits so the installer can put the
// new version in its place; the installer starts it. Only the window may ask for this (server.go).
func (m *Manager) InstallUpdate() error {
	m.mu.Lock()
	v := m.upd.view
	if m.upd.busy {
		m.mu.Unlock()
		return errors.New("Zipline is already updating")
	}
	if !v.Install || v.URL == "" {
		m.mu.Unlock()
		return errors.New("this copy of Zipline doesn't update itself: download the new version instead")
	}
	m.upd.busy = true
	m.upd.view.State, m.upd.view.Pct, m.upd.view.Error = "downloading", 0, ""
	m.mu.Unlock()
	go func() {
		err := m.fetchAndRun(v)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.upd.busy = false
		if err != nil {
			m.upd.view.State, m.upd.view.Error = "failed", err.Error()
			m.notify("updateErr", "", "Couldn't update Zipline: "+err.Error())
		}
	}()
	return nil
}

func (m *Manager) fetchAndRun(v UpdateView) error {
	dir := filepath.Join(m.dataDir, "update")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(dir, v.Asset)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", v.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Zipline")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't download the installer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("couldn't download the installer: GitHub answered %s", resp.Status)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, 256<<10)
	var n int64
	for {
		k, rerr := resp.Body.Read(buf)
		if k > 0 {
			f.Write(buf[:k])
			h.Write(buf[:k])
			n += int64(k)
			if v.size > 0 {
				m.mu.Lock()
				m.upd.view.Pct = int(min(n*100/v.size, 100))
				m.mu.Unlock()
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(p)
			return fmt.Errorf("couldn't download the installer: %v", rerr)
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, v.digest) || v.size > 0 && n != v.size {
		os.Remove(p)
		return errors.New("the downloaded installer doesn't match the checksum GitHub gives for it, so it wasn't run")
	}
	m.mu.Lock()
	m.upd.view.State, m.upd.view.Pct = "installing", 100
	m.mu.Unlock()
	if err := runInstaller(p); err != nil {
		return fmt.Errorf("couldn't start the installer: %v", err)
	}
	time.Sleep(500 * time.Millisecond) // (the window hears "installing" first)
	m.Quit()
	return nil
}

// runInstaller starts the installer on its own (it outlives Zipline): silent, starting Zipline again
// when it's done (installer.nsi's /RUN). Replaced in tests.
var runInstaller = func(p string) error {
	cmd := exec.Command(p, "/S", "/RUN")
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// updateH: check now, install (the window only: it runs a program), or open the release's page.
func (s *Server) updateH(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		s.m.mu.Lock()
		v := s.m.updateView()
		s.m.mu.Unlock()
		writeJSON(w, v)
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	readJSON(r, &req)
	switch req.Action {
	case "check":
		writeJSON(w, s.m.CheckUpdate(r.Context()))
	case "install":
		if authOf(r) != authWindow {
			http.Error(w, "Zipline can only be updated from its window", 403)
			return
		}
		if err := s.m.InstallUpdate(); err != nil {
			writeJSON(w, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case "page":
		s.m.mu.Lock()
		p := s.m.upd.view.Page
		s.m.mu.Unlock()
		if p == "" {
			p = updatePage + "latest"
		}
		openPath(p)
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "unknown action", 400)
	}
}
