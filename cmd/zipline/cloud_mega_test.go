package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// megaEncrypt encrypts a file the way MEGA's apps do, written from the format's description (not
// from cloud_mega.go): the data in AES-128-CTR with the nonce and the block number as the counter,
// a CBC-MAC of each chunk (128 KB, 256 KB … 1 MB, then 1 MB each) from the nonce twice, those
// MACs CBC-MACed in turn and folded to 8 bytes, the name as "MEGA" + JSON in AES-CBC with a zero
// IV, and the link's key: the AES key XORed with nonce and MAC, then the nonce and the MAC.
func megaEncrypt(plain []byte, name string, key [16]byte, nonce [8]byte) (ct, linkKey []byte, at string) {
	block, _ := aes.NewCipher(key[:])
	ct = make([]byte, len(plain))
	var ctr, ks [16]byte
	copy(ctr[:8], nonce[:])
	for i := 0; i < len(plain); i += 16 {
		binary.BigEndian.PutUint64(ctr[8:], uint64(i/16))
		block.Encrypt(ks[:], ctr[:])
		for j := i; j < min(i+16, len(plain)); j++ {
			ct[j] = plain[j] ^ ks[j-i]
		}
	}
	var meta [16]byte
	for pos, size := 0, 128<<10; pos < len(plain); {
		n := min(size, len(plain)-pos)
		var mac [16]byte
		copy(mac[:8], nonce[:])
		copy(mac[8:], nonce[:])
		for off := 0; off < n; off += 16 {
			var b [16]byte
			copy(b[:], plain[pos+off:pos+min(off+16, n)])
			for k := range mac {
				mac[k] ^= b[k]
			}
			block.Encrypt(mac[:], mac[:])
		}
		for k := range meta {
			meta[k] ^= mac[k]
		}
		block.Encrypt(meta[:], meta[:])
		pos += n
		if size < 1<<20 {
			size += 128 << 10
		}
	}
	var tail [16]byte // nonce, then the condensed MAC
	copy(tail[:8], nonce[:])
	for i := 0; i < 4; i++ {
		tail[8+i] = meta[i] ^ meta[4+i]
		tail[12+i] = meta[8+i] ^ meta[12+i]
	}
	linkKey = make([]byte, 32)
	for i := 0; i < 16; i++ {
		linkKey[i] = key[i] ^ tail[i]
	}
	copy(linkKey[16:], tail[:])
	attr, _ := json.Marshal(map[string]string{"n": name})
	a := append([]byte("MEGA"), attr...)
	a = append(a, make([]byte, (16-len(a)%16)%16)...)
	cipher.NewCBCEncrypter(block, make([]byte, 16)).CryptBlocks(a, a)
	return ct, linkKey, base64.RawURLEncoding.EncodeToString(a)
}

// testBytes is megajs's test buffer: byte i is (start + i·step) mod 255.
func testBytes(n, start, step int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((start + i*step) % 255)
	}
	return b
}

// A known answer from megajs's tests (test/crypto-stream.test.mjs), whose encryption MEGA accepts:
// the same file and key must give the same ciphertext and link key here.
func TestMegaKnownAnswer(t *testing.T) {
	plain, k := testBytes(151511, 0, 1), testBytes(24, 100, 7)
	var key [16]byte
	var nonce [8]byte
	copy(key[:], k[:16])
	copy(nonce[:], k[16:])
	ct, linkKey, _ := megaEncrypt(plain, "x", key, nonce)
	if got := fmt.Sprintf("%x", sha1.Sum(ct)); got != "addb96c07ac4e6b66316b81530256c911b0b49d1" {
		t.Fatalf("ciphertext sha1 %s", got)
	}
	if got := hex.EncodeToString(linkKey); got != "b0b0909070707093e957d163217c2f3fd4dbe2e9f0f7fe0675f47bd299c3e9f2" {
		t.Fatalf("link key %s", got)
	}
	// cloud_mega.go reads the same key, decrypts from any offset and gets the same MAC
	mk, err := megaParseKey(base64.RawURLEncoding.EncodeToString(linkKey))
	if err != nil || !bytes.Equal(mk.aes[:], key[:]) || !bytes.Equal(mk.nonce[:], nonce[:]) {
		t.Fatalf("key %v %+v", err, mk)
	}
	for _, off := range []int{0, 1, 15, 16, 17, 131072, 131075, 151510} {
		b := append([]byte(nil), ct[off:]...)
		megaStream(mk, int64(off)).XORKeyStream(b, b)
		if !bytes.Equal(b, plain[off:]) {
			t.Fatalf("decrypting from %d gave different bytes", off)
		}
	}
	mac, err := megaMAC(context.Background(), bytes.NewReader(plain), mk, int64(len(plain)), nil)
	if err != nil || mac != mk.mac {
		t.Fatalf("MAC %x %v, want %x", mac, err, mk.mac)
	}
	plain[100000] ^= 1
	if mac, _ := megaMAC(context.Background(), bytes.NewReader(plain), mk, int64(len(plain)), nil); mac == mk.mac {
		t.Fatal("a changed byte should change the MAC")
	}
}

func TestMegaLinks(t *testing.T) {
	for raw, want := range map[string]string{
		"https://mega.nz/file/AbCd1234#key":       "AbCd1234 key",
		"https://mega.nz/#!AbCd1234!key":          "AbCd1234 key",
		"https://mega.co.nz/#!AbCd1234!key":       "AbCd1234 key",
		"https://mega.nz/embed/AbCd1234#key":      "AbCd1234 key",
		"https://mega.nz/embed#!AbCd1234!key":     "AbCd1234 key",
		"https://mega.nz/file/AbCd1234":           "AbCd1234 ",
		"https://mega.nz/folder/AbCd1234#fkey":    "folder",
		"https://mega.nz/#F!AbCd1234!fkey":        "folder",
		"https://mega.nz/":                        " ",
		"https://mega.nz/file/short#key":          " ",
		"https://mega.io/file/AbCd1234#key":       "not MEGA",
		"https://www.mega.nz/file/AbCd1234#k-_ey": "AbCd1234 k-_ey",
	} {
		u, _ := url.Parse(raw)
		id, key, folder := megaLink(u)
		got := id + " " + key
		if folder {
			got = "folder"
		}
		if !megaHost(u.Hostname()) {
			got = "not MEGA"
		}
		if got != want {
			t.Errorf("%s: %q, want %q", raw, got, want)
		}
	}
	if _, err := megaParseKey("too-short"); err == nil {
		t.Error("a damaged key should be refused")
	}
}

// megaFake is MEGA's API and a storage server for one or more encrypted files.
type megaFake struct {
	*httptest.Server
	mu      sync.Mutex
	files   map[string]megaFakeFile // by handle
	folders map[string][]any        // a shared folder's nodes, by its handle
	ranges  int32                   // range requests (…/start-end)
	rate    atomic.Int64            // bytes a second per connection (0 = as fast as it goes)
	headers int32                   // requests that used a Range header instead
	calls   int32
}

type megaFakeFile struct {
	ct    []byte
	at    string
	quota bool // the storage server says the free quota is used up
}

func newMegaFake(t *testing.T) *megaFake {
	f := &megaFake{files: map[string]megaFakeFile{}, folders: map[string][]any{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	old := megaAPI
	megaAPI = f.URL
	t.Cleanup(func() { megaAPI = old })
	return f
}

// add encrypts plain as a MEGA file and returns its link.
func (f *megaFake) add(handle, name string, plain []byte) string {
	var key [16]byte
	var nonce [8]byte
	copy(key[:], randData(16))
	copy(nonce[:], randData(8))
	ct, lk, at := megaEncrypt(plain, name, key, nonce)
	f.mu.Lock()
	f.files[handle] = megaFakeFile{ct: ct, at: at}
	f.mu.Unlock()
	return "https://mega.nz/file/" + handle + "#" + base64.RawURLEncoding.EncodeToString(lk)
}

func (f *megaFake) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/cs" && r.Method == "POST" {
		atomic.AddInt32(&f.calls, 1)
		var cmds []map[string]any
		json.NewDecoder(r.Body).Decode(&cmds)
		folder := r.URL.Query().Get("n")
		if len(cmds) == 1 && cmds[0]["a"] == "f" {
			f.mu.Lock()
			nodes, ok := f.folders[folder]
			f.mu.Unlock()
			if !ok {
				fmt.Fprint(w, "-9")
				return
			}
			json.NewEncoder(w).Encode([]any{map[string]any{"f": nodes, "ok": []any{}, "s": []any{}}})
			return
		}
		if len(cmds) != 1 || cmds[0]["a"] != "g" {
			fmt.Fprint(w, "-2")
			return
		}
		handle := fmt.Sprint(cmds[0]["p"])
		if folder != "" { // a file in a shared folder
			handle = fmt.Sprint(cmds[0]["n"])
		}
		f.mu.Lock()
		file, ok := f.files[handle]
		f.mu.Unlock()
		switch {
		case !ok:
			fmt.Fprint(w, "[-9]")
		case fmt.Sprint(cmds[0]["p"]) == "Takendwn":
			fmt.Fprint(w, `[{"e":-16}]`)
		default:
			fmt.Fprintf(w, `[{"s":%d,"at":%q,"msd":1,"g":%q}]`, len(file.ct), file.at, f.URL+"/dl/tok-"+handle)
		}
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/dl/tok-")
	if !ok {
		http.NotFound(w, r)
		return
	}
	handle, rng, _ := strings.Cut(rest, "/")
	f.mu.Lock()
	file, ok := f.files[handle]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if file.quota {
		w.Header().Set("X-MEGA-Time-Left", "4500")
		w.WriteHeader(509)
		return
	}
	if r.Header.Get("Range") != "" {
		atomic.AddInt32(&f.headers, 1)
	}
	data := file.ct
	if a, b, ok := strings.Cut(rng, "-"); ok { // MEGA's ranges: …/start-end, answered with 200
		atomic.AddInt32(&f.ranges, 1)
		s, _ := strconv.Atoi(a)
		e, _ := strconv.Atoi(b)
		if s > e || e >= len(data) {
			http.Error(w, "bad range", 416)
			return
		}
		data = data[s : e+1]
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	rate := int(f.rate.Load())
	step := 64 << 10 // (a bit at a time, as a server does: one write of the whole file can outlast a reader that left)
	if rate > 0 {
		step = rate / 10
	}
	for len(data) > 0 && r.Context().Err() == nil {
		n := min(len(data), step)
		if _, err := w.Write(data[:n]); err != nil {
			return
		}
		data = data[n:]
		if rate > 0 {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func TestMegaDownloadDecryptsAndChecksTheMAC(t *testing.T) {
	f := newMegaFake(t)
	plain := randData(5<<20 + 12345) // (not a whole number of blocks or chunks)
	link := f.add("AbCd1234", "Holiday video.mp4", plain)
	m := newTestManager(t)
	m.settings.Conns = 4
	srv := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, srv, "probe", map[string]any{"url": link})
	if info["name"] != "Holiday video.mp4" || info["size"] != float64(len(plain)) || info["service"] != "MEGA" || info["resumable"] != true {
		t.Fatalf("probe: %v", info)
	}
	d := m.Add(AddReq{URL: link, Start: true})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	fileIs(t, d, plain)
	if d.FileName != "Holiday video.mp4" {
		t.Fatalf("name %q", d.FileName)
	}
	if n := atomic.LoadInt32(&f.ranges); n < 2 {
		t.Fatalf("ranges asked in MEGA's way: %d (several connections expected)", n)
	}
	if n := atomic.LoadInt32(&f.headers); n != 0 {
		t.Fatalf("%d requests sent a Range header to MEGA's storage", n)
	}
	// the older link form works the same
	_, key, _ := strings.Cut(link, "#")
	old := "https://mega.nz/#!AbCd1234!" + key
	if info := apiCall(t, srv, "probe", map[string]any{"url": old}); !strings.HasPrefix(fmt.Sprint(info["name"]), "Holiday video") || info["service"] != "MEGA" || info["size"] != float64(len(plain)) {
		t.Fatalf("old link: %v", info)
	}
}

func TestMegaDamagedFileFailsItsMAC(t *testing.T) {
	f := newMegaFake(t)
	plain := randData(3 << 20)
	link := f.add("Damaged1", "disk.img", plain)
	f.mu.Lock()
	file := f.files["Damaged1"]
	file.ct[2<<20+7] ^= 0x40 // one bit changed on the way
	f.mu.Unlock()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: link, Start: true})
	waitFor(t, m, d, StError, 60*time.Second)
	m.mu.Lock()
	msg := d.Error
	m.mu.Unlock()
	if !strings.Contains(msg, "MAC") || !strings.Contains(msg, "Redownload") {
		t.Fatalf("error %q", msg)
	}
}

func TestMegaSaysWhatsWrong(t *testing.T) {
	f := newMegaFake(t)
	link := f.add("Quota123", "big.bin", randData(1000))
	f.mu.Lock()
	q := f.files["Quota123"]
	q.quota = true
	f.files["Quota123"] = q
	f.files["Takendwn"] = megaFakeFile{}
	f.mu.Unlock()
	ctx := context.Background()
	for _, c := range []struct{ link, want string }{
		{link, "Bandwidth Limit Exceeded"},
		{"https://mega.nz/file/Missing1#" + strings.Repeat("A", 43), "no longer available"},
		{"https://mega.nz/file/Takendwn#" + strings.Repeat("A", 43), "taken down"},
		{"https://mega.nz/file/Missing1", "no key"},
		{"https://mega.nz/folder/Fold1234#abc", "folder link (the part after #) is damaged"},
	} {
		_, err := probe(ctx, &Download{URL: c.link}, c.link, 0, -1)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.link, err, c.want)
		}
	}
	// a key that isn't this file's: its name doesn't decrypt
	other := f.add("Wrongkey", "a.bin", randData(100))
	f.mu.Lock()
	f.files["Wrongkey"] = f.files["Quota123"]
	f.mu.Unlock()
	if _, err := resolveMega(ctx, other); err == nil || !strings.Contains(err.Error(), "doesn't fit") {
		t.Errorf("wrong key: %v", err)
	}
}

// an empty file needs no request to the storage server, and no MAC check
func TestMegaEmptyFile(t *testing.T) {
	f := newMegaFake(t)
	link := f.add("Empty123", "empty.txt", nil)
	m := newTestManager(t)
	d := m.Add(AddReq{URL: link, Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	fileIs(t, d, nil)
}

// Paused half-way and carried on later (the file's address from before is asked for again),
// every connection decrypts its own part from its own offset.
func TestMegaResumes(t *testing.T) {
	f := newMegaFake(t)
	f.rate.Store(1 << 20)
	plain := randData(4<<20 + 777)
	link := f.add("Resume12", "film.mkv", plain)
	m := newTestManager(t)
	m.settings.Conns = 3
	d := m.Add(AddReq{URL: link, Start: true})
	for i := 0; i < 200 && atomic.LoadInt64(&d.done) < 1<<20; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	m.Pause([]string{d.ID})
	waitFor(t, m, d, StPaused, 10*time.Second)
	for i := 0; i < 100; i++ { // (until its connections have stopped)
		m.mu.Lock()
		running := d.running
		m.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	done := atomic.LoadInt64(&d.done)
	if done <= 0 || done >= int64(len(plain)) {
		t.Fatalf("paused at %d of %d", done, len(plain))
	}
	// what Zipline knew about the address is gone, as after a restart: requests to it are refused
	// so the engine asks the share link again
	m.mu.Lock()
	final := d.FinalURL
	m.mu.Unlock()
	cloudFacts.Lock()
	delete(cloudFacts.m, final)
	cloudFacts.Unlock()
	req, _ := buildRequest(context.Background(), d, final, "bytes=16-31")
	if _, err := doReq(d, req); err != errMegaAgain {
		t.Fatalf("an address without its key: %v", err)
	}
	f.rate.Store(0)
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	fileIs(t, d, plain)
}

// addFolder makes a shared MEGA folder: its nodes' keys encrypted with the folder's key block by
// block, and their names with their own keys. files maps a path inside it to the file's bytes.
func (f *megaFake) addFolder(fid, name string, files map[string][]byte) string {
	fk := randData(16)
	block, _ := aes.NewCipher(fk)
	ecb := func(k []byte) string {
		b := append([]byte(nil), k...)
		for i := 0; i < len(b); i += 16 {
			block.Encrypt(b[i:i+16], b[i:i+16])
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	attrs := func(name string, key []byte) string {
		a, _ := json.Marshal(map[string]string{"n": name})
		a = append([]byte("MEGA"), a...)
		a = append(a, make([]byte, (16-len(a)%16)%16)...)
		c, _ := aes.NewCipher(key)
		cipher.NewCBCEncrypter(c, make([]byte, 16)).CryptBlocks(a, a)
		return base64.RawURLEncoding.EncodeToString(a)
	}
	root := fid[:4] + "Root"
	nodes := []any{map[string]any{"h": root, "p": "", "t": 1, "a": attrs(name, fk), "k": root + ":" + ecb(fk)}}
	dirs := map[string]string{"": root}
	n := 0
	var paths []string
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		dir, base := "", p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			dir, base = p[:i], p[i+1:]
		}
		if dirs[dir] == "" {
			n++
			h := fmt.Sprintf("%sDir%d", fid[:4], n)
			dk := randData(16)
			nodes = append(nodes, map[string]any{"h": h, "p": root, "t": 1, "a": attrs(dir, dk), "k": root + ":" + ecb(dk)})
			dirs[dir] = h
		}
		n++
		h := fmt.Sprintf("%sFil%d", fid[:4], n)
		var key [16]byte
		var nonce [8]byte
		copy(key[:], randData(16))
		copy(nonce[:], randData(8))
		ct, lk, at := megaEncrypt(files[p], base, key, nonce)
		nodes = append(nodes, map[string]any{"h": h, "p": dirs[dir], "t": 0, "s": len(ct), "a": at, "k": root + ":" + ecb(lk)})
		f.mu.Lock()
		f.files[h] = megaFakeFile{ct: ct, at: at}
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.folders[fid] = nodes
	f.mu.Unlock()
	return "https://mega.nz/folder/" + fid + "#" + base64.RawURLEncoding.EncodeToString(fk)
}

func TestMegaSharedFolder(t *testing.T) {
	f := newMegaFake(t)
	files := map[string][]byte{"cover.jpg": randData(70000), "Day 1/clip.mp4": randData(2<<20 + 5), "Day 1/notes.txt": []byte("tide at 6")}
	link := f.addFolder("FoldAbcd", "Beach trip", files)
	m := newTestManager(t)
	srv := (&Server{m: m, token: "t"}).routes()
	info := apiCall(t, srv, "probe", map[string]any{"url": link})
	folder, _ := info["folder"].(map[string]any)
	if folder == nil || folder["name"] != "Beach trip" || info["service"] != "MEGA" || len(folder["files"].([]any)) != 3 {
		t.Fatalf("probe: %v", info)
	}
	res := apiCall(t, srv, "add", map[string]any{"url": link, "meta": true, "metaFile": -1, "start": true})
	for _, id := range res["ids"].([]any) {
		waitFor(t, m, earlyDownload(m, id.(string)), StCompleted, 30*time.Second)
	}
	for rel, want := range files {
		if got, err := os.ReadFile(filepath.Join(m.settings.DownloadDir, "Beach trip", filepath.FromSlash(rel))); err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: %v", rel, err)
		}
	}
	// the older form of the link, and a link to the folder's file
	_, fk, _ := strings.Cut(link, "#")
	if info := apiCall(t, srv, "probe", map[string]any{"url": "https://mega.nz/#F!FoldAbcd!" + fk}); info["folder"] == nil {
		t.Fatalf("old folder link: %v", info)
	}
	one := ""
	for _, x := range res["ids"].([]any) {
		if d := earlyDownload(m, x.(string)); d.FileName == "notes.txt" {
			one = d.URL
		}
	}
	if !strings.HasPrefix(one, link+"/file/") {
		t.Fatalf("a file's own link: %q", one)
	}
	if info := apiCall(t, srv, "probe", map[string]any{"url": one}); info["name"] != "notes.txt" || info["size"] != float64(len("tide at 6")) {
		t.Fatalf("a file in the folder: %v", info)
	}
	if _, err := resolveCloud(context.Background(), &Download{}, "https://mega.nz/folder/FoldAbcd#"+strings.Repeat("A", 22)); err == nil || !strings.Contains(err.Error(), "doesn't fit") {
		t.Errorf("wrong folder key: %v", err)
	}
}
