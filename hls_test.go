package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func encryptSeg(plain, key []byte, seq int64) []byte {
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	b := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	iv := make([]byte, 16)
	binary.BigEndian.PutUint64(iv[8:], uint64(seq))
	blk, _ := aes.NewCipher(key)
	out := make([]byte, len(b))
	cipher.NewCBCEncrypter(blk, iv).CryptBlocks(out, b)
	return out
}

type hlsSite struct {
	srv    *httptest.Server
	plain  [][]byte // decrypted segment contents, in order
	init   []byte
	served int32
}

// fakeTS is n MPEG-TS packets of noise: it looks like a piece of a video stream (the sync byte
// every 188 bytes) but holds no audio or video.
func fakeTS(n int) []byte {
	b := make([]byte, 0, n*188)
	for k := 0; k < n; k++ {
		p := make([]byte, 188)
		rand.Read(p[4:])
		p[0], p[1], p[2], p[3] = 0x47, 0x01, 0x00, 0x10|byte(k&15)
		b = append(b, p...)
	}
	return b
}

// fakeBox is an MP4 box of noise.
func fakeBox(typ string, n int) []byte {
	b := make([]byte, 8+n)
	rand.Read(b[8:])
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	copy(b[4:], typ)
	return b
}

// newHLSSite serves a master playlist with two qualities; "hi" is AES-128 encrypted. The pieces
// are noise shaped like MPEG-TS (or MP4 fragments).
func newHLSSite(t *testing.T, n int, fmp4 bool, delay time.Duration) *hlsSite {
	key := []byte("0123456789abcdef")
	s := &hlsSite{init: append(fakeBox("ftyp", 16), fakeBox("moov", 300)...)}
	for i := 0; i < n; i++ {
		b := fakeTS(210 + rand.Intn(100))
		if fmp4 {
			b = append(fakeBox("moof", 200), fakeBox("mdat", 40_000+rand.Intn(20_000))...)
		}
		s.plain = append(s.plain, b)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v/master.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nlo/index.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=2500000,RESOLUTION=1280x720\nhi/index.m3u8\n")
	})
	mux.HandleFunc("/v/hi/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n")
		if fmp4 {
			b.WriteString("#EXT-X-MAP:URI=\"init.mp4\"\n")
		}
		b.WriteString("#EXT-X-KEY:METHOD=AES-128,URI=\"/keys/k1\"\n")
		for i := range s.plain {
			fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts\n", i)
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		fmt.Fprint(w, b.String())
	})
	mux.HandleFunc("/v/hi/init.mp4", func(w http.ResponseWriter, r *http.Request) { w.Write(s.init) })
	mux.HandleFunc("/keys/k1", func(w http.ResponseWriter, r *http.Request) { w.Write(key) })
	mux.HandleFunc("/v/hi/", func(w http.ResponseWriter, r *http.Request) {
		var i int
		if _, err := fmt.Sscanf(filepath.Base(r.URL.Path), "seg%d.ts", &i); err != nil || i >= len(s.plain) {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&s.served, 1)
		time.Sleep(delay)
		w.Write(encryptSeg(s.plain[i], key, int64(i)))
	})
	mux.HandleFunc("/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXTINF:4,\na.ts\n")
	})
	mux.HandleFunc("/drm.m3u8", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://x\",KEYFORMAT=\"com.apple.streamingkeydelivery\"\n#EXTINF:4,\na.ts\n#EXT-X-ENDLIST\n")
	})
	for path, key := range map[string]string{ // SAMPLE-AES is only decrypted with a plain key at an ordinary address
		"/drm-skd.m3u8":      `METHOD=SAMPLE-AES,URI="skd://key"`,
		"/drm-widevine.m3u8": `METHOD=SAMPLE-AES,URI="https://lic.example/k",KEYFORMAT="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"`,
		"/drm-ctr.m3u8":      `METHOD=SAMPLE-AES-CTR,URI="https://lic.example/k"`,
		"/drm-cenc.m3u8":     `METHOD=SAMPLE-AES,URI="data:text/plain;base64,AAAA"`,
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "#EXTM3U\n#EXT-X-KEY:"+key+"\n#EXTINF:4,\na.ts\n#EXT-X-ENDLIST\n")
		})
	}
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *hlsSite) want(fmp4 bool) []byte {
	var b []byte
	if fmp4 {
		b = append(b, s.init...)
	}
	for _, p := range s.plain {
		b = append(b, p...)
	}
	return b
}

func TestHLSParallelDecryptAndResume(t *testing.T) {
	site := newHLSSite(t, 60, false, 40*time.Millisecond)
	m := newTestManager(t)
	m.settings.Conns = 8
	t0 := time.Now()
	d := m.Add(AddReq{URL: site.srv.URL + "/v/master.m3u8", Name: "My Clip.mp4", Start: true})
	time.Sleep(250 * time.Millisecond)
	m.Pause([]string{d.ID})
	waitFor(t, m, d, StPaused, 5*time.Second)
	for {
		m.mu.Lock()
		r := d.running
		m.mu.Unlock()
		if !r {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.mu.Lock()
	mid := d.PartsDone
	m.mu.Unlock()
	if mid == 0 || mid >= 60 {
		t.Fatalf("unexpected pause point: %d segments", mid)
	}
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	el := time.Since(t0)
	got, err := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, site.want(false)) {
		t.Fatalf("HLS output mismatch: got %d bytes, want %d", len(got), len(site.want(false)))
	}
	if d.Variant != 2500000 || d.Kind != "hls" {
		t.Fatalf("expected best quality hls, got variant=%d kind=%q", d.Variant, d.Kind)
	}
	// test segments aren't real video, so the result must stay a .ts, with a note saying why
	if filepath.Ext(d.FileName) != ".ts" {
		t.Fatalf("expected .ts, got %s", d.FileName)
	}
	if !strings.Contains(d.Note, "Saved as .ts") || !strings.Contains(d.Note, "no audio or video") {
		t.Fatalf("expected a note explaining the MP4 conversion fell back to .ts, got %q", d.Note)
	}
	t.Logf("60 segments x 40ms latency in %v (sequential would be %v), paused at %d, served=%d", el, 60*40*time.Millisecond, mid, site.served)
	if el > 1800*time.Millisecond {
		t.Fatalf("segments not fetched in parallel: %v", el)
	}
}

func TestHLSFragmentedMP4(t *testing.T) {
	site := newHLSSite(t, 10, true, 0)
	m := newTestManager(t)
	d := m.Add(AddReq{URL: site.srv.URL + "/v/hi/index.m3u8", Start: true})
	waitFor(t, m, d, StCompleted, 15*time.Second)
	got, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if !bytes.Equal(got, site.want(true)) || filepath.Ext(d.FileName) != ".mp4" {
		t.Fatalf("fMP4 mismatch: %s, %d bytes", d.FileName, len(got))
	}
}

func TestHLSRefusesDRM(t *testing.T) {
	site := newHLSSite(t, 1, false, 0)
	m := newTestManager(t)
	for path, want := range map[string]string{"/drm.m3u8": "DRM", "/drm-skd.m3u8": "DRM", "/drm-widevine.m3u8": "DRM", "/drm-ctr.m3u8": "DRM", "/drm-cenc.m3u8": "DRM"} {
		d := m.Add(AddReq{URL: site.srv.URL + path, Start: true})
		waitFor(t, m, d, StError, 10*time.Second)
		if !strings.Contains(d.Error, want) {
			t.Fatalf("%s: expected %q in error, got %q", path, want, d.Error)
		}
	}
}

func TestParseM3U8Attributes(t *testing.T) {
	a := attrs(`BANDWIDTH=1280000,CODECS="avc1.4d401f,mp4a.40.2",RESOLUTION=1280x720,AUDIO="aud"`)
	if a["CODECS"] != "avc1.4d401f,mp4a.40.2" || a["RESOLUTION"] != "1280x720" || a["AUDIO"] != "aud" {
		t.Fatalf("attrs: %#v", a)
	}
	v, _, err := parseM3U8("https://cdn.example/x/master.m3u8", "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"en\",DEFAULT=YES,URI=\"audio/en.m3u8\"\n#EXT-X-STREAM-INF:BANDWIDTH=5,AUDIO=\"aud\"\nv/a.m3u8\n")
	if err != nil || len(v) != 1 || v[0].AudioURL != "https://cdn.example/x/audio/en.m3u8" || v[0].URL != "https://cdn.example/x/v/a.m3u8" {
		t.Fatalf("variants: %#v %v", v, err)
	}
}

// TestHLSRealVideoToMP4 builds a genuine HLS stream with ffmpeg and checks Zipline turns it
// into a valid MP4 on its own: ffmpeg is hidden from the app while it downloads.
func TestHLSRealVideoToMP4(t *testing.T) {
	ff := ffmpegPath()
	if ff == "" {
		t.Skip("ffmpeg not installed")
	}
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	t.Cleanup(func() { ffmpegPath = orig })
	src := t.TempDir()
	cmd := exec.Command(ff, "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440",
		"-t", "8", "-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac", "-shortest", "-hls_time", "1", "-hls_list_size", "0",
		"-hls_playlist_type", "vod", filepath.Join(src, "index.m3u8"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg couldn't make a test stream: %v %s", err, out)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(src)))
	defer srv.Close()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/index.m3u8", Name: "clip.mp4", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	if filepath.Ext(d.FileName) != ".mp4" || d.Note != "" {
		t.Fatalf("expected a converted .mp4, got %s (note %q)", d.FileName, d.Note)
	}
	probe, err := exec.Command(strings.TrimSuffix(ff, "ffmpeg")+"ffprobe", "-v", "error", "-show_entries", "format=format_name,duration:stream=codec_type",
		"-of", "default=nw=1", filepath.Join(d.Dir, d.FileName)).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe failed: %v %s", err, probe)
	}
	out := string(probe)
	if !strings.Contains(out, "mp4") || !strings.Contains(out, "codec_type=video") || !strings.Contains(out, "codec_type=audio") {
		t.Fatalf("not a valid mp4 with audio+video:\n%s", out)
	}
	t.Logf("ffprobe: %s", strings.ReplaceAll(strings.TrimSpace(out), "\n", " | "))

	// a restart just after converting (before the final rename) must keep the MP4 as it is
	final := filepath.Join(d.Dir, d.FileName)
	want, _ := os.ReadFile(final)
	m.mu.Lock()
	d.Status = StPaused
	m.mu.Unlock()
	if err := os.Rename(final, d.partPath()); err != nil {
		t.Fatal(err)
	}
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	got, _ := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if !bytes.Equal(got, want) {
		t.Fatalf("resumed after conversion: file changed (%d → %d bytes)", len(want), len(got))
	}
}
