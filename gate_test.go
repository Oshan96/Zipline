package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rawPeer is a minimal BitTorrent peer that asks Zipline for blocks.
type rawPeer struct {
	t *testing.T
	c net.Conn
}

func dialRawPeer(t *testing.T, port int, ih [20]byte) *rawPeer {
	t.Helper()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	var pid [20]byte
	copy(pid[:], "-XX0001-gatetestpeer")
	c.SetDeadline(time.Now().Add(10 * time.Second))
	c.Write(handshakeBytes(ih, pid))
	var reply [68]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		t.Fatal(err)
	}
	return &rawPeer{t, c}
}

func (p *rawPeer) send(msg ...byte) {
	b := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(b, uint32(len(msg)))
	copy(b[4:], msg)
	p.c.Write(b)
}

func (p *rawPeer) request(piece, begin, length int) {
	m := make([]byte, 13)
	m[0] = msgRequest
	binary.BigEndian.PutUint32(m[1:], uint32(piece))
	binary.BigEndian.PutUint32(m[5:], uint32(begin))
	binary.BigEndian.PutUint32(m[9:], uint32(length))
	p.send(m...)
}

// waitFor reads messages until one with this id arrives (false: none within d).
func (p *rawPeer) waitFor(id byte, d time.Duration) bool {
	p.c.SetReadDeadline(time.Now().Add(d))
	for {
		msg, err := readPeerMsg(p.c)
		if err != nil {
			return false
		}
		if len(msg) > 0 && msg[0] == id {
			return true
		}
	}
}

// D1: a seeding torrent stops uploading on a metered connection and carries on after; what it
// uploads counts as data used.
func TestSeedingPausesWhenMetered(t *testing.T) {
	tor, _, _ := webSeedTorrent(t)
	m := torrentManager(t)
	var metered atomic.Bool
	m.mu.Lock()
	m.gate.meteredFn = func() (bool, bool) { return metered.Load(), true } // (its own: other managers read the system's)
	m.settings.Plan.Metered = true
	m.mu.Unlock()
	d, mi := addTorrentBytes(t, m, tor, nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	runningTorrent(t, m, d.ID) // sharing
	m.mu.Lock()
	port := m.tport
	m.mu.Unlock()

	p := dialRawPeer(t, port, mi.InfoHash)
	p.send(msgInterested)
	if !p.waitFor(msgUnchoke, 5*time.Second) {
		t.Fatal("not unchoked while sharing")
	}
	upBefore, _ := m.gate.upUsage.used(1)
	p.request(0, 0, blockSize)
	if !p.waitFor(msgPiece, 5*time.Second) {
		t.Fatal("no block while sharing")
	}
	time.Sleep(100 * time.Millisecond)
	if up, _ := m.gate.upUsage.used(1); up-upBefore < blockSize {
		t.Fatalf("the upload wasn't counted: %d bytes", up-upBefore)
	}
	if day := m.gate.days.day("", time.Now()); day.Up < blockSize {
		t.Fatalf("today's uploads: %+v", day)
	}

	metered.Store(true) // the laptop moved to a phone hotspot
	m.meteredTick()
	m.gateTick(time.Now())
	if !p.waitFor(msgChoke, 5*time.Second) {
		t.Fatal("still unchoked on a metered connection")
	}
	p.request(0, blockSize, blockSize)
	if p.waitFor(msgPiece, 1500*time.Millisecond) {
		t.Fatal("uploaded on a metered connection")
	}
	m.mu.Lock()
	sharing := m.torrents[d.ID] != nil
	m.mu.Unlock()
	if !sharing {
		t.Fatal("sharing should pause, not end")
	}

	metered.Store(false)
	m.meteredTick()
	m.gateTick(time.Now())
	if !p.waitFor(msgUnchoke, 5*time.Second) {
		t.Fatal("sharing didn't carry on after the metered connection")
	}
	p.request(0, blockSize, blockSize)
	if !p.waitFor(msgPiece, 5*time.Second) {
		t.Fatal("no block after the metered connection")
	}
}

// D1: who may upload when: metered, free hours for Free data hours torrents (only while uploads
// are free in them), a profile that doesn't share.
func TestUploadHoldRules(t *testing.T) {
	m := newTestManager(t)
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings.Plan = PlanSettings{On: true, Start: now.Add(2 * time.Hour).Format("15:04"), End: now.Add(4 * time.Hour).Format("15:04"), Days: []int{0, 1, 2, 3, 4, 5, 6}}
	m.normalizeSettings()
	m.applyTraffic()
	if why := m.uploadHold(true, true, now); why != "outside free hours" {
		t.Fatalf("a Free data hours torrent outside them: %q", why)
	}
	if why := m.uploadHold(false, true, now); why != "" {
		t.Fatalf("another torrent: %q", why)
	}
	m.settings.Plan.UpPaid = true // uploads cost the same in free hours: the window makes no difference
	if why := m.uploadHold(true, true, now); why != "" {
		t.Fatalf("uploads not free in free hours: %q", why)
	}
	m.settings.Plan.UpPaid = false
	m.settings.Plan.Start, m.settings.Plan.End = now.Add(-time.Hour).Format("15:04"), now.Add(time.Hour).Format("15:04")
	if why := m.uploadHold(true, true, now); why != "" {
		t.Fatalf("inside free hours: %q", why)
	}
	m.metered = true
	if why := m.uploadHold(false, false, now); why != "metered connection" {
		t.Fatalf("metered: %q", why)
	}
	m.metered = false
	m.settings.Traffic.Default = "light" // doesn't share finished torrents
	m.applyTraffic()
	if m.uploadHold(false, true, now) != "traffic profile" || m.uploadHold(false, false, now) != "" {
		t.Fatal("the Light profile should pause sharing finished torrents only")
	}
}

// D1/D7: the data cap and quotas count downloads, uploads or both, and hold back what they count.
func TestCapAndQuotaHoldWhatTheyCount(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Cap = DataCap{On: true, MB: 1, Hours: 1}
	m.settings.Quota.Count = "up"
	m.normalizeSettings()
	m.applyLimit()
	m.mu.Unlock()
	m.countDown(3 << 20)
	if m.gate.downHeld() || m.gate.upHeld() {
		t.Fatal("counting uploads only: downloads don't use the cap")
	}
	m.countUp(2 << 20)
	if !m.gate.upHeld() || m.gate.downHeld() {
		t.Fatal("uploads used the cap up: uploads wait, downloads don't")
	}
	m.mu.Lock()
	m.settings.Quota.Count = "both"
	m.applyLimit()
	v := m.capView()
	m.mu.Unlock()
	if !m.gate.downHeld() || !m.gate.upHeld() || v.Used != 5<<20 || !v.Blocked {
		t.Fatalf("both count: held %v/%v, view %+v", m.gate.downHeld(), m.gate.upHeld(), v)
	}
	if used, _ := m.usage.used(1); used != 5<<20 {
		t.Fatalf("the rolling meter counts everything moved: %d", used)
	}
}

// D1: the site grabber waits while the data cap is used up, and its pages count as data used.
func TestGrabberWaitsForCap(t *testing.T) {
	var hits atomic.Int32
	page := `<html><body><a href="/a.zip">a</a><a href="/b.zip">b</a></body></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, page)
	}))
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Cap = DataCap{On: true, MB: 1, Hours: 1}
	m.applyLimit()
	m.mu.Unlock()
	m.countDown(2 << 20) // the cap is used up
	j, err := m.grab.Start(GrabOptions{URL: srv.URL + "/", Types: []string{"Compressed"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("the grabber fetched %d pages past the data cap", n)
	}
	before, _ := m.usage.used(1)
	m.mu.Lock()
	m.settings.Cap.MB = 10
	m.applyLimit()
	m.mu.Unlock()
	for i := 0; i < 100 && j.view()["state"] == "running"; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if v := j.view(); v["state"] != "done" || len(v["files"].([]GrabFile)) != 2 {
		t.Fatalf("grab after the cap freed up: %v", v)
	}
	if after, _ := m.usage.used(1); after-before < int64(len(page)) {
		t.Fatalf("the page wasn't counted: %d bytes", after-before)
	}
}

// D1: sync checks and crawls wait out a metered connection.
func TestBackgroundTrafficWaitsWhenMetered(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	m.metered = true
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		time.Sleep(300 * time.Millisecond)
		m.mu.Lock()
		m.metered = false // back on the normal connection
		m.mu.Unlock()
	}()
	t0 := time.Now()
	if !m.netGateWait(ctx) || time.Since(t0) < 250*time.Millisecond {
		t.Fatalf("waited %v", time.Since(t0))
	}
	m.mu.Lock()
	m.metered = true
	m.mu.Unlock()
	short, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if m.netGateWait(short) {
		t.Fatal("went ahead on a metered connection")
	}
}

// D1: the tray, the phone and the command line can't start the Free data hours queue outside the
// free hours; the window can, once you say so.
func TestFreeHoursQueueStartsOnlyThroughTheGate(t *testing.T) {
	m, pc, _ := phoneManager(t)
	now := time.Now()
	m.mu.Lock()
	m.settings.Plan = PlanSettings{On: true, Start: now.Add(2 * time.Hour).Format("15:04"), End: now.Add(4 * time.Hour).Format("15:04"), Days: []int{0, 1, 2, 3, 4, 5, 6}, Margin: 5}
	m.normalizeSettings()
	m.mu.Unlock()
	d := m.Add(AddReq{URL: "http://127.0.0.1:1/later.bin", QueueID: planQueueID})
	running := func(q string) bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.qRun[q]
	}

	// the tray's "Start all queues"
	m.StartAllQueues()
	if running(planQueueID) {
		t.Fatal("Start all queues: the Free data hours queue started outside its hours")
	}
	m.mu.Lock()
	said, mainRan := false, false
	for _, n := range m.notices {
		said = said || n.Kind == "refused" && strings.Contains(n.Text, "outside your free hours")
		mainRan = mainRan || n.Kind == "queueDone" && n.ID == "main" // (empty: done at once)
	}
	m.mu.Unlock()
	if !said || !mainRan {
		t.Fatalf("the window wasn't told why (%v), or the other queues didn't start (%v)", said, mainRan)
	}
	// the phone
	pc.do("POST", "hello", map[string]any{"name": "Test phone"}, nil)
	if code, body := pc.do("POST", "queue", map[string]any{"action": "start", "id": planQueueID}, nil); code != 200 || !strings.Contains(body, "outside your free hours") || running(planQueueID) {
		t.Fatalf("phone queue start: %d %s", code, body)
	}
	if code, body := pc.do("POST", "action", map[string]any{"action": "start", "ids": []string{d.ID}}, nil); code != 200 || !strings.Contains(body, "error") {
		t.Fatalf("phone resume: %d %s", code, body)
	}
	// the command line
	if note, _ := m.ApplyCLI(&CLIRequest{StartQueue: planQueueID}); !strings.Contains(note, "outside your free hours") || running(planQueueID) {
		t.Fatalf("command line: %q", note)
	}
	var fe *FreeHoursError
	if err := m.StartQueueChecked(planQueueID, false); !errors.As(err, &fe) || !errors.Is(err, errOutsideFree) {
		t.Fatalf("err %v", err)
	}
	// a schedule (or anything else) that starts the queue itself: it runs, but waits for the hours
	m.StartQueue(planQueueID, true)
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	st, tried := d.Status, !d.LastTry.IsZero()
	m.mu.Unlock()
	if !running(planQueueID) || st != StQueued || tried {
		t.Fatalf("started by a schedule outside the free hours: running %v, download %s (tried %v)", running(planQueueID), st, tried)
	}
	m.StopQueue(planQueueID)
	// the window: asked first, then sure
	h := (&Server{m: m, token: "t"}).routes()
	if r := apiCall(t, h, "queue", map[string]any{"action": "start", "id": planQueueID}); r["ask"] != "freeHours" || running(planQueueID) {
		t.Fatalf("window without asking: %v", r)
	}
	if r := apiCall(t, h, "action", map[string]any{"action": "start", "ids": []string{d.ID}}); r["ask"] != "freeHours" {
		t.Fatalf("window resume without asking: %v", r)
	}
	apiCall(t, h, "queue", map[string]any{"action": "start", "id": planQueueID, "sure": true})
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	tried = !d.LastTry.IsZero()
	m.mu.Unlock()
	if !running(planQueueID) || !tried {
		t.Fatalf("the window's yes should start it (running %v, download tried %v)", running(planQueueID), tried)
	}
	m.StopQueue(planQueueID)
	m.Pause([]string{d.ID})
}

// D1/D7: a used-up quota stops queues starting downloads; carrying on lets them start.
func TestQueueWaitsForQuota(t *testing.T) {
	data := bytes.Repeat([]byte("z"), 64<<10)
	srv := testServer(data, 1<<20, true)
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Quota.Rules = []Quota{{On: true, Period: "day", MB: 1}}
	m.normalizeSettings()
	m.applyLimit()
	id := m.settings.Quota.Rules[0].ID
	m.mu.Unlock()
	m.countDown(2 << 20)
	m.mu.Lock()
	m.gateRecompute(time.Now())
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.URL + "/q.bin", QueueID: "main"})
	m.StartQueue("main", false)
	time.Sleep(300 * time.Millisecond)
	m.mu.Lock()
	st := d.Status
	stop := false
	for _, n := range m.notices {
		stop = stop || n.Kind == "quotaStop" && n.ID == id
	}
	m.mu.Unlock()
	if st != StQueued || !stop {
		t.Fatalf("with the quota used up: status %s, told %v", st, stop)
	}
	h := (&Server{m: m, token: "t"}).routes()
	apiCall(t, h, "quota", map[string]any{"id": id}) // carry on anyway this time
	waitFor(t, m, d, StCompleted, 10*time.Second)
}

// D1: a sync queue's checks wait while the connection is metered, and go ahead after.
func TestSyncCheckWaitsWhenMetered(t *testing.T) {
	data := []byte("version 1")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.ServeContent(w, r, "s.txt", time.Now(), bytes.NewReader(data))
	}))
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Queues = append(m.settings.Queues, Queue{ID: "sq", Name: "Sync", MaxConcurrent: 1, Sync: true})
	m.normalizeSettings()
	dir := m.settings.DownloadDir
	m.mu.Unlock()
	if err := os.WriteFile(filepath.Join(dir, "s.txt"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	d := &Download{ID: newID(), URL: srv.URL + "/s.txt", FileName: "s.txt", Dir: dir, Size: int64(len(data)), Status: StCompleted, Queue: "sq", Added: time.Now()}
	m.list = append(m.list, d)
	m.byID[d.ID] = d
	m.metered = true
	m.mu.Unlock()
	m.StartQueue("sq", false)
	time.Sleep(1200 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("checked %d times on a metered connection", n)
	}
	m.mu.Lock()
	m.metered = false
	m.mu.Unlock()
	for i := 0; i < 100; i++ {
		m.mu.Lock()
		syncing := m.qSyncing["sq"]
		m.mu.Unlock()
		if !syncing {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	m.mu.Lock()
	note := d.SyncNote
	m.mu.Unlock()
	if hits.Load() == 0 || note != "Up to date" {
		t.Fatalf("after the metered connection: %d checks, note %q", hits.Load(), note)
	}
}
