package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// finishedPack adds a web-seeded two-file torrent in a folder of its own and waits until it is done.
// It is sharing when this returns. dir is where its files are.
func finishedPack(t *testing.T, m *Manager, folder string, skip []bool) (w *webPack, d *Download, mi *Metainfo, dir string) {
	t.Helper()
	w = newWebPack(t)
	mi, err := parseTorrent(w.tor)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.rememberTorrent(mi, w.tor)
	base := filepath.Join(m.settings.DownloadDir, folder)
	m.mu.Unlock()
	d, err = m.AddTorrent(TorrentAdd{Hash: mi.HashHex(), Skip: skip, Dir: base, Start: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, d, StCompleted, 60*time.Second)
	runningTorrent(t, m, d.ID)
	return w, d, mi, filepath.Join(base, "Pack")
}

// stopSharing ends a finished torrent's sharing and waits until it has.
func stopSharing(t *testing.T, m *Manager, d *Download) {
	t.Helper()
	m.StopSharing(d.ID)
	eventually(t, "still sharing", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.torrents[d.ID] == nil
	})
	time.Sleep(300 * time.Millisecond) // (it saves its progress and drops the hidden file as it ends)
}

func sharing(m *Manager, id string) *torrent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.torrents[id]
}

// noShareNotice is what the window was told about a torrent that wouldn't share.
func noShareNotice(m *Manager, id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.notices {
		if n.Kind == "noShare" && n.ID == id {
			return n.Text
		}
	}
	return ""
}

// A finished torrent that stopped sharing shares again when it is resumed, until its ratio, counted
// from now, is reached: one that already uploaded three times its size doesn't stop at once, and
// the window says where it stops.
func TestShareAgain(t *testing.T) {
	m := torrentManager(t)
	w, d, mi, _ := finishedPack(t, m, "got", nil)
	size := int64(len(w.a) + len(w.b))
	m.mu.Lock()
	m.settings.Torrent.SeedRatio = 0.1
	m.mu.Unlock()
	stopSharing(t, m, d)
	m.mu.Lock()
	d.Torrent.Uploaded = 3 * size
	m.mu.Unlock()

	m.Start([]string{d.ID}) // Resume
	tr := runningTorrent(t, m, d.ID)
	tr.mu.Lock()
	seeding, base := tr.seeding, tr.shareBase
	tr.mu.Unlock()
	if !seeding || base != 3*size {
		t.Fatalf("sharing %v from %d bytes uploaded, want yes, from %d", seeding, base, 3*size)
	}
	row, status := torrentRow(m, d.ID)
	if status != StCompleted || row == nil || !row.Seeding || row.NoShare != nil || row.StopRatio < 3.099 || row.StopRatio > 3.101 {
		t.Fatalf("the window sees %s %+v, want completed and sharing, stopping at ratio 3.1", status, row)
	}
	time.Sleep(2500 * time.Millisecond) // (counted from the start it would be over its ratio and have stopped)
	if sharing(m, d.ID) != tr {
		t.Fatal("stopped sharing at once: the ratio counted what it uploaded before")
	}

	m.mu.Lock()
	port := m.tport
	m.mu.Unlock()
	p := dialRawPeer(t, port, mi.InfoHash)
	p.send(msgInterested)
	if !p.waitFor(msgUnchoke, 5*time.Second) {
		t.Fatal("not unchoked")
	}
	for piece := 0; piece < 3; piece++ { // 96 KiB: past a tenth of its size
		p.request(piece, 0, blockSize)
		p.request(piece, blockSize, blockSize)
	}
	for i := 0; i < 6; i++ {
		if !p.waitFor(msgPiece, 5*time.Second) {
			t.Fatal("no block")
		}
	}
	eventually(t, "still sharing after its ratio", func() bool { return sharing(m, d.ID) == nil })
	if _, status := torrentRow(m, d.ID); status != StCompleted {
		t.Fatalf("status %s once it stopped sharing", status)
	}
}

// Resume on a finished torrent that isn't sharing shares it, and on anything else finished does
// nothing; a sharing target of 0 refuses, and says so.
func TestShareAgainTargetOff(t *testing.T) {
	m := torrentManager(t)
	_, d, _, _ := finishedPack(t, m, "got", nil)
	stopSharing(t, m, d)
	m.mu.Lock()
	m.settings.Torrent.SeedMins = 0 // (don't share after finishing)
	m.mu.Unlock()

	err := m.ShareAgain(d.ID)
	var why *ShareError
	if e, ok := err.(*ShareError); ok {
		why = e
	}
	if why == nil || why.Why != "off" || sharing(m, d.ID) != nil || !strings.Contains(noShareNotice(m, d.ID), "set to 0") {
		t.Fatalf("a target of 0: %v (notice %q)", err, noShareNotice(m, d.ID))
	}
	row, _ := torrentRow(m, d.ID)
	if row.NoShare == nil || row.NoShare.Why != "off" {
		t.Fatalf("the window isn't told: %+v", row)
	}
	// its own target (Properties) is used before Options'
	m.mu.Lock()
	d.Torrent.Seed, d.Torrent.SeedRatio, d.Torrent.SeedMins = seedCustom, 1, 30
	m.mu.Unlock()
	m.Start([]string{d.ID})
	runningTorrent(t, m, d.ID)
	if row, _ := torrentRow(m, d.ID); row.NoShare != nil {
		t.Fatalf("still says it wouldn't share: %+v", row.NoShare)
	}
}

// A file that is gone or changed since the torrent finished is not shared; the window says which,
// and sharing carries on once the file is as it was.
func TestShareAgainChecksTheFiles(t *testing.T) {
	m := torrentManager(t)
	w, d, _, dir := finishedPack(t, m, "got", nil)
	stopSharing(t, m, d)
	m.mu.Lock()
	saved := d.Torrent.Stamps[1]
	m.mu.Unlock()
	file := filepath.Join(dir, "b.txt")

	os.Remove(file)
	m.Start([]string{d.ID})
	row, status := torrentRow(m, d.ID)
	if sharing(m, d.ID) != nil || status != StCompleted || row.NoShare == nil || row.NoShare.Why != "missing" || row.NoShare.File != "b.txt" {
		t.Fatalf("a missing file: sharing, or %s %+v", status, row.NoShare)
	}
	if n := noShareNotice(m, d.ID); !strings.Contains(n, `"b.txt" is missing`) {
		t.Fatalf("the notice says %q", n)
	}

	os.WriteFile(file, w.b, 0o644) // the same bytes, put back…
	m.Start([]string{d.ID})
	if sharing(m, d.ID) != nil {
		t.Fatal("shared a file with another modified time")
	}
	if row, _ := torrentRow(m, d.ID); row.NoShare == nil || row.NoShare.Why != "changed" {
		t.Fatalf("a file put back: %+v", row.NoShare)
	}
	when := time.Unix(0, saved.Mod)
	os.Chtimes(file, when, when) // …and as it was
	m.Start([]string{d.ID})
	runningTorrent(t, m, d.ID)
	if row, _ := torrentRow(m, d.ID); row.NoShare != nil {
		t.Fatalf("shared, but the window still says %+v", row.NoShare)
	}

	stopSharing(t, m, d)
	damage(t, file, 10) // (edited: the same size, a later time)
	m.Start([]string{d.ID})
	if row, _ := torrentRow(m, d.ID); sharing(m, d.ID) != nil || row.NoShare == nil || row.NoShare.Why != "changed" {
		t.Fatalf("an edited file: %+v", row.NoShare)
	}
}

// The bytes of files you didn't choose live in a hidden file while sharing, and it goes away when
// sharing ends: sharing again says so instead of sending pieces it can't read.
func TestShareAgainNeedsTheHiddenPart(t *testing.T) {
	m := torrentManager(t)
	_, d, _, _ := finishedPack(t, m, "got", []bool{false, true}) // (b.txt isn't chosen; piece 9 holds the end of a.bin and the start of b.txt)
	stopSharing(t, m, d)
	m.Start([]string{d.ID})
	if row, _ := torrentRow(m, d.ID); sharing(m, d.ID) != nil || row.NoShare == nil || row.NoShare.Why != "parts" {
		t.Fatalf("the hidden file is gone: %+v", row.NoShare)
	}
}

// restart is what main does at exit, then a new Zipline starting from what was saved: the
// settings, the list and the torrent files.
func restart(t *testing.T, m *Manager, clean bool, edit func(settings map[string]any)) string {
	t.Helper()
	dir := exitLike(t, m, clean)
	os.MkdirAll(filepath.Join(dir, "torrents"), 0o755)
	files, _ := os.ReadDir(filepath.Join(m.dataDir, "torrents"))
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(m.dataDir, "torrents", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "torrents", f.Name()), b, 0o600)
	}
	if edit != nil {
		path := filepath.Join(dir, "settings.json")
		b, _ := os.ReadFile(path)
		var s map[string]any
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		edit(s)
		b, _ = json.Marshal(s)
		os.WriteFile(path, b, 0o600)
	}
	m.Quit() // (the old one stops sharing)
	return dir
}

func byHash(m *Manager, mi *Metainfo) *Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.list {
		if d.Torrent != nil && d.Torrent.InfoHash == mi.HashHex() {
			return d
		}
	}
	return nil
}

// D9: torrents that were sharing when Zipline closed (or crashed) carry on sharing at start-up,
// after the countdown, and their ratio counts from then.
func TestSharingCarriesOnAtStartUp(t *testing.T) {
	shortResume(t)
	for _, clean := range []bool{true, false} {
		m := torrentManager(t)
		w, d, mi, _ := finishedPack(t, m, "got", nil)
		_, ended, endedMi, _ := finishedPack(t, m, "ended", nil)
		stopSharing(t, m, ended) // (this one wasn't sharing when it closed)
		size := int64(len(w.a) + len(w.b))
		m.mu.Lock()
		d.Torrent.Uploaded = 2 * size
		m.mu.Unlock()
		dir := restart(t, m, clean, nil)

		m2 := d9Manager(t, dir)
		eventually(t, "no countdown shown", func() bool {
			m2.mu.Lock()
			defer m2.mu.Unlock()
			v := m2.powerView()
			return v.Resume != nil && v.Resume.Sharing == 1 && v.Resume.Downloads == 0
		})
		d2 := byHash(m2, mi)
		tr := runningTorrent(t, m2, d2.ID)
		tr.mu.Lock()
		seeding, base := tr.seeding, tr.shareBase
		tr.mu.Unlock()
		if !seeding || base < 2*size {
			t.Fatalf("clean=%v: sharing %v from %d bytes uploaded, want yes, from at least %d", clean, seeding, base, 2*size)
		}
		if sharing(m2, byHash(m2, endedMi).ID) != nil {
			t.Fatalf("clean=%v: a torrent that wasn't sharing started sharing", clean)
		}
	}
}

// dlView is what the window gets for a download.
func dlView(m *Manager, id string) DLView {
	for _, v := range m.Snapshot(nil).Downloads {
		if v.ID == id {
			return v
		}
	}
	return DLView{}
}

// D9: on a metered connection the sharing waits for the normal one; no more torrents start sharing
// than "at most so many at once" allows (the ones that finished last), and a missing file is
// reported instead of shared.
func TestSharingAtStartUpRespectsTheLimits(t *testing.T) {
	shortResume(t)
	var metered atomic.Bool
	t.Cleanup(setMeteredCheck(func() (bool, bool) { return metered.Load(), true }))

	m := torrentManager(t)
	_, _, mi1, _ := finishedPack(t, m, "one", nil)
	_, _, mi2, dir2 := finishedPack(t, m, "two", nil)
	_, _, mi3, _ := finishedPack(t, m, "three", nil)
	m.mu.Lock()
	m.settings.Plan.Metered = true
	m.mu.Unlock()
	next := restart(t, m, true, func(s map[string]any) {
		s["torrent"].(map[string]any)["maxSeeds"] = 2
	})
	gone := filepath.Join(dir2, "a.bin")            // gone while Zipline was closed
	eventually(t, "the file removed", func() bool { // (Windows can't remove it until the old one's sharing has closed it)
		err := os.Remove(gone)
		return err == nil || os.IsNotExist(err)
	})

	metered.Store(true) // started on a phone hotspot
	m2 := d9Manager(t, next)
	d1, d2, d3 := byHash(m2, mi1), byHash(m2, mi2), byHash(m2, mi3)
	eventually(t, "not held for the metered connection", func() bool {
		m2.mu.Lock()
		defer m2.mu.Unlock()
		return m2.sx.resume == nil && len(m2.meterPaused) == 2
	})
	m2.mu.Lock()
	waiting := m2.meterPaused[d3.ID] && m2.meterPaused[d2.ID] && !m2.meterPaused[d1.ID] // the first finished first: it isn't one of two
	m2.mu.Unlock()
	if !waiting || sharing(m2, d3.ID) != nil || sharing(m2, d2.ID) != nil {
		t.Fatalf("held for the metered connection: %v", waiting)
	}
	if !dlView(m2, d3.ID).Metered {
		t.Fatal("the window isn't told it waits for the normal connection")
	}

	metered.Store(false) // back on the normal connection
	m2.meteredTick()
	runningTorrent(t, m2, d3.ID)
	eventually(t, "the missing file isn't reported", func() bool { return noShareNotice(m2, d2.ID) != "" })
	if sharing(m2, d2.ID) != nil || sharing(m2, d1.ID) != nil {
		t.Fatal("shared a torrent with a missing file, or more than two")
	}
}

// A traffic profile that doesn't share finished torrents holds a torrent that carries on sharing:
// it starts, uploads nothing, and the window says so.
func TestShareAgainUnderAProfileThatDoesntShare(t *testing.T) {
	m := torrentManager(t)
	_, d, _, _ := finishedPack(t, m, "got", nil)
	stopSharing(t, m, d)
	if err := m.SetProfile("light"); err != nil {
		t.Fatal(err)
	}
	m.Start([]string{d.ID})
	runningTorrent(t, m, d.ID)
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if row, _ := torrentRow(m, d.ID); row != nil && row.Seeding && row.Held {
			return
		} else if time.Now().After(end) {
			t.Fatalf("not held under a profile that doesn't share: %+v", row)
		}
	}
}

// Stop sharing pressed just after a sharing torrent's files were chosen anew (a piece it shared with
// the file now skipped is fetched again, so its run starts over) still ends its sharing: it doesn't
// share once that piece is in.
func TestStopSharingWhileItFetchesAgain(t *testing.T) {
	m := torrentManager(t)
	_, d, _, _ := finishedPack(t, m, "got", nil)
	if err := m.SetTorrentFiles(d.ID, []bool{false, true}); err != nil {
		t.Fatal(err)
	}
	m.StopSharing(d.ID) // (before or after the new run has begun)
	eventually(t, "it to finish without sharing", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.torrents[d.ID] == nil && !d.running && d.Status == StCompleted
	})
	time.Sleep(1500 * time.Millisecond)
	if tr := sharing(m, d.ID); tr != nil {
		t.Fatal("it shares again after Stop sharing")
	}
	m.mu.Lock()
	st, skip := d.Status, d.Torrent.Skip
	m.mu.Unlock()
	if st != StCompleted || len(skip) != 2 || !skip[1] {
		t.Fatalf("status %s, skip %v", st, skip)
	}
}
