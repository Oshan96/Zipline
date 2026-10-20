package main

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// deadMagnet is a magnet link for a torrent nobody has (no trackers, no DHT in tests).
func deadMagnet() string {
	var ih [20]byte
	rand.Read(ih[:])
	return "magnet:?xt=urn:btih:" + hex.EncodeToString(ih[:]) + "&dn=nobody+has+this"
}

// D10: a torrent going nowhere doesn't hold its queue's place: the next download starts, and the
// torrent keeps trying in the background.
func TestSlowTorrentFreesQueueSlot(t *testing.T) {
	data := make([]byte, 256<<10)
	srv := testServer(data, 4<<20, true)
	defer srv.Close()
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Queues[0].MaxConcurrent = 1
	m.gate.slowFor = 300 * time.Millisecond // (a minute in real life)
	m.mu.Unlock()
	tor, err := m.AddTorrent(TorrentAdd{Magnet: deadMagnet(), QueueID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	d := m.Add(AddReq{URL: srv.URL + "/next.bin", QueueID: "main"})
	m.StartQueue("main", false)
	waitFor(t, m, tor, StDownloading, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	st := d.Status
	m.mu.Unlock()
	if st != StQueued {
		t.Fatalf("the queue's one place is taken: the next waits (%s)", st)
	}
	for i := 0; i < 40; i++ { // (the gate's own clock ticks every second)
		m.gateTick(time.Now())
		m.mu.Lock()
		st = d.Status
		m.mu.Unlock()
		if st != StQueued {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitFor(t, m, d, StCompleted, 10*time.Second)
	m.mu.Lock()
	running, slow, qRun := tor.running, m.slowTorrent(tor), m.qRun["main"]
	m.mu.Unlock()
	if !running || !slow || !qRun {
		t.Fatalf("the torrent should keep trying (running %v, slow %v) and its queue keep running (%v)", running, slow, qRun)
	}
	m.StopQueue("main")
}

// D10: a magnet link that never gets its file list gives up after the wait in Options, with a
// clear error, so its queue can finish.
func TestMagnetGivesUpOnFileList(t *testing.T) {
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.MetaMins = 3
	m.gate.metaUnit = 100 * time.Millisecond // a "minute"
	m.mu.Unlock()
	d, err := m.AddTorrent(TorrentAdd{Magnet: deadMagnet(), QueueID: "main"})
	if err != nil {
		t.Fatal(err)
	}
	m.StartQueue("main", false)
	waitFor(t, m, d, StError, 10*time.Second)
	m.mu.Lock()
	msg := d.Error
	done := false
	for _, n := range m.notices {
		done = done || n.Kind == "queueDone" && n.ID == "main"
	}
	m.mu.Unlock()
	if !strings.Contains(msg, "file list from other peers") || !done {
		t.Fatalf("error %q, queue finished %v", msg, done)
	}
}

// D10: beyond "most torrents sharing at once" the longest-sharing ones stop sharing.
func TestMostTorrentsSharingAtOnce(t *testing.T) {
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.MaxSeeds = 2
	m.mu.Unlock()
	var ds []*Download
	for i := 0; i < 3; i++ {
		tor, _, _ := webSeedTorrent(t)
		d, _ := addTorrentBytes(t, m, tor, nil)
		waitFor(t, m, d, StCompleted, 60*time.Second)
		runningTorrent(t, m, d.ID)
		ds = append(ds, d)
		time.Sleep(20 * time.Millisecond) // (one after the other)
	}
	m.gateTick(time.Now())
	sharing := func(d *Download) bool {
		for i := 0; i < 100; i++ {
			m.mu.Lock()
			on := m.torrents[d.ID] != nil
			m.mu.Unlock()
			if !on {
				return false
			}
			time.Sleep(20 * time.Millisecond)
		}
		return true
	}
	if sharing(ds[0]) {
		t.Fatal("the longest-sharing torrent should stop")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.torrents[ds[1].ID] == nil || m.torrents[ds[2].ID] == nil {
		t.Fatal("the other two keep sharing")
	}
}
