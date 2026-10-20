package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests use ffmpeg/ffprobe only to make test streams and to check results.
// The conversion itself is Zipline's own code.

func needFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
}

func ffmpegRun(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", append([]string{"-y", "-v", "error"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

type probeStream struct {
	Type     string         `json:"codec_type"`
	Codec    string         `json:"codec_name"`
	Width    int            `json:"width"`
	Height   int            `json:"height"`
	Frames   string         `json:"nb_frames"`
	Start    string         `json:"start_time"`
	Duration string         `json:"duration"`
	Rate     string         `json:"sample_rate"`
	Channels int            `json:"channels"`
	SAR      string         `json:"sample_aspect_ratio"`
	CodecTag string         `json:"codec_tag_string"`
	Profile  string         `json:"profile"`
	Disposit map[string]int `json:"disposition"`
}

type probeOut struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		Name     string `json:"format_name"`
		Duration string `json:"duration"`
	} `json:"format"`
}

func ffprobe(t *testing.T, path string) probeOut {
	t.Helper()
	b, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p probeOut
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p probeOut) stream(kind string) *probeStream {
	for i := range p.Streams {
		if p.Streams[i].Type == kind {
			return &p.Streams[i]
		}
	}
	return nil
}

func fnum(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

// decodedHashes decodes one stream and returns an MD5 per decoded frame.
func decodedHashes(t *testing.T, path, sel string) []string {
	t.Helper()
	b, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", "0:"+sel, "-f", "framemd5", "-").Output()
	if err != nil {
		t.Fatalf("decode %s %s: %v", path, sel, err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Split(l, ",")
		out = append(out, strings.TrimSpace(f[len(f)-1]))
	}
	return out
}

func decodeErrors(t *testing.T, path string) string {
	t.Helper()
	out, _ := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
	return strings.TrimSpace(string(out))
}

func remuxFile(t *testing.T, in string) (string, remuxResult) {
	t.Helper()
	out := strings.TrimSuffix(in, filepath.Ext(in)) + ".mp4"
	var last float64
	t0 := time.Now()
	res, err := remuxTS(context.Background(), in, out, func(p float64) {
		if p < last-1e-9 {
			t.Errorf("progress went backwards: %v → %v", last, p)
		}
		last = p
	})
	if err != nil {
		t.Fatalf("remux %s: %v", filepath.Base(in), err)
	}
	st, _ := os.Stat(out)
	t.Logf("%s → %d bytes in %v", filepath.Base(in), st.Size(), time.Since(t0).Round(time.Millisecond))
	return out, res
}

func sameHashes(t *testing.T, what string, a, b []string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: %d frames in the source, %d after conversion", what, len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("%s: frame %d differs", what, i)
		}
	}
}

func syncOffset(p probeOut) float64 {
	v, a := p.stream("video"), p.stream("audio")
	return fnum(v.Start) - fnum(a.Start)
}

func TestRemuxTSCodecs(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	src := []string{"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6"}
	cases := []struct {
		name  string
		args  []string
		vcod  string
		acod  string
		check func(t *testing.T, p probeOut)
	}{
		{"h264-bframes-aac", []string{"-c:v", "libx264", "-preset", "fast", "-bf", "3", "-g", "50", "-c:a", "aac", "-ac", "2"}, "h264", "aac", nil},
		{"h264-baseline-mp3", []string{"-c:v", "libx264", "-profile:v", "baseline", "-c:a", "libmp3lame", "-ar", "44100"}, "h264", "mp3", nil},
		{"hevc-ac3", []string{"-c:v", "libx265", "-x265-params", "log-level=error", "-c:a", "ac3", "-ac", "6"}, "hevc", "ac3", func(t *testing.T, p probeOut) {
			if p.stream("video").CodecTag != "hvc1" || p.stream("audio").Channels != 6 {
				t.Fatalf("hevc/ac3 details: %+v %+v", p.stream("video"), p.stream("audio"))
			}
		}},
		{"h264-eac3", []string{"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "eac3", "-ac", "2"}, "h264", "eac3", nil},
		{"h264-anamorphic-hiprofile", []string{"-c:v", "libx264", "-pix_fmt", "yuv422p", "-vf", "setsar=4/3", "-c:a", "aac"}, "h264", "aac", func(t *testing.T, p probeOut) {
			if p.stream("video").SAR != "4:3" {
				t.Fatalf("aspect ratio lost: %q", p.stream("video").SAR)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := filepath.Join(dir, c.name+".ts")
			ffmpegRun(t, append(append(append([]string{}, src...), c.args...), "-f", "mpegts", in)...)
			out, res := remuxFile(t, in)
			if !res.video || !res.audio {
				t.Fatalf("result %+v", res)
			}
			pi, po := ffprobe(t, in), ffprobe(t, out)
			if !strings.Contains(po.Format.Name, "mp4") || po.stream("video").Codec != c.vcod || po.stream("audio").Codec != c.acod {
				t.Fatalf("output: %+v", po)
			}
			if e := decodeErrors(t, out); e != "" {
				t.Fatalf("decoder complained:\n%s", e)
			}
			sameHashes(t, "video", decodedHashes(t, in, "v:0"), decodedHashes(t, out, "v:0"))
			sameHashes(t, "audio", decodedHashes(t, in, "a:0"), decodedHashes(t, out, "a:0"))
			if d := math.Abs(syncOffset(pi) - syncOffset(po)); d > 0.002 {
				t.Fatalf("audio/video offset changed by %.4fs (ts %.4f, mp4 %.4f)", d, syncOffset(pi), syncOffset(po))
			}
			if dd := math.Abs(fnum(pi.Format.Duration) - fnum(po.Format.Duration)); dd > 0.1 {
				t.Fatalf("duration %s vs %s", pi.Format.Duration, po.Format.Duration)
			}
			if c.check != nil {
				c.check(t, po)
			}
		})
	}
}

func TestRemuxTSAudioOnly(t *testing.T) {
	needFFmpeg(t)
	in := filepath.Join(t.TempDir(), "radio.ts")
	ffmpegRun(t, "-f", "lavfi", "-i", "sine=frequency=330:sample_rate=44100", "-t", "5", "-c:a", "aac", "-f", "mpegts", in)
	out, res := remuxFile(t, in)
	if res.video || !res.audio {
		t.Fatalf("result %+v", res)
	}
	sameHashes(t, "audio", decodedHashes(t, in, "a:0"), decodedHashes(t, out, "a:0"))
}

// TestRemuxTSDiscontinuity joins two clips whose timestamps both start from zero, the way an
// ad break or a joined playlist looks, plus a resolution change between them.
func TestRemuxTSDiscontinuity(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.ts"), filepath.Join(dir, "b.ts")
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "4",
		"-c:v", "libx264", "-preset", "fast", "-c:a", "aac", "-f", "mpegts", a)
	ffmpegRun(t, "-f", "lavfi", "-i", "smptebars=size=480x270:rate=25", "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000", "-t", "3",
		"-c:v", "libx264", "-preset", "fast", "-c:a", "aac", "-f", "mpegts", b)
	ab := filepath.Join(dir, "joined.ts")
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	os.WriteFile(ab, append(x, y...), 0o644)

	out, _ := remuxFile(t, ab)
	po := ffprobe(t, out)
	if d := fnum(po.Format.Duration); d < 6.8 || d > 7.3 {
		t.Fatalf("joined duration %.3f, want about 7s", d)
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	// decode the joined source the same way (ffmpeg scales frames after the size change)
	sameHashes(t, "video", decodedHashes(t, ab, "v:0"), decodedHashes(t, out, "v:0"))
	if n := len(decodedHashes(t, a, "v:0")) + len(decodedHashes(t, b, "v:0")); n != len(decodedHashes(t, out, "v:0")) {
		t.Fatalf("frame count %d, want %d", len(decodedHashes(t, out, "v:0")), n)
	}
	sameHashes(t, "audio", decodedHashes(t, ab, "a:0"), decodedHashes(t, out, "a:0"))
	// the second clip must start where the first ends, for both tracks
	ts, _ := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "frame=pts_time,width", "-of", "csv=p=0", out).Output()
	lines := strings.Split(strings.TrimSpace(string(ts)), "\n")
	var prev float64 = -1
	for i, l := range lines {
		f := strings.Split(l, ",")
		pt := fnum(f[0])
		if prev >= 0 && (pt-prev > 0.1 || pt <= prev) { // a held frame of up to ~2 frames at the join is fine
			t.Fatalf("frame %d: gap/back step %.3f → %.3f", i, prev, pt)
		}
		prev = pt
	}
	width := func(l string) string { return strings.Split(l, ",")[1] }
	if width(lines[0]) != "320" || width(lines[len(lines)-1]) != "480" {
		t.Fatalf("resolution change lost: first %q last %q", lines[0], lines[len(lines)-1])
	}
}

// TestRemuxTSTimestampWrap starts the clock just before the 33-bit timestamp wraps (~26.5 hours).
func TestRemuxTSTimestampWrap(t *testing.T) {
	needFFmpeg(t)
	in := filepath.Join(t.TempDir(), "wrap.ts")
	wrap := float64(int64(1)<<33) / 90000
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6",
		"-c:v", "libx264", "-preset", "fast", "-c:a", "aac", "-output_ts_offset", fmt.Sprintf("%.3f", wrap-3), "-f", "mpegts", in)
	out, _ := remuxFile(t, in)
	po := ffprobe(t, out)
	if d := fnum(po.Format.Duration); d < 5.8 || d > 6.3 {
		t.Fatalf("duration %.3f across the wrap, want about 6s", d)
	}
	sameHashes(t, "video", decodedHashes(t, in, "v:0"), decodedHashes(t, out, "v:0"))
	sameHashes(t, "audio", decodedHashes(t, in, "a:0"), decodedHashes(t, out, "a:0"))
}

// TestRemuxTSFromHLSSegments uses real HLS segments joined back to back, exactly what the
// downloader writes, and checks it's also robust to junk between segments.
func TestRemuxTSFromHLSSegments(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=640x360:rate=30", "-f", "lavfi", "-i", "sine=frequency=500:sample_rate=44100", "-t", "10",
		"-c:v", "libx264", "-preset", "veryfast", "-g", "30", "-c:a", "aac", "-hls_time", "2", "-hls_list_size", "0", "-hls_segment_filename", filepath.Join(dir, "s%03d.ts"), filepath.Join(dir, "x.m3u8"))
	segs, _ := filepath.Glob(filepath.Join(dir, "s*.ts"))
	var all []byte
	for i, s := range segs {
		b, _ := os.ReadFile(s)
		all = append(all, b...)
		if i == 1 {
			all = append(all, []byte("junk!")...) // lost sync once
		}
	}
	in := filepath.Join(dir, "all.ts")
	os.WriteFile(in, all, 0o644)
	out, _ := remuxFile(t, in)
	po := ffprobe(t, out)
	if v := po.stream("video"); v == nil || v.Width != 640 || fnum(po.Format.Duration) < 9.8 {
		t.Fatalf("output %+v", po)
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
}

func TestRemuxTSUnsupported(t *testing.T) {
	needFFmpeg(t)
	dir := t.TempDir()
	in := filepath.Join(dir, "mpeg2.ts")
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-t", "1", "-c:v", "mpeg2video", "-f", "mpegts", in)
	_, err := remuxTS(context.Background(), in, filepath.Join(dir, "x.mp4"), nil)
	if err == nil || friendlyRemuxErr(err) != "MPEG-2 video isn't supported" {
		t.Fatalf("expected unsupported MPEG-2, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "x.mp4")); serr == nil {
		t.Fatal("left a half-written file behind")
	}
	junk := filepath.Join(dir, "junk.ts")
	os.WriteFile(junk, []byte(strings.Repeat("not a video ", 5000)), 0o644)
	if _, err := remuxTS(context.Background(), junk, filepath.Join(dir, "y.mp4"), nil); err == nil || !strings.Contains(err.Error(), "MPEG-TS") {
		t.Fatalf("junk: %v", err)
	}
}
