package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// makeSeparateHLS has ffmpeg write an HLS stream whose audio is a separate rendition.
// kind is "ts" or "fmp4". Returns the folder holding master.m3u8.
func makeSeparateHLS(t *testing.T, kind string, secs int) string {
	t.Helper()
	dir := t.TempDir()
	args := []string{"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", strconv.Itoa(secs),
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "fast", "-bf", "2", "-g", "50", "-c:a", "aac",
		"-f", "hls", "-hls_time", "2", "-hls_playlist_type", "vod", "-master_pl_name", "master.m3u8",
		"-var_stream_map", "v:0,agroup:aud a:0,agroup:aud,default:yes"}
	if kind == "fmp4" {
		args = append(args, "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4", "-hls_segment_filename", filepath.Join(dir, "s%v_%03d.m4s"))
	} else {
		args = append(args, "-hls_segment_filename", filepath.Join(dir, "s%v_%03d.ts"))
	}
	ffmpegRun(t, append(args, filepath.Join(dir, "p%v.m3u8"))...)
	return dir
}

// joinPieces concatenates a rendition's pieces the way the downloader writes them.
func joinPieces(t *testing.T, dir, prefix, init, out string) string {
	t.Helper()
	var all []byte
	if init != "" {
		b, err := os.ReadFile(filepath.Join(dir, init))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, b...)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, prefix+"*"))
	sort.Strings(segs)
	for _, s := range segs {
		b, _ := os.ReadFile(s)
		all = append(all, b...)
	}
	p := filepath.Join(t.TempDir(), out)
	os.WriteFile(p, all, 0o644)
	return p
}

func streamStart(t *testing.T, path, sel string) float64 {
	t.Helper()
	b, err := exec.Command("ffprobe", "-v", "error", "-select_streams", sel, "-show_entries", "stream=start_time", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	return fnum(strings.Trim(strings.TrimSpace(strings.Split(string(b), "\n")[0]), ","))
}

func checkMerged(t *testing.T, out, video, audio string) {
	t.Helper()
	po := ffprobe(t, out)
	if po.stream("video") == nil || po.stream("audio") == nil || !isRegularMP4(out) {
		t.Fatalf("expected one regular MP4 with video and audio: %+v", po)
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	sameHashes(t, "video", decodedHashes(t, video, "v:0"), decodedHashes(t, out, "v:0"))
	sameHashes(t, "audio", decodedHashes(t, audio, "a:0"), decodedHashes(t, out, "a:0"))
	want := streamStart(t, video, "v:0") - streamStart(t, audio, "a:0")
	got := streamStart(t, out, "v:0") - streamStart(t, out, "a:0")
	if math.Abs(want-got) > 0.002 {
		t.Fatalf("audio/video offset %.4fs, want %.4fs", got, want)
	}
}

func TestMergeTSWithSeparateAudio(t *testing.T) {
	needFFmpeg(t)
	dir := makeSeparateHLS(t, "ts", 8)
	video := joinPieces(t, dir, "s0_", "", "video.ts")
	audio := joinPieces(t, dir, "s1_", "", "audio.ts")
	out := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := remuxFiles(context.Background(), out, []string{video, audio}, []bool{true, false}, nil); err != nil {
		t.Fatal(err)
	}
	checkMerged(t, out, video, audio)
}

func TestMergeFMP4WithSeparateAudio(t *testing.T) {
	needFFmpeg(t)
	dir := makeSeparateHLS(t, "fmp4", 8)
	video := joinPieces(t, dir, "s0_", "init_0.mp4", "video.mp4")
	audio := joinPieces(t, dir, "s1_", "init_1.mp4", "audio.mp4")
	if isRegularMP4(video) {
		t.Fatal("the downloaded pieces should look fragmented")
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := remuxFiles(context.Background(), out, []string{video, audio}, []bool{true, false}, nil); err != nil {
		t.Fatal(err)
	}
	checkMerged(t, out, video, audio)
}

// id3Timestamp builds the ID3 tag HLS packed audio starts each piece with.
func id3Timestamp(pts int64) []byte {
	owner := []byte(id3TimestampOwner)
	body := append(append([]byte{}, owner...), make([]byte, 8)...)
	binary.BigEndian.PutUint64(body[len(owner):], uint64(pts))
	ss := func(n int) []byte {
		return []byte{byte(n >> 21 & 0x7f), byte(n >> 14 & 0x7f), byte(n >> 7 & 0x7f), byte(n & 0x7f)}
	}
	frame := append(append([]byte("PRIV"), ss(len(body))...), 0, 0)
	frame = append(frame, body...)
	return append(append([]byte{'I', 'D', '3', 4, 0, 0}, ss(len(frame))...), frame...)
}

// packAudio turns audio-only .ts pieces into packed audio (ID3 timestamp + raw ADTS frames).
func packAudio(t *testing.T, dir, prefix string) string {
	t.Helper()
	segs, _ := filepath.Glob(filepath.Join(dir, prefix+"*.ts"))
	sort.Strings(segs)
	var all []byte
	for _, s := range segs {
		b, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0", "-show_entries", "packet=pts", "-of", "csv=p=0", "-read_intervals", "%+#1", s).Output()
		if err != nil {
			t.Fatal(err)
		}
		pts, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(strings.Split(string(b), "\n")[0]), ","), 10, 64)
		if err != nil || pts <= 0 {
			t.Fatalf("first audio timestamp of %s: %q", s, b)
		}
		adts := filepath.Join(t.TempDir(), "x.aac")
		ffmpegRun(t, "-i", s, "-c", "copy", "-f", "adts", adts)
		raw, _ := os.ReadFile(adts)
		all = append(all, id3Timestamp(pts)...)
		all = append(all, raw...)
	}
	p := filepath.Join(t.TempDir(), "audio.aac")
	os.WriteFile(p, all, 0o644)
	return p
}

func TestMergeTSWithPackedAudio(t *testing.T) {
	needFFmpeg(t)
	dir := makeSeparateHLS(t, "ts", 8)
	video := joinPieces(t, dir, "s0_", "", "video.ts")
	audioTS := joinPieces(t, dir, "s1_", "", "audio.ts")
	packed := packAudio(t, dir, "s1_")
	out := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := remuxFiles(context.Background(), out, []string{video, packed}, []bool{true, false}, nil); err != nil {
		t.Fatal(err)
	}
	// same frames and same timing as the .ts version of that audio
	checkMerged(t, out, video, audioTS)
}

// TestFMP4ToRegularMP4 rewrites fragmented MP4 (both tracks in one file, B-frames with
// negative composition offsets) as a regular MP4.
func TestFMP4ToRegularMP4(t *testing.T) {
	needFFmpeg(t)
	in := filepath.Join(t.TempDir(), "frag.mp4")
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "5",
		"-c:v", "libx264", "-bf", "3", "-g", "30", "-c:a", "aac", "-movflags", "frag_keyframe+empty_moov+default_base_moof+negative_cts_offsets", in)
	out := filepath.Join(t.TempDir(), "out.mp4")
	res, err := remuxFiles(context.Background(), out, []string{in}, nil, nil)
	if err != nil || !res.video || !res.audio {
		t.Fatal(res, err)
	}
	if !isRegularMP4(out) || isRegularMP4(in) {
		t.Fatal("expected fragmented in, regular out")
	}
	sameHashes(t, "video", decodedHashes(t, in, "v:0"), decodedHashes(t, out, "v:0"))
	sameHashes(t, "audio", decodedHashes(t, in, "a:0"), decodedHashes(t, out, "a:0"))
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	pi, po := ffprobe(t, in), ffprobe(t, out)
	if d := math.Abs(fnum(pi.Format.Duration) - fnum(po.Format.Duration)); d > 0.05 {
		t.Fatalf("duration %s → %s", pi.Format.Duration, po.Format.Duration)
	}
}

// slowFiles serves a folder with a delay per request, counting requests.
func slowFiles(dir string, delay time.Duration, hits *int32) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		if !strings.HasSuffix(r.URL.Path, ".m3u8") {
			time.Sleep(delay)
		}
		fs.ServeHTTP(w, r)
	})
}

// TestHLSSeparateAudioDownload downloads real streams with a separate audio track (TS and
// MP4 pieces), pausing part-way, and checks the result is one MP4 with both tracks, made
// without ffmpeg.
func TestHLSSeparateAudioDownload(t *testing.T) {
	needFFmpeg(t)
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	t.Cleanup(func() { ffmpegPath = orig })
	for _, kind := range []string{"ts", "fmp4"} {
		t.Run(kind, func(t *testing.T) {
			dir := makeSeparateHLS(t, kind, 12)
			var hits int32
			srv := httptest.NewServer(slowFiles(dir, 25*time.Millisecond, &hits))
			defer srv.Close()
			m := newTestManager(t)
			m.settings.Conns = 2
			d := m.Add(AddReq{URL: srv.URL + "/master.m3u8", Name: "talk.ts", Start: true})
			time.Sleep(120 * time.Millisecond)
			m.Pause([]string{d.ID})
			waitFor(t, m, d, StPaused, 10*time.Second)
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
			snapA, snapV := d.ADone, d.PartsDone
			m.mu.Unlock()
			if snapA == 0 || d.AParts == 0 || snapV > 0 && snapA < d.AParts {
				t.Fatalf("expected the audio to come first and the pause to land part-way: audio %d/%d video %d/%d", snapA, d.AParts, snapV, d.Parts)
			}
			v := m.Snapshot(nil).Downloads[0]
			if v.Prog <= 0 || v.Prog >= 1 || v.AParts != d.AParts {
				t.Fatalf("progress while paused: %+v", v)
			}
			m.Start([]string{d.ID})
			waitFor(t, m, d, StCompleted, 60*time.Second)
			out := filepath.Join(d.Dir, d.FileName)
			if filepath.Ext(out) != ".mp4" || d.Note != "" {
				t.Fatalf("got %s (note %q)", d.FileName, d.Note)
			}
			if _, err := os.Stat(d.audioPartPath()); err == nil {
				t.Fatal("the audio part file was left behind")
			}
			checkMerged(t, out, filepath.Join(dir, "p0.m3u8"), filepath.Join(dir, "p1.m3u8"))
			t.Logf("%s: paused at audio %d/%d, video %d/%d; %d requests", kind, snapA, d.AParts, snapV, d.Parts, hits)
		})
	}
}

// TestHLSSeparateAudioFallback: when the audio can't be merged, the video is still saved as
// MP4 and the audio is kept next to it.
func TestHLSSeparateAudioFallback(t *testing.T) {
	needFFmpeg(t)
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	t.Cleanup(func() { ffmpegPath = orig })
	dir := makeSeparateHLS(t, "ts", 6)
	// replace the audio pieces with junk that looks like TS (the packets are there) but holds no audio
	segs, _ := filepath.Glob(filepath.Join(dir, "s1_*.ts"))
	for _, s := range segs {
		os.WriteFile(s, fakeTS(5000/188), 0o644)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/master.m3u8", Name: "clip.mp4", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	if filepath.Ext(d.FileName) != ".mp4" || !strings.Contains(d.Note, "saved separately as clip (audio).ts") {
		t.Fatalf("got %s, note %q", d.FileName, d.Note)
	}
	if fi, err := os.Stat(filepath.Join(d.Dir, "clip (audio).ts")); err != nil || fi.Size() != int64(len(segs))*(5000/188*188) {
		t.Fatalf("separate audio file: %v", err)
	}
	po := ffprobe(t, filepath.Join(d.Dir, d.FileName))
	if po.stream("video") == nil || po.stream("audio") != nil {
		t.Fatalf("expected the video alone: %+v", po)
	}
}

func TestHLSProgressWeights(t *testing.T) {
	d := &Download{Parts: 10, PartsDone: 5, PartBytes: 50 << 20, AParts: 10, ADone: 10, ABytes: 5 << 20}
	p, est := hlsProgress(d)
	if est != 105<<20 || math.Abs(p-(100.0/105*0.5+5.0/105)) > 1e-9 {
		t.Fatalf("progress %.4f est %d", p, est)
	}
	d = &Download{Parts: 10, AParts: 10, ADone: 5, ABytes: 1 << 20}
	if p, est = hlsProgress(d); p <= 0 || p > 0.08 || est < 10<<20 {
		t.Fatalf("audio-only so far: %.4f %d", p, est)
	}
	_ = fmt.Sprint(bytes.MinRead)
}

// TestHLSFragmentedRealToRegular: a stream of MP4 pieces (audio and video together) is saved
// as a regular MP4, which more players and editors accept than the raw pieces.
func TestHLSFragmentedRealToRegular(t *testing.T) {
	needFFmpeg(t)
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	t.Cleanup(func() { ffmpegPath = orig })
	dir := t.TempDir()
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6",
		"-c:v", "libx264", "-preset", "fast", "-g", "25", "-c:a", "aac", "-f", "hls", "-hls_time", "2", "-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4", "-hls_segment_filename", filepath.Join(dir, "s%03d.m4s"), filepath.Join(dir, "index.m3u8"))
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/index.m3u8", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	out := filepath.Join(d.Dir, d.FileName)
	if filepath.Ext(out) != ".mp4" || d.Note != "" || !isRegularMP4(out) {
		t.Fatalf("got %s regular=%v note=%q", d.FileName, isRegularMP4(out), d.Note)
	}
	src := filepath.Join(dir, "index.m3u8")
	sameHashes(t, "video", decodedHashes(t, src, "v:0"), decodedHashes(t, out, "v:0"))
	sameHashes(t, "audio", decodedHashes(t, src, "a:0"), decodedHashes(t, out, "a:0"))
}
