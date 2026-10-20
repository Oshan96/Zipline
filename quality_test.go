package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeQualityHLS makes four qualities: H.264 and HEVC at the same bandwidth, a 60 fps one with
// the most bandwidth of the SDR ones, and an HDR-flagged HEVC one above them all.
func makeQualityHLS(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := func(rate string, size string) []string {
		return []string{"-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=" + rate, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "4",
			"-c:a", "aac", "-g", "50"}
	}
	ffHLS(t, dir, "h264", "ts", append(src("25", "640x360"), "-c:v", "libx264", "-preset", "ultrafast")...)
	ffHLS(t, dir, "hevc", "ts", append(src("25", "640x360"), "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error")...)
	ffHLS(t, dir, "h60", "ts", append(src("60", "640x360"), "-c:v", "libx264", "-preset", "ultrafast")...)
	ffHLS(t, dir, "hdr", "fmp4", append(src("25", "1280x720"), "-c:v", "libx265", "-preset", "ultrafast", "-tag:v", "hvc1", "-x265-params", "log-level=error")...)
	master := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=5000000,AVERAGE-BANDWIDTH=4000000,RESOLUTION=1280x720,FRAME-RATE=25.000,CODECS="hvc1.2.4.L93.B0,mp4a.40.2",VIDEO-RANGE=PQ
hdr.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1200000,RESOLUTION=640x360,FRAME-RATE=60.000,CODECS="avc1.64001f,mp4a.40.2",VIDEO-RANGE=SDR
h60.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=640x360,FRAME-RATE=25.000,CODECS="avc1.64001e,mp4a.40.2",VIDEO-RANGE=SDR
h264.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=1000000,RESOLUTION=640x360,FRAME-RATE=25.000,CODECS="hvc1.1.6.L90.B0,mp4a.40.2",VIDEO-RANGE=SDR
hevc.m3u8
`
	os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(master), 0o644)
	return dir
}

func TestHLSQualityPicker(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := makeQualityHLS(t)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	u := srv.URL + "/master.m3u8"
	info, err := m.resolveHLS(t.Context(), &Download{}, u, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Variants) != 4 || info.Variants[0].VideoRange != "PQ" || info.Variants[0].AvgBandwidth != 4000000 || info.Variants[1].FrameRate != 60 {
		t.Fatalf("variants %+v", info.Variants)
	}
	if !strings.HasSuffix(info.Chosen.URL, "/h60.m3u8") { // the best SDR one, not the HDR one
		t.Fatalf("default quality %+v", info.Chosen)
	}
	video := func(d *Download) tagStream {
		t.Helper()
		ss, _, _ := probeTags(t, filepath.Join(d.Dir, d.FileName))
		v := streamsOf(ss, "video")
		if len(v) != 1 || len(streamsOf(ss, "audio")) != 1 {
			t.Fatalf("%s: streams %+v", d.FileName, ss)
		}
		return v[0]
	}
	if v := video(addStream(t, m, AddReq{URL: u, Name: "best.mp4"})); v.Codec != "h264" || v.Rate != "60/1" {
		t.Fatalf("default download: %+v", v)
	}
	pick := func(v hlsVariant, name string) *Download {
		return addStream(t, m, AddReq{URL: u, Name: name, Variant: v.Bandwidth, VarRes: v.Resolution, VarCodecs: v.Codecs, VarFPS: v.FrameRate})
	}
	// H.264 and HEVC share a bandwidth: the codecs tell them apart
	if v := video(pick(info.Variants[3], "hevc.mp4")); v.Codec != "hevc" || v.Width != 640 {
		t.Fatalf("HEVC pick: %+v", v)
	}
	if v := video(pick(info.Variants[2], "h264.mp4")); v.Codec != "h264" || v.Rate != "25/1" {
		t.Fatalf("H.264 pick: %+v", v)
	}
	if v := video(pick(info.Variants[0], "hdr.mp4")); v.Codec != "hevc" || v.Width != 1280 {
		t.Fatalf("HDR pick: %+v", v)
	}
	// the sound alone
	d := addStream(t, m, AddReq{URL: u, Name: "sound.mp4", StreamPick: StreamPick{AudioOnly: true}})
	ss, format, _ := probeTags(t, filepath.Join(d.Dir, d.FileName))
	if filepath.Ext(d.FileName) != ".m4a" || len(ss) != 1 || ss[0].Codec != "aac" || !strings.Contains(format, "mp4") {
		t.Fatalf("audio only: %s %+v", d.FileName, ss)
	}
}

// TestDASHWebMVideoWithAAC: VP9 in WebM pieces with AAC in MP4 pieces can't be one MP4 or WebM:
// it becomes Matroska.
func TestDASHWebMVideoWithAAC(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := t.TempDir()
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6",
		"-map", "0:v", "-map", "1:a", "-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-g", "25", "-b:v", "300k", "-c:a", "aac",
		"-f", "dash", "-dash_segment_type", "auto", "-seg_duration", "2", "-adaptation_sets", "id=0,streams=v id=1,streams=a", filepath.Join(dir, "s.mpd"))
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	info, err := m.resolveHLS(t.Context(), &Download{}, srv.URL+"/s.mpd", 0)
	if err != nil || info.Ext != ".mkv" {
		t.Fatalf("expected .mkv from the manifest: %+v %v", info, err)
	}
	d := addStream(t, m, AddReq{URL: srv.URL + "/s.mpd"})
	out := filepath.Join(d.Dir, d.FileName)
	ss, format, dur := probeTags(t, out)
	if filepath.Ext(out) != ".mkv" || !strings.Contains(format, "matroska") || len(ss) != 2 || ss[0].Codec != "vp9" || ss[1].Codec != "aac" || d.Note != "" {
		t.Fatalf("got %s (%s) %+v note %q", d.FileName, format, ss, d.Note)
	}
	if dur < 5.8 || dur > 6.3 {
		t.Fatalf("duration %.3f", dur)
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	video := joinPieces(t, dir, "chunk-stream0-", "init-stream0.webm", "v.webm")
	audio := joinPieces(t, dir, "chunk-stream1-", "init-stream1.m4s", "a.mp4")
	sameHashes(t, "video", decodedHashes(t, video, "v:0"), decodedHashes(t, out, "v:0"))
	sameHashes(t, "audio", decodedHashes(t, audio, "a:0"), decodedHashes(t, out, "a:0"))
	want := streamStart(t, video, "v:0") - streamStart(t, audio, "a:0")
	if got := streamStart(t, out, "v:0") - streamStart(t, out, "a:0"); math.Abs(want-got) > 0.002 {
		t.Fatalf("audio/video offset %.4f, want %.4f", got, want)
	}
}

// TestDASHAudioOnlyOpus: the sound alone of a WebM stream is saved as Ogg Opus.
func TestDASHAudioOnlyOpus(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := t.TempDir()
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "5",
		"-map", "0:v", "-map", "1:a", "-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-g", "25", "-b:v", "200k", "-c:a", "libopus",
		"-f", "dash", "-dash_segment_type", "webm", "-seg_duration", "2", filepath.Join(dir, "s.mpd"))
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	d := addStream(t, m, AddReq{URL: srv.URL + "/s.mpd", StreamPick: StreamPick{AudioOnly: true}})
	out := filepath.Join(d.Dir, d.FileName)
	ss, format, dur := probeTags(t, out)
	if filepath.Ext(out) != ".opus" || format != "ogg" || len(ss) != 1 || ss[0].Codec != "opus" {
		t.Fatalf("got %s (%s) %+v", d.FileName, format, ss)
	}
	if dur < 4.8 || dur > 5.3 {
		t.Fatalf("duration %.3f", dur)
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	audio := joinPieces(t, dir, "chunk-stream1-", "init-stream1.webm", "a.webm")
	sameHashes(t, "audio", decodedHashes(t, audio, "a:0"), decodedHashes(t, out, "a:0"))
}

// TestDASHH264WithOpus: H.264 (with B-frames) from MP4 pieces and Opus from WebM pieces make a
// Matroska file; the frames and their order survive.
func TestDASHH264WithOpus(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := t.TempDir()
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "fast", "-bf", "2", "-g", "50", "-c:a", "libopus",
		"-f", "dash", "-dash_segment_type", "auto", "-seg_duration", "2", "-adaptation_sets", "id=0,streams=v id=1,streams=a", filepath.Join(dir, "s.mpd"))
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	d := addStream(t, m, AddReq{URL: srv.URL + "/s.mpd"})
	out := filepath.Join(d.Dir, d.FileName)
	ss, format, dur := probeTags(t, out)
	if filepath.Ext(out) != ".mkv" || !strings.Contains(format, "matroska") || len(ss) != 2 || ss[0].Codec != "h264" || ss[1].Codec != "opus" {
		t.Fatalf("got %s (%s) %+v", d.FileName, format, ss)
	}
	if dur < 5.9 || dur > 6.2 {
		t.Fatalf("duration %.3f", dur)
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	video := joinPieces(t, dir, "chunk-stream0-", "init-stream0.m4s", "v.mp4")
	audio := joinPieces(t, dir, "chunk-stream1-", "init-stream1.webm", "a.webm")
	sameHashes(t, "video", decodedHashes(t, video, "v:0"), decodedHashes(t, out, "v:0"))
	sameHashes(t, "audio", decodedHashes(t, audio, "a:0"), decodedHashes(t, out, "a:0"))
	checkContinuous(t, out, "v:0", 0.05)
	want := streamStart(t, video, "v:0") - streamStart(t, audio, "a:0")
	if got := streamStart(t, out, "v:0") - streamStart(t, out, "a:0"); math.Abs(want-got) > 0.01 {
		t.Fatalf("audio/video offset %.4f, want %.4f", got, want)
	}
}
