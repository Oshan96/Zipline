package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for choosing a stream's tracks: audio languages, qualities, subtitles. ffmpeg makes the
// streams; Zipline's own code downloads and repackages them; ffprobe checks the result.

// tagStream is an ffprobe stream with its tags.
type tagStream struct {
	Index    int               `json:"index"`
	Type     string            `json:"codec_type"`
	Codec    string            `json:"codec_name"`
	Width    int               `json:"width"`
	Height   int               `json:"height"`
	Channels int               `json:"channels"`
	Rate     string            `json:"r_frame_rate"`
	Tags     map[string]string `json:"tags"`
	Disposit map[string]int    `json:"disposition"`
}

func probeTags(t *testing.T, path string) (streams []tagStream, format string, dur float64) {
	t.Helper()
	b, err := exec.Command("ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	var p struct {
		Streams []tagStream `json:"streams"`
		Format  struct {
			Name     string `json:"format_name"`
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	return p.Streams, p.Format.Name, fnum(p.Format.Duration)
}

func streamsOf(ss []tagStream, kind string) []tagStream {
	var out []tagStream
	for _, s := range ss {
		if s.Type == kind {
			out = append(out, s)
		}
	}
	return out
}

// hlsRun writes an HLS stream of one input with ffmpeg: name.m3u8 and its pieces in dir.
func ffHLS(t *testing.T, dir, name string, kind string, args ...string) {
	t.Helper()
	a := append([]string{}, args...)
	a = append(a, "-f", "hls", "-hls_time", "2", "-hls_playlist_type", "vod")
	if kind == "fmp4" {
		a = append(a, "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", name+"_init.mp4", "-hls_segment_filename", filepath.Join(dir, name+"_%03d.m4s"))
	} else {
		a = append(a, "-hls_segment_filename", filepath.Join(dir, name+"_%03d.ts"))
	}
	ffmpegRun(t, append(a, filepath.Join(dir, name+".m3u8"))...)
}

// makeLangHLS makes a stream with video alone and three audio renditions, each a tone of its own
// so they can be told apart: English, German (DEFAULT, 5.1) and English audio description.
func makeLangHLS(t *testing.T, kind string, secs int) string {
	t.Helper()
	dir := t.TempDir()
	d := fmt.Sprint(secs)
	ffHLS(t, dir, "v", kind, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-t", d, "-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-an")
	for _, a := range []struct{ name, freq, ch string }{{"en", "440", "2"}, {"de", "660", "6"}, {"ad", "880", "2"}} {
		ffHLS(t, dir, "a_"+a.name, kind, "-f", "lavfi", "-i", "sine=frequency="+a.freq+":sample_rate=48000", "-t", d, "-c:a", "aac", "-ac", a.ch, "-vn")
	}
	master := `#EXTM3U
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English",LANGUAGE="en",AUTOSELECT=YES,CHANNELS="2",URI="a_en.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="Deutsch",LANGUAGE="de",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="6",URI="a_de.m3u8"
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="aud",NAME="English (AD)",LANGUAGE="en",CHARACTERISTICS="public.accessibility.describes-video",CHANNELS="2",URI="a_ad.m3u8"
#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=320x240,CODECS="avc1.64000d,mp4a.40.2",AUDIO="aud"
v.m3u8
`
	os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte(master), 0o644)
	return dir
}

func hideFFmpeg(t *testing.T) {
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	t.Cleanup(func() { ffmpegPath = orig })
}

func addStream(t *testing.T, m *Manager, r AddReq) *Download {
	t.Helper()
	r.Start = true
	d := m.Add(r)
	waitFor(t, m, d, StCompleted, 90*time.Second)
	return d
}

func TestHLSAudioLanguages(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	for _, kind := range []string{"ts", "fmp4"} {
		t.Run(kind, func(t *testing.T) {
			dir := makeLangHLS(t, kind, 6)
			srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
			defer srv.Close()
			m := newTestManager(t)
			info, err := m.resolveHLS(t.Context(), &Download{}, srv.URL+"/master.m3u8", 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(info.Audios) != 3 || info.AudioTrack == nil || info.AudioTrack.Lang != "de" {
				t.Fatalf("audio tracks %+v, default %+v", info.Audios, info.AudioTrack)
			}
			if a := info.Audios[2]; !a.Desc || a.Lang != "en" || a.Channels != "2" || info.Audios[1].Channels != "6" || a.Key != "aud/English (AD)" {
				t.Fatalf("audio description track: %+v", a)
			}
			want := func(name string) []string { return decodedHashes(t, filepath.Join(dir, name), "a:0") }

			// the stream's DEFAULT (German) when nothing is chosen
			d := addStream(t, m, AddReq{URL: srv.URL + "/master.m3u8"})
			out := filepath.Join(d.Dir, d.FileName)
			ss, _, _ := probeTags(t, out)
			if a := streamsOf(ss, "audio"); len(a) != 1 || a[0].Tags["language"] != "deu" || a[0].Channels != 6 {
				t.Fatalf("default pick: %+v", a)
			}
			sameHashes(t, "German", want("a_de.m3u8"), decodedHashes(t, out, "a:0"))

			// a chosen track: the audio description
			d = addStream(t, m, AddReq{URL: srv.URL + "/master.m3u8", Name: "ad.mp4", StreamPick: StreamPick{AudioKey: "aud/English (AD)"}})
			out = filepath.Join(d.Dir, d.FileName)
			ss, _, _ = probeTags(t, out)
			if a := streamsOf(ss, "audio"); len(a) != 1 || a[0].Tags["language"] != "eng" || a[0].Tags["handler_name"] != "English (AD)" {
				t.Fatalf("chosen pick: %+v", a)
			}
			sameHashes(t, "audio description", want("a_ad.m3u8"), decodedHashes(t, out, "a:0"))
			if d.AudioKey != "aud/English (AD)" {
				t.Fatalf("the choice isn't kept on the download: %+v", d.StreamPick)
			}

			// keep all languages: the chosen one first and default, each labelled
			d = addStream(t, m, AddReq{URL: srv.URL + "/master.m3u8", Name: "all.mp4", StreamPick: StreamPick{AudioKey: "aud/English", AudioAll: true}})
			out = filepath.Join(d.Dir, d.FileName)
			ss, _, _ = probeTags(t, out)
			a := streamsOf(ss, "audio")
			if len(a) != 3 {
				t.Fatalf("keep all: %d audio tracks: %+v", len(a), a)
			}
			var got []string
			for _, s := range a {
				got = append(got, s.Tags["language"]+"/"+s.Tags["handler_name"]+fmt.Sprintf("/%d", s.Disposit["default"]))
			}
			if strings.Join(got, " ") != "eng/English/1 deu/Deutsch/0 eng/English (AD)/0" {
				t.Fatalf("keep all labels: %v", got)
			}
			if a[0].Tags["title"] != "English" && a[0].Tags["handler_name"] != "English" {
				t.Fatalf("no title: %+v", a[0].Tags)
			}
			for i, n := range []string{"a_en.m3u8", "a_de.m3u8", "a_ad.m3u8"} {
				sameHashes(t, n, want(n), decodedHashes(t, out, fmt.Sprintf("a:%d", i)))
			}
			if e := decodeErrors(t, out); e != "" {
				t.Fatalf("decoder complained:\n%s", e)
			}
			for i := range d.Extra {
				if _, err := os.Stat(d.extraPartPath(i)); err == nil {
					t.Fatal("an extra track's part file was left behind")
				}
			}
		})
	}
}

// TestDASHAudioLanguages: DASH audio adaptation sets are told apart by language, Role and
// Accessibility (an audio description set is never the default).
func TestDASHAudioLanguages(t *testing.T) {
	needFFmpeg(t)
	hideFFmpeg(t)
	dir := t.TempDir()
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-f", "lavfi", "-i", "sine=frequency=660:sample_rate=48000", "-f", "lavfi", "-i", "sine=frequency=880:sample_rate=48000", "-t", "4",
		"-map", "0:v", "-map", "1:a", "-map", "2:a", "-map", "3:a", "-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-c:a", "aac",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=ger", "-metadata:s:a:2", "language=eng",
		"-f", "dash", "-seg_duration", "2", "-adaptation_sets", "id=0,streams=v id=1,streams=1 id=2,streams=2 id=3,streams=3", filepath.Join(dir, "s.mpd"))
	mpd, _ := os.ReadFile(filepath.Join(dir, "s.mpd"))
	s := strings.Replace(string(mpd), `lang="ger">`, `lang="ger"><Role schemeIdUri="urn:mpeg:dash:role:2011" value="main"/>`, 1)
	i := strings.LastIndex(s, `lang="eng">`)
	s = s[:i] + `lang="eng"><Accessibility schemeIdUri="urn:tva:metadata:cs:AudioPurposeCS:2007" value="1"/><Label>Described</Label>` + s[i+len(`lang="eng">`):]
	os.WriteFile(filepath.Join(dir, "s.mpd"), []byte(s), 0o644)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	m := newTestManager(t)
	u := srv.URL + "/s.mpd"
	info, err := m.resolveHLS(t.Context(), &Download{}, u, 0)
	if err != nil || len(info.Audios) != 3 || info.AudioTrack == nil || info.AudioTrack.Lang != "ger" {
		t.Fatalf("tracks %+v default %+v %v", info.Audios, info.AudioTrack, err)
	}
	var ad *streamTrack
	for _, a := range info.Audios {
		if a.Desc {
			ad = a
		}
	}
	if ad == nil || ad.Name != "Described" || ad.Lang != "eng" {
		t.Fatalf("no audio description track: %+v", info.Audios)
	}
	src := func(set string) []string {
		return decodedHashes(t, joinPieces(t, dir, "chunk-stream"+set+"-", "init-stream"+set+".m4s", "a"+set+".mp4"), "a:0")
	}
	check := func(d *Download, want ...string) {
		t.Helper()
		out := filepath.Join(d.Dir, d.FileName)
		ss, _, _ := probeTags(t, out)
		a := streamsOf(ss, "audio")
		if len(a) != len(want) {
			t.Fatalf("%s: %d audio tracks, want %d", d.FileName, len(a), len(want))
		}
		for i, w := range want {
			lang, set, _ := strings.Cut(w, ":")
			if a[i].Tags["language"] != lang {
				t.Fatalf("%s track %d: %+v, want %s", d.FileName, i, a[i].Tags, w)
			}
			sameHashes(t, w, src(set), decodedHashes(t, out, fmt.Sprintf("a:%d", i)))
		}
		if a[0].Disposit["default"] != 1 {
			t.Fatalf("the first track isn't the default one")
		}
	}
	check(addStream(t, m, AddReq{URL: u, Name: "def.mp4"}), "deu:2")
	check(addStream(t, m, AddReq{URL: u, Name: "ad.mp4", StreamPick: StreamPick{AudioKey: ad.Key}}), "eng:3")
	check(addStream(t, m, AddReq{URL: u, Name: "all.mp4", StreamPick: StreamPick{AudioAll: true}}), "deu:2", "eng:1", "eng:3")
}

func TestCodecsAndLanguages(t *testing.T) {
	for c, want := range map[string]string{"avc1.640028": "h264", "hev1.1.6.L93": "hevc", "dvh1.05.06": "dv", "av01.0.08M.10": "av1", "vp09.02.10.10": "vp9",
		"mp4a.40.2": "aac", "mp4a.40.34": "mp3", "ec-3": "eac3", "ac-3": "ac3", "Opus": "opus", "wvtt": "wvtt", "stpp.ttml.im1t": "ttml", "mp4v.20.9": "mpeg4", "xyz": ""} {
		if got := codecFamily(c); got != want {
			t.Errorf("codecFamily(%q) = %q, want %q", c, got, want)
		}
	}
	for v, want := range map[hlsVariant]bool{
		{VideoRange: "PQ", Codecs: "hvc1.2.4.L150"}: true, {VideoRange: "HLG"}: true, {VideoRange: "SDR", Codecs: "avc1.640028"}: false,
		{Codecs: "dvh1.05.06,ec-3"}: true, {Codecs: "vp09.02.51.10.01.09.16.09.00"}: true, {Codecs: "vp09.00.40.08.01.01.01.01.00"}: false,
		{Codecs: "av01.0.13M.10.0.110.09.16.09.0"}: true, {Codecs: "av01.0.08M.08"}: false,
	} {
		if isHDR(v) != want {
			t.Errorf("isHDR(%+v) = %v", v, !want)
		}
	}
	vs := shownVariants([]hlsVariant{{URL: "a", Codecs: "mp4v.20.9,mp4a.40.2"}, {URL: "b", Codecs: "avc1.4d401f,mp4a.40.2"}})
	if len(vs) != 1 || vs[0].URL != "b" {
		t.Errorf("an MPEG-4 Part 2 quality should be hidden: %+v", vs)
	}
	for tag, want := range map[string][2]string{"en": {"eng", "eng"}, "de-AT": {"deu", "ger"}, "ger": {"deu", "ger"}, "fra": {"fra", "fre"}, "zh_Hant": {"zho", "chi"}, "": {"und", "und"}, "xx": {"und", "und"}} {
		if a, b := lang639(tag, false), lang639(tag, true); a != want[0] || b != want[1] {
			t.Errorf("lang639(%q) = %s/%s, want %v", tag, a, b, want)
		}
	}
}
