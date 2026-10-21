package main

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"zipline/internal/bencode"
)

// ---------------------------------------------------------------- helpers

// mlEntry is one <file> of a test metalink.
type mlEntry struct {
	name      string
	data      []byte
	fileHash  bool   // a whole-file SHA-256
	piece     int    // piece checksums of this length (0 = none)
	pieceAlgo string // "sha-1" or "sha-256"
	urls      []string
	locs      []string // per url (optional)
	torrent   string   // <metaurl mediatype="torrent">
	inTorrent string   // its name attribute
}

func mlDocOf(es ...mlEntry) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<metalink xmlns="urn:ietf:params:xml:ns:metalink">` + "\n")
	for _, e := range es {
		fmt.Fprintf(&b, "  <file name=\"%s\">\n    <size>%d</size>\n", e.name, len(e.data))
		if e.fileHash {
			fmt.Fprintf(&b, "    <hash type=\"sha-256\">%s</hash>\n", sha256hex(e.data))
		}
		if e.piece > 0 {
			fmt.Fprintf(&b, "    <pieces length=\"%d\" type=\"%s\">", e.piece, e.pieceAlgo)
			for _, h := range pieceHashesOf(e.data, e.piece, normAlgo(e.pieceAlgo)).Hashes {
				fmt.Fprintf(&b, "<hash>%s</hash>", h)
			}
			b.WriteString("</pieces>\n")
		}
		for i, u := range e.urls {
			loc := ""
			if i < len(e.locs) && e.locs[i] != "" {
				loc = ` location="` + e.locs[i] + `"`
			}
			fmt.Fprintf(&b, "    <url priority=\"%d\"%s>%s</url>\n", i+1, loc, u)
		}
		if e.torrent != "" {
			name := ""
			if e.inTorrent != "" {
				name = ` name="` + e.inTorrent + `"`
			}
			fmt.Fprintf(&b, "    <metaurl mediatype=\"torrent\"%s>%s</metaurl>\n", name, e.torrent)
		}
		b.WriteString("  </file>\n")
	}
	b.WriteString("</metalink>\n")
	return b.String()
}

func pieceHashesOf(data []byte, piece int, algo string) *PieceHashes {
	ph := &PieceHashes{Algo: algo, Length: int64(piece)}
	for off := 0; off < len(data); off += piece {
		var h string
		if algo == "sha1" {
			s := sha1.Sum(data[off:min(off+piece, len(data))])
			h = hex.EncodeToString(s[:])
		} else {
			s := sha256.Sum256(data[off:min(off+piece, len(data))])
			h = hex.EncodeToString(s[:])
		}
		ph.Hashes = append(ph.Hashes, h)
	}
	return ph
}

// serveDocs answers the given paths with fixed bodies (a metalink, a .torrent file).
func serveDocs(t *testing.T, docs map[string]string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := docs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, ".meta4"):
			w.Header().Set("Content-Type", "application/metalink4+xml")
		case strings.HasSuffix(r.URL.Path, ".torrent"):
			w.Header().Set("Content-Type", "application/x-bittorrent")
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// rangeStarts records where the ranges a host is asked for start.
func rangeStarts(h *fileHost) func() []int64 {
	var mu sync.Mutex
	var starts []int64
	h.route = func(w http.ResponseWriter, r *http.Request) bool {
		if v, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok {
			a, _, _ := strings.Cut(v, "-")
			if n, err := strconv.ParseInt(a, 10, 64); err == nil {
				mu.Lock()
				starts = append(starts, n)
				mu.Unlock()
			}
		}
		return false
	}
	return func() []int64 {
		mu.Lock()
		defer mu.Unlock()
		return append([]int64(nil), starts...)
	}
}

func viewOf(m *Manager, id string) DLView {
	for _, v := range m.Snapshot(map[string]bool{id: true}).Downloads {
		if v.ID == id {
			return v
		}
	}
	return DLView{}
}

// ---------------------------------------------------------------- piece checks

// One mirror serves damaged pieces: each is found as soon as it's written (long before the file is
// done), fetched again from another mirror, and after maxBadPieces the mirror is dropped, which the
// progress window's list of servers shows. The file comes out right.
func TestPieceCheckDropsDamagingMirror(t *testing.T) {
	const piece = 256 << 10
	data := randData(8 << 20)
	bad := newFileHost(t, data, 400<<10)
	good1 := newFileHost(t, data, 400<<10)
	good2 := newFileHost(t, data, 400<<10)
	bad.mangle = func(b []byte, _ int64) { // every other piece has a byte wrong
		for off := piece; off < len(b); off += 2 * piece {
			b[off+1000] ^= 0x5a
		}
	}
	badStarts := rangeStarts(bad)
	ml := serveDocs(t, map[string]string{"/big.meta4": metalinkDoc("big.iso", data, piece, bad.URL+"/big.iso", good1.URL+"/big.iso", good2.URL+"/big.iso")})
	m := newTestManager(t)
	m.settings.Conns = 6
	srv := (&Server{m: m, token: "t"}).routes()
	r := apiCall(t, srv, "add", map[string]any{"url": ml.URL + "/big.meta4", "meta": true, "metaFile": 0, "start": true})
	m.mu.Lock()
	d := m.byID[r["id"].(string)]
	m.mu.Unlock()
	var foundEarly, droppedShown bool
	for deadline := time.Now().Add(60 * time.Second); ; {
		v := viewOf(m, d.ID)
		if v.Status == StCompleted {
			break
		}
		if v.Status == StError || time.Now().After(deadline) {
			t.Fatalf("status %s: %s", v.Status, v.Error)
		}
		if v.PiecesBad > 0 && v.Done < v.Size*3/4 {
			foundEarly = true
		}
		for _, x := range v.Sources {
			if x.Damaged && x.Bad >= maxBadPieces && strings.HasPrefix(x.URL, bad.URL) && x.Dead != "" {
				droppedShown = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	fileIs(t, d, data)
	if !foundEarly {
		t.Error("damaged pieces should be found as they're written, not when the file is done")
	}
	if !droppedShown {
		t.Error("the progress window should show the mirror dropped for sending damaged data")
	}
	for _, a := range badStarts() { // a damaged piece is fetched again from another mirror
		if a%piece == 0 && a/piece%2 == 1 {
			t.Errorf("piece %d was asked of the mirror that damaged it", a/piece)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.Check == nil || d.Check.State != "ok" || !strings.Contains(d.Note, "damaged piece") || d.PieceOK != nil {
		t.Fatalf("check %+v, note %q, pieceOk %v", d.Check, d.Note, d.PieceOK)
	}
	if bad.bytesSent("") >= int64(len(data))/2 {
		t.Errorf("the damaging mirror sent %d KB", bad.bytesSent("")>>10)
	}
}

// Piece checksums work on their own: no checksum of the whole file is needed.
func TestPieceHashesWithoutFileHash(t *testing.T) {
	const piece = 128 << 10
	data := randData(3<<20 + 12345)
	bad := newFileHost(t, data, 300<<10)
	good := newFileHost(t, data, 300<<10)
	bad.mangle = func(b []byte, _ int64) { b[2*piece+5] ^= 1 }
	doc := mlDocOf(mlEntry{name: "pieces-only.bin", data: data, piece: piece, pieceAlgo: "sha-256", urls: []string{bad.URL + "/f", good.URL + "/f"}})
	files, err := parseMetalink(strings.NewReader(doc))
	if err != nil || files[0].Hash != nil || files[0].Pieces == nil {
		t.Fatalf("%+v %v", files, err)
	}
	m := newTestManager(t)
	m.settings.Conns = 4
	ds, err := m.AddMetalink(AddReq{URL: "http://example.invalid/x.meta4", Start: true}, files)
	if err != nil {
		t.Fatal(err)
	}
	d := ds[0]
	waitFor(t, m, d, StCompleted, 60*time.Second)
	fileIs(t, d, data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if d.Check != nil {
		t.Fatalf("no file checksum expected: %+v", d.Check)
	}
	if !strings.Contains(d.Note, "1 damaged piece") { // (the first mirror's first connection sends it)
		t.Fatalf("note %q", d.Note)
	}
}

// A server that can't send ranges can't send a damaged piece again: the download fails instead of
// passing a damaged file as complete.
func TestPieceCheckWithoutRanges(t *testing.T) {
	const piece = 64 << 10
	data := randData(300 << 10)
	damaged := append([]byte(nil), data...)
	damaged[piece+3] ^= 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(damaged)))
		w.Write(damaged)
	}))
	t.Cleanup(srv.Close)
	files, err := parseMetalink(strings.NewReader(mlDocOf(mlEntry{name: "f.bin", data: data, piece: piece, pieceAlgo: "sha-1", urls: []string{srv.URL + "/f.bin"}})))
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t)
	ds, err := m.AddMetalink(AddReq{URL: srv.URL + "/f.meta4", Start: true}, files)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, ds[0], StError, 20*time.Second)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !strings.Contains(ds[0].Error, "a piece of the file doesn't match the metalink's checksums") {
		t.Fatalf("error %q", ds[0].Error)
	}
}

// After a restart, pieces that were complete but never checked are checked: one that's damaged on
// disk is downloaded again, both while the rest downloads and when the whole file was already there.
func TestPieceCheckAfterRestart(t *testing.T) {
	const piece = 256 << 10
	data := randData(3 << 20) // 12 pieces
	h := newFileHost(t, data, 0)
	m := newTestManager(t)
	ph := pieceHashesOf(data, piece, "sha256")
	setup := func(name string, upto int64, damaged int) *Download {
		d := m.Add(AddReq{URL: h.URL + "/" + name, Name: name, Size: int64(len(data))})
		part := make([]byte, len(data))
		copy(part, data[:upto])
		part[damaged*piece+7] ^= 1 // complete, never checked, and wrong
		m.mu.Lock()
		d.Pieces, d.Resumable = ph, true
		m.mu.Unlock()
		os.MkdirAll(d.Dir, 0o755)
		if err := os.WriteFile(d.partPath(), part, 0o644); err != nil {
			t.Fatal(err)
		}
		d.mu.Lock()
		d.Segments = []*Segment{{Start: 0, End: upto - 1, Pos: upto}}
		if upto < int64(len(data)) {
			d.Segments = append(d.Segments, &Segment{Start: upto, End: int64(len(data)) - 1, Pos: upto})
		}
		d.PieceOK = newBitfield(len(ph.Hashes))
		d.PieceOK.set(0) // (checked before the restart)
		d.mu.Unlock()
		atomic.StoreInt64(&d.done, upto)
		return d
	}
	half := setup("half.bin", 6*piece, 4)
	whole := setup("whole.bin", int64(len(data)), 9)
	m.Start([]string{half.ID, whole.ID})
	waitFor(t, m, half, StCompleted, 30*time.Second)
	waitFor(t, m, whole, StCompleted, 30*time.Second)
	fileIs(t, half, data)
	fileIs(t, whole, data)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range []*Download{half, whole} {
		if !strings.Contains(d.Note, "1 damaged piece") {
			t.Errorf("%s: note %q", d.FileName, d.Note)
		}
	}
	if n := h.bytesSent("/whole.bin"); n < piece || n > 2*piece { // (a second connection may share the piece)
		t.Errorf("the finished file should fetch only its damaged piece again: %d bytes", n)
	}
}

// unmark carves a damaged piece out of the segments that hold it, leaving the rest as it was.
func TestUnmarkKeepsTheRest(t *testing.T) {
	d := &Download{Segments: []*Segment{{Start: 0, End: 99, Pos: 100}, {Start: 100, End: 299, Pos: 250}}}
	active := d.Segments[1]
	d.unmark(80, 160)
	var got []string
	for _, s := range d.Segments {
		got = append(got, fmt.Sprintf("%d-%d@%d", s.Start, s.End, s.Pos))
	}
	// (the part before it is done, the active segment keeps its connection's place)
	if want := []string{"0-79@80", "160-299@250", "80-159@80"}; !slices.Equal(got, want) || d.Segments[1] != active {
		t.Fatalf("segments %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------- torrents in metalinks

// A metalink with a torrent: the torrent engine downloads it, with the metalink's mirror as a web
// seed (no peers at all here). The torrent comes from libtorrent.
func TestMetalinkTorrentWithMirrorsAsWebSeeds(t *testing.T) {
	py := ltPython(t)
	dir := t.TempDir()
	data := randData(700 << 10)
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	mk := ltStart(t, py, "make", filepath.Join(dir, "disk.img"), "-", "-", "65536")
	tor, err := os.ReadFile(mk.wait(t, "torrent", 30*time.Second)["torrent"].(string))
	if err != nil {
		t.Fatal(err)
	}
	mirror := newFileHost(t, data, 0)
	var ml *httptest.Server
	docs := map[string]string{"/x.torrent": string(tor)}
	ml = serveDocs(t, docs)
	docs["/x.meta4"] = mlDocOf(mlEntry{name: "images/disk.img", data: data, fileHash: true, urls: []string{mirror.URL + "/pub/disk.img"}, torrent: ml.URL + "/x.torrent"})
	m := torrentManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, srv, "probe", map[string]any{"url": ml.URL + "/x.meta4"})
	fs := info["metalink"].(map[string]any)["files"].([]any)
	if f := fs[0].(map[string]any); f["torrent"] != true || f["name"] != "images/disk.img" || info["name"] != "images/disk.img" {
		t.Fatalf("probe: %v", info)
	}
	r := apiCall(t, srv, "add", map[string]any{"url": ml.URL + "/x.meta4", "meta": true, "metaFile": 0, "start": true, "name": info["name"], "dir": m.settings.DownloadDir})
	if r["error"] != nil {
		t.Fatal(r["error"])
	}
	m.mu.Lock()
	d := m.byID[r["id"].(string)]
	kind, seeds := d.Kind, append([]string(nil), d.Torrent.WebSeeds...)
	m.mu.Unlock()
	if kind != "torrent" || !slices.Contains(seeds, mirror.URL+"/pub/disk.img") {
		t.Fatalf("kind %q, web seeds %v", kind, seeds)
	}
	waitFor(t, m, d, StCompleted, 60*time.Second)
	got, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, "images", "disk.img"))
	if err != nil || string(got) != string(data) {
		t.Fatalf("file: %d bytes, %v", len(got), err)
	}
	if mirror.bytesSent("/pub/disk.img") < int64(len(data)) {
		t.Fatalf("the mirror served %d bytes as a web seed", mirror.bytesSent("/pub/disk.img"))
	}
}

// A torrent of several files: the metalink picks one, the mirrors' addresses become the web seeds'
// base (BEP 19), and the others aren't downloaded. A torrent that can't be fetched falls back to
// the mirrors.
func TestMetalinkMultiFileTorrentAndFallback(t *testing.T) {
	tor, a, b := webSeedTorrent(t) // (its own web seed is a server of its own; ours is below)
	v, _ := bencode.Decode(tor)
	top := v.(map[string]any)
	delete(top, "url-list") // only the metalink's mirrors serve it
	tor = bencode.Encode(top)
	web := t.TempDir()
	os.MkdirAll(filepath.Join(web, "pub", "Pack"), 0o755)
	os.WriteFile(filepath.Join(web, "pub", "Pack", "a.bin"), a, 0o644)
	os.WriteFile(filepath.Join(web, "pub", "Pack", "b.txt"), b, 0o644)
	mirror := httptest.NewServer(http.FileServer(http.Dir(web)))
	t.Cleanup(mirror.Close)
	docs := map[string]string{"/pack.torrent": string(tor)}
	ml := serveDocs(t, docs)
	docs["/pack.meta4"] = mlDocOf(
		mlEntry{name: "Pack/a.bin", data: a, urls: []string{mirror.URL + "/pub/Pack/a.bin", "ftp://ftp.example.invalid/Pack/a.bin"}, torrent: ml.URL + "/pack.torrent", inTorrent: "Pack/a.bin"},
		mlEntry{name: "Pack/b.txt", data: b, urls: []string{mirror.URL + "/pub/Pack/b.txt"}, torrent: ml.URL + "/pack.torrent", inTorrent: "Pack/b.txt"},
		mlEntry{name: "other.bin", data: b, urls: []string{mirror.URL + "/pub/Pack/b.txt"}, torrent: ml.URL + "/missing.torrent"},
	)
	m := torrentManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	dir := filepath.Join(m.settings.DownloadDir, "got")
	r := apiCall(t, srv, "add", map[string]any{"url": ml.URL + "/pack.meta4", "meta": true, "metaFile": 0, "start": true, "dir": dir})
	m.mu.Lock()
	d := m.byID[r["id"].(string)]
	skip, seeds := append([]bool(nil), d.Torrent.Skip...), append([]string(nil), d.Torrent.WebSeeds...)
	m.mu.Unlock()
	if !slices.Equal(skip, []bool{false, true}) || !slices.Equal(seeds, []string{mirror.URL + "/pub/"}) {
		t.Fatalf("skip %v, web seeds %v", skip, seeds)
	}
	waitFor(t, m, d, StCompleted, 60*time.Second)
	if got, err := os.ReadFile(filepath.Join(dir, "Pack", "a.bin")); err != nil || string(got) != string(a) {
		t.Fatalf("a.bin: %d bytes, %v", len(got), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Pack", "b.txt")); err == nil {
		t.Error("b.txt wasn't picked")
	}

	// the torrent is gone: the mirrors download the file as usual
	r = apiCall(t, srv, "add", map[string]any{"url": ml.URL + "/pack.meta4", "meta": true, "metaFile": 2, "start": true, "dir": dir})
	m.mu.Lock()
	d = m.byID[r["id"].(string)]
	kind := d.Kind
	m.mu.Unlock()
	if kind != "" {
		t.Fatalf("kind %q", kind)
	}
	waitFor(t, m, d, StCompleted, 30*time.Second)
	fileIs(t, d, b)

	// the same torrent again: it's already in the list, so nothing is fetched twice
	r = apiCall(t, srv, "add", map[string]any{"url": ml.URL + "/pack.meta4", "meta": true, "metaFile": 0, "start": true, "dir": dir})
	if e, _ := r["error"].(string); !strings.Contains(e, "already in the list") {
		t.Fatalf("adding it twice: %v", r)
	}
}

// ---------------------------------------------------------------- folders

// Folder paths in file names are kept, safely: no climbing out of the chosen folder, nothing absolute,
// every part cleaned like any other name.
func TestMetalinkFolderPaths(t *testing.T) {
	data := randData(64 << 10)
	h := newFileHost(t, data, 0)
	names := map[string][2]string{ // as written → folders, name
		"iso/x86/disk.iso":       {"iso/x86", "disk.iso"},
		`win\sub\setup.exe`:      {"win/sub", "setup.exe"},
		"../../../escape.bin":    {"", "escape.bin"},
		"/abs/path.bin":          {"abs", "path.bin"},
		`C:\Windows\evil.dll`:    {"Windows", "evil.dll"},
		"a/./b/../c.txt":         {"a/b", "c.txt"},
		"con/aux.txt":            {"_con", "_aux.txt"},
		"spa ced/ trail. /x.bin": {"spa ced/trail", "x.bin"},
	}
	var es []mlEntry
	var order []string
	for n := range names {
		order = append(order, n)
	}
	slices.Sort(order)
	for _, n := range order {
		es = append(es, mlEntry{name: n, data: data, urls: []string{h.URL + "/f"}})
	}
	es = append(es, mlEntry{name: "../", data: data, urls: []string{h.URL + "/f"}}) // nothing left of it
	files, err := parseMetalink(strings.NewReader(mlDocOf(es...)))
	if err != nil || len(files) != len(names) {
		t.Fatalf("%d files, %v", len(files), err)
	}
	for i, f := range files {
		if want := names[order[i]]; f.Dir != want[0] || f.Name != want[1] {
			t.Errorf("%q → %q + %q, want %q", order[i], f.Dir, f.Name, want)
		}
	}

	// all of them, into a chosen folder
	m := newTestManager(t)
	ml := serveDocs(t, map[string]string{"/f.meta4": mlDocOf(es...)})
	srv := (&Server{m: m, token: "t"}).routes()
	got := filepath.Join(m.settings.DownloadDir, "got")
	r := apiCall(t, srv, "add", map[string]any{"url": ml.URL + "/f.meta4", "meta": true, "metaFile": -1, "start": true, "dir": got})
	if ids, _ := r["ids"].([]any); len(ids) != len(names) {
		t.Fatalf("add: %v", r)
	}
	for _, id := range r["ids"].([]any) {
		m.mu.Lock()
		d := m.byID[id.(string)]
		m.mu.Unlock()
		waitFor(t, m, d, StCompleted, 20*time.Second)
	}
	for _, want := range names {
		p := filepath.Join(got, filepath.FromSlash(want[0]), want[1])
		if b, err := os.ReadFile(p); err != nil || len(b) != len(data) {
			t.Errorf("%s: %v", p, err)
		}
	}
	filepath.Walk(m.settings.DownloadDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() && strings.HasSuffix(p, ".bin") && !strings.HasPrefix(p, got) {
			t.Errorf("%s is outside the chosen folder", p)
		}
		return nil
	})

	// one file, its path edited in the Add dialog (folders still made safe)
	r = apiCall(t, srv, "add", map[string]any{"url": ml.URL + "/f.meta4", "meta": true, "metaFile": 0, "start": true, "dir": got, "name": "x/../../y/z.bin"})
	m.mu.Lock()
	d := m.byID[r["id"].(string)]
	m.mu.Unlock()
	waitFor(t, m, d, StCompleted, 20*time.Second)
	if want := filepath.Join(got, "x", "y", "z.bin"); d.path() != want {
		t.Fatalf("saved as %s, want %s", d.path(), want)
	}
}

// ---------------------------------------------------------------- mirror order

func withCountry(t *testing.T, c string) {
	countryOverride.Store(&c)
	t.Cleanup(func() { countryOverride.Store(nil) })
}

// Mirrors go by priority, those in the user's country first (metalink 4 and 3, and Link headers).
func TestMirrorOrderByPriorityAndLocation(t *testing.T) {
	withCountry(t, "de")
	data := []byte("x")
	files, err := parseMetalink(strings.NewReader(mlDocOf(mlEntry{name: "f", data: data,
		urls: []string{"http://us1/f", "http://de1/f", "http://fr/f", "http://de2/f", "http://uk/f"},
		locs: []string{"us", "de", "fr", "DE", ""}})))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, u := range files[0].URLs {
		got = append(got, u.URL)
	}
	if want := []string{"http://de1/f", "http://de2/f", "http://us1/f", "http://fr/f", "http://uk/f"}; !slices.Equal(got, want) {
		t.Errorf("metalink 4: %v", got)
	}
	v3 := `<metalink version="3.0" xmlns="http://www.metalinker.org/"><files><file name="old.zip"><size>10</size><resources>
<url type="http" preference="100" location="us">http://best/old.zip</url><url type="http" preference="50" location="de">http://near/old.zip</url>
<url type="http" preference="90">http://other/old.zip</url><url type="bittorrent">http://t/old.torrent</url></resources></file></files></metalink>`
	files, err = parseMetalink(strings.NewReader(v3))
	if err != nil {
		t.Fatal(err)
	}
	got = nil
	for _, u := range files[0].URLs {
		got = append(got, u.URL)
	}
	if want := []string{"http://near/old.zip", "http://best/old.zip", "http://other/old.zip"}; !slices.Equal(got, want) || files[0].Torrent != "http://t/old.torrent" {
		t.Errorf("metalink 3: %v, torrent %q", got, files[0].Torrent)
	}
	h := http.Header{}
	h.Add("Link", `<http://a/f>; rel=duplicate; pri=1; geo=us, <http://b/f>; rel=duplicate; pri=3; geo=de, <http://c/f>; rel=duplicate; pri=2`)
	if got := mirrorsFromLinks(parseLinks([]http.Header{h}, nil)); !slices.Equal(got, []string{"http://b/f", "http://a/f", "http://c/f"}) {
		t.Errorf("Link headers: %v", got)
	}
	withCountry(t, "")
	if got := mirrorsFromLinks(parseLinks([]http.Header{h}, nil)); !slices.Equal(got, []string{"http://a/f", "http://c/f", "http://b/f"}) {
		t.Errorf("Link headers, country unknown: %v", got)
	}

	for in, want := range map[string]string{"en_US.UTF-8": "us", "de-AT": "at", "sr_RS@latin": "rs", "zh-Hans-CN": "cn", "C": "", "POSIX": "", "en": "", "en_GB": "gb"} {
		if got := localeCountry(in); got != want {
			t.Errorf("localeCountry(%q) = %q, want %q", in, got, want)
		}
	}
	if countryCode("UK") != "gb" || countryCode("001") != "" {
		t.Error("countryCode")
	}
	if _, err := os.Stat("/usr/share/zoneinfo/zone.tab"); err == nil {
		if zoneCountry("Asia/Colombo") != "lk" || zoneCountry("../usr/share/zoneinfo/Europe/Berlin") != "de" || zoneCountry("Etc/UTC") != "" {
			t.Error("zoneCountry")
		}
	}
}
