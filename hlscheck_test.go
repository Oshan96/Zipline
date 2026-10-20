package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// makeHLS has ffmpeg write a finished HLS stream (secs long, pieces of piece seconds) with its
// pieces as MPEG-TS ("ts") or MP4 ("fmp4"). Returns the folder holding index.m3u8.
func makeHLS(t *testing.T, kind string, secs, piece int) string {
	t.Helper()
	dir := t.TempDir()
	args := []string{"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", strconv.Itoa(secs), "-c:v", "libx264", "-preset", "ultrafast", "-g", strconv.Itoa(25 * piece), "-c:a", "aac",
		"-f", "hls", "-hls_time", strconv.Itoa(piece), "-hls_playlist_type", "vod"}
	if kind == "fmp4" {
		args = append(args, "-hls_segment_type", "fmp4", "-hls_segment_filename", filepath.Join(dir, "s%03d.m4s"))
	} else {
		args = append(args, "-hls_segment_filename", filepath.Join(dir, "s%03d.ts"))
	}
	ffmpegRun(t, append(args, filepath.Join(dir, "index.m3u8"))...)
	return dir
}

// faultySite serves a folder, but one piece misbehaves: it answers with web pages for its first
// pages requests (an HTML error page, then one that says it's video), or is always cut short to
// cut of its bytes (TS pieces at a packet boundary, so only the timestamps can tell).
type faultySite struct {
	dir, bad string
	pages    int
	cut      float64
	mu       sync.Mutex
	hits     map[string]int
}

func (s *faultySite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Path)
	s.mu.Lock()
	s.hits[name]++
	n := s.hits[name]
	s.mu.Unlock()
	if name == s.bad && n <= s.pages {
		if n%2 == 1 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<!DOCTYPE html><html><body><h1>503 Service Temporarily Unavailable</h1></body></html>")
		} else {
			w.Header().Set("Content-Type", "video/mp2t") // a page that claims to be video
			fmt.Fprint(w, "\n  <html><head><title>Access denied</title></head><body>Sign in to watch</body></html>")
		}
		return
	}
	if name == s.bad && s.cut > 0 {
		b, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		k := int(float64(len(b)) * s.cut)
		if strings.HasSuffix(name, ".ts") {
			k -= k % 188
		}
		w.Header().Set("Content-Length", strconv.Itoa(k)) // the server itself sends it short
		w.Write(b[:k])
		return
	}
	http.FileServer(http.Dir(s.dir)).ServeHTTP(w, r)
}

func (s *faultySite) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[name]
}

// quickRetries makes failed pieces come back fast.
func quickRetries(t *testing.T, m *Manager) {
	setVar(t, &retryBase, 10*time.Millisecond)
	setVar(t, &retryCap, 80*time.Millisecond)
	stopAll(t, m)
}

func TestSniffPieces(t *testing.T) {
	ts := fakeTS(4)
	frag := append(fakeBox("moof", 100), fakeBox("mdat", 1000)...)
	adts := []byte{0xFF, 0xF1, 0x50, 0x80, 0x02, 0x1F, 0xFC}
	cases := []struct {
		name, want string
		b          []byte
	}{
		{"ts", "ts", ts},
		{"m2ts", "ts", append([]byte{0, 0, 0, 0}, ts[:188]...)},
		{"fmp4", "mp4", frag},
		{"init", "mp4", fakeBox("ftyp", 20)},
		{"webm cluster", "webm", []byte{0x1F, 0x43, 0xB6, 0x75, 0x01}},
		{"adts", "audio", adts},
		{"packed audio", "audio", []byte("ID3\x04\x00\x00\x00\x00\x00\x00")},
		{"ac3", "audio", []byte{0x0B, 0x77, 0, 0}},
		{"webvtt", "text", []byte("\xEF\xBB\xBFWEBVTT\n\n00:00.000 --> 00:01.000\nhi\n")},
		{"ttml", "text", []byte(`<?xml version="1.0"?><tt xmlns="http://www.w3.org/ns/ttml"><body/></tt>`)},
		{"html", "", []byte("<!DOCTYPE html><html><body>Not found</body></html>")},
		{"html after spaces", "", []byte("\r\n\t <html>")},
		{"xml error", "", []byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code></Error>`)},
		{"json", "", []byte(`{"error":"token expired"}`)},
		{"json list", "", []byte(` [1,2]`)},
		{"png", "", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")},
		{"empty", "", nil},
	}
	for _, c := range cases {
		got, err := sniffPiece(c.b)
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("%s: %q, %v (want %q)", c.name, got, err, c.want)
		}
	}
	for ct, page := range map[string]bool{"text/html; charset=UTF-8": true, "application/json": true, "video/mp2t": false, "application/octet-stream": false, "text/plain": false, "": false} {
		if pageType(ct) != page {
			t.Errorf("pageType(%q) = %v", ct, !page)
		}
	}
}

func TestCheckPieceCutShort(t *testing.T) {
	// MPEG-TS: a packet cut off at the end
	ts := fakeTS(10)
	if c, err := checkPiece(ts[:len(ts)-50], false, nil, 0); err != nil || !c.short || c.keep != 9*188 {
		t.Fatalf("cut TS: %+v %v", c, err)
	}
	if c, _ := checkPiece(ts, false, nil, 0); c.short || c.keep != len(ts) {
		t.Fatalf("whole TS: %+v", c)
	}
	// MP4: the mdat runs past the end: the fragment can't be kept, the one before it can
	one := append(fakeBox("moof", 100), fakeBox("mdat", 1000)...)
	two := append(append(append([]byte{}, fakeBox("styp", 8)...), one...), one...)
	if c, err := checkPiece(two[:len(two)-10], false, nil, 0); err != nil || !c.short || c.keep != 16+len(one) {
		t.Fatalf("cut MP4: %+v %v", c, err)
	}
	if c, _ := checkPiece(two[:16+len(one)+108], false, nil, 0); !c.short || c.keep != 16+len(one) { // a moof without its mdat
		t.Fatalf("moof alone: %+v", c)
	}
	if c, _ := checkPiece(two, false, nil, 0); c.short || c.keep != len(two) {
		t.Fatalf("whole MP4: %+v", c)
	}
	// a plain byte range of one file can start with anything
	if c, err := checkPiece([]byte("<html> is fine in the middle of a file"), true, nil, 0); err != nil || c.short {
		t.Fatalf("chunk: %+v %v", c, err)
	}
	// AAC frames: the last one cut off
	frame := func() []byte {
		f := make([]byte, 200)
		copy(f, []byte{0xFF, 0xF1, 0x4C, 0x80, 0, 0, 0xFC}) // 48 kHz
		f[3] |= byte(200 >> 11)
		f[4] = byte(200 >> 3)
		f[5] = byte(200&7)<<5 | 0x1F
		return f
	}
	var aac []byte
	for i := 0; i < 47; i++ {
		aac = append(aac, frame()...)
	}
	c, err := checkPiece(aac[:len(aac)-30], false, nil, 0)
	if err != nil || !c.short || c.keep != 46*200 || math.Abs(c.span-46*1024/48000.0) > 1e-9 {
		t.Fatalf("cut AAC: %+v %v", c, err)
	}
	// fewer seconds than the playlist says
	if c, _ := checkPiece(aac, false, nil, 4); !c.short {
		t.Fatalf("1 s of AAC for a 4 s piece: %+v", c)
	}
	if c, _ := checkPiece(aac, false, nil, 1); c.short {
		t.Fatalf("1 s of AAC for a 1 s piece: %+v", c)
	}
}

// TestMeasureRealPieces: the media time of real MPEG-TS and MP4 pieces matches the playlist's.
func TestMeasureRealPieces(t *testing.T) {
	needFFmpeg(t)
	for _, kind := range []string{"ts", "fmp4"} {
		dir := makeHLS(t, kind, 6, 2)
		pl, _ := os.ReadFile(filepath.Join(dir, "index.m3u8"))
		_, media, err := parseM3U8("http://x/index.m3u8", string(pl))
		if err != nil {
			t.Fatal(err)
		}
		var tracks map[uint32]mp4Track
		if kind == "fmp4" {
			init, _ := os.ReadFile(filepath.Join(dir, filepath.Base(media.InitURL)))
			if tracks = initTracks(init); len(tracks) != 2 || initID(init) == "" {
				t.Fatalf("init tracks %+v id %q", tracks, initID(init))
			}
		}
		for _, s := range media.Segments {
			b, _ := os.ReadFile(filepath.Join(dir, filepath.Base(s.URL)))
			c, err := checkPiece(b, false, tracks, s.Dur)
			if err != nil || c.short || math.Abs(c.span-s.Dur) > 0.1 {
				t.Fatalf("%s %s: %+v %v (playlist %.3f)", kind, filepath.Base(s.URL), c, err, s.Dur)
			}
			half := b[:len(b)/2]
			if kind == "ts" {
				half = half[:len(half)/188*188]
			}
			if c, _ := checkPiece(half, false, tracks, s.Dur); !c.short || c.span >= s.Dur*0.8 {
				t.Fatalf("%s half of %s: %+v", kind, filepath.Base(s.URL), c)
			}
			if kind == "ts" && tsID(b) != "ts:1b,0f" {
				t.Fatalf("stream types %q", tsID(b))
			}
		}
	}
}

// TestStreamPiecePages: a piece that comes back as a web page is tried again, not saved; one
// that's always cut short makes the download fail clearly instead of finishing short. HLS with
// MPEG-TS and MP4 pieces, and DASH.
func TestStreamPiecePages(t *testing.T) {
	needFFmpeg(t)
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	t.Cleanup(func() { ffmpegPath = orig })
	type stream struct {
		name, dir, list, bad string
		src                  func(dir string) (video, audio string)
	}
	hls := func(kind string) stream {
		dir := makeHLS(t, kind, 10, 2)
		bad := "s002.ts"
		if kind == "fmp4" {
			bad = "s002.m4s"
		}
		return stream{"hls-" + kind, dir, "index.m3u8", bad, func(dir string) (string, string) {
			p := filepath.Join(dir, "index.m3u8")
			return p, p
		}}
	}
	dashDir := makeDASH(t, 10, "-use_template", "1", "-use_timeline", "1")
	streams := []stream{hls("ts"), hls("fmp4"), {"dash", dashDir, "stream.mpd", "chunk-stream0-00003.m4s", func(dir string) (string, string) {
		return joinPieces(t, dir, "chunk-stream0-", "init-stream0.m4s", "v.mp4"), joinPieces(t, dir, "chunk-stream1-", "init-stream1.m4s", "a.mp4")
	}}}
	for _, st := range streams {
		t.Run(st.name+"/page", func(t *testing.T) {
			site := &faultySite{dir: st.dir, bad: st.bad, pages: 2, hits: map[string]int{}}
			srv := httptest.NewServer(site)
			defer srv.Close()
			m := newTestManager(t)
			quickRetries(t, m)
			d := m.Add(AddReq{URL: srv.URL + "/" + st.list, Name: "clip.mp4", Start: true})
			waitFor(t, m, d, StCompleted, 60*time.Second)
			out := filepath.Join(d.Dir, d.FileName)
			if site.count(st.bad) != 3 || filepath.Ext(out) != ".mp4" || d.Note != "" || !isRegularMP4(out) {
				t.Fatalf("%d requests for the bad piece; %s note %q", site.count(st.bad), d.FileName, d.Note)
			}
			video, audio := st.src(st.dir)
			sameHashes(t, "video", decodedHashes(t, video, "v:0"), decodedHashes(t, out, "v:0"))
			sameHashes(t, "audio", decodedHashes(t, audio, "a:0"), decodedHashes(t, out, "a:0"))
			if math.Abs(d.WantSecs-10) > 0.2 || d.ShortSecs != 0 {
				t.Fatalf("measured %.2f s, %.2f s short", d.WantSecs, d.ShortSecs)
			}
		})
		t.Run(st.name+"/cut", func(t *testing.T) {
			site := &faultySite{dir: st.dir, bad: st.bad, cut: 0.3, hits: map[string]int{}}
			srv := httptest.NewServer(site)
			defer srv.Close()
			m := newTestManager(t)
			quickRetries(t, m)
			d := m.Add(AddReq{URL: srv.URL + "/" + st.list, Name: "clip.mp4", Start: true})
			waitFor(t, m, d, StError, 60*time.Second)
			if !strings.Contains(d.Error, "the video stopped early: got 0:0") || !strings.Contains(d.Error, " of 0:10") || !strings.Contains(d.Error, "pieces were missing") {
				t.Fatalf("error %q", d.Error)
			}
			if n := site.count(st.bad); n != pieceTries {
				t.Fatalf("the cut piece was fetched %d times", n)
			}
			if _, err := os.Stat(filepath.Join(d.Dir, "clip.mp4")); err == nil {
				t.Fatal("a short video was saved as if complete")
			}
			// starting it again fetches the stream again (what's there isn't whole)
			site.mu.Lock()
			site.cut = 0
			site.mu.Unlock()
			m.Start([]string{d.ID})
			waitFor(t, m, d, StCompleted, 60*time.Second)
			if d.ShortSecs != 0 || math.Abs(d.WantSecs-10) > 0.2 {
				t.Fatalf("again: measured %.2f s, %.2f s short", d.WantSecs, d.ShortSecs)
			}
		})
	}
}

// fakeTSID builds a piece of MPEG-TS with a program map of the given stream types, then noise.
func fakeTSID(types ...byte) []byte {
	pkt := func(pid int, sec []byte) []byte {
		p := bytes.Repeat([]byte{0xFF}, 188)
		p[0], p[1], p[2], p[3] = 0x47, 0x40|byte(pid>>8), byte(pid), 0x10
		p[4] = 0
		copy(p[5:], sec)
		return p
	}
	pat := []byte{0, 0xB0, 13, 0, 1, 0xC1, 0, 0, 0, 1, 0xF0, 0x00}
	pat = binary.BigEndian.AppendUint32(pat, crc32MPEG(pat))
	pmt := []byte{2, 0xB0, 0, 0, 1, 0xC1, 0, 0, 0xE1, 0x00, 0xF0, 0}
	for i, ty := range types {
		pmt = append(pmt, ty, 0xE1, byte(i+1), 0xF0, 0)
	}
	pmt[2] = byte(len(pmt) + 4 - 3)
	pmt = binary.BigEndian.AppendUint32(pmt, crc32MPEG(pmt))
	return append(append(pkt(0, pat), pkt(0x1000, pmt)...), fakeTS(30)...)
}

func TestTSIDAndRetype(t *testing.T) {
	b := fakeTSID(0xDB, 0xCF)
	if id := tsID(b); id != "ts:db,cf" || tsIDClear(id) != "ts:1b,0f" {
		t.Fatalf("id %q", id)
	}
	out, err := rewriteTS(b, clearType, func(es []byte) []byte { return es }, func(string, []byte) {})
	if err != nil || tsID(out) != "ts:1b,0f" {
		t.Fatalf("retyped: %q %v", tsID(out), err)
	}
	sec := psiSection(out[188+5-1:])
	if crc32MPEG(sec) != 0 { // a section with its CRC appended checks to zero
		t.Fatal("the program map's CRC wasn't updated")
	}
}
