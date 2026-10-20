package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// call is a phone's request, and the reply as JSON.
func call(t *testing.T, c *phoneClient, path string, body any) map[string]any {
	t.Helper()
	time.Sleep(3 * time.Millisecond) // (a request signed in the same millisecond as the last would be a replay)
	method := "POST"
	if body == nil {
		method = "GET"
	}
	st, out := c.do(method, path, body, nil)
	var res map[string]any
	if st != 200 || json.Unmarshal([]byte(out), &res) != nil {
		t.Fatalf("%s: %d %s", path, st, out)
	}
	return res
}

func torrentSettings(m *Manager) {
	m.mu.Lock()
	m.settings.Torrent = defaultTorrentSettings()
	m.settings.Torrent.NoDHT = true // tests stay on this computer
	m.mu.Unlock()
}

func dlByID(m *Manager, id string) *Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byID[id]
}

func serveBytes(t *testing.T, ctype string, b []byte) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

// The phone reads a link with the window's probe: a file's name, size and kind, and what's wrong
// with a link the window would turn down.
func TestPhoneProbeIsTheAddDialogsProbe(t *testing.T) {
	_, c, _ := phoneManager(t)
	h := newFileHost(t, randData(1<<20), 0)
	r := call(t, c, "probe", map[string]any{"url": h.URL + "/files/photos.zip"})
	if r["name"] != "photos.zip" || r["size"] != float64(1<<20) || r["resumable"] != true || r["category"] != "Compressed" || r["error"] != nil {
		t.Fatalf("probe: %v", r)
	}
	if r["dir"] != nil || r["early"] != nil {
		t.Fatalf("the window's folders and early starts aren't the phone's: %v", r)
	}
	if r := call(t, c, "probe", map[string]any{"url": "https://www.youtube.com/watch?v=abc"}); r["protected"] != true || !strings.Contains(r["error"].(string), "YouTube") {
		t.Fatalf("protected: %v", r)
	}
	if r := call(t, c, "probe", map[string]any{"url": "file:///etc/passwd"}); r["error"] == nil {
		t.Fatalf("a file: link: %v", r)
	}
	// a link that doesn't answer is an error the sheet can show
	if r := call(t, c, "probe", map[string]any{"url": "http://127.0.0.1:1/x.bin"}); r["error"] == nil {
		t.Fatalf("unreachable: %v", r)
	}
}

// A .torrent link is a torrent, with its file list and its choices; it's never saved as a plain file.
func TestPhoneAddsATorrentLinkWithChosenFiles(t *testing.T) {
	m, c, _ := phoneManager(t)
	torrentSettings(m)
	tor, _, _ := webSeedTorrent(t)
	s := serveBytes(t, "application/x-bittorrent", tor)
	link := s.URL + "/pack.torrent"
	r := call(t, c, "probe", map[string]any{"url": link})
	files, _ := r["files"].([]any)
	ref, _ := r["url"].(string)
	if r["kind"] != "torrent" || len(files) != 2 || !strings.HasPrefix(ref, "torrent:") || r["name"] != "Pack" {
		t.Fatalf("probe: %v", r)
	}
	// "Download later" with the second file unticked and the first at high priority
	add := call(t, c, "add", map[string]any{"url": ref, "probed": true, "start": false, "queueId": "main", "skip": []bool{false, true}, "prio": []int8{1, 0}, "seq": true})
	d := dlByID(m, add["id"].(string))
	if d == nil || d.Kind != "torrent" {
		t.Fatalf("add: %v → %+v", add, d)
	}
	m.mu.Lock()
	skip, prio, seq, q, st := d.Torrent.Skip, d.Torrent.Prio, d.Torrent.Seq, d.Queue, d.Status
	m.mu.Unlock()
	if len(skip) != 2 || skip[0] || !skip[1] || prio[0] != 1 || !seq || q != "main" || st != StQueued {
		t.Fatalf("torrent: skip %v prio %v seq %v queue %q %s", skip, prio, seq, q, st)
	}
	// the same torrent again is refused the way the window refuses it
	if r := call(t, c, "add", map[string]any{"url": ref, "probed": true, "start": false}); r["error"] == nil {
		t.Fatalf("twice: %v", r)
	}
}

// A caller that doesn't probe first (an older page, a script) still gets a torrent, not a plain file.
func TestPhoneAddWithoutProbeStillKnowsTorrentsAndMetalinks(t *testing.T) {
	m, c, _ := phoneManager(t)
	torrentSettings(m)
	tor, _, _ := webSeedTorrent(t)
	s := serveBytes(t, "application/x-bittorrent", tor)
	add := call(t, c, "add", map[string]any{"url": s.URL + "/pack.torrent", "start": false})
	if d := dlByID(m, add["id"].(string)); d == nil || d.Kind != "torrent" || d.Torrent.HasMeta != true {
		t.Fatalf("torrent link: %v → %+v", add, d)
	}

	data := randData(300 << 10)
	host := newFileHost(t, data, 0)
	doc := metalinkDoc("disk.img", data, 128<<10, host.URL+"/disk.img")
	ml := serveBytes(t, "application/metalink4+xml", []byte(doc))
	add = call(t, c, "add", map[string]any{"url": ml.URL + "/disk.img.meta4", "start": false})
	d := dlByID(m, add["id"].(string))
	if d == nil {
		t.Fatalf("metalink: %v", add)
	}
	m.mu.Lock()
	url, name, pieces, check, st := d.URL, d.FileName, d.Pieces, d.Check, d.Status
	m.mu.Unlock()
	if url != host.URL+"/disk.img" || name != "disk.img" || pieces == nil || check == nil || st != StQueued {
		t.Fatalf("metalink download: %s %s pieces %v check %v %s", url, name, pieces, check, st)
	}
}

func TestPhoneProbesAndAddsAMetalink(t *testing.T) {
	m, c, _ := phoneManager(t)
	data := randData(300 << 10)
	host := newFileHost(t, data, 0)
	ml := serveBytes(t, "application/metalink4+xml", []byte(metalinkDoc("disk.img", data, 128<<10, host.URL+"/disk.img")))
	link := ml.URL + "/disk.img.meta4"
	r := call(t, c, "probe", map[string]any{"url": link})
	mv, _ := r["metalink"].(map[string]any)
	if mv == nil || len(mv["files"].([]any)) != 1 || r["name"] != "disk.img" {
		t.Fatalf("probe: %v", r)
	}
	add := call(t, c, "add", map[string]any{"url": link, "probed": true, "meta": true, "metaFile": 0, "start": true})
	d := dlByID(m, add["id"].(string))
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, data)
}

// "Download later" goes where the person chose; "Start download" starts and uses no queue.
func TestPhoneAddChoosesTheQueue(t *testing.T) {
	m, c, _ := phoneManager(t)
	m.mu.Lock()
	m.settings.Queues = append(m.settings.Queues, Queue{ID: "nights", Name: "Nights", MaxConcurrent: 1})
	m.normalizeSettings()
	m.mu.Unlock()
	h := newFileHost(t, randData(256<<10), 0)
	link := func(n string) string { return h.URL + "/files/" + n }
	q := func(args map[string]any) *Download {
		t.Helper()
		return dlByID(m, call(t, c, "add", args)["id"].(string))
	}
	if d := q(map[string]any{"url": link("a.bin"), "start": false, "queueId": "nights"}); d.Queue != "nights" || d.Status != StQueued {
		t.Fatalf("nights: %q %s", d.Queue, d.Status)
	}
	if d := q(map[string]any{"url": link("b.bin"), "start": false}); d.Queue != "main" {
		t.Fatalf("default: %q", d.Queue)
	}
	d := q(map[string]any{"url": link("c.bin"), "start": true, "queueId": "nights"})
	if d.Queue != "" {
		t.Fatalf("started now, yet in %q", d.Queue)
	}
	waitFor(t, m, d, StCompleted, 20*time.Second)
}

// The window's "Carry on with this link": the probe says which stopped download the link is for, and
// the phone gives it the link.
func TestPhoneRenewsAnExpiredLink(t *testing.T) {
	data := randData(3 << 20)
	h, allow, revoke := expiringHost(t, data, 300<<10)
	h.cut = 512 << 10
	allow("a")
	m, c, _ := phoneManager(t)
	m.mu.Lock()
	m.settings.Conns = 3
	m.mu.Unlock()
	d := m.Add(AddReq{URL: h.URL + "/dl/a/video.mp4", Headers: map[string]string{"Referer": h.URL + "/watch"}, Start: true})
	waitDone(t, m, d, 512<<10)
	revoke("a")
	waitLink(t, m, d, "expired", 20*time.Second)
	allow("b")
	fresh := h.URL + "/dl/b/video.mp4"
	r := call(t, c, "probe", map[string]any{"url": fresh})
	mt, _ := r["matches"].(map[string]any)
	if mt == nil || mt["id"] != d.ID {
		t.Fatalf("the probe should say which download this link is for: %v", r)
	}
	if r := call(t, c, "renew", map[string]any{"id": "nope", "url": fresh}); r["error"] == nil {
		t.Fatalf("renewed a download that isn't there: %v", r)
	}
	if r := call(t, c, "renew", map[string]any{"id": d.ID, "url": fresh}); r["ok"] != true {
		t.Fatalf("renew: %v", r)
	}
	m.mu.Lock()
	link, url := d.Link, d.URL
	m.mu.Unlock()
	if link != "" || url != fresh {
		t.Fatalf("link %q, url %s", link, url)
	}
	call(t, c, "action", map[string]any{"action": "start", "ids": []string{d.ID}})
	waitFor(t, m, d, StCompleted, 40*time.Second)
	fileIs(t, d, data)
	// a link for another file is turned down, as in the window
	other := newFileHost(t, randData(900<<10), 0)
	if r := call(t, c, "renew", map[string]any{"id": d.ID, "url": other.URL + "/x.bin"}); r["error"] == nil {
		t.Fatalf("renewed with another file's link: %v", r)
	}
}

// Outside the free hours the phone is told so: the Add sheet suggests them, and starting one of their
// downloads asks first and goes ahead on a yes.
func TestPhoneFreeHours(t *testing.T) {
	m, c, _ := phoneManager(t)
	now := time.Now()
	m.mu.Lock()
	m.settings.Plan = PlanSettings{On: true, Start: now.Add(2 * time.Hour).Format("15:04"), End: now.Add(4 * time.Hour).Format("15:04"), Days: []int{0, 1, 2, 3, 4, 5, 6}, Margin: 5}
	m.normalizeSettings()
	m.mu.Unlock()
	st := call(t, c, "state", nil)
	plan, _ := st["plan"].(map[string]any)
	if plan == nil || plan["on"] != true || plan["in"] != false || plan["from"] == "" || plan["to"] == "" {
		t.Fatalf("plan: %v", st["plan"])
	}
	ids := map[string]bool{}
	for _, q := range st["queues"].([]any) {
		ids[q.(map[string]any)["id"].(string)] = true
	}
	if !ids[planQueueID] {
		t.Fatalf("queues: %v", st["queues"])
	}
	h := newFileHost(t, randData(128<<10), 0)
	add := call(t, c, "add", map[string]any{"url": h.URL + "/files/night.bin", "start": false, "queueId": planQueueID})
	d := dlByID(m, add["id"].(string))
	if d.Queue != planQueueID {
		t.Fatalf("queue %q", d.Queue)
	}
	r := call(t, c, "action", map[string]any{"action": "start", "ids": []string{d.ID}})
	if r["ask"] != "freeHours" || r["error"] == nil {
		t.Fatalf("start outside the hours: %v", r)
	}
	if r := call(t, c, "action", map[string]any{"action": "start", "ids": []string{d.ID}, "sure": true}); r["ok"] != true {
		t.Fatalf("start after yes: %v", r)
	}
	waitFor(t, m, d, StCompleted, 20*time.Second)
	r = call(t, c, "queue", map[string]any{"action": "start", "id": planQueueID})
	if r["ask"] != "freeHours" {
		t.Fatalf("queue start outside the hours: %v", r)
	}
	if r := call(t, c, "queue", map[string]any{"action": "start", "id": planQueueID, "sure": true}); r["ok"] != true {
		t.Fatalf("queue start after yes: %v", r)
	}
}

func TestPhoneLimitsOneDownload(t *testing.T) {
	m, c, _ := phoneManager(t)
	h := newFileHost(t, randData(128<<10), 0)
	d := m.Add(AddReq{URL: h.URL + "/files/a.bin"})
	call(t, c, "dlimit", map[string]any{"id": d.ID, "kb": 300})
	m.mu.Lock()
	kb := d.LimitKB
	m.mu.Unlock()
	if kb != 300 {
		t.Fatalf("limit %d", kb)
	}
	var found map[string]any
	for _, x := range call(t, c, "state", nil)["downloads"].([]any) {
		found = x.(map[string]any)
	}
	if found["limitKB"] != float64(300) {
		t.Fatalf("state: %v", found)
	}
	call(t, c, "dlimit", map[string]any{"id": d.ID, "kb": 0})
	m.mu.Lock()
	kb = d.LimitKB
	m.mu.Unlock()
	if kb != 0 {
		t.Fatalf("limit %d", kb)
	}
	if r := call(t, c, "dlimit", map[string]any{"id": "nope", "kb": 10}); r["error"] == nil {
		t.Fatalf("limit of nothing: %v", r)
	}
	if st, _ := c.do("POST", "dlimit", map[string]any{"id": d.ID, "kb": -5}, nil); st != 400 {
		t.Fatalf("negative limit: %d", st)
	}
}

// A sharing torrent can be told to stop; its files can be chosen after it was added.
func TestPhoneTorrentSharingAndFiles(t *testing.T) {
	m, c, _ := phoneManager(t)
	torrentSettings(m)
	_, d, _, _ := finishedPack(t, m, "got", nil)
	torrentOf := func() map[string]any {
		for _, x := range call(t, c, "state", nil)["downloads"].([]any) {
			if x.(map[string]any)["id"] == d.ID {
				return x.(map[string]any)
			}
		}
		return nil
	}
	waitUntil(t, "the state to say it shares", func() bool {
		tv, _ := torrentOf()["torrent"].(map[string]any)
		return tv != nil && tv["seeding"] == true
	})
	x := torrentOf()
	tv := x["torrent"].(map[string]any)
	if x["kind"] != "torrent" || tv["meta"] != true || tv["files"] != float64(2) || x["text"] != "Complete · sharing" {
		t.Fatalf("torrent in state: %v", x)
	}
	f := call(t, c, "tfiles", map[string]any{"id": d.ID})
	files, _ := f["files"].([]any)
	if len(files) != 2 || files[0].(map[string]any)["path"] != "a.bin" || f["peers"] != nil || f["trackers"] != nil {
		t.Fatalf("files: %v", f)
	}
	// untick b.txt
	call(t, c, "tpick", map[string]any{"id": d.ID, "skip": []bool{false, true}, "prio": []int8{1, 0}})
	m.mu.Lock()
	skip, prio := d.Torrent.Skip, d.Torrent.Prio
	m.mu.Unlock()
	if len(skip) != 2 || skip[0] || !skip[1] || len(prio) != 2 || prio[0] != 1 {
		t.Fatalf("skip %v prio %v", skip, prio)
	}
	if r := call(t, c, "tpick", map[string]any{"id": d.ID, "skip": []bool{true, true}}); r["error"] == nil {
		t.Fatalf("no file at all: %v", r)
	}
	// Stop sharing
	call(t, c, "action", map[string]any{"action": "stopSharing", "ids": []string{d.ID}})
	waitUntil(t, "sharing to stop", func() bool { return sharing(m, d.ID) == nil })
	waitUntil(t, "the state to follow", func() bool {
		tv, _ := torrentOf()["torrent"].(map[string]any)
		return tv != nil && tv["seeding"] == false
	})
	if x := torrentOf(); x["text"] != "Complete" {
		t.Fatalf("after: %v", x)
	}
}

// A magnet link for a torrent the window has already read answers with its files; asking for the
// list of one it hasn't read starts the fetch and answers without files yet.
func TestPhoneMagnetFileList(t *testing.T) {
	m, c, _ := phoneManager(t)
	torrentSettings(m)
	tor, _, _ := webSeedTorrent(t)
	mi, err := parseTorrent(tor)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.rememberTorrent(mi, tor)
	m.mu.Unlock()
	magnet := "magnet:?xt=urn:btih:" + mi.HashHex() + "&dn=Pack"
	r := call(t, c, "probe", map[string]any{"url": magnet})
	if r["kind"] != "torrent" || len(r["files"].([]any)) != 2 || r["needMeta"] != false {
		t.Fatalf("magnet probe: %v", r)
	}
	r = call(t, c, "tmeta", map[string]any{"url": magnet})
	if len(r["files"].([]any)) != 2 {
		t.Fatalf("tmeta: %v", r)
	}
	if r := call(t, c, "tmeta", map[string]any{"url": "http://example.com/x"}); r["error"] == nil {
		t.Fatalf("tmeta of a web link: %v", r)
	}
	unknown := "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)
	r = call(t, c, "probe", map[string]any{"url": unknown})
	if r["kind"] != "torrent" || r["needMeta"] != true || r["files"] != nil {
		t.Fatalf("unknown magnet probe: %v", r)
	}
	r = call(t, c, "tmeta", map[string]any{"url": unknown})
	if r["needMeta"] != true {
		t.Fatalf("tmeta: %v", r)
	}
	call(t, c, "tmeta", map[string]any{"url": unknown, "stop": true})
	// adding it without waiting for the list works
	add := call(t, c, "add", map[string]any{"url": unknown, "start": false})
	if d := dlByID(m, add["id"].(string)); d == nil || d.Kind != "torrent" {
		t.Fatalf("add magnet: %v", add)
	}
}
