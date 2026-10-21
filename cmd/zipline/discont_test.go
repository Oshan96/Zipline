package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// frameTimes lists a stream's frame times and widths (video) as decoded.
func frameTimes(t *testing.T, path, sel string) (pts []float64, widths []string) {
	t.Helper()
	b, err := exec.Command("ffprobe", "-v", "error", "-select_streams", sel, "-show_entries", "frame=pts_time,width", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe frames: %v", err)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Split(l, ",")
		pts = append(pts, fnum(f[0]))
		if len(f) > 1 {
			widths = append(widths, f[1])
		}
	}
	return
}

// checkContinuous fails when frames go back, overlap, start below zero or leave a gap.
func checkContinuous(t *testing.T, path, sel string, maxGap float64) []string {
	t.Helper()
	pts, w := frameTimes(t, path, sel)
	if len(pts) == 0 || pts[0] < -0.001 {
		t.Fatalf("%s: frames start at %v", sel, pts)
	}
	for i := 1; i < len(pts); i++ {
		if pts[i] <= pts[i-1] || pts[i]-pts[i-1] > maxGap {
			t.Fatalf("%s frame %d: %.3f after %.3f", sel, i, pts[i], pts[i-1])
		}
	}
	return w
}

// discPlaylist joins media playlists of parts with discontinuities between them. Each part is
// name:kind (ts or fmp4); extra tags can go in front of a part.
func discPlaylist(t *testing.T, dir string, parts []string, before map[int]string) {
	t.Helper()
	pl := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n#EXT-X-PLAYLIST-TYPE:VOD\n"
	segRE := regexp.MustCompile(`(?m)^(#EXTINF:[^\n]*\n[^#\n][^\n]*)$`)
	mapRE := regexp.MustCompile(`(?m)^#EXT-X-MAP:[^\n]*$`)
	for i, p := range parts {
		b, err := os.ReadFile(filepath.Join(dir, p+".m3u8"))
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			pl += "#EXT-X-DISCONTINUITY\n"
		}
		pl += before[i]
		if m := mapRE.FindString(string(b)); m != "" {
			pl += m + "\n"
		}
		for _, s := range segRE.FindAllString(string(b), -1) {
			pl += s + "\n"
		}
		pl += after(before, i)
	}
	os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(pl+"#EXT-X-ENDLIST\n"), 0o644)
}

func after(tags map[int]string, i int) string { return tags[-1-i] }

func avArgs(src string, secs int, extra ...string) []string {
	a := []string{"-f", "lavfi", "-i", src, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", fmt.Sprint(secs), "-c:a", "aac", "-g", "50"}
	return append(a, extra...)
}

func durationOf(t *testing.T, path string) float64 {
	_, _, d := probeTags(t, path)
	return d
}

// TestHLSDiscontinuity: two clips joined with a discontinuity, the second one's timestamps reset
// to the start or jumping 2 s ahead (too small for the timestamp guesswork to notice). The
// output plays through: its length is the sum, nothing overlaps or stands still.
func TestHLSDiscontinuity(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	for _, c := range []struct{ name, offset string }{{"reset", "0"}, {"jump", "6"}} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			ffHLS(t, dir, "a", "ts", avArgs("testsrc2=size=320x240:rate=25", 4, "-c:v", "libx264", "-preset", "ultrafast")...)
			ffHLS(t, dir, "b", "ts", avArgs("smptebars=size=320x240:rate=25", 3, "-c:v", "libx264", "-preset", "ultrafast", "-output_ts_offset", c.offset)...)
			discPlaylist(t, dir, []string{"a", "b"}, nil)
			srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
			defer srv.Close()
			m := newTestManager(t)
			d := addStream(t, m, AddReq{URL: srv.URL + "/index.m3u8", Name: "joined.mp4"})
			out := filepath.Join(d.Dir, d.FileName)
			if dd := durationOf(t, out); math.Abs(dd-7) > 0.15 || d.Note != "" {
				t.Fatalf("duration %.3f, want 7 (note %q)", dd, d.Note)
			}
			if e := decodeErrors(t, out); e != "" {
				t.Fatalf("decoder complained:\n%s", e)
			}
			checkContinuous(t, out, "v:0", 0.1)
			want := len(decodedHashes(t, filepath.Join(dir, "a.m3u8"), "v:0")) + len(decodedHashes(t, filepath.Join(dir, "b.m3u8"), "v:0"))
			if n := len(decodedHashes(t, out, "v:0")); n != want {
				t.Fatalf("%d frames, want %d", n, want)
			}
			if len(d.Marks[""]) != 2 {
				t.Fatalf("part marks %+v", d.Marks)
			}
		})
	}
}

// TestHLSInitChange: MP4 pieces whose second part brings a new init section (another size and
// another timescale): one file, the size change kept as a second sample description.
func TestHLSInitChange(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := t.TempDir()
	ffHLS(t, dir, "a", "fmp4", avArgs("testsrc2=size=320x240:rate=25", 4, "-c:v", "libx264", "-preset", "ultrafast")...)
	ffHLS(t, dir, "b", "fmp4", avArgs("smptebars=size=480x270:rate=25", 4, "-c:v", "libx264", "-preset", "ultrafast", "-video_track_timescale", "90000")...)
	discPlaylist(t, dir, []string{"a", "b"}, nil)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	d := addStream(t, m, AddReq{URL: srv.URL + "/index.m3u8", Name: "init.mp4"})
	out := filepath.Join(d.Dir, d.FileName)
	if dd := durationOf(t, out); math.Abs(dd-8) > 0.15 || d.Note != "" || !isRegularMP4(out) {
		t.Fatalf("duration %.3f, want 8 (note %q)", dd, d.Note)
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	w := checkContinuous(t, out, "v:0", 0.1)
	if w[0] != "320" || w[len(w)-1] != "480" {
		t.Fatalf("size change lost: %s … %s", w[0], w[len(w)-1])
	}
	// the sound goes on without a gap (the second part's lead-in frame plays at the join)
	checkContinuous(t, out, "a:0", 0.03)
	n := len(decodedHashes(t, filepath.Join(dir, "a.m3u8"), "a:0")) + len(decodedHashes(t, filepath.Join(dir, "b.m3u8"), "a:0"))
	if got := len(decodedHashes(t, out, "a:0")); got < n || got > n+2 {
		t.Fatalf("%d audio frames, want %d", got, n)
	}
}

// TestHLSCodecChange: a part in HEVC inside an H.264 stream can't share its track: the
// programme is one file and the other part is saved beside it.
func TestHLSCodecChange(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := t.TempDir()
	ffHLS(t, dir, "a", "ts", avArgs("testsrc2=size=320x240:rate=25", 6, "-c:v", "libx264", "-preset", "ultrafast")...)
	ffHLS(t, dir, "b", "ts", avArgs("smptebars=size=320x240:rate=25", 2, "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error")...)
	discPlaylist(t, dir, []string{"a", "b"}, nil)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	d := addStream(t, m, AddReq{URL: srv.URL + "/index.m3u8", Name: "show.mp4"})
	out := filepath.Join(d.Dir, d.FileName)
	ss, _, dd := probeTags(t, out)
	if math.Abs(dd-6) > 0.15 || streamsOf(ss, "video")[0].Codec != "h264" || !strings.Contains(d.Note, "show (part 2).mp4") {
		t.Fatalf("main file: %.3f s %+v note %q", dd, ss, d.Note)
	}
	ss, _, dd = probeTags(t, filepath.Join(d.Dir, "show (part 2).mp4"))
	if math.Abs(dd-2) > 0.15 || streamsOf(ss, "video")[0].Codec != "hevc" || len(streamsOf(ss, "audio")) != 1 {
		t.Fatalf("separate part: %.3f s %+v", dd, ss)
	}
}

// TestHLSAdBreak: a break marked with CUE-OUT/CUE-IN is listed, kept by default and left out
// on request, with the programme joined across the gap.
func TestHLSAdBreak(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := t.TempDir()
	ffHLS(t, dir, "a", "ts", avArgs("testsrc2=size=320x240:rate=25", 4, "-c:v", "libx264", "-preset", "ultrafast")...)
	ffHLS(t, dir, "ad", "ts", avArgs("smptebars=size=320x240:rate=25", 2, "-c:v", "libx264", "-preset", "ultrafast")...)
	ffHLS(t, dir, "c", "ts", avArgs("testsrc2=size=320x240:rate=25", 4, "-c:v", "libx264", "-preset", "ultrafast", "-output_ts_offset", "4")...)
	discPlaylist(t, dir, []string{"a", "ad", "c"}, map[int]string{1: "#EXT-X-CUE-OUT:2.0\n", -2: "#EXT-X-CUE-IN\n"})
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	u := srv.URL + "/index.m3u8"
	info, err := m.resolveHLS(t.Context(), &Download{}, u, 0)
	if err != nil || len(info.Ads) != 1 || info.Ads[0].Start != 4 || info.Ads[0].Dur != 2 {
		t.Fatalf("ad breaks %+v %v", info.Ads, err)
	}
	for _, drop := range []bool{false, true} {
		d := addStream(t, m, AddReq{URL: u, Name: fmt.Sprintf("ads-%v.mp4", drop), StreamPick: StreamPick{NoAds: drop}})
		out := filepath.Join(d.Dir, d.FileName)
		want := 10.0
		if drop {
			want = 8
		}
		if dd := durationOf(t, out); math.Abs(dd-want) > 0.15 {
			t.Fatalf("drop=%v: duration %.3f, want %.0f", drop, dd, want)
		}
		checkContinuous(t, out, "v:0", 0.1)
		if e := decodeErrors(t, out); e != "" {
			t.Fatalf("decoder complained:\n%s", e)
		}
	}
}

// makePeriodsDASH joins three ffmpeg DASH streams as periods of one manifest; the middle one
// is an ad break marked with a SCTE-35 event stream.
func makePeriodsDASH(t *testing.T, webm bool) string {
	t.Helper()
	dir := t.TempDir()
	var periods []string
	at := 0
	for i, p := range []struct {
		name, src string
		secs      int
	}{{"p1", "testsrc2=size=320x240:rate=25", 4}, {"ad", "smptebars=size=320x240:rate=25", 2}, {"p2", "testsrc=size=320x240:rate=25", 4}} {
		sub := filepath.Join(dir, p.name)
		os.MkdirAll(sub, 0o755)
		codecs := []string{"-c:v", "libx264", "-preset", "ultrafast", "-bf", "2"}
		if webm {
			codecs = []string{"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-b:v", "200k", "-c:a", "libopus", "-dash_segment_type", "webm"}
		}
		ffmpegRun(t, append(append(avArgs(p.src, p.secs, "-map", "0:v", "-map", "1:a"), codecs...),
			"-f", "dash", "-seg_duration", "2", "-use_template", "1", "-use_timeline", "1", "-adaptation_sets", "id=0,streams=v id=1,streams=a", filepath.Join(sub, "s.mpd"))...)
		b, _ := os.ReadFile(filepath.Join(sub, "s.mpd"))
		body := regexp.MustCompile(`(?s)<Period[^>]*>(.*)</Period>`).FindStringSubmatch(string(b))[1]
		ev := ""
		if p.name == "ad" {
			ev = `<EventStream schemeIdUri="urn:scte:scte35:2013:xml" timescale="90000"><Event presentationTime="0" duration="180000" id="1"/></EventStream>`
		}
		periods = append(periods, fmt.Sprintf(`<Period id="%d" start="PT%dS" duration="PT%dS"><BaseURL>%s/</BaseURL>%s%s</Period>`, i, at, p.secs, p.name, ev, body))
		at += p.secs
	}
	mpd := fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" profiles="urn:mpeg:dash:profile:isoff-live:2011" type="static" mediaPresentationDuration="PT%dS" minBufferTime="PT2S">
%s
</MPD>`, at, strings.Join(periods, "\n"))
	os.WriteFile(filepath.Join(dir, "s.mpd"), []byte(mpd), 0o644)
	return dir
}

// TestDASHPeriods: three periods are stitched in order (not just the longest one kept); the
// marked ad period is left out on request.
func TestDASHPeriods(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	for _, webm := range []bool{false, true} {
		t.Run(map[bool]string{false: "mp4", true: "webm"}[webm], func(t *testing.T) { testDASHPeriods(t, webm) })
	}
}

func testDASHPeriods(t *testing.T, webm bool) {
	dir := makePeriodsDASH(t, webm)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	u := srv.URL + "/s.mpd"
	info, err := m.resolveHLS(t.Context(), &Download{}, u, 0)
	if err != nil || len(info.Ads) != 1 || math.Abs(info.Ads[0].Start-4) > 0.01 || math.Abs(info.Ads[0].Dur-2) > 0.01 || math.Abs(info.Duration-10) > 0.01 {
		t.Fatalf("periods: ads %+v duration %.2f err %v", info.Ads, info.Duration, err)
	}
	frames := func(name string) int {
		return len(decodedHashes(t, filepath.Join(dir, name, "s.mpd"), "v:0"))
	}
	for _, drop := range []bool{false, true} {
		d := addStream(t, m, AddReq{URL: u, Name: fmt.Sprintf("periods-%v", drop), StreamPick: StreamPick{NoAds: drop}})
		out := filepath.Join(d.Dir, d.FileName)
		if ext := map[bool]string{false: ".mp4", true: ".webm"}[webm]; filepath.Ext(out) != ext {
			t.Fatalf("got %s, want %s", d.FileName, ext)
		}
		want, n := 10.0, frames("p1")+frames("ad")+frames("p2")
		if drop {
			want, n = 8, frames("p1")+frames("p2")
		}
		if dd := durationOf(t, out); math.Abs(dd-want) > 0.15 || d.Note != "" {
			t.Fatalf("drop=%v: duration %.3f, want %.0f (note %q)", drop, dd, want, d.Note)
		}
		if got := len(decodedHashes(t, out, "v:0")); got != n {
			t.Fatalf("drop=%v: %d frames, want %d", drop, got, n)
		}
		checkContinuous(t, out, "v:0", 0.1)
		if e := decodeErrors(t, out); e != "" {
			t.Fatalf("decoder complained:\n%s", e)
		}
	}
}

func TestDASHShortPeriodIsAd(t *testing.T) {
	doc := &mpdDoc{Periods: []mpdPeriod{{Duration: "PT10M"}, {Duration: "PT30S"}, {Duration: "PT12M"}, {Duration: "PT20S"}}}
	ps := dashPeriods(doc)
	if ps[0].ad || !ps[1].ad || ps[2].ad || ps[3].ad || ps[2].start != 630 {
		t.Fatalf("periods %+v %+v %+v %+v", *ps[0], *ps[1], *ps[2], *ps[3])
	}
}

// TestHLSAdMarks reads the ways a playlist marks its breaks: CUE-OUT with a length, CUE-OUT …
// CUE-IN, and DATERANGE entries placed by PROGRAM-DATE-TIME (SCTE-35 or an ad class; the
// player's own interstitials are not breaks in the programme).
func TestHLSAdMarks(t *testing.T) {
	seg := func(n int) string { return strings.Repeat("#EXTINF:2.0,\ns.ts\n", n) }
	for _, c := range []struct {
		name, body string
		want       string
	}{
		{"cue-length", seg(2) + "#EXT-X-CUE-OUT:4\n" + seg(3), "--AA-"},
		{"cue-in", seg(1) + "#EXT-X-CUE-OUT\n" + seg(2) + "#EXT-X-CUE-IN\n" + seg(2), "-AA--"},
		{"daterange-scte35", "#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00Z\n#EXT-X-DATERANGE:ID=\"a\",START-DATE=\"2026-01-01T00:00:04Z\",DURATION=4.0,SCTE35-OUT=0xFC30\n" + seg(5), "--AA-"},
		{"daterange-in", "#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00Z\n#EXT-X-DATERANGE:ID=\"b\",START-DATE=\"2026-01-01T00:00:02Z\",SCTE35-OUT=0xFC\n" + seg(2) +
			"#EXT-X-DATERANGE:ID=\"b\",START-DATE=\"2026-01-01T00:00:06Z\",SCTE35-IN=0xFC\n" + seg(3), "-AA--"},
		{"daterange-class", "#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00Z\n#EXT-X-DATERANGE:ID=\"c\",CLASS=\"com.example.ad\",START-DATE=\"2026-01-01T00:00:00Z\",DURATION=2\n" + seg(3), "A--"},
		{"interstitial", "#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00Z\n#EXT-X-DATERANGE:ID=\"i\",CLASS=\"com.apple.hls.interstitial\",START-DATE=\"2026-01-01T00:00:02Z\",DURATION=2,X-ASSET-URI=\"ad.m3u8\"\n" + seg(3), "---"},
	} {
		_, md, err := parseM3U8("https://x.example/a.m3u8", "#EXTM3U\n"+c.body+"#EXT-X-ENDLIST\n")
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, s := range md.Segments {
			got += map[bool]string{true: "A", false: "-"}[s.Ad]
		}
		if got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

// TestHLSPartsResume: two languages kept, both tracks in two parts with a timestamp jump
// between; paused part-way and resumed. The marks and the other track's state survive a save
// (JSON), and the result plays through with both languages.
func TestHLSPartsResume(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := t.TempDir()
	clip := func(name, src string, offset string, video bool) {
		args := []string{"-f", "lavfi", "-i", src, "-t", "4", "-output_ts_offset", offset}
		if video {
			args = append(args, "-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-an")
		} else {
			args = append(args, "-c:a", "aac", "-vn")
		}
		ffHLS(t, dir, name, "ts", args...)
	}
	for _, p := range []struct{ suffix, offset string }{{"1", "0"}, {"2", "7"}} {
		clip("v"+p.suffix, "testsrc2=size=320x240:rate=25", p.offset, true)
		clip("en"+p.suffix, "sine=frequency=440:sample_rate=48000", p.offset, false)
		clip("de"+p.suffix, "sine=frequency=660:sample_rate=48000", p.offset, false)
	}
	for _, n := range []string{"v", "en", "de"} {
		discPlaylist(t, dir, []string{n + "1", n + "2"}, nil)
		os.Rename(filepath.Join(dir, "index.m3u8"), filepath.Join(dir, n+".m3u8"))
	}
	os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(`#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="English",LANGUAGE="en",DEFAULT=YES,URI="en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="a",NAME="Deutsch",LANGUAGE="de",URI="de.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=320x240,CODECS="avc1.64000d,mp4a.40.2",AUDIO="a"
v.m3u8
`), 0o644)
	var hits int32
	srv := httptest.NewServer(slowFiles(dir, 40*time.Millisecond, &hits))
	defer srv.Close()
	m := newTestManager(t)
	m.settings.Conns = 1
	d := m.Add(AddReq{URL: srv.URL + "/master.m3u8", Name: "both.mp4", Start: true, StreamPick: StreamPick{AudioAll: true}})
	for i := 0; i < 200; i++ { // until the other language is part-way
		m.mu.Lock()
		n := 0
		if len(d.Extra) > 0 {
			n = d.Extra[0].Done
		}
		m.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	saved, _ := json.Marshal(d)
	m.mu.Unlock()
	var back Download
	if err := json.Unmarshal(saved, &back); err != nil {
		t.Fatal(err)
	}
	if !back.AudioAll || len(back.Extra) != 1 || back.Extra[0].Done == 0 || back.Extra[0].Done >= back.Extra[0].Parts || len(back.Marks[".audio"]) != 2 || len(back.Marks[".x1"]) == 0 {
		t.Fatalf("saved state: %s", saved)
	}
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 60*time.Second)
	out := filepath.Join(d.Dir, d.FileName)
	ss, _, dd := probeTags(t, out)
	a := streamsOf(ss, "audio")
	if len(a) != 2 || a[0].Tags["language"] != "eng" || a[1].Tags["language"] != "deu" || math.Abs(dd-8) > 0.15 {
		t.Fatalf("result %.3f s %+v", dd, ss)
	}
	checkContinuous(t, out, "v:0", 0.1)
	checkContinuous(t, out, "a:1", 0.05)
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
}
