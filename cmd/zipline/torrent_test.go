package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zipline/internal/bencode"
)

// ---------------------------------------------------------------- unit tests

func TestMagnetLinks(t *testing.T) {
	mg, err := parseMagnet("magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a&dn=Ubuntu+24.04+Desktop&tr=udp%3A%2F%2Ftracker.example.org%3A1337&tr=http%3A%2F%2Ft.example%2Fannounce&tr=javascript:alert(1)&x.pe=10.0.0.5:51413&so=0,2-4")
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(mg.InfoHash[:]) != "c12fe1c06bba254a9dc9f519b335aa7c1367a88a" || mg.Name != "Ubuntu 24.04 Desktop" || len(mg.Trackers) != 2 || len(mg.Peers) != 1 || fmt.Sprint(mg.Select) != "[0 2 3 4]" {
		t.Fatalf("%+v", mg)
	}
	b32, err := parseMagnet("magnet:?xt=urn:btih:YEX6DQDLXISUVHOJ6UM3GNNKPQJWPKEK")
	if err != nil || b32.InfoHash != mg.InfoHash {
		t.Fatalf("base32: %v", err)
	}
	if _, err := parseMagnet("magnet:?xt=urn:btmh:1220abcd"); err != errV2Only {
		t.Fatalf("v2 only: %v", err)
	}
	if _, err := parseMagnet("magnet:?dn=nothing"); err == nil {
		t.Fatal("no hash")
	}
	if l := magnetLink(mg.InfoHash, "a b", []string{"udp://t:1"}); !strings.HasPrefix(l, "magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a&dn=a+b&tr=udp") {
		t.Fatal(l)
	}
}

func TestTorrentPathsAreSafe(t *testing.T) {
	info := bencode.Encode(map[string]any{"name": "..", "piece length": int64(16384), "pieces": string(make([]byte, 20)),
		"files": []any{
			map[string]any{"length": int64(5), "path": []any{"..", "..", "etc", "passwd"}},
			map[string]any{"length": int64(5), "path": []any{"CON.txt"}},
			map[string]any{"length": int64(5), "path": []any{"a", "b:c?.txt"}},
			map[string]any{"length": int64(5), "path": []any{"A", "B:C?.TXT"}},
		}})
	mi, err := parseInfo(info)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range mi.Files {
		got = append(got, strings.Join(f.Path, "/"))
	}
	if strings.Contains(strings.Join(got, "|"), "..") || got[1] != "_CON.txt" || got[2] != "a/b_c_.txt" || got[3] == got[2] || mi.Name == ".." {
		t.Fatalf("paths %q name %q", got, mi.Name)
	}
	if _, err := parseInfo(bencode.Encode(map[string]any{"name": "x", "piece length": int64(16384), "length": int64(40000), "pieces": string(make([]byte, 20))})); err == nil {
		t.Fatal("pieces that don't add up must be refused")
	}
}

// ---------------------------------------------------------------- a tracker for tests (HTTP and UDP)

type testTracker struct {
	mu    sync.Mutex
	peers map[string]map[string]netip.AddrPort
	hits  map[string]int
	srv   *httptest.Server
	udp   *net.UDPConn

	httpFail int // the HTTP tracker's next answers are failures (the next tier is asked then: BEP 12)
}

func newTestTracker(t *testing.T) *testTracker {
	tt := &testTracker{peers: map[string]map[string]netip.AddrPort{}, hits: map[string]int{}}
	tt.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tt.mu.Lock()
		fail := tt.httpFail > 0
		if fail {
			tt.httpFail--
			tt.hits["http"]++
		}
		tt.mu.Unlock()
		if fail {
			w.Write(bencode.Encode(map[string]any{"failure reason": "busy, try another tracker"}))
			return
		}
		port, _ := strconv.Atoi(q.Get("port"))
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip, _ := netip.ParseAddr(host)
		peers := tt.add("http", q.Get("info_hash"), netip.AddrPortFrom(ip, uint16(port)), q.Get("event"))
		var compact []byte
		for _, p := range peers {
			compact = append(compact, compactPeer(p)...)
		}
		w.Write(bencode.Encode(map[string]any{"interval": int64(60), "complete": int64(1), "incomplete": int64(len(peers)), "peers": string(compact)}))
	}))
	t.Cleanup(tt.srv.Close)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tt.udp = conn
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if n >= 16 && binary.BigEndian.Uint64(buf) == udpProtocolID && binary.BigEndian.Uint32(buf[8:]) == 0 {
				out := binary.BigEndian.AppendUint32(nil, 0)
				out = append(out, buf[12:16]...)
				out = binary.BigEndian.AppendUint64(out, 0xC0FFEE)
				conn.WriteToUDPAddrPort(out, from)
				continue
			}
			if n >= 98 && binary.BigEndian.Uint32(buf[8:]) == 1 && binary.BigEndian.Uint64(buf) == 0xC0FFEE {
				ih := string(buf[16:36])
				port := binary.BigEndian.Uint16(buf[96:])
				ev := map[uint32]string{1: "completed", 2: "started", 3: "stopped"}[binary.BigEndian.Uint32(buf[80:])]
				peers := tt.add("udp", ih, netip.AddrPortFrom(from.Addr().Unmap(), port), ev)
				out := binary.BigEndian.AppendUint32(nil, 1)
				out = append(out, buf[12:16]...)
				out = binary.BigEndian.AppendUint32(out, 60)
				out = binary.BigEndian.AppendUint32(out, uint32(len(peers)))
				out = binary.BigEndian.AppendUint32(out, 1)
				for _, p := range peers {
					out = append(out, compactPeer(p)...)
				}
				conn.WriteToUDPAddrPort(out, from)
			}
		}
	}()
	return tt
}

func (tt *testTracker) add(kind, ih string, who netip.AddrPort, event string) []netip.AddrPort {
	tt.mu.Lock()
	defer tt.mu.Unlock()
	tt.hits[kind]++
	m := tt.peers[ih]
	if m == nil {
		m = map[string]netip.AddrPort{}
		tt.peers[ih] = m
	}
	if event == "stopped" {
		delete(m, who.String())
	} else if who.Port() != 0 {
		m[who.String()] = who
	}
	var out []netip.AddrPort
	for k, p := range m {
		if k != who.String() {
			out = append(out, p)
		}
	}
	return out
}

func (tt *testTracker) httpURL() string { return tt.srv.URL + "/announce" }
func (tt *testTracker) udpURL() string  { return "udp://" + tt.udp.LocalAddr().String() + "/announce" }

// ---------------------------------------------------------------- libtorrent, a real BitTorrent engine

func ltPython(t *testing.T) string {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 isn't installed")
	}
	if exec.Command(py, "-c", "import libtorrent").Run() != nil {
		t.Skip("libtorrent for Python isn't installed (pip install libtorrent)")
	}
	return py
}

type ltProc struct {
	cmd   *exec.Cmd
	lines chan map[string]any
}

func ltStart(t *testing.T, py string, args ...string) *ltProc {
	cmd := exec.Command(py, append([]string{"testdata/ltpeer.py"}, args...)...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &ltProc{cmd: cmd, lines: make(chan map[string]any, 1000)}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				p.lines <- m
			}
		}
		close(p.lines)
	}()
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return p
}

func (p *ltProc) wait(t *testing.T, key string, max time.Duration) map[string]any {
	t.Helper()
	timer := time.After(max)
	for {
		select {
		case m, ok := <-p.lines:
			if !ok {
				t.Fatalf("libtorrent helper ended before %q", key)
			}
			if os.Getenv("ZL_LTDEBUG") != "" {
				t.Logf("lt: %v", m)
			}
			if _, has := m[key]; has {
				return m
			}
		case <-timer:
			t.Fatalf("libtorrent helper: no %q in %v", key, max)
		}
	}
}

// makeTestFiles writes a small folder of files and returns their contents by relative path.
func makeTestFiles(t *testing.T, root string) map[string][]byte {
	files := map[string][]byte{
		"intro.txt":          []byte("Zipline torrent test\n"),
		"video/clip one.mp4": randData(700 << 10),
		"video/clip two.mp4": randData(1<<20 + 12345),
		"data.bin":           randData(333 << 10),
	}
	for p, b := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func checkFiles(t *testing.T, root string, want map[string][]byte, skip ...string) {
	t.Helper()
	for p, b := range want {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		skipped := false
		for _, s := range skip {
			skipped = skipped || s == p
		}
		if skipped {
			if err == nil && bytes.Equal(got, b) {
				t.Errorf("%s wasn't chosen but was downloaded whole", p)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s: %v (%d bytes, want %d)", p, err, len(got), len(b))
		}
	}
}

type swarm struct {
	dir     string
	torrent string
	magnet  string
	hash    string
	files   map[string][]byte
	tracker *testTracker
	seeder  *ltProc
}

// newSwarm makes a torrent with libtorrent and starts a libtorrent seeder that announces to our tracker.
func newSwarm(t *testing.T, py string, webseed string) *swarm {
	sw := &swarm{dir: t.TempDir(), tracker: newTestTracker(t)}
	root := filepath.Join(sw.dir, "Holiday Pack")
	sw.files = makeTestFiles(t, root)
	mk := ltStart(t, py, "make", root, sw.tracker.httpURL()+","+sw.tracker.udpURL(), webseedOr(webseed), "65536")
	info := mk.wait(t, "torrent", 30*time.Second)
	sw.torrent, sw.magnet, sw.hash = info["torrent"].(string), info["magnet"].(string), info["hash"].(string)
	return sw
}

func webseedOr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (sw *swarm) seed(t *testing.T, py string) int {
	sw.seeder = ltStart(t, py, "seed", sw.torrent, sw.dir)
	r := sw.seeder.wait(t, "port", 30*time.Second)
	if r["seeding"] != true {
		t.Fatalf("the libtorrent seeder isn't seeding: %v", r)
	}
	return int(r["port"].(float64))
}

func torrentManager(t *testing.T) *Manager {
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Torrent = defaultTorrentSettings()
	m.settings.Torrent.NoDHT = true // tests stay on this computer
	m.settings.SpeedLimitOn = false
	m.mu.Unlock()
	return m
}

func addTorrentFile(t *testing.T, m *Manager, path string, skip []bool) *Download {
	b, err := os.ReadFile(path)
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
	d, err := m.AddTorrent(TorrentAdd{Hash: mi.HashHex(), Skip: skip, Dir: filepath.Join(m.settings.DownloadDir, "got"), Start: true})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// ---------------------------------------------------------------- interoperability

// A .torrent made by libtorrent, seeded by libtorrent, found through our HTTP and UDP trackers.
func TestTorrentFromLibtorrent(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seed(t, py)
	m := torrentManager(t)
	d := addTorrentFile(t, m, sw.torrent, nil)
	waitFor(t, m, d, StCompleted, 90*time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), sw.files)
	m.mu.Lock()
	size, name, cat := d.Size, d.FileName, d.Category
	m.mu.Unlock()
	var total int64
	for _, b := range sw.files {
		total += int64(len(b))
	}
	if size != total || name != "Holiday Pack" || cat != "Video" {
		t.Fatalf("size %d (want %d), name %q, category %q", size, total, name, cat)
	}
	sw.tracker.mu.Lock()
	t.Logf("tracker announces: %v", sw.tracker.hits)
	sw.tracker.mu.Unlock()
}

// Zipline starts first and waits; the seeder turns up later and connects to Zipline.
func TestTorrentSeederArrivesLater(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	m := torrentManager(t)
	d := addTorrentFile(t, m, sw.torrent, nil)
	time.Sleep(3 * time.Second)
	sw.seed(t, py)
	waitFor(t, m, d, StCompleted, 120*time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), sw.files)
}

// A magnet link: the file list comes from the libtorrent seeder (BEP 9), found via the UDP tracker.
func TestMagnetFromLibtorrent(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seed(t, py)
	m := torrentManager(t)
	ih, _ := hex.DecodeString(sw.hash)
	magnet := magnetLink([20]byte(ih), "", []string{sw.tracker.udpURL()})
	d, err := m.AddTorrent(TorrentAdd{Magnet: magnet, Dir: filepath.Join(m.settings.DownloadDir, "got"), Start: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, d, StCompleted, 90*time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), sw.files)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !d.Torrent.HasMeta || d.FileName != "Holiday Pack" {
		t.Fatalf("file list: %+v", d.Torrent)
	}
	if _, err := os.Stat(m.torrentPath(sw.hash)); err != nil {
		t.Fatal("the file list should be saved for next time")
	}
}

// After finishing, Zipline shares: a libtorrent leecher gets the whole torrent (file list
// included) from Zipline alone.
func TestZiplineSharesWithLibtorrent(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seed(t, py)
	m := torrentManager(t)
	d := addTorrentFile(t, m, sw.torrent, nil)
	waitFor(t, m, d, StCompleted, 90*time.Second)
	sw.seeder.cmd.Process.Kill() // only Zipline has it now
	m.mu.Lock()
	port := m.tport
	seeding := m.torrents[d.ID] != nil
	m.mu.Unlock()
	if !seeding {
		t.Fatal("a finished torrent should keep sharing")
	}
	out := t.TempDir()
	leech := ltStart(t, py, "leech", sw.magnet, out, fmt.Sprintf("127.0.0.1:%d", port))
	r := leech.wait(t, "done", 120*time.Second)
	if r["done"] != true {
		t.Fatal("libtorrent couldn't download from Zipline")
	}
	checkFiles(t, filepath.Join(out, "Holiday Pack"), sw.files)
	time.Sleep(1500 * time.Millisecond)
	m.mu.Lock()
	t.Logf("Zipline uploaded %d bytes", d.Torrent.Uploaded)
	m.mu.Unlock()
}

// Only some files: the rest isn't downloaded, and the hidden parts file goes away at the end.
func TestTorrentChosenFiles(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seed(t, py)
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.SeedRatio = 0 // don't share afterwards
	m.mu.Unlock()
	b, _ := os.ReadFile(sw.torrent)
	mi, _ := parseTorrent(b)
	skip := make([]bool, len(mi.Files))
	var skipped []string
	for i, f := range mi.Files {
		if strings.Contains(strings.Join(f.Path, "/"), "clip two") {
			skip[i] = true
			skipped = append(skipped, strings.Join(f.Path, "/"))
		}
	}
	d := addTorrentFile(t, m, sw.torrent, skip)
	waitFor(t, m, d, StCompleted, 90*time.Second)
	root := filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack")
	checkFiles(t, root, sw.files, skipped...)
	time.Sleep(300 * time.Millisecond)
	if left, _ := filepath.Glob(filepath.Join(m.settings.DownloadDir, "got", ".*.parts")); len(left) > 0 {
		t.Fatalf("parts file left behind: %v", left)
	}
}

// Web seeds (BEP 19): no peers at all, the files come from a web server.
func TestTorrentWebSeed(t *testing.T) {
	py := ltPython(t)
	dir := t.TempDir()
	web := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer web.Close()
	root := filepath.Join(dir, "Holiday Pack")
	files := makeTestFiles(t, root)
	mk := ltStart(t, py, "make", root, "-", web.URL+"/", "65536")
	info := mk.wait(t, "torrent", 30*time.Second)
	m := torrentManager(t)
	d := addTorrentFile(t, m, info["torrent"].(string), nil)
	waitFor(t, m, d, StCompleted, 60*time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), files)
}

// A peer that sends damaged data gets found out: the piece is fetched again from someone else.
func TestTorrentPieceCheck(t *testing.T) {
	data := randData(200 << 10)
	var hashes []byte
	for off := 0; off < len(data); off += 32 << 10 {
		h := sha1.Sum(data[off:min(off+32<<10, len(data))])
		hashes = append(hashes, h[:]...)
	}
	info := bencode.Encode(map[string]any{"name": "x.bin", "piece length": int64(32 << 10), "pieces": string(hashes), "length": int64(len(data))})
	mi, err := parseInfo(info)
	if err != nil || mi.NumPieces() != 7 {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		h := sha1.Sum(data[i*(32<<10) : min((i+1)*(32<<10), len(data))])
		if !bytes.Equal(h[:], mi.PieceHash(i)) {
			t.Fatal("hash layout")
		}
	}
}

// The DHT: a magnet link with no trackers. libtorrent nodes on this computer stand in for
// the worldwide DHT; the seeder announces itself there and Zipline finds it.
func TestDHTFindsSeed(t *testing.T) {
	py := ltPython(t)
	node := ltStart(t, py, "dhtnode")
	nport := int(node.wait(t, "port", 20*time.Second)["port"].(float64))
	sw := newSwarm(t, py, "")
	boot := fmt.Sprintf("127.0.0.1:%d", nport)
	sw.seeder = ltStart(t, py, "seed", sw.torrent, sw.dir, boot)
	sw.seeder.wait(t, "port", 30*time.Second)
	old := dhtBootstrap
	dhtBootstrap = []string{boot}
	defer func() { dhtBootstrap = old }()
	// the seeder announces to the DHT (and to our tracker, which this magnet link leaves out)
	time.Sleep(4 * time.Second)
	m := torrentManager(t)
	m.mu.Lock()
	m.settings.Torrent.NoDHT = false
	m.mu.Unlock()
	ih, _ := hex.DecodeString(sw.hash)
	d, err := m.AddTorrent(TorrentAdd{Magnet: magnetLink([20]byte(ih), "", nil), Dir: filepath.Join(m.settings.DownloadDir, "got"), Start: true})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, d, StCompleted, 120*time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), sw.files)
	m.mu.Lock()
	tr := m.torrents[d.ID]
	m.mu.Unlock()
	if tr == nil {
		t.Fatal("should be sharing")
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	srcs := map[string]int{}
	for _, c := range tr.cands {
		srcs[c.src]++
	}
	if srcs["DHT"] == 0 {
		t.Fatalf("peers should come from the DHT: %v", srcs)
	}
	t.Logf("peer sources: %v", srcs)
}

// fakeBadSeed claims to have everything and sends junk for every block.
func fakeBadSeed(t *testing.T, ih [20]byte, pieces int) (netip.AddrPort, *int32) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var served int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var h [68]byte
				if _, err := io.ReadFull(c, h[:]); err != nil {
					return
				}
				var id [20]byte
				copy(id[:], "-XX0001-badbadbadbad")
				c.Write(handshakeBytes(ih, id))
				bf := make([]byte, (pieces+7)/8)
				for i := 0; i < pieces; i++ {
					bf[i/8] |= 0x80 >> (i % 8)
				}
				msg := func(b []byte) { c.Write(binary.BigEndian.AppendUint32(nil, uint32(len(b)))); c.Write(b) }
				msg(append([]byte{msgBitfield}, bf...))
				msg([]byte{msgUnchoke})
				for {
					var lb [4]byte
					if _, err := io.ReadFull(c, lb[:]); err != nil {
						return
					}
					m := make([]byte, binary.BigEndian.Uint32(lb[:]))
					if _, err := io.ReadFull(c, m); err != nil {
						return
					}
					if len(m) == 13 && m[0] == msgRequest {
						n := binary.BigEndian.Uint32(m[9:])
						out := append([]byte{msgPiece}, m[1:9]...)
						msg(append(out, randData(int(n))...))
						atomic.AddInt32(&served, 1)
					}
				}
			}(c)
		}
	}()
	ap, _ := netip.ParseAddrPort(ln.Addr().String())
	return ap, &served
}

func TestTorrentBadPeerIsShutOut(t *testing.T) {
	py := ltPython(t)
	sw := newSwarm(t, py, "")
	sw.seed(t, py)
	b, _ := os.ReadFile(sw.torrent)
	mi, _ := parseTorrent(b)
	bad, served := fakeBadSeed(t, mi.InfoHash, mi.NumPieces())
	sw.tracker.add("http", string(mi.InfoHash[:]), bad, "started")
	m := torrentManager(t)
	d := addTorrentFile(t, m, sw.torrent, nil)
	waitFor(t, m, d, StCompleted, 120*time.Second)
	checkFiles(t, filepath.Join(m.settings.DownloadDir, "got", "Holiday Pack"), sw.files)
	m.mu.Lock()
	tr := m.torrents[d.ID]
	m.mu.Unlock()
	if tr != nil {
		tr.mu.Lock()
		banned := tr.bannedAt[bad]
		tr.mu.Unlock()
		if !banned && atomic.LoadInt32(served) > 0 {
			t.Fatal("the peer that sent damaged data should be shut out")
		}
	}
	t.Logf("junk blocks sent by the bad peer: %d", atomic.LoadInt32(served))
}
