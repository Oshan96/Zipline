package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// uTP against a real BitTorrent engine: a libtorrent seeder that speaks uTP only (no TCP in or
// out) gives Zipline the whole torrent.
func TestUTPFromLibtorrent(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seeder = ltStart(t, py, "seed", "--utp", sw.torrent, sw.dir)
	if r := sw.seeder.wait(t, "port", 30*time.Second); r["seeding"] != true {
		t.Fatalf("the libtorrent seeder isn't seeding: %v", r)
	}
	m := torrentManager(t)
	d := addTorrentFile(t, m, sw.torrent, nil)
	waitFor(t, m, d, StCompleted, 90*time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), sw.files)
	if u := m.torrentUTP(); u == nil || u.Dialed.Load() == 0 {
		t.Fatal("no uTP connection was made")
	}
}

// …and the other way: Zipline shares with a libtorrent leecher that only speaks uTP, which
// connects to Zipline's port.
func TestUTPSharesWithLibtorrent(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seed(t, py)
	m := torrentManager(t)
	d := addTorrentFile(t, m, sw.torrent, nil)
	waitFor(t, m, d, StCompleted, 90*time.Second)
	sw.seeder.cmd.Process.Kill() // only Zipline has it now
	m.mu.Lock()
	port := m.tport
	m.mu.Unlock()
	out := t.TempDir()
	leech := ltStart(t, py, "leech", "--utp", sw.magnet, out, fmt.Sprintf("127.0.0.1:%d", port))
	if r := leech.wait(t, "done", 120*time.Second); r["done"] != true {
		t.Fatal("libtorrent couldn't download from Zipline over uTP")
	}
	checkFiles(t, filepath.Join(out, "Holiday Pack"), sw.files)
	if u := m.torrentUTP(); u == nil || u.Accepted.Load() == 0 {
		t.Fatal("the leecher didn't come in over uTP")
	}
}
