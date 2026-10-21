package main

// Tests for the bugs found in the October 2026 review (A1–A17).

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zipline/internal/bencode"
	"zipline/internal/filename"
)

// A2: two downloads whose server-given names are the same must not share a part file.
func TestServerNamedDownloadsGetTheirOwnFiles(t *testing.T) {
	one, two := randData(700<<10), randData(900<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := one
		if strings.HasPrefix(r.URL.Path, "/b/") {
			data = two
		}
		http.ServeContent(w, r, "get.php", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	m := newTestManager(t)
	d1 := m.Add(AddReq{URL: srv.URL + "/a/get.php", Start: true})
	d2 := m.Add(AddReq{URL: srv.URL + "/b/get.php", Start: true})
	waitFor(t, m, d1, StCompleted, 30*time.Second)
	waitFor(t, m, d2, StCompleted, 30*time.Second)
	m.mu.Lock()
	p1, p2 := d1.path(), d2.path()
	m.mu.Unlock()
	if p1 == p2 {
		t.Fatalf("both saved as %s", p1)
	}
	if b, _ := os.ReadFile(p1); !bytes.Equal(b, one) {
		t.Fatal("first file damaged")
	}
	if b, _ := os.ReadFile(p2); !bytes.Equal(b, two) {
		t.Fatal("second file damaged")
	}
}

// A4: a scheduled queue whose downloads fail doesn't run its "turn off computer" action.
func TestQueueWithFailuresKeepsTheComputerOn(t *testing.T) {
	data := randData(64 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "f.bin", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	var shutdowns int32
	old := shutdownHook
	shutdownHook = func() { atomic.AddInt32(&shutdowns, 1) }
	defer func() { shutdownHook = old }()
	oldDelay := actionDelay
	actionDelay = time.Second // (the countdown someone at the computer could cancel)
	defer func() { actionDelay = oldDelay }()

	run := func(urls ...string) {
		m := newTestManager(t)
		m.mu.Lock()
		m.settings.Queues[0].Sched.WhenDone = "shutdown"
		m.mu.Unlock()
		for _, u := range urls {
			m.Add(AddReq{URL: u, QueueID: "main"})
		}
		m.StartQueue("main", true)
		for i := 0; i < 600; i++ { // a missing file takes a few retries to give up on
			m.mu.Lock()
			r := m.qRun["main"]
			m.mu.Unlock()
			if !r {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("queue did not finish")
	}
	run(srv.URL+"/ok.bin", srv.URL+"/missing.bin")
	time.Sleep(3500 * time.Millisecond)
	if n := atomic.LoadInt32(&shutdowns); n != 0 {
		t.Fatalf("turned the computer off although a download failed")
	}
	run(srv.URL+"/ok.bin", srv.URL+"/ok2.bin")
	for i := 0; i < 60 && atomic.LoadInt32(&shutdowns) == 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if atomic.LoadInt32(&shutdowns) != 1 {
		t.Fatal("a queue that finished cleanly should run its action")
	}
}

// privateWebSeedTorrent is webSeedTorrent with the private flag set.
func privateWebSeedTorrent(t *testing.T) []byte {
	tor, _, _ := webSeedTorrent(t)
	v, _ := bencode.Decode(tor)
	top := v.(map[string]any)
	top["info"].(map[string]any)["private"] = int64(1)
	return bencode.Encode(top)
}

func addTorrentBytes(t *testing.T, m *Manager, tor []byte, skip []bool) (*Download, *Metainfo) {
	mi, err := parseTorrent(tor)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.rememberTorrent(mi, tor)
	m.mu.Unlock()
	d, err := m.AddTorrent(TorrentAdd{Hash: mi.HashHex(), Skip: skip, Dir: filepath.Join(m.settings.DownloadDir, "got"), Start: true})
	if err != nil {
		t.Fatal(err)
	}
	return d, mi
}

func runningTorrent(t *testing.T, m *Manager, id string) *torrent {
	for i := 0; i < 100; i++ {
		m.mu.Lock()
		tr := m.torrents[id]
		m.mu.Unlock()
		if tr != nil {
			return tr
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("torrent not running")
	return nil
}

// A5a: unticking a file must never leave pieces "had" that would now read as zeros.
func TestTorrentUntickedFileNeverServesZeros(t *testing.T) {
	tor, _, _ := webSeedTorrent(t)
	m := torrentManager(t)
	d, mi := addTorrentBytes(t, m, tor, nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	tr := runningTorrent(t, m, d.ID) // sharing
	if err := m.SetTorrentFiles(d.ID, []bool{false, true}); err != nil {
		t.Fatal(err)
	}
	// piece 9 holds the end of a.bin and the start of b.txt: it has to be fetched again, so the
	// part of it that now lives in the hidden parts file is real data
	time.Sleep(300 * time.Millisecond)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	tr = runningTorrent(t, m, d.ID)
	tr.mu.Lock()
	st := tr.st
	have := append(bitfield(nil), tr.have...)
	tr.mu.Unlock()
	for p := 0; p < mi.NumPieces(); p++ {
		if !have.has(p) {
			continue
		}
		buf := make([]byte, mi.PieceSize(p))
		if err := st.ReadAt(buf, int64(p)*mi.PieceLen); err != nil {
			t.Fatal(err)
		}
		if h := sha1.Sum(buf); !bytes.Equal(h[:], mi.PieceHash(p)) {
			t.Fatalf("piece %d is marked as had but reads back wrong (it would be shared as zeros)", p)
		}
	}
	if have.has(10) || have.has(12) {
		t.Fatal("pieces only in the unticked file should be dropped")
	}
}

// readPeerMsg reads one length-prefixed peer message.
func readPeerMsg(c net.Conn) ([]byte, error) {
	var l [4]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint32(l[:]))
	_, err := io.ReadFull(c, b)
	return b, err
}

// A5b: a private torrent doesn't offer peer exchange (BEP 27).
func TestPrivateTorrentHasNoPeerExchange(t *testing.T) {
	m := torrentManager(t)
	d, mi := addTorrentBytes(t, m, privateWebSeedTorrent(t), nil)
	if !mi.Private {
		t.Fatal("not private")
	}
	waitFor(t, m, d, StCompleted, 60*time.Second)
	runningTorrent(t, m, d.ID)
	m.mu.Lock()
	port := m.tport
	m.mu.Unlock()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	var pid [20]byte
	copy(pid[:], "-XX0001-abcdefghijkl")
	h := handshakeBytes(mi.InfoHash, pid)
	h[25] |= 0x10 // we speak the extension protocol
	c.Write(h)
	var reply [68]byte
	if _, err := io.ReadFull(c, reply[:]); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		msg, err := readPeerMsg(c)
		if err != nil {
			t.Fatal(err)
		}
		if len(msg) > 2 && msg[0] == 20 && msg[1] == 0 {
			v, _ := bencode.Decode(msg[2:])
			ext := bencode.Dict(v.(map[string]any), "m")
			if _, ok := ext["ut_pex"]; ok {
				t.Fatalf("private torrent offers peer exchange: %v", ext)
			}
			if _, ok := ext["ut_metadata"]; !ok {
				t.Fatal("extension handshake without metadata")
			}
			return
		}
	}
	t.Fatal("no extension handshake")
}

// A5c: "completed" goes to the tracker once, and the tracker's min interval is kept.
func TestTrackerGetsCompletedOnce(t *testing.T) {
	var mu sync.Mutex
	events := map[string]int{}
	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		events[r.URL.Query().Get("event")]++
		mu.Unlock()
		w.Write(bencode.Encode(map[string]any{"interval": int64(1800), "min interval": int64(900), "peers": "", "tracker id": "abc"}))
	}))
	defer tracker.Close()
	tor, _, _ := webSeedTorrent(t)
	v, _ := bencode.Decode(tor)
	top := v.(map[string]any)
	top["announce"] = tracker.URL + "/announce"
	m := torrentManager(t)
	d, _ := addTorrentBytes(t, m, bencode.Encode(top), nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	time.Sleep(6 * time.Second) // a few tracker-loop rounds while sharing with no peers
	mu.Lock()
	defer mu.Unlock()
	if events["started"] != 1 || events["completed"] != 1 || events[""] != 0 {
		t.Fatalf("tracker saw %v; want one started, one completed, no extra announces", events)
	}
}

// A5d: checking a piece that another source already finished doesn't crash.
func TestDuplicatePieceCheckIsHarmless(t *testing.T) {
	tor, _, _ := webSeedTorrent(t)
	m := torrentManager(t)
	d, mi := addTorrentBytes(t, m, tor, nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	tr := runningTorrent(t, m, d.ID)
	f, err := os.OpenFile(filepath.Join(m.settings.DownloadDir, "got", "Pack", "a.bin"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt(make([]byte, 100), 0) // damage piece 0 on disk
	f.Close()
	tr.checkPiece(0) // no piece in progress: must simply return
	tr.mu.Lock()
	ok := tr.have.has(0)
	tr.mu.Unlock()
	if !ok {
		t.Fatal("a stray check changed the piece's state")
	}
	_ = mi
}

// A5e: changing the torrent port while torrents run doesn't hang.
func TestTorrentPortChangeWhileRunning(t *testing.T) {
	tor, _, _ := webSeedTorrent(t)
	m := torrentManager(t)
	d, _ := addTorrentBytes(t, m, tor, nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	tr := runningTorrent(t, m, d.ID)
	done := make(chan bool)
	go func() {
		for i := 0; i < 20; i++ {
			m.mu.Lock()
			m.settings.Torrent.Port = 0
			m.mu.Unlock()
			m.restartTorrentListener()
			tr.mu.Lock()
			tr.rechoke()
			tr.mu.Unlock()
		}
		done <- true
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("port change deadlocked")
	}
}

// A5f: the upload limit holds across many senders.
func TestUploadLimitIsShared(t *testing.T) {
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.UpKB = 100
	m.mu.Unlock()
	var sent int64
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				m.upLimiter().Wait(ctx, 16<<10)
				if ctx.Err() == nil {
					atomic.AddInt64(&sent, 16<<10)
				}
			}
		}()
	}
	wg.Wait()
	// 2 s at 100 KB/s, plus a short burst
	if s := atomic.LoadInt64(&sent); s > 320<<10 {
		t.Fatalf("sent %d KB in 2 s with a 100 KB/s limit", s>>10)
	}
}

// A5g: a torrent's own speed limit applies again after a restart.
func TestTorrentKeepsItsSpeedLimit(t *testing.T) {
	tor, _, _ := webSeedTorrent(t)
	m := torrentManager(t)
	mi, _ := parseTorrent(tor)
	m.mu.Lock()
	m.rememberTorrent(mi, tor)
	m.mu.Unlock()
	d, err := m.AddTorrent(TorrentAdd{Hash: mi.HashHex(), Dir: filepath.Join(m.settings.DownloadDir, "got")})
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	d.LimitKB = 50
	m.mu.Unlock()
	m.Start([]string{d.ID})
	runningTorrent(t, m, d.ID)
	if r := d.lim.Rate(); r != 50<<10 {
		t.Fatalf("limit after start = %d", r)
	}
}

// A6: an audio rendition with no URI is the audio inside the video, and it's kept.
func TestHLSKeepsAudioInsideTheVideo(t *testing.T) {
	master := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Español",LANGUAGE="es",AUTOSELECT=YES,URI="es/audio.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=2000000,RESOLUTION=1280x720,AUDIO="aud"
v720.m3u8
`
	vs, _, err := parseM3U8("https://example.com/m/master.m3u8", master)
	if err != nil || len(vs) != 1 {
		t.Fatal(err)
	}
	if vs[0].AudioURL != "" {
		t.Fatalf("picked separate audio %q instead of the default inside the video", vs[0].AudioURL)
	}
	withURI := strings.Replace(master, `DEFAULT=YES,AUTOSELECT=YES`, `DEFAULT=YES,AUTOSELECT=YES,URI="en/audio.m3u8"`, 1)
	vs, _, _ = parseM3U8("https://example.com/m/master.m3u8", withURI)
	if vs[0].AudioURL != "https://example.com/m/en/audio.m3u8" {
		t.Fatalf("default audio track: %q", vs[0].AudioURL)
	}
	desc := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English (described)",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,CHARACTERISTICS="public.accessibility.describes-video",URI="ad.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",AUTOSELECT=YES,URI="main.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=2000000,AUDIO="aud"
v.m3u8
`
	vs, _, _ = parseM3U8("https://example.com/m/master.m3u8", desc)
	if vs[0].AudioURL != "https://example.com/m/main.m3u8" {
		t.Fatalf("picked audio description by default: %q", vs[0].AudioURL)
	}
}

// A7: the grabber follows the site after its start page redirects, and sends a typed login only
// to that site's files.
func TestGrabberFollowsRedirectedSiteAndScopesLogins(t *testing.T) {
	otherURL := "http://127.0.0.2:9" // another site (only listed, never fetched)
	var site *httptest.Server
	site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			if strings.HasPrefix(r.Host, "localhost") { // the "apex" sends you to the real site
				http.Redirect(w, r, site.URL+"/home", http.StatusFound)
				return
			}
		case "/home":
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprintf(w, `<a href="/page2">more</a><a href="/files/one.zip">one</a><a href="%s/elsewhere.zip">other site</a>`, otherURL)
			return
		case "/page2":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<a href="/files/two.zip">two</a>`))
			return
		}
		http.NotFound(w, r)
	}))
	defer site.Close()
	start := strings.Replace(site.URL, "127.0.0.1", "localhost", 1) + "/"
	m := newTestManager(t)
	j, err := m.grab.Start(GrabOptions{URL: start, Depth: 2, Scope: "site", Exts: "zip", CrossSite: true,
		Headers: map[string]string{"Cookie": "session=secret"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	state := func() string { j.mu.Lock(); defer j.mu.Unlock(); return j.State }
	for i := 0; i < 200 && state() == "running"; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	j.mu.Lock()
	files := append([]GrabFile{}, j.Files...)
	j.mu.Unlock()
	var names []string
	for _, f := range files {
		names = append(names, f.Name)
	}
	if len(names) != 3 {
		t.Fatalf("found %v; want one.zip, two.zip (behind the redirect) and elsewhere.zip", names)
	}
	j.mu.Lock()
	opts := j.Opts
	j.mu.Unlock()
	m.grab.addDownloadsWith(files, opts, m.settings.DownloadDir, false, "main", false, nil)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.list {
		hasCookie := d.Headers["Cookie"] != ""
		if strings.Contains(d.URL, "elsewhere") == hasCookie {
			t.Fatalf("%s: cookie sent = %v", d.URL, hasCookie)
		}
	}
}

// A9: a scanner's own error isn't reported as a threat.
func TestScannerExitCodes(t *testing.T) {
	cases := []struct {
		name   string
		code   int
		threat bool
	}{{"ClamAV", 1, true}, {"ClamAV", 2, false}, {"Microsoft Defender", 2, true}, {"Microsoft Defender", 1, false}, {"myscan", 3, true}}
	for _, c := range cases {
		if got := scanFoundThreat(c.name, c.code); got != c.threat {
			t.Errorf("%s exit %d: threat=%v", c.name, c.code, got)
		}
	}
}

// A11: a damaged list falls back to its backup and is kept aside, never written over.
func TestDamagedListFallsBackToBackup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "downloads.json")
	writeAtomic(p, []byte(`[{"id":"a1","url":"https://example.com/one.zip","status":"completed"}]`))
	writeAtomic(p, []byte(`[{"id":"a2","url":"https://example.com/two.zip","status":"completed"}]`)) // the first is now the .bak
	os.WriteFile(p, []byte(`[{"id":"a2","url":"https://exa`), 0o644)                                 // a damaged write
	list, note := loadState[[]*Download](p, nil, "download list")
	if len(list) != 1 || list[0].ID != "a1" || !strings.Contains(note, "backup") {
		t.Fatalf("list %v note %q", list, note)
	}
	kept, _ := filepath.Glob(p + ".damaged-*")
	if len(kept) != 1 {
		t.Fatal("damaged file not kept aside")
	}
	// a missing file with a backup (a save that was cut off) loads quietly
	os.Remove(p)
	if list, note := loadState[[]*Download](p, nil, "download list"); len(list) != 1 || note != "" {
		t.Fatalf("interrupted save: %v %q", list, note)
	}
}

// A13: invisible direction marks can't disguise a file's type.
func TestNamesLoseInvisibleFormatCharacters(t *testing.T) {
	if got := sanitizeName("invoice‮fdp.exe"); got != "invoicefdp.exe" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeName("family 👨‍👩‍👧.jpg"); !strings.Contains(got, "‍") {
		t.Fatal("joiners in emoji should stay")
	}
}

// Downloads are named through sanitizeName, which must stay the shared file-name cleaner.
func TestSanitizeNameIsCleanFileName(t *testing.T) {
	for _, n := range []string{"plain.txt", `a<b>c:d"e/f\g|h?i*j.txt`, "..", "con.txt", "evil.lnk", strings.Repeat("a", 300) + ".mp4", strings.Repeat("日本", 60) + ".tar.gz"} {
		if a, b := filename.Clean(n), sanitizeName(n); a != b {
			t.Errorf("%q: clean %q, sanitize %q", n, a, b)
		}
	}
}

// A14: torrents, mirrors and checksums survive export and import.
func TestExportImportKeepsTorrentsAndChecks(t *testing.T) {
	tor, _, _ := webSeedTorrent(t)
	m := torrentManager(t)
	mi, _ := parseTorrent(tor)
	m.mu.Lock()
	m.rememberTorrent(mi, tor)
	m.mu.Unlock()
	if _, err := m.AddTorrent(TorrentAdd{Hash: mi.HashHex(), Skip: []bool{false, true}}); err != nil {
		t.Fatal(err)
	}
	d := m.Add(AddReq{URL: "https://example.com/big.iso", Mirrors: []string{"https://mirror.example.org/big.iso"},
		Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"})
	if d == nil {
		t.Fatal("add")
	}
	b, _ := m.Export("json", nil, false)
	m2 := torrentManager(t)
	res, err := m2.Import(string(b), "", false, true)
	if err != nil || res.Added != 2 {
		t.Fatalf("import: %+v %v", res, err)
	}
	m2.mu.Lock()
	defer m2.mu.Unlock()
	var gotT, gotD bool
	for _, x := range m2.list {
		if x.Torrent != nil {
			gotT = x.Torrent.InfoHash == mi.HashHex() && x.Torrent.HasMeta && len(x.Torrent.Skip) == 2 && x.Torrent.Skip[1]
		} else {
			gotD = len(x.Mirrors) == 1 && x.Check != nil && x.Check.Algo == "sha256"
		}
	}
	if !gotT || !gotD {
		t.Fatalf("torrent kept: %v, mirrors and checksum kept: %v", gotT, gotD)
	}
	// a plain list of links takes magnet links too
	m3 := torrentManager(t)
	if r, _ := m3.Import("magnet:?xt=urn:btih:"+mi.HashHex()+"&dn=Pack\nhttps://example.com/x.zip\n", "", false, true); r.Added != 2 {
		t.Fatalf("text import: %+v", r)
	}
}

// A15: a stream's sync check ignores sign-in tokens that change on every request.
func TestPlaylistFingerprintIgnoresTokens(t *testing.T) {
	a := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin?token=1\"\n#EXTINF:4,\nseg1.ts?token=abc\n#EXT-X-ENDLIST\n"
	b := strings.NewReplacer("token=1", "token=2", "token=abc", "token=xyz").Replace(a)
	if playlistSig(a) != playlistSig(b) {
		t.Fatal("tokens changed the fingerprint")
	}
	if playlistSig(a) == playlistSig(strings.Replace(a, "seg1", "seg2", 1)) {
		t.Fatal("a real change went unnoticed")
	}
	m := newTestManager(t)
	if changed, note := m.remoteChanged(&Download{Torrent: &TorrentState{}, Dir: os.TempDir(), FileName: "."}); changed || note != "" {
		t.Fatal("torrents have nothing to sync")
	}
}

// A16: with only torrents running, polite mode measures a stand-in address instead of switching off.
func TestPoliteTargetWithOnlyTorrents(t *testing.T) {
	m := newTestManager(t)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.list = append(m.list, &Download{ID: "t", URL: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567", Torrent: &TorrentState{}, running: true, speed: 5e6})
	if addr, _ := m.politeTarget(); addr != politeReflector {
		t.Fatalf("target %q", addr)
	}
	setProxySettings(ProxySettings{Mode: "none"}) // (this test machine has a proxy in its environment)
	defer setProxySettings(ProxySettings{Mode: "system"})
	m.list = append(m.list, &Download{ID: "h", URL: "https://files.example.com/a.zip", running: true, speed: 1e6})
	if addr, _ := m.politeTarget(); addr != "files.example.com:443" {
		t.Fatalf("target %q", addr)
	}
}

// A17: a phone that unpairs itself can't get back in with its old key.
func TestPhoneUnpairRevokesKey(t *testing.T) {
	m, pc, _ := phoneManager(t)
	if code, body := pc.do("POST", "hello", map[string]any{"name": "Test phone"}, nil); code != 200 {
		t.Fatalf("hello: %d %s", code, body)
	}
	if code, body := pc.do("POST", "unpair", map[string]any{}, nil); code != 200 {
		t.Fatalf("unpair: %d %s", code, body)
	}
	m.mu.Lock()
	n := len(m.settings.Phone.Devices)
	m.mu.Unlock()
	if n != 0 {
		t.Fatal("device still listed")
	}
	if code, _ := pc.do("POST", "state", map[string]any{}, nil); code == 200 {
		t.Fatal("old key still works")
	}
}

var _ = base64.StdEncoding
var _ = json.Marshal
