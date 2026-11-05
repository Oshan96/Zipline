package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zipline/internal/unpack"
)

// Unpacking archives when they finish (Options → File checks, or one download's Properties): a .zip
// or .tar (.tar.gz, .tar.bz2) download is unpacked into a folder next to it, once it has been checked
// and, with the virus scan on, scanned. internal/unpack does the unpacking itself.

type UnpackSettings struct {
	On     bool `json:"on"`
	Delete bool `json:"delete"` // the archive goes once it's unpacked
}

var unpackSem = make(chan struct{}, 1) // one at a time: they're disk-bound

// unpackable: a finished download that is an archive (not a stream's file or a torrent, whose files
// are shared as they are). Caller holds m.mu.
func unpackable(d *Download) bool {
	return d.Status == StCompleted && d.Torrent == nil && d.Kind == "" && unpack.Kind(d.FileName) != ""
}

// shouldUnpack: the download's own choice first, then the setting. Caller holds m.mu.
func (m *Manager) shouldUnpack(d *Download) bool {
	if !unpackable(d) {
		return false
	}
	switch d.Unpack {
	case "on":
		return true
	case "off":
		return false
	}
	return m.settings.Unpack.On
}

// queueUnpack marks a download for unpacking and starts it. Caller holds m.mu.
func (m *Manager) queueUnpack(d *Download) {
	if d.UnpackState == "waiting" || d.UnpackState == "unpacking" {
		return
	}
	d.UnpackState, d.UnpackNote = "waiting", "Waiting to unpack"
	m.dirty = true
	go m.unpackFile(d)
}

// UnpackNow unpacks finished archives on demand (right-click → Unpack now).
func (m *Manager) UnpackNow(ids []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		if d := m.byID[id]; d != nil && unpackable(d) {
			m.queueUnpack(d)
		}
	}
}

// unpackFile unpacks a finished archive after its checks: never one that doesn't match its checksum or
// in which the virus scan found something.
func (m *Manager) unpackFile(d *Download) {
	for { // the virus scan first, when there is one
		m.mu.Lock()
		st := d.ScanState
		m.mu.Unlock()
		if st != "waiting" && st != "scanning" {
			break
		}
		select {
		case <-m.quit:
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
	unpackSem <- struct{}{}
	defer func() { <-unpackSem }()
	m.mu.Lock()
	fail := func(note string) {
		d.UnpackState, d.UnpackNote = "failed", note
		m.dirty = true
		m.mu.Unlock()
	}
	switch {
	case !unpackable(d) || d.UnpackState != "waiting": // redownloaded or removed meanwhile
		if d.UnpackState == "waiting" {
			d.UnpackState, d.UnpackNote = "", ""
		}
		m.mu.Unlock()
		return
	case d.ScanState == "threat":
		fail("Not unpacked: the virus scan found a threat")
		return
	case d.Check != nil && d.Check.State == "bad":
		fail("Not unpacked: the file doesn't match its checksum")
		return
	}
	archive, src, ref := d.path(), d.URL, d.Headers["Referer"]
	del := m.settings.Unpack.Delete && !m.inSyncQueue(d)
	d.UnpackState, d.UnpackNote = "unpacking", ""
	d.stage, d.stagePct = "Unpacking", 0
	m.dirty = true
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-m.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	o := unpack.Options{
		Mark: func(p string) { markFromInternet(p, src, ref) }, // (as the archive was: SmartScreen and Gatekeeper still check what's in it)
		Progress: func(pct int) {
			m.mu.Lock()
			d.stagePct = pct
			m.mu.Unlock()
		},
	}
	if free, _, maxFile, err := diskInfo(filepath.Dir(archive)); err == nil {
		o.Room, o.MaxFile = max(free-64<<20, 1), maxFile // (leaving a little room for everything else)
	}
	res, err := unpack.Unpack(ctx, archive, o)

	m.mu.Lock()
	defer m.mu.Unlock()
	d.stage, d.stagePct = "", 0
	m.dirty = true
	if err != nil {
		d.UnpackState = "failed"
		if errors.Is(err, context.Canceled) {
			d.UnpackState, d.UnpackNote = "", "" // Zipline quit: Unpack now does it again
			return
		}
		d.UnpackNote = "Couldn't unpack it: " + strings.TrimSuffix(err.Error(), ".")
		m.notify("unpackErr", d.ID, "Couldn't unpack "+iso(d.FileName)+": "+strings.TrimSuffix(err.Error(), "."))
		return
	}
	d.UnpackState, d.UnpackDir = "done", res.Dir
	note := fmt.Sprintf("Unpacked %s into %s", plural(res.Files, "file"), filepath.Base(res.Dir))
	if res.Skipped > 0 {
		note += fmt.Sprintf(" (%s left out: links, or names that pointed outside the folder)", plural(res.Skipped, "item"))
	}
	if del && os.Remove(archive) == nil {
		note += ". The archive was deleted"
	}
	d.UnpackNote = note
	m.notify("unpacked", d.ID, iso(d.FileName)+": "+note)
}

// inSyncQueue: the download is in a synchronization queue, which keeps comparing it with the server (so
// its archive stays). Caller holds m.mu.
func (m *Manager) inSyncQueue(d *Download) bool {
	q := m.queueByID(d.Queue)
	return q != nil && q.Sync
}

// unpackedOrArchive is what Open and Show in folder use for a download: the archive, or the folder it
// was unpacked into once the archive is gone. Caller holds m.mu.
func unpackedOrArchive(d *Download, p string) string {
	if d.UnpackDir != "" {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return d.UnpackDir
		}
	}
	return p
}
