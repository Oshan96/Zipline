package main

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Protocol encryption, the fast extension and peer exchange against libtorrent.

// waitPeerList waits for a libtorrent "peerlist" line with a peer that passes ok.
func (p *ltProc) waitPeerList(t *testing.T, max time.Duration, ok func(peer map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		m := p.wait(t, "peerlist", time.Until(deadline))
		list, _ := m["peerlist"].([]any)
		for _, x := range list {
			if pm, _ := x.(map[string]any); pm != nil && ok(pm) {
				return pm
			}
		}
	}
	t.Fatalf("libtorrent never listed such a peer")
	return nil
}

// A libtorrent peer that takes only RC4-encrypted connections seeds to Zipline, then leeches
// from it.
func TestTorrentEncryptedWithLibtorrent(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seeder = ltStart(t, py, "seed", sw.torrent, sw.dir, "--enc=forced", "--peers")
	if r := sw.seeder.wait(t, "port", 30*time.Second); r["seeding"] != true {
		t.Fatalf("the libtorrent seeder isn't seeding: %v", r)
	}
	m := torrentManager(t)
	d := addTorrentFile(t, m, sw.torrent, nil)
	waitFor(t, m, d, StCompleted, 90*time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), sw.files)
	sw.seeder.waitPeerList(t, 10*time.Second, func(p map[string]any) bool { return p["rc4"] == true })
	sw.seeder.cmd.Process.Kill()
	m.mu.Lock()
	port := m.tport
	m.settings.Torrent.Enc = "require" // and Zipline turns plain peers away now
	m.mu.Unlock()
	out := t.TempDir()
	leech := ltStart(t, py, "leech", sw.magnet, out, fmt.Sprintf("127.0.0.1:%d", port), "--enc=forced")
	if r := leech.wait(t, "done", 120*time.Second); r["done"] != true { // (it takes nothing but RC4)
		t.Fatal("libtorrent couldn't download from Zipline over an encrypted connection")
	}
	checkFiles(t, filepath.Join(out, "Holiday Pack"), sw.files)
}

// The fast extension against libtorrent: its seeder answers with Have All and an allowed-fast
// set (here every piece), and its leecher takes Zipline's Have All and allowed-fast set in turn.
func TestTorrentFastExtensionWithLibtorrent(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seeder = ltStart(t, py, "seed", sw.torrent, sw.dir, "--allowed-fast=1000")
	if r := sw.seeder.wait(t, "port", 30*time.Second); r["seeding"] != true {
		t.Fatalf("the libtorrent seeder isn't seeding: %v", r)
	}
	m := torrentManager(t)
	d := addTorrentFile(t, m, sw.torrent, nil)
	var fast, haveAll bool
	var allowed, rejected int
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		tr, st := m.torrents[d.ID], d.Status
		m.mu.Unlock()
		if st == StCompleted {
			break
		}
		if tr != nil {
			tr.mu.Lock()
			for _, p := range tr.peers {
				fast, haveAll = fast || p.x.fast, haveAll || p.x.haveAll
				allowed, rejected = max(allowed, len(p.x.allowedIn)), max(rejected, p.x.rejected)
			}
			tr.mu.Unlock()
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, m, d, StCompleted, time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), sw.files)
	if !fast || !haveAll || allowed == 0 || rejected > 0 {
		t.Fatalf("fast %v, have all %v, allowed fast %d, rejected %d", fast, haveAll, allowed, rejected)
	}
	sw.seeder.cmd.Process.Kill()
	m.mu.Lock()
	port := m.tport
	tr := m.torrents[d.ID]
	m.mu.Unlock()
	out := t.TempDir()
	leech := ltStart(t, py, "leech", sw.magnet, out, fmt.Sprintf("127.0.0.1:%d", port))
	sent := false
	for i := 0; i < 6000 && !sent; i++ { // (libtorrent tries uTP first, which Zipline doesn't speak)
		tr.mu.Lock()
		for _, p := range tr.peers {
			sent = sent || p.x.fast && len(p.x.allowedOut) == allowedFastK
		}
		tr.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if r := leech.wait(t, "done", 120*time.Second); r["done"] != true || !sent {
		t.Fatalf("libtorrent should download from Zipline (Have All and an allowed-fast set sent: %v)", sent)
	}
	checkFiles(t, filepath.Join(out, "Holiday Pack"), sw.files)
}

// seedFromDisk starts Zipline sharing a swarm's files where they are (every piece had).
func seedFromDisk(t *testing.T, m *Manager, sw *swarm) *Download {
	b, err := os.ReadFile(sw.torrent)
	if err != nil {
		t.Fatal(err)
	}
	mi, err := parseTorrent(b)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.rememberTorrent(mi, b)
	m.mu.Unlock()
	d, err := m.AddTorrent(TorrentAdd{Hash: mi.HashHex(), Dir: sw.dir})
	if err != nil {
		t.Fatal(err)
	}
	all := newBitfield(mi.NumPieces())
	for i := 0; i < mi.NumPieces(); i++ {
		all.set(i)
	}
	m.mu.Lock()
	d.Torrent.Have = all
	m.mu.Unlock()
	m.Start([]string{d.ID})
	return d
}

// Peer exchange both ways: libtorrent peer B knows only Zipline and learns peer A from
// Zipline's ut_pex; Zipline learns of one of them from theirs.
func TestTorrentPexWithLibtorrent(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.UpKB = 64 // slow enough that nobody finishes before the exchange
	m.settings.Torrent.SeedRatio = 100
	m.mu.Unlock()
	d := seedFromDisk(t, m, sw)
	tr := runningTorrent(t, m, d.ID)
	m.mu.Lock()
	port := m.tport
	m.mu.Unlock()
	// (over TCP: a uTP peer comes from its listening port, so Zipline would know it already and
	// what peer exchange tells it would be nothing new)
	a := ltStart(t, py, "leech", sw.torrent, t.TempDir(), "-", "--peers", "--tcp") // finds Zipline through the tracker
	a.waitPeerList(t, 30*time.Second, func(p map[string]any) bool { return int(p["port"].(float64)) == port || p["outgoing"] == false })
	ih, _ := hex.DecodeString(sw.hash)
	b := ltStart(t, py, "leech", magnetLink([20]byte(ih), "", nil), t.TempDir(), fmt.Sprintf("127.0.0.1:%d", port), "--peers", "--tcp")
	got := b.waitPeerList(t, 60*time.Second, func(p map[string]any) bool {
		src, _ := p["source"].([]any)
		for _, s := range src {
			if s == "pex" {
				return true
			}
		}
		return false
	})
	t.Logf("libtorrent learnt %v:%v through Zipline's peer exchange", got["ip"], got["port"])
	deadline := time.Now().Add(90 * time.Second)
	for {
		tr.mu.Lock()
		var from []netip.AddrPort
		for _, c := range tr.cands {
			if c.src == "peer exchange" {
				from = append(from, c.addr)
			}
		}
		tr.mu.Unlock()
		if len(from) > 0 {
			t.Logf("Zipline learnt %v through peer exchange", from)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Zipline should hear of a peer through libtorrent's peer exchange")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
