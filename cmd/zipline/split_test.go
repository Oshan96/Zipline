package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// splitClip has ffmpeg write a clip as a player streams it: the picture in one file and the sound in
// another (DASH's one-file-per-track MP4s), copied into dir as <name>-v.mp4 and <name>-a.mp4.
func splitClip(t *testing.T, dir, name string, secs int, size string) {
	t.Helper()
	tmp := t.TempDir()
	ext, codecs := ".mp4", []string{"-c:v", "libx264", "-preset", "fast", "-g", "50", "-c:a", "aac", "-global_sidx", "1"}
	if strings.HasSuffix(name, "webm") {
		ext, codecs = ".webm", []string{"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-g", "25", "-b:v", "300k", "-c:a", "libopus", "-dash_segment_type", "webm"}
	}
	args := []string{"-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", fmt.Sprint(secs),
		"-map", "0:v", "-map", "1:a"}
	ffmpegRun(t, append(append(args, codecs...), "-f", "dash", "-seg_duration", "2", "-single_file", "1", filepath.Join(tmp, "s.mpd"))...)
	for from, to := range map[string]string{"s-stream0" + ext: name + "-v" + ext, "s-stream1" + ext: name + "-a" + ext} {
		b, err := os.ReadFile(filepath.Join(tmp, from))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, to), b, 0o600)
	}
}

// A page played two videos, the second (8 s) in two qualities, each as separate picture and sound
// files fetched in pieces. The newest picture's video comes down: its best quality with its own sound
// (not the other video's), combined into one MP4 with both exactly as they were.
func TestSplitVideo(t *testing.T) {
	needFFmpeg(t)
	orig := ffmpegPath
	ffmpegPath = func() string { return "" } // (Zipline's own converter, as on a computer without ffmpeg)
	t.Cleanup(func() { ffmpegPath = orig })
	oldPiece := dashPieceSize
	dashPieceSize = 64 << 10 // so each file comes down in many ranges
	t.Cleanup(func() { dashPieceSize = oldPiece })

	dir := t.TempDir()
	splitClip(t, dir, "first", 5, "320x240")
	splitClip(t, dir, "hd", 8, "640x360")
	splitClip(t, dir, "sd", 8, "320x180")
	os.Remove(filepath.Join(dir, "sd-a.mp4")) // (both qualities share one sound)
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.URL.RequestURI())
		mu.Unlock()
		if r.Header.Get("Referer") != "https://videos.example/watch/2" {
			http.Error(w, "no", 403)
			return
		}
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	piece := func(name string) string { return srv.URL + "/" + name + "?sig=a%2Fb&bytestart=0&byteend=65535" }
	split := []string{piece("sd-v.mp4"), piece("hd-a.mp4"), piece("first-v.mp4"), piece("hd-v.mp4"), piece("first-a.mp4")} // newest first
	hdr := map[string]string{"Referer": "https://videos.example/watch/2"}

	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	res := apiCall(t, h, "probe", map[string]any{"url": split[0], "split": split, "headers": hdr, "title": "Second video"})
	vs, _ := res["variants"].([]any)
	if res["kind"] != "hls" || res["split"] != true || res["separateAudio"] != true || len(vs) != 2 || math.Abs(res["duration"].(float64)-8) > 0.1 ||
		res["name"] != "Second video.mp4" || res["error"] != nil {
		t.Fatalf("probe: %v", res)
	}
	mu.Lock()
	for _, a := range asked {
		if strings.Contains(a, "bytestart") || !strings.Contains(a, "sig=a%2Fb") {
			t.Fatalf("asked for %s: the piece's own range in the address, or the rest of it changed", a)
		}
	}
	mu.Unlock()

	d := m.Add(AddReq{URL: split[0], Split: split, Headers: hdr, Name: "Second video.mp4", Start: true})
	waitFor(t, m, d, StCompleted, 90*time.Second)
	out := filepath.Join(d.Dir, d.FileName)
	checkMerged(t, out, filepath.Join(dir, "hd-v.mp4"), filepath.Join(dir, "hd-a.mp4"))
	po := ffprobe(t, out)
	if v := po.stream("video"); v == nil || v.Width != 640 || math.Abs(fnum(po.Format.Duration)-8) > 0.15 || d.Parts < 3 || d.AParts < 1 {
		t.Fatalf("got %+v, %d+%d pieces", po, d.Parts, d.AParts)
	}
}

// WebM: VP9 picture, Opus sound, into one WebM.
func TestSplitWebM(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	splitClip(t, dir, "webm", 4, "320x240")
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/webm-a.webm", Split: []string{srv.URL + "/webm-a.webm", srv.URL + "/webm-v.webm"}, Name: "clip", Start: true})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	out := filepath.Join(d.Dir, d.FileName)
	po := ffprobe(t, out)
	if filepath.Ext(out) != ".webm" || po.stream("video") == nil || po.stream("video").Codec != "vp9" || po.stream("audio") == nil || po.stream("audio").Codec != "opus" {
		t.Fatalf("%s: %+v", d.FileName, po.Streams)
	}
	sameHashes(t, "video", decodedHashes(t, filepath.Join(dir, "webm-v.webm"), "v:0"), decodedHashes(t, out, "v:0"))
	sameHashes(t, "audio", decodedHashes(t, filepath.Join(dir, "webm-a.webm"), "a:0"), decodedHashes(t, out, "a:0"))
}

// An encrypted file is DRM: refused, as a DASH stream's is. A protected site's files never get that far.
func TestSplitRefusals(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	splitClip(t, dir, "c", 4, "320x240")
	v, _ := os.ReadFile(filepath.Join(dir, "c-v.mp4"))
	os.WriteFile(filepath.Join(dir, "c-v.mp4"), bytes.Replace(v, []byte("avc1"), []byte("encv"), 1), 0o600)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	split := []string{srv.URL + "/c-v.mp4", srv.URL + "/c-a.mp4"}
	if res := apiCall(t, h, "probe", map[string]any{"url": split[0], "split": split}); !strings.Contains(fmt.Sprint(res["error"]), "DRM") {
		t.Fatalf("an encrypted picture: %v", res)
	}
	d := m.Add(AddReq{URL: split[0], Split: split, Start: true})
	waitFor(t, m, d, StError, 30*time.Second)
	if !strings.Contains(d.Error, "DRM") {
		t.Fatalf("error %q", d.Error)
	}

	yt := []string{"https://rr3---sn-abc.googlevideo.com/videoplayback?itag=137&range=0-999", "https://rr3---sn-abc.googlevideo.com/videoplayback?itag=140&range=0-999"}
	for _, c := range []struct {
		split []string
		ref   string
	}{{yt, ""}, {split, "https://www.youtube.com/watch?v=x"}} {
		if res := apiCall(t, h, "probe", map[string]any{"url": c.split[0], "split": c.split, "headers": map[string]string{"Referer": c.ref}}); res["protected"] != true {
			t.Errorf("probe %v: %v", c, res)
		}
		if res := apiCall(t, h, "ext/add", map[string]any{"url": c.split[0], "split": c.split, "referer": c.ref}); res["protected"] != true {
			t.Errorf("from the extension %v: %v", c, res)
		}
	}
}

func TestWholeFile(t *testing.T) {
	for in, want := range map[string]string{
		"https://cdn.example/v/1.mp4?oh=a%2Fb&bytestart=0&byteend=1023&oe=6": "https://cdn.example/v/1.mp4?oh=a%2Fb&oe=6",
		"https://cdn.example/p/a.mp4?pathsig=x&range=100-200":                "https://cdn.example/p/a.mp4?pathsig=x",
		"https://cdn.example/a.webm?Range=1-2":                               "https://cdn.example/a.webm",
		"https://cdn.example/a.mp4?rangeX=1":                                 "https://cdn.example/a.mp4?rangeX=1",
	} {
		if got := wholeFile(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
	if got := cleanSplit([]string{"ftp://x/a", "https://a.example/1.mp4?range=0-9", "https://a.example/1.mp4", "javascript:x", "https://a.example/2.mp4"}); strings.Join(got, " ") != "https://a.example/1.mp4 https://a.example/2.mp4" {
		t.Errorf("cleanSplit: %v", got)
	}
}

// splitParts on its own: the newest picture decides; files that don't say how long they are go with anything.
func TestSplitParts(t *testing.T) {
	f := func(video, audio bool, secs float64) *splitFile {
		sf := &splitFile{}
		sf.info.Video, sf.info.Audio, sf.info.Duration = video, audio, secs
		return sf
	}
	a8, v5, a5, v8 := f(false, true, 8.02), f(true, false, 5), f(false, true, 5.1), f(true, false, 8)
	vs, as, err := splitParts([]*splitFile{a8, v5, a5, v8})
	if err != nil || len(vs) != 1 || vs[0] != v5 || len(as) != 1 || as[0] != a5 {
		t.Fatalf("%v %v %v", vs, as, err)
	}
	whole := f(true, true, 8)
	if vs, as, _ := splitParts([]*splitFile{whole, a8, v8}); len(vs) != 1 || vs[0] != whole || len(as) != 0 {
		t.Errorf("a file with both: %v %v", vs, as)
	}
	if vs, as, _ := splitParts([]*splitFile{a8}); len(vs) != 0 || len(as) != 1 {
		t.Errorf("sound alone: %v %v", vs, as)
	}
	bad := &splitFile{err: context.DeadlineExceeded}
	if _, _, err := splitParts([]*splitFile{bad}); err != context.DeadlineExceeded {
		t.Errorf("unreadable: %v", err)
	}
}
