package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pieceSite serves a stream of noise pieces shaped like MPEG-TS (each starts with a program map
// naming H.264 and AAC), with a master playlist whose qualities can be changed while it runs.
// Each piece takes delay to send; hook can answer a piece request itself.
type pieceSite struct {
	srv    *httptest.Server
	pieces [][]byte
	delay  time.Duration

	mu        sync.Mutex
	master    string // the master playlist ("" = none: /v/index.m3u8 is the stream)
	token     int    // pieces need ?tok= this (0 = no token)
	hits      map[int]int
	times     []pieceHit
	active    int
	peak      int
	limit     int // more pieces at once than this are turned away (0 = no limit)
	refused   int
	lists     int // playlists served
	served    int64
	hook      func(w http.ResponseWriter, r *http.Request, i int) bool
	types     []byte
	playlistX string // extra lines for the media playlist
}

type pieceHit struct {
	i  int
	at time.Time
}

func newPieceSite(t *testing.T, n int, delay time.Duration) *pieceSite {
	s := &pieceSite{delay: delay, hits: map[int]int{}, types: []byte{0x1B, 0x0F}}
	for i := 0; i < n; i++ {
		s.pieces = append(s.pieces, append(fakeTSID(s.types...), fakeTS(150+i%7*20)...))
	}
	s.srv = httptest.NewServer(s)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *pieceSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	s.mu.Lock()
	tok, master, extra := s.token, s.master, s.playlistX
	s.mu.Unlock()
	switch {
	case strings.HasSuffix(p, "/master.m3u8"):
		if master == "" {
			http.NotFound(w, r)
			return
		}
		s.count(&s.lists, len(master))
		fmt.Fprint(w, master)
	case strings.HasSuffix(p, ".m3u8"):
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n" + extra)
		for i := range s.pieces {
			if tok > 0 {
				fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts?tok=%d\n", i, tok)
			} else {
				fmt.Fprintf(&b, "#EXTINF:4.0,\nseg%d.ts\n", i)
			}
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		s.count(&s.lists, b.Len())
		fmt.Fprint(w, b.String())
	case strings.HasSuffix(p, ".ts"):
		var i int
		if _, err := fmt.Sscanf(filepath.Base(p), "seg%d.ts", &i); err != nil || i >= len(s.pieces) {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Range") != "" { // a look at the start of a piece (is it still the same stream?)
			s.mu.Lock()
			body := s.pieces[i]
			s.mu.Unlock()
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
			return
		}
		s.mu.Lock()
		s.hits[i]++
		s.times = append(s.times, pieceHit{i, time.Now()})
		hook := s.hook
		if s.limit > 0 && s.active >= s.limit {
			s.refused++
			s.mu.Unlock()
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		s.active++
		s.peak = max(s.peak, s.active)
		cur := s.token
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.active--
			s.mu.Unlock()
		}()
		if cur > 0 && r.URL.Query().Get("tok") != strconv.Itoa(cur) {
			http.Error(w, "link expired", http.StatusForbidden)
			return
		}
		if hook != nil && hook(w, r, i) {
			return
		}
		time.Sleep(s.delay)
		s.mu.Lock()
		body := s.pieces[i]
		s.mu.Unlock()
		w.Header().Set("Content-Type", "video/mp2t")
		w.Write(body)
		s.mu.Lock()
		s.served += int64(len(body))
		s.mu.Unlock()
	default:
		http.NotFound(w, r)
	}
}

func (s *pieceSite) count(n *int, size int) {
	s.mu.Lock()
	*n++
	s.served += int64(size)
	s.mu.Unlock()
}

func (s *pieceSite) want() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Join(s.pieces, nil)
}

func (s *pieceSite) hitCount(i int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[i]
}

func streamResult(t *testing.T, d *Download, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(d.Dir, d.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: got %d bytes, want %d", d.FileName, len(got), len(want))
	}
}

// TestStreamPieceRetryAfter: a piece turned away with Retry-After is asked for again only when
// the server said, and nothing else is asked of that server meanwhile.
func TestStreamPieceRetryAfter(t *testing.T) {
	site := newPieceSite(t, 12, 10*time.Millisecond)
	var at time.Time
	site.hook = func(w http.ResponseWriter, r *http.Request, i int) bool {
		if i != 4 || site.hitCount(4) > 1 {
			return false
		}
		site.mu.Lock()
		at = time.Now()
		site.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		http.Error(w, "slow down", http.StatusServiceUnavailable)
		return true
	}
	m := newTestManager(t)
	quickRetries(t, m)
	m.settings.Conns = 3
	d := m.Add(AddReq{URL: site.srv.URL + "/v/index.m3u8", Name: "clip.ts", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	streamResult(t, d, site.want())
	site.mu.Lock()
	defer site.mu.Unlock()
	var again time.Time
	for _, h := range site.times {
		if dt := h.at.Sub(at); dt > 100*time.Millisecond && dt < 900*time.Millisecond {
			t.Errorf("piece %d was asked for %v after the server said to wait a second", h.i, dt)
		}
		if h.i == 4 && h.at.After(at) && again.IsZero() {
			again = h.at
		}
	}
	if dt := again.Sub(at); dt < 950*time.Millisecond || dt > 3*time.Second {
		t.Fatalf("piece 4 asked for again after %v", dt)
	}
}

// TestStreamConnectionLimit: a server that turns away extra connections gets fewer pieces at
// once (and is offered one more now and then, as it may only have been busy).
func TestStreamConnectionLimit(t *testing.T) {
	site := newPieceSite(t, 60, 30*time.Millisecond)
	site.limit = 2
	m := newTestManager(t)
	quickRetries(t, m)
	setVar(t, &learnedCreep, 400*time.Millisecond)
	m.settings.Conns = 8
	t0 := time.Now()
	d := m.Add(AddReq{URL: site.srv.URL + "/v/index.m3u8", Name: "clip.ts", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	el := time.Since(t0)
	streamResult(t, d, site.want())
	site.mu.Lock()
	refused, peak := site.refused, site.peak
	site.mu.Unlock()
	// the first burst is turned away; after that, one more try per creep period at most
	if creeps := int(el/learnedCreep) + 1; refused > 8+2*creeps || peak > 2 {
		t.Fatalf("%d pieces turned away (peak %d) in %v", refused, peak, el)
	}
	if el < 60/2*30*time.Millisecond {
		t.Fatalf("expected the pieces two at a time, took %v", el)
	}
	t.Logf("%d refused at first and while creeping up, in %v", refused, el.Round(time.Millisecond))
}

// TestStreamOutageRecovers: a server that goes away for a while (every connection dropped)
// costs the pieces their tries, then the whole download tries again by itself and carries on
// from the pieces already written.
func TestStreamOutageRecovers(t *testing.T) {
	site := newPieceSite(t, 30, 15*time.Millisecond)
	var down sync.Map
	site.hook = func(w http.ResponseWriter, r *http.Request, i int) bool {
		if i < 10 {
			return false
		}
		first, _ := down.LoadOrStore("from", time.Now())
		if time.Since(first.(time.Time)) > 1500*time.Millisecond {
			return false
		}
		c, _, err := w.(http.Hijacker).Hijack() // the connection drops
		if err == nil {
			c.Close()
		}
		return true
	}
	m := newTestManager(t)
	quickRetries(t, m)
	setVar(t, &maxTries, 4)
	setVar(t, &wholeRetry, []time.Duration{300 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond})
	m.settings.Conns = 2
	d := m.Add(AddReq{URL: site.srv.URL + "/v/index.m3u8", Name: "clip.ts", Start: true})
	sawRetry := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		v := m.Snapshot(nil).Downloads[0]
		if v.Status == StConnecting && v.RetryAt > 0 {
			sawRetry = true
		}
		if v.Status == StCompleted || v.Status == StError {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, m, d, StCompleted, time.Second)
	streamResult(t, d, site.want())
	if !sawRetry {
		t.Fatal("expected the whole download to wait and try again")
	}
	for i := 0; i < 8; i++ {
		if n := site.hitCount(i); n != 1 {
			t.Fatalf("piece %d was fetched %d times: the download should carry on, not start over", i, n)
		}
	}
}

// TestStreamOfflineWaits: while the computer is offline, failed pieces don't use up their tries.
func TestStreamOfflineWaits(t *testing.T) {
	site := newPieceSite(t, 12, 5*time.Millisecond)
	var off sync.Map
	site.hook = func(w http.ResponseWriter, r *http.Request, i int) bool {
		if v, ok := off.Load("off"); !ok || !v.(bool) {
			return false
		}
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			c.Close()
		}
		return true
	}
	m := newTestManager(t)
	quickRetries(t, m)
	setVar(t, &maxTries, 3)
	var offline sync.Map
	setVar(t, &onlineCheck, func() bool { v, _ := offline.Load("x"); return v == nil })
	off.Store("off", true)
	offline.Store("x", true)
	d := m.Add(AddReq{URL: site.srv.URL + "/v/index.m3u8", Name: "clip.ts", Start: true})
	time.Sleep(1200 * time.Millisecond) // many failed tries, none counted
	m.mu.Lock()
	st := d.Status
	m.mu.Unlock()
	if st == StError {
		t.Fatalf("failed while offline: %s", d.Error)
	}
	off.Store("off", false)
	offline.Delete("x")
	waitFor(t, m, d, StCompleted, 20*time.Second)
	streamResult(t, d, site.want())
}

// TestStreamExpiredLinks: signed piece links run out part-way; the playlist is loaded again for
// fresh ones and the download carries on. When the playlist's own link runs out too, a fresh
// one for the same stream (from the browser, or clicked again) carries it on.
func TestStreamExpiredLinks(t *testing.T) {
	site := newPieceSite(t, 40, 25*time.Millisecond)
	site.token = 1
	stopTok := make(chan struct{})
	go func() { // the CDN's signature changes every 300 ms
		for {
			select {
			case <-stopTok:
				return
			case <-time.After(300 * time.Millisecond):
			}
			site.mu.Lock()
			site.token++
			site.mu.Unlock()
		}
	}()
	defer close(stopTok)
	m := newTestManager(t)
	quickRetries(t, m)
	m.settings.Conns = 2
	d := m.Add(AddReq{URL: site.srv.URL + "/v/index.m3u8", Name: "clip.ts", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	streamResult(t, d, site.want())
	site.mu.Lock()
	lists := site.lists
	site.mu.Unlock()
	if lists < 2 {
		t.Fatalf("the playlist was loaded %d times: expected it again for fresh links", lists)
	}
}

func TestStreamPlaylistLinkRenewed(t *testing.T) {
	site := newPieceSite(t, 30, 20*time.Millisecond)
	gen := 1
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/s/", func(w http.ResponseWriter, r *http.Request) { // /s/<gen>/<file>: signed links, playlist and pieces
		parts := strings.Split(r.URL.Path, "/")
		g, _ := strconv.Atoi(parts[2])
		mu.Lock()
		ok := g == gen
		mu.Unlock()
		if !ok || len(parts) != 4 {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/v/" + parts[3]
		site.ServeHTTP(w, r2)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	pieces := site.pieces
	site.hook = func(w http.ResponseWriter, r *http.Request, i int) bool {
		if i >= 12 {
			mu.Lock()
			gen = 2 // the old links run out (playlist and pieces alike)
			mu.Unlock()
		}
		return false
	}
	m := newTestManager(t)
	quickRetries(t, m)
	m.settings.Conns = 2
	d := m.Add(AddReq{URL: srv.URL + "/s/1/index.m3u8", Name: "clip.ts", Start: true})
	waitLink(t, m, d, "expired", 20*time.Second)
	m.mu.Lock()
	done := d.PartsDone
	m.mu.Unlock()
	if done < 10 || done >= 30 {
		t.Fatalf("stopped at piece %d", done)
	}
	// a link for another stream isn't taken
	other := newPieceSite(t, 30, 0)
	if got := m.tryLinks(context.Background(), []linkCand{{URL: other.srv.URL + "/v/index.m3u8"}}, "", ""); got != nil {
		t.Fatal("a different stream was taken as the same one")
	}
	before := make([]int, done)
	for i := range before {
		before[i] = site.hitCount(i)
	}
	if got := m.tryLinks(context.Background(), []linkCand{{URL: srv.URL + "/s/2/index.m3u8"}}, "", ""); got != d {
		t.Fatal("the fresh link for the same stream wasn't recognised")
	}
	waitFor(t, m, d, StCompleted, 30*time.Second)
	streamResult(t, d, bytes.Join(pieces, nil))
	for i := range before {
		if site.hitCount(i) != before[i] {
			t.Fatalf("piece %d was fetched again: the download should carry on", i)
		}
	}
}

// TestStreamResumeSameQuality: resuming against a regenerated master playlist picks the same
// quality by what it is, even when its bandwidth is stated differently; when that quality is gone
// (a different resolution), or its pieces are now a different encoding, it fails clearly
// instead of mixing two.
func TestStreamResumeSameQuality(t *testing.T) {
	master := func(hiBW, hiRes, hiPath string) string {
		return "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,AVERAGE-BANDWIDTH=700000,RESOLUTION=640x360,CODECS=\"avc1.4d401e,mp4a.40.2\",FRAME-RATE=25\nlo/index.m3u8\n" +
			"#EXT-X-STREAM-INF:BANDWIDTH=" + hiBW + ",RESOLUTION=" + hiRes + ",CODECS=\"avc1.64001f,mp4a.40.2\",FRAME-RATE=25.000\n" + hiPath + "\n"
	}
	cases := []struct {
		name, master string
		types        []byte
		fail         bool
	}{
		{"new bandwidths", master("2300000", "1280x720", "hi2/index.m3u8"), nil, false},
		{"other resolution", master("2500000", "1920x1080", "hi/index.m3u8"), nil, true},
		{"other encoding", master("2500000", "1280x720", "hi/index.m3u8"), []byte{0x24, 0x0F}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			site := newPieceSite(t, 30, 25*time.Millisecond)
			site.master = master("2500000", "1280x720", "hi/index.m3u8")
			m := newTestManager(t)
			quickRetries(t, m)
			m.settings.Conns = 2
			d := m.Add(AddReq{URL: site.srv.URL + "/v/master.m3u8", Name: "clip.ts", Start: true})
			for site.hitCount(8) == 0 {
				time.Sleep(5 * time.Millisecond)
			}
			m.Pause([]string{d.ID})
			m.waitStopped(d)
			m.mu.Lock()
			res, codecs, fps, init, bw := d.VarRes, d.VarCodecs, d.VarFPS, d.VarInit, d.Variant
			m.mu.Unlock()
			if res != "1280x720" || codecs != "avc1.64001f,mp4a.40.2" || fps != 25 || init != "ts:1b,0f" || bw != 2500000 {
				t.Fatalf("quality kept: %q %q %v %q %d", res, codecs, fps, init, bw)
			}
			site.mu.Lock()
			site.master = c.master
			if c.types != nil {
				for i := range site.pieces {
					site.pieces[i] = append(fakeTSID(c.types...), site.pieces[i][2*188:]...)
				}
			}
			site.mu.Unlock()
			m.Start([]string{d.ID})
			if c.fail {
				waitFor(t, m, d, StError, 20*time.Second)
				if !strings.Contains(d.Error, "qualities changed") {
					t.Fatalf("error %q", d.Error)
				}
				return
			}
			waitFor(t, m, d, StCompleted, 20*time.Second)
			streamResult(t, d, site.want())
		})
	}
}

// TestStreamBytesCountedOnce: a piece whose connection drops part-way is fetched again, and only
// the copy that's kept counts, in the data used and in the progress.
func TestStreamBytesCountedOnce(t *testing.T) {
	site := newPieceSite(t, 8, 5*time.Millisecond)
	for i := range site.pieces {
		site.pieces[i] = append(site.pieces[i], fakeTS(400)...) // big enough to arrive in several reads
	}
	var cut int64
	site.hook = func(w http.ResponseWriter, r *http.Request, i int) bool {
		if i != 3 || site.hitCount(3) > 1 {
			return false
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(site.pieces[3])))
		w.Write(site.pieces[3][:40000])
		w.(http.Flusher).Flush()
		site.mu.Lock()
		cut = 40000
		site.mu.Unlock()
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			c.Close()
		}
		return true
	}
	// a page instead of a piece: fetched again, not counted
	var page atomic.Bool
	hook := site.hook
	site.hook = func(w http.ResponseWriter, r *http.Request, i int) bool {
		if i == 5 && page.CompareAndSwap(false, true) {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, strings.Repeat("<p>not here</p>", 100))
			return true
		}
		return hook(w, r, i)
	}
	m := newTestManager(t)
	quickRetries(t, m)
	m.settings.Conns = 2
	d := m.Add(AddReq{URL: site.srv.URL + "/v/index.m3u8", Name: "clip.ts", Start: true})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	streamResult(t, d, site.want())
	used, _ := m.usage.used(1)
	site.mu.Lock()
	served := site.served
	site.mu.Unlock()
	if cut == 0 || used != served {
		t.Fatalf("data used %d, the pieces and playlists kept are %d (%d bytes of a dropped piece)", used, served, cut)
	}
	if done := m.Snapshot(nil).Downloads[0].Done; done != int64(len(site.want())) {
		t.Fatalf("progress says %d bytes, the file has %d", done, len(site.want()))
	}
}

// TestStreamPiecesRefusedNeedNewLink: the pieces are refused part-way and loading the playlist
// again doesn't help (a session that ran out): the download waits for a fresh link instead of
// trying for minutes, and one from the browser (with its new cookie) carries it on.
func TestStreamPiecesRefusedNeedNewLink(t *testing.T) {
	site := newPieceSite(t, 30, 10*time.Millisecond)
	var session atomic.Value
	session.Store("s=1")
	site.hook = func(w http.ResponseWriter, r *http.Request, i int) bool {
		if i >= 10 && session.Load() == "s=1" {
			session.Store("s=2") // the session runs out
		}
		if r.Header.Get("Cookie") != session.Load() {
			http.Error(w, "sign in again", http.StatusForbidden)
			return true
		}
		return false
	}
	m := newTestManager(t)
	quickRetries(t, m)
	m.settings.Conns = 2
	u := site.srv.URL + "/v/index.m3u8"
	d := m.Add(AddReq{URL: u, Name: "clip.ts", Start: true, Headers: map[string]string{"Cookie": "s=1"}})
	t0 := time.Now()
	waitLink(t, m, d, "expired", 10*time.Second)
	if el := time.Since(t0); el > 3*time.Second {
		t.Fatalf("took %v to ask for a fresh link", el)
	}
	if got := m.tryLinks(context.Background(), []linkCand{{URL: u, Cookie: "s=2"}}, "", ""); got != d {
		t.Fatal("the fresh link wasn't taken")
	}
	waitFor(t, m, d, StCompleted, 20*time.Second)
	streamResult(t, d, site.want())
}
