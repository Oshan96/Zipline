package main

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveSite serves a live stream made of real pieces (by ffmpeg): at first the first pieces are
// out, then one more each step. The playlist lists the latest window of them (0: all, an event
// stream), ends with EXT-X-ENDLIST once end are out, or stops changing at stall.
type liveSite struct {
	srv    *httptest.Server
	tracks map[string][][]byte // playlist name → its pieces, in order
	durs   map[string][]float64
	start  time.Time
	step   time.Duration
	first  int
	window int
	end    int
	stall  int
	event  bool
	master string

	mu    sync.Mutex
	hits  map[string]int
	lists int
}

// newLiveSite serves the pieces of the HLS playlists in dir (name → playlist file) as live ones.
func newLiveSite(t *testing.T, dir string, playlists map[string]string) *liveSite {
	s := &liveSite{tracks: map[string][][]byte{}, durs: map[string][]float64{}, hits: map[string]int{}, step: 400 * time.Millisecond}
	for name, file := range playlists {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		_, media, err := parseM3U8("http://x/"+file, string(b))
		if err != nil {
			t.Fatal(err)
		}
		for _, sg := range media.Segments {
			p, err := os.ReadFile(filepath.Join(dir, filepath.Base(sg.URL)))
			if err != nil {
				t.Fatal(err)
			}
			s.tracks[name] = append(s.tracks[name], p)
			s.durs[name] = append(s.durs[name], sg.Dur)
		}
	}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *liveSite) out(name string) int {
	n := s.first + int(time.Since(s.start)/s.step)
	if s.stall > 0 {
		n = min(n, s.stall)
	}
	if s.end > 0 {
		n = min(n, s.end)
	}
	return min(n, len(s.tracks[name]))
}

func (s *liveSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "master.m3u8" && s.master != "" {
		fmt.Fprint(w, s.master)
		return
	}
	if strings.HasSuffix(name, ".m3u8") {
		tr := strings.TrimSuffix(name, ".m3u8")
		n := s.out(tr)
		from := 0
		if s.window > 0 && n > s.window {
			from = n - s.window
		}
		var b strings.Builder
		fmt.Fprintf(&b, "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", int(math.Ceil(s.durs[tr][0])), from)
		if s.event {
			b.WriteString("#EXT-X-PLAYLIST-TYPE:EVENT\n")
		}
		for i := from; i < n; i++ {
			fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s-%d.ts\n", s.durs[tr][i], tr, i)
		}
		if s.end > 0 && n >= s.end {
			b.WriteString("#EXT-X-ENDLIST\n")
		}
		s.mu.Lock()
		s.lists++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		fmt.Fprint(w, b.String())
		return
	}
	var tr string
	var i int
	if k := strings.LastIndexByte(name, '-'); k > 0 {
		tr = name[:k]
		fmt.Sscanf(name[k+1:], "%d.ts", &i)
	}
	if p := s.tracks[tr]; i < 0 || i >= len(p) || i >= s.out(tr) {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.hits[name]++
	s.mu.Unlock()
	w.Write(s.tracks[tr][i])
}

func (s *liveSite) fetched() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, n := range s.hits {
		for j := 0; j < n; j++ {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func liveManager(t *testing.T) *Manager {
	needFFmpeg(t)
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	t.Cleanup(func() { ffmpegPath = orig })
	m := newTestManager(t)
	quickRetries(t, m)
	return m
}

func probeLength(t *testing.T, path string) float64 {
	t.Helper()
	return fnum(ffprobe(t, path).Format.Duration)
}

// TestLiveRecordSlidingWindow records a live stream whose playlist keeps the latest few pieces,
// from three target durations behind the live edge until EXT-X-ENDLIST: each piece once, in
// order, reloading the playlist as RFC 8216 says, and the result is one MP4.
func TestLiveRecordSlidingWindow(t *testing.T) {
	m := liveManager(t)
	dir := makeHLS(t, "ts", 12, 1)
	site := newLiveSite(t, dir, map[string]string{"v": "index.m3u8"})
	site.first, site.window, site.end = 4, 4, 9
	site.start = time.Now()
	d := m.Add(AddReq{URL: site.srv.URL + "/v.m3u8", Start: true})
	sawRec := false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		s := m.Snapshot(nil)
		v := s.Downloads[0]
		if v.Status == StDownloading && v.Live && v.Rec > 0 && v.Stage == "" {
			text, _ := phoneStatus(&v, &s, time.Now())
			if !strings.HasPrefix(text, "Recording · 0:0") {
				t.Fatalf("status while recording: %q", text)
			}
			sawRec = true
		}
		if v.Status == StCompleted || v.Status == StError {
			break
		}
	}
	waitFor(t, m, d, StCompleted, time.Second)
	if !sawRec {
		t.Fatal("never shown as recording")
	}
	// started at piece 1 (pieces 1–3 are the last 3 seconds of the first playlist), ended at 8
	want := []string{"v-1.ts", "v-2.ts", "v-3.ts", "v-4.ts", "v-5.ts", "v-6.ts", "v-7.ts", "v-8.ts"}
	if got := site.fetched(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("fetched %v, want %v", got, want)
	}
	out := filepath.Join(d.Dir, d.FileName)
	if filepath.Ext(out) != ".mp4" || !isRegularMP4(out) || math.Abs(probeLength(t, out)-8) > 0.15 {
		t.Fatalf("%s: %.3f s", d.FileName, probeLength(t, out))
	}
	if math.Abs(d.WantSecs-8) > 0.01 || !strings.Contains(d.Note, "Recorded 0:08") {
		t.Fatalf("recorded %.2f s, note %q", d.WantSecs, d.Note)
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	site.mu.Lock()
	lists := site.lists
	site.mu.Unlock()
	// about 2 s of stream with a 1 s target duration: a load per target duration (or half of one)
	if lists < 3 || lists > 10 {
		t.Fatalf("the playlist was loaded %d times", lists)
	}
}

// TestLiveRecordIdle: a stream that stops updating (no EXT-X-ENDLIST) stops being recorded after
// a while; what came is saved.
func TestLiveRecordIdle(t *testing.T) {
	m := liveManager(t)
	setVar(t, &liveIdle, 2.0)
	dir := makeHLS(t, "ts", 8, 1)
	site := newLiveSite(t, dir, map[string]string{"v": "index.m3u8"})
	site.first, site.stall = 4, 6
	site.start = time.Now()
	t0 := time.Now()
	d := m.Add(AddReq{URL: site.srv.URL + "/v.m3u8", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	if el := time.Since(t0); el < 2*time.Second {
		t.Fatalf("stopped after %v: too soon for a stream that went quiet", el)
	}
	out := filepath.Join(d.Dir, d.FileName)
	if d.WantSecs != 5 || math.Abs(probeLength(t, out)-5) > 0.15 {
		t.Fatalf("recorded %.2f s, the file %.2f s", d.WantSecs, probeLength(t, out))
	}
}

// TestLiveRecordEventFromStartWithLimit: an event stream recorded from its beginning, for as
// long as asked.
func TestLiveRecordEventFromStartWithLimit(t *testing.T) {
	m := liveManager(t)
	dir := makeHLS(t, "ts", 10, 1)
	site := newLiveSite(t, dir, map[string]string{"v": "index.m3u8"})
	site.first, site.event = 3, true
	site.start = time.Now()
	d := m.Add(AddReq{URL: site.srv.URL + "/v.m3u8", Start: true, LiveFrom: "start", StopAfter: 5})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	want := []string{"v-0.ts", "v-1.ts", "v-2.ts", "v-3.ts", "v-4.ts"}
	if got := site.fetched(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("fetched %v, want %v", got, want)
	}
	if out := filepath.Join(d.Dir, d.FileName); math.Abs(probeLength(t, out)-5) > 0.15 {
		t.Fatalf("%.2f s", probeLength(t, out))
	}
}

// TestLiveRecordPause: Pause stops a recording and saves what it has, with the separate audio
// track merged in.
func TestLiveRecordPause(t *testing.T) {
	m := liveManager(t)
	dir := makeSeparateHLS(t, "ts", 16)
	site := newLiveSite(t, dir, map[string]string{"v": "p0.m3u8", "a": "p1.m3u8"})
	site.first, site.window, site.step = 3, 3, 500*time.Millisecond
	site.master = "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"aud\",NAME=\"en\",DEFAULT=YES,URI=\"a.m3u8\"\n" +
		"#EXT-X-STREAM-INF:BANDWIDTH=900000,RESOLUTION=640x360,CODECS=\"avc1.64001e,mp4a.40.2\",AUDIO=\"aud\"\nv.m3u8\n"
	site.start = time.Now()
	d := m.Add(AddReq{URL: site.srv.URL + "/master.m3u8", Name: "show.mp4", Start: true})
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		m.mu.Lock()
		n := d.PartsDone
		m.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("nothing recorded")
		}
	}
	m.Pause([]string{d.ID})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	out := filepath.Join(d.Dir, d.FileName)
	po := ffprobe(t, out)
	if d.FileName != "show.mp4" || po.stream("video") == nil || po.stream("audio") == nil || !isRegularMP4(out) {
		t.Fatalf("%s: %+v", d.FileName, po)
	}
	if got := fnum(po.Format.Duration); got < d.WantSecs-0.2 || got > d.WantSecs+2.2 || d.WantSecs < 6 { // (the audio may be a piece ahead)
		t.Fatalf("file %.2f s, recorded %.2f s", got, d.WantSecs)
	}
	if _, err := os.Stat(d.audioPartPath()); err == nil {
		t.Fatal("the audio part file was left behind")
	}
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
}

// A recording cut off (Zipline closed or crashed before it could package it) is saved when
// it's started again: the stream has moved on, so it isn't recorded over or carried on.
func TestLiveRecordCutOffIsSaved(t *testing.T) {
	m := liveManager(t)
	dir := makeHLS(t, "ts", 6, 1)
	site := newLiveSite(t, dir, map[string]string{"v": "index.m3u8"})
	site.first, site.window = 4, 4
	site.start = time.Now()
	var part []byte
	for i := 1; i <= 3; i++ {
		part = append(part, site.tracks["v"][i]...)
	}
	m.mu.Lock()
	d := &Download{ID: newID(), URL: site.srv.URL + "/v.m3u8", Kind: "hls", Live: true, Resumable: true, FileName: "cut.mp4", Dir: m.settings.DownloadDir,
		Category: "Video", Status: StPaused, Size: -1, Parts: 3, PartsDone: 3, PartBytes: int64(len(part)), WantSecs: 3, Added: time.Now()}
	m.list = append(m.list, d)
	m.byID[d.ID] = d
	m.mu.Unlock()
	if err := os.WriteFile(d.partPath(), part, 0o644); err != nil {
		t.Fatal(err)
	}
	m.Start([]string{d.ID})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	if got := site.fetched(); len(got) != 0 {
		t.Fatalf("fetched %v: a cut-off recording should only be saved", got)
	}
	if out := filepath.Join(d.Dir, d.FileName); d.FileName != "cut.mp4" || math.Abs(probeLength(t, out)-3) > 0.15 {
		t.Fatalf("%s: %.2f s", d.FileName, probeLength(t, out))
	}
}
