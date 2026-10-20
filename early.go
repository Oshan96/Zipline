package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Early start: like IDM, the download begins as soon as the Add dialog has read the file's
// details, while you're still choosing where to save it. Confirming keeps the progress (the
// partial file moves if you pick another folder or name); cancelling deletes it.

// earlyTimeout drops a provisional download whose dialog stopped checking in (window closed).
var earlyTimeout = 45 * time.Second

// startEarly creates and starts a hidden download for a file the Add dialog is showing (more sets
// what else it has: a form's request, a private window).
func (m *Manager) startEarly(url string, headers map[string]string, name, dir string, size int64, finalURL, etag, lastMod string, more ...func(*Download)) *Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := &Download{
		ID: newID(), URL: url, Size: size, Added: time.Now(), Conns: m.settings.Conns, Headers: headers,
		Status: StPaused, FinalURL: finalURL, ETag: etag, LastMod: lastMod,
		Provisional: true, keepAlive: time.Now(),
	}
	d.FileName = sanitizeName(name)
	d.Category = m.categoryFor(d.FileName)
	d.Dir = dir
	if d.Dir == "" {
		d.Dir = m.dirFor(d.Category)
	}
	d.FileName = m.uniqueName(d.Dir, d.FileName, d)
	for _, f := range more {
		f(d)
	}
	m.list = append(m.list, d)
	m.byID[d.ID] = d
	m.startLocked(d)
	return d
}

func (m *Manager) keepEarly(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.byID[id]
	if d == nil || !d.Provisional {
		return false
	}
	d.keepAlive = time.Now()
	return true
}

// dropStaleEarly removes provisional downloads whose dialog is gone. Called from the loop.
func (m *Manager) dropStaleEarly() {
	m.mu.Lock()
	var stale []string
	for _, d := range m.list {
		if d.Provisional && time.Since(d.keepAlive) > earlyTimeout {
			stale = append(stale, d.ID)
		}
	}
	m.mu.Unlock()
	if len(stale) > 0 {
		m.Remove(stale, true)
	}
}

// dropAllEarly discards every provisional download and its file (on exit), and what private
// downloads left unfinished (they aren't saved, so nothing could carry them on).
func (m *Manager) dropAllEarly() {
	m.mu.Lock()
	var early []*Download
	for _, d := range m.list {
		if d.Provisional || d.Private && d.Status != StCompleted {
			early = append(early, d)
		}
	}
	m.mu.Unlock()
	for _, d := range early {
		m.waitStopped(d)
		d.removeParts()
		if d.Status == StCompleted {
			os.Remove(d.path())
		}
	}
	m.mu.Lock()
	out := m.list[:0]
	for _, d := range m.list {
		if !(d.Provisional || d.Private && d.Status != StCompleted) {
			out = append(out, d)
		} else {
			delete(m.byID, d.ID)
		}
	}
	m.list = out
	m.mu.Unlock()
}

// waitStopped cancels a download and waits for its connections to finish.
func (m *Manager) waitStopped(d *Download) {
	m.mu.Lock()
	cancel := d.cancel
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for i := 0; i < 1000; i++ {
		m.mu.Lock()
		r := d.running
		m.mu.Unlock()
		if !r {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// moveFile renames, or copies and deletes when the folders are on different drives.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if os.IsNotExist(err) {
		return nil // nothing written yet
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		in.Close()
		return err
	}
	_, err = io.Copy(out, in)
	in.Close()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

type earlyCommit struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Dir      string   `json:"dir"`
	Desc     string   `json:"desc"`
	Start    bool     `json:"start"`
	QueueID  string   `json:"queueId"`
	Checksum string   `json:"checksum"`
	Mirrors  []string `json:"mirrors"` // used from the next start (it's already running)
}

var errEarlyGone = errors.New("this download is no longer waiting for confirmation")

// commitEarly turns a provisional download into a normal one with the dialog's choices.
func (m *Manager) commitEarly(c earlyCommit) (*Download, error) {
	m.mu.Lock()
	d := m.byID[c.ID]
	if d == nil || !d.Provisional {
		m.mu.Unlock()
		return nil, errEarlyGone
	}
	name := sanitizeName(strings.TrimSpace(c.Name))
	if name == "" {
		name = d.FileName
	}
	dir := strings.TrimSpace(c.Dir)
	if dir == "" {
		dir = d.Dir
	}
	d.Desc = c.Desc
	d.keepAlive = time.Now()
	if ck, err := parseChecksum(c.Checksum, name); err == nil && ck != nil {
		d.Check = ck // checked when it finishes
	}
	if ms := cleanMirrors(c.Mirrors, d.URL); len(ms) > 0 {
		d.Mirrors = ms
	}
	moving := name != d.FileName || filepath.Clean(dir) != filepath.Clean(d.Dir)
	q := m.queueByID(c.QueueID)
	if !moving && c.Start {
		d.Provisional = false // keeps running as it is
		if d.Status == StError {
			m.startLocked(d)
		}
		if ck := d.Check; d.Status == StCompleted && ck != nil && ck.Want != "" && ck.State == "" {
			go m.checkFinished(d)
		}
		if d.announce {
			m.announceDone(d)
		}
		m.dirty = true
		m.mu.Unlock()
		return d, nil
	}
	m.mu.Unlock()

	m.waitStopped(d) // pause it so the partial file can move
	m.mu.Lock()
	defer func() {
		m.dirty = true
		m.mu.Unlock()
		m.kick()
	}()
	if moving {
		oldPart, oldFinal := d.partPath(), d.path()
		done := d.Status == StCompleted
		d.Dir = dir
		d.FileName = m.uniqueName(dir, name, d)
		d.Category = m.categoryFor(d.FileName)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		src, dst := oldPart, d.partPath()
		if done {
			src, dst = oldFinal, d.path()
		}
		if err := moveFile(src, dst); err != nil {
			return nil, err
		}
	}
	d.Provisional = false
	switch {
	case d.Status == StCompleted:
		if c := d.Check; c != nil && c.Want != "" && c.State == "" {
			go m.checkFinished(d) // finished before the checksum was given
		}
		if d.announce {
			m.announceDone(d)
		}
	case c.Start:
		m.startLocked(d)
	case q != nil:
		d.Queue, d.Status = q.ID, StQueued
	default:
		d.Status = StPaused
	}
	return d, nil
}
