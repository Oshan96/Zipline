package main

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeDASH has ffmpeg write a DASH stream. extra are ffmpeg options for the dash muxer.
func makeDASH(t *testing.T, secs int, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	args := []string{"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", fmt.Sprint(secs),
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "fast", "-bf", "2", "-g", "50", "-c:a", "aac",
		"-f", "dash", "-seg_duration", "2"}
	args = append(args, extra...)
	ffmpegRun(t, append(args, filepath.Join(dir, "stream.mpd"))...)
	return dir
}

func downloadDASH(t *testing.T, dir, name string) (*Manager, *Download) {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	t.Cleanup(srv.Close)
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/" + name, Start: true})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	return m, d
}

func TestDASHDownloads(t *testing.T) {
	needFFmpeg(t)
	oldPiece := dashPieceSize
	dashPieceSize = 64 << 10 // so one-file tracks come down in many ranges
	t.Cleanup(func() { dashPieceSize = oldPiece })
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	t.Cleanup(func() { ffmpegPath = orig })
	cases := []struct {
		name  string
		extra []string
		check func(t *testing.T, mpd string)
	}{
		{"template-timeline", []string{"-use_template", "1", "-use_timeline", "1"}, func(t *testing.T, mpd string) {
			if !strings.Contains(mpd, "SegmentTimeline") {
				t.Fatal("expected a SegmentTimeline manifest")
			}
		}},
		{"template-number", []string{"-use_template", "1", "-use_timeline", "0"}, func(t *testing.T, mpd string) {
			if strings.Contains(mpd, "SegmentTimeline") || !strings.Contains(mpd, "$Number") {
				t.Fatal("expected a $Number$ manifest")
			}
		}},
		{"single-file", []string{"-single_file", "1"}, func(t *testing.T, mpd string) {
			if !strings.Contains(mpd, "SegmentBase") && !strings.Contains(mpd, "SegmentList") {
				t.Fatalf("expected one file per track:\n%s", mpd)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := makeDASH(t, 8, c.extra...)
			b, _ := os.ReadFile(filepath.Join(dir, "stream.mpd"))
			c.check(t, string(b))
			_, d := downloadDASH(t, dir, "stream.mpd")
			out := filepath.Join(d.Dir, d.FileName)
			if filepath.Ext(out) != ".mp4" || d.Note != "" || !isRegularMP4(out) {
				t.Fatalf("got %s note=%q regular=%v", d.FileName, d.Note, isRegularMP4(out))
			}
			if e := decodeErrors(t, out); e != "" {
				t.Fatalf("decoder complained:\n%s", e)
			}
			// compare with the pieces themselves (ffmpeg's own DASH reader drops frames at the
			// ends and stalls on single-file manifests, so it can't be the reference)
			video, audio := filepath.Join(dir, "stream-stream0.mp4"), filepath.Join(dir, "stream-stream1.mp4")
			if _, err := os.Stat(video); err != nil {
				video = joinPieces(t, dir, "chunk-stream0-", "init-stream0.m4s", "v.mp4")
				audio = joinPieces(t, dir, "chunk-stream1-", "init-stream1.m4s", "a.mp4")
			}
			checkMerged(t, out, video, audio)
			po := ffprobe(t, out)
			if dd := fnum(po.Format.Duration); math.Abs(dd-8) > 0.15 {
				t.Fatalf("duration %.3f", dd)
			}
			t.Logf("%s: %d+%d pieces", c.name, d.Parts, d.AParts)
			if c.name == "single-file" && d.Parts < 3 {
				t.Fatalf("expected the one-file track in several ranges, got %d", d.Parts)
			}
		})
	}
}

// TestDASHQualities: the best quality is picked by default, and a chosen one is honoured.
func TestDASHQualities(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "4",
		"-map", "0:v", "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-s:v:0", "1280x720", "-b:v:0", "1500k", "-s:v:1", "426x240", "-b:v:1", "300k",
		"-c:a", "aac", "-f", "dash", "-seg_duration", "2", "-adaptation_sets", "id=0,streams=v id=1,streams=a", filepath.Join(dir, "q.mpd"))
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	info, err := m.resolveHLS(t.Context(), &Download{}, srv.URL+"/q.mpd", 0)
	if err != nil || info.Format != "DASH" || len(info.Variants) != 2 || info.Chosen.Resolution != "1280x720" || info.Audio == nil {
		t.Fatalf("info %+v err %v", info, err)
	}
	low := info.Variants[1].Bandwidth
	d := m.Add(AddReq{URL: srv.URL + "/q.mpd", Variant: low, Start: true})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	po := ffprobe(t, filepath.Join(d.Dir, d.FileName))
	if v := po.stream("video"); v == nil || v.Width != 426 || po.stream("audio") == nil {
		t.Fatalf("expected the 426x240 quality with audio: %+v", po)
	}
}

func TestDASHRefusesDRMAndLive(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/drm.mpd", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT10S"><Period><AdaptationSet contentType="video" mimeType="video/mp4"><ContentProtection schemeIdUri="urn:uuid:edef8ba9-79d6-4ace-a3c8-27dcd51d21ed"/><Representation id="v" bandwidth="1000"><SegmentTemplate media="v$Number$.m4s" initialization="v.mp4" duration="2" timescale="1"/></Representation></AdaptationSet></Period></MPD>`)
	})
	mux.HandleFunc("/live.mpd", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic"><Period/></MPD>`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m := newTestManager(t)
	for path, want := range map[string]string{"/drm.mpd": "DRM", "/live.mpd": "live"} {
		d := m.Add(AddReq{URL: srv.URL + path, Start: true})
		waitFor(t, m, d, StError, 10*time.Second)
		if !strings.Contains(d.Error, want) {
			t.Fatalf("%s: expected %q in error, got %q", path, want, d.Error)
		}
	}
}

func TestDASHTemplateAndDuration(t *testing.T) {
	if got := expandTemplate("seg-$RepresentationID$-$Number%05d$-$Time$-$Bandwidth$$$.m4s", "v1", 800000, 7, 90000); got != "seg-v1-00007-90000-800000$.m4s" {
		t.Fatal(got)
	}
	for s, want := range map[string]float64{"PT1H2M3.5S": 3723.5, "PT0.4S": 0.4, "P1DT1S": 86401, "PT10M": 600, "": 0, "junk": 0} {
		if got := isoDuration(s); math.Abs(got-want) > 1e-9 {
			t.Fatalf("%q: %v", s, got)
		}
	}
}

// TestDASHWebM: VP9 video and Opus audio as separate WebM tracks become one WebM.
func TestDASHWebM(t *testing.T) {
	needFFmpeg(t)
	for _, mode := range []string{"pieces", "single-file"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6",
				"-map", "0:v", "-map", "1:a", "-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-g", "25", "-b:v", "300k", "-c:a", "libopus",
				"-f", "dash", "-dash_segment_type", "webm", "-seg_duration", "2"}
			if mode == "single-file" {
				args = append(args, "-single_file", "1")
			}
			ffmpegRun(t, append(args, filepath.Join(dir, "stream.mpd"))...)
			_, d := downloadDASH(t, dir, "stream.mpd")
			out := filepath.Join(d.Dir, d.FileName)
			if filepath.Ext(out) != ".webm" || d.Note != "" || !isWebMFile(out) {
				t.Fatalf("got %s note=%q", d.FileName, d.Note)
			}
			video, audio := filepath.Join(dir, "stream-stream0.webm"), filepath.Join(dir, "stream-stream1.webm")
			if _, err := os.Stat(video); err != nil {
				video = joinPieces(t, dir, "chunk-stream0-", "init-stream0.webm", "v.webm")
				audio = joinPieces(t, dir, "chunk-stream1-", "init-stream1.webm", "a.webm")
			}
			po := ffprobe(t, out)
			if po.stream("video") == nil || po.stream("video").Codec != "vp9" || po.stream("audio") == nil || po.stream("audio").Codec != "opus" {
				t.Fatalf("streams: %+v", po.Streams)
			}
			if dd := fnum(po.Format.Duration); math.Abs(dd-6) > 0.15 {
				t.Fatalf("duration %.3f", dd)
			}
			if e := decodeErrors(t, out); e != "" {
				t.Fatalf("decoder complained:\n%s", e)
			}
			sameHashes(t, "video", decodedHashes(t, video, "v:0"), decodedHashes(t, out, "v:0"))
			sameHashes(t, "audio", decodedHashes(t, audio, "a:0"), decodedHashes(t, out, "a:0"))
			want := streamStart(t, video, "v:0") - streamStart(t, audio, "a:0")
			if got := streamStart(t, out, "v:0") - streamStart(t, out, "a:0"); math.Abs(want-got) > 0.002 {
				t.Fatalf("audio/video offset %.4f, want %.4f", got, want)
			}
			// seeking uses the index (Cues): decoding from 4s must start at a keyframe near 4s
			b, err := exec.Command("ffprobe", "-v", "error", "-read_intervals", "4%+0.5", "-select_streams", "v:0", "-show_entries", "frame=pts_time,key_frame", "-of", "csv=p=0", out).Output()
			if err != nil || !strings.HasPrefix(strings.TrimSpace(string(b)), "1,4.") { // key_frame,pts_time
				t.Fatalf("seek to 4s: %v %q", err, b)
			}
		})
	}
}
