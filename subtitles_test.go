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
)

type cueWant struct {
	start, end float64
	text       string
}

// subPackets reads a subtitle stream's cues from a file (empty tx3g samples left out).
func subPackets(t *testing.T, path, sel string) []cueWant {
	t.Helper()
	b, err := exec.Command("ffprobe", "-v", "error", "-select_streams", sel, "-show_entries", "packet=pts_time,duration_time,size", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe packets: %v", err)
	}
	var out []cueWant
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		f := strings.Split(l, ",")
		if len(f) < 3 || fnum(f[2]) <= 2 {
			continue
		}
		out = append(out, cueWant{start: fnum(f[0]), end: fnum(f[0]) + fnum(f[1])})
	}
	return out
}

func sameCues(t *testing.T, what string, got, want []cueWant) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d cues %+v, want %d %+v", what, len(got), got, len(want), want)
	}
	for i := range want {
		if math.Abs(got[i].start-want[i].start) > 0.05 || math.Abs(got[i].end-want[i].end) > 0.05 {
			t.Fatalf("%s cue %d: %.3f–%.3f, want %.3f–%.3f", what, i, got[i].start, got[i].end, want[i].start, want[i].end)
		}
		if want[i].text != "" && got[i].text != "" && got[i].text != want[i].text {
			t.Fatalf("%s cue %d text %q, want %q", what, i, got[i].text, want[i].text)
		}
	}
}

// readSRT reads a SubRip file's cues.
func readSRT(t *testing.T, path string) []cueWant {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []cueWant
	for _, blk := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n\n") {
		lines := strings.Split(strings.TrimSpace(blk), "\n")
		if len(lines) < 3 {
			continue
		}
		a, z, _ := strings.Cut(lines[1], " --> ")
		st, _ := vttTime(a)
		en, _ := vttTime(z)
		out = append(out, cueWant{start: float64(st) / 1000, end: float64(en) / 1000, text: strings.Join(lines[2:], "\n")})
	}
	return out
}

// makeSubHLS makes a stream whose video starts at 11.4 s on the MPEG-TS clock, with English and
// French WebVTT renditions. The English segments map local time 0 to 2 s before the video, the
// French ones local 5 s to 5 s into it, so the cue times must go through X-TIMESTAMP-MAP.
func makeSubHLS(t *testing.T) (dir string, en, fr []cueWant) {
	dir = t.TempDir()
	ffHLS(t, dir, "v", "ts", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-c:a", "aac", "-output_ts_offset", "10")
	start := streamStart(t, filepath.Join(dir, "v_000.ts"), "a:0") // the output's time 0 (the audio starts first)
	video90 := int64(math.Round(streamStart(t, filepath.Join(dir, "v_000.ts"), "v:0") * 90000))
	vtt := func(mpegts int64, local string, cues ...string) string {
		s := fmt.Sprintf("WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:%s\n", mpegts, local)
		for _, c := range cues {
			s += "\n" + c + "\n"
		}
		return s
	}
	enMap, frMap := video90-180000, video90+450000
	segs := map[string]string{
		"s_en_0.vtt": vtt(enMap, "00:00:00.000", "1\n00:00:03.000 --> 00:00:04.500\nHello <b>there</b>"),
		"s_en_1.vtt": vtt(enMap, "00:00:00.000", "00:00:05.500 --> 00:00:06.900 align:start\nSpanning cue"),
		"s_en_2.vtt": vtt(enMap, "00:00:00.000", "00:00:05.500 --> 00:00:06.900 align:start\nSpanning cue", "00:00:07.000 --> 00:00:07.800\nLast &amp; final"),
		"s_fr_0.vtt": vtt(frMap, "00:00:05.000", "00:00.500 --> 00:01.500\n<v Marie>Bonjour</v>"),
		"s_fr_1.vtt": vtt(frMap, "00:00:05.000"),
		"s_fr_2.vtt": vtt(frMap, "00:00:05.000", "00:00:04.200 --> 00:00:05.000\nAu revoir"),
	}
	for n, c := range segs {
		os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644)
	}
	for _, l := range []string{"en", "fr"} {
		pl := "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXT-X-PLAYLIST-TYPE:VOD\n"
		for i := 0; i < 3; i++ {
			pl += fmt.Sprintf("#EXTINF:2.0,\ns_%s_%d.vtt\n", l, i)
		}
		os.WriteFile(filepath.Join(dir, "s_"+l+".m3u8"), []byte(pl+"#EXT-X-ENDLIST\n"), 0o644)
	}
	master := `#EXTM3U
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,URI="s_en.m3u8"
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Français",LANGUAGE="fr",CHARACTERISTICS="public.accessibility.transcribes-spoken-dialog",URI="s_fr.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=500000,RESOLUTION=320x240,CODECS="avc1.64000d,mp4a.40.2",SUBTITLES="subs"
v.m3u8
`
	os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(master), 0o644)
	at := func(pts90 int64) float64 { return float64(pts90)/90000 - start }
	en = []cueWant{
		{at(enMap + 270000), at(enMap + 405000), "Hello <b>there</b>"},
		{at(enMap + 495000), at(enMap + 621000), "Spanning cue"},
		{at(enMap + 630000), at(enMap + 702000), "Last & final"},
	}
	fr = []cueWant{
		{at(frMap - 405000), at(frMap - 315000), "Bonjour"},
		{at(frMap - 72000), at(frMap), "Au revoir"},
	}
	return dir, en, fr
}

func TestHLSSubtitles(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir, en, fr := makeSubHLS(t)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	u := srv.URL + "/master.m3u8"
	info, err := m.resolveHLS(t.Context(), &Download{}, u, 0)
	if err != nil || len(info.Subs) != 2 || info.Subs[1].Lang != "fr" || !info.Subs[1].SDH || info.Subs[0].Key != "subs/English" {
		t.Fatalf("subtitle tracks %+v %v", info.Subs, err)
	}

	// all of them: two tx3g tracks in the MP4, and an .srt for each beside it
	d := addStream(t, m, AddReq{URL: u, Name: "clip.mp4", StreamPick: StreamPick{SubAll: true}})
	out := filepath.Join(d.Dir, d.FileName)
	ss, _, _ := probeTags(t, out)
	subs := streamsOf(ss, "subtitle")
	if len(subs) != 2 || subs[0].Codec != "mov_text" || subs[0].Tags["language"] != "eng" || subs[1].Tags["language"] != "fra" || subs[1].Tags["handler_name"] != "Français" {
		t.Fatalf("subtitle streams: %+v", subs)
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	sameCues(t, "English (MP4)", subPackets(t, out, "s:0"), en)
	sameCues(t, "French (MP4)", subPackets(t, out, "s:1"), fr)
	enSRT := readSRT(t, filepath.Join(d.Dir, "clip.en.srt"))
	sameCues(t, "English (.srt)", enSRT, en)
	if enSRT[0].text != "Hello <b>there</b>" || enSRT[2].text != "Last & final" {
		t.Fatalf("srt text %+v", enSRT)
	}
	frSRT := readSRT(t, filepath.Join(d.Dir, "clip.fr.sdh.srt"))
	sameCues(t, "French (.srt)", frSRT, fr)
	if frSRT[0].text != "Bonjour" {
		t.Fatalf("srt text %+v", frSRT)
	}

	// one chosen track: shown by default
	d = addStream(t, m, AddReq{URL: u, Name: "one.mp4", StreamPick: StreamPick{SubKey: "subs/Français"}})
	ss, _, _ = probeTags(t, filepath.Join(d.Dir, d.FileName))
	if subs := streamsOf(ss, "subtitle"); len(subs) != 1 || subs[0].Tags["language"] != "fra" || subs[0].Disposit["default"] != 1 {
		t.Fatalf("one subtitle track: %+v", subs)
	}
	if _, err := os.Stat(filepath.Join(d.Dir, "one.fr.sdh.srt")); err != nil {
		t.Fatal("no .srt beside the video")
	}
}

// TestDASHSubtitlesMKV: a WebVTT file per period goes into Matroska as WebVTT text.
func TestDASHSubtitlesMKV(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := t.TempDir()
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6",
		"-map", "0:v", "-map", "1:a", "-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-g", "25", "-b:v", "200k", "-c:a", "aac",
		"-f", "dash", "-dash_segment_type", "auto", "-seg_duration", "2", "-adaptation_sets", "id=0,streams=v id=1,streams=a", filepath.Join(dir, "s.mpd"))
	os.WriteFile(filepath.Join(dir, "en.vtt"), []byte("WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nHi there\n\n00:00:03.500 --> 00:00:04.250\nBye\n"), 0o644)
	mpd, _ := os.ReadFile(filepath.Join(dir, "s.mpd"))
	set := `<AdaptationSet id="2" contentType="text" mimeType="text/vtt" lang="en"><Role schemeIdUri="urn:mpeg:dash:role:2011" value="subtitle"/>` +
		`<Representation id="sub_en" bandwidth="256"><BaseURL>en.vtt</BaseURL></Representation></AdaptationSet></Period>`
	os.WriteFile(filepath.Join(dir, "s.mpd"), []byte(strings.Replace(string(mpd), "</Period>", set, 1)), 0o644)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	d := addStream(t, m, AddReq{URL: srv.URL + "/s.mpd", StreamPick: StreamPick{SubLang: "en"}})
	out := filepath.Join(d.Dir, d.FileName)
	ss, format, _ := probeTags(t, out)
	subs := streamsOf(ss, "subtitle")
	if filepath.Ext(out) != ".mkv" || !strings.Contains(format, "matroska") || len(subs) != 1 || subs[0].Codec != "webvtt" || subs[0].Tags["language"] != "eng" {
		t.Fatalf("got %s %+v", d.FileName, ss)
	}
	sameCues(t, "MKV", subPackets(t, out, "s:0"), []cueWant{{1, 2, ""}, {3.5, 4.25, ""}})
	sameCues(t, ".srt", readSRT(t, filepath.Join(d.Dir, strings.TrimSuffix(d.FileName, ".mkv")+".en.srt")), []cueWant{{1, 2, "Hi there"}, {3.5, 4.25, "Bye"}})
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
}

func TestSidecarNames(t *testing.T) {
	m := newTestManager(t)
	d := &Download{Dir: t.TempDir()}
	cue := [][]placedCue{{{0, 1000, "a"}}, {{0, 1000, "b"}}, {{0, 1000, "c"}}, {{0, 1000, "d"}}}
	subs := []*subTrack{{track: &streamTrack{Lang: "en", Name: "English"}}, {track: &streamTrack{Lang: "en", Name: "Commentary"}},
		{track: &streamTrack{Lang: "en", Forced: true}}, {track: &streamTrack{Lang: "pt-BR", SDH: true}}}
	names, err := m.writeSidecars(d, "Movie.mkv", subs, cue)
	if err != nil || strings.Join(names, " ") != "Movie.en.srt Movie.en.Commentary.srt Movie.en.forced.srt Movie.pt-br.sdh.srt" {
		t.Fatalf("%v %v", names, err)
	}
	if b, _ := os.ReadFile(filepath.Join(d.Dir, "Movie.en.Commentary.srt")); string(b) != "1\r\n00:00:00,000 --> 00:00:01,000\r\nb\r\n\r\n" {
		t.Fatalf("srt %q", b)
	}
}
