package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Stream pieces are fetched with the main engine's care: a piece is tried again after trouble
// with the same backoff (woken early when the network comes back; time offline doesn't count),
// servers that ask to be left alone (Retry-After) or that turn away extra connections are
// respected, a download keeps to its fair share of each server, and a piece's bytes are counted
// once, for the attempt that's kept. When pieces start being refused part-way (signed links that
// ran out), the playlist is loaded again for fresh ones.

// pieceTries is how many times a piece that arrives cut short is fetched before what came is kept
// (the end-of-stream check then decides), and liveTries how often a live piece is tried before
// the recording goes on without it.
var (
	pieceTries = 3
	liveTries  = 4
)

// hlsRun is what one run of a stream download shares among its piece fetches. Its lock is
// taken before the hostState's and never with m.mu or d.mu held.
type hlsRun struct {
	m    *Manager
	d    *Download
	keys *keyCache
	live bool // recording: a piece that keeps failing is skipped

	loadMu sync.Mutex // one playlist reload at a time
	reload func(ctx context.Context) (*hlsInfo, error)

	mu      sync.Mutex
	hosts   map[string]*runHost
	wake    chan struct{} // closed (and replaced) when a connection is given back
	gen     int           // bumped each time fresh links came in
	worked  bool          // a piece came through: refused pieces now mean the links ran out
	kept    bool          // a piece was kept cut short: don't fetch such pieces again and again
	tracks  map[*hlsMedia]*runTrack
	workers int
}

// runHost is one server as this run sees it.
type runHost struct {
	active    int
	learned   int // the pieces it accepted at once when it turned more away (0 = no limit seen)
	learnedAt time.Time
	waiting   []int // pieces waiting for a connection to it: the lowest goes first
}

// runTrack is one playlist being fetched (guarded by the run's lock).
type runTrack struct {
	audio bool                // the separate audio track
	extra int                 // another audio language kept (1 + its index in info.Extras): not measured
	mp4   map[uint32]mp4Track // the init piece's tracks, to measure MP4 pieces
}

// newRun prepares a run of d. info is what the playlist said.
func (m *Manager) newRun(d *Download, info *hlsInfo) *hlsRun {
	r := &hlsRun{m: m, d: d, keys: &keyCache{m: map[string][]byte{}}, hosts: map[string]*runHost{}, wake: make(chan struct{}),
		tracks: map[*hlsMedia]*runTrack{}, live: info.Live}
	m.mu.Lock()
	r.worked = d.PartsDone > 0 || d.ADone > 0
	m.hosts.setDefault(m.connsWanted())
	m.mu.Unlock()
	if info.Media != nil {
		r.tracks[info.Media] = &runTrack{}
	}
	if info.Audio != nil {
		r.tracks[info.Audio] = &runTrack{audio: true}
	}
	for i, x := range info.Extras {
		r.tracks[x.media] = &runTrack{extra: i + 1}
	}
	if !info.Live {
		r.reload = func(ctx context.Context) (*hlsInfo, error) { return m.resolveHLS(ctx, d, d.URL, d.Variant) }
	}
	return r
}

func (r *hlsRun) track(media *hlsMedia) *runTrack {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tracks[media]
	if t == nil {
		t = &runTrack{}
		r.tracks[media] = t
	}
	return t
}

// isAudio: media is the separate audio track.
func (r *hlsRun) isAudio(media *hlsMedia) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tracks[media]
	return t != nil && t.audio
}

func (r *hlsRun) isExtra(media *hlsMedia) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tracks[media]
	return t != nil && t.extra > 0
}

// seg is piece i of media as it stands (fresh links replace the old ones); -1 is the init piece.
func (r *hlsRun) seg(media *hlsMedia, i int) hlsSegment {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 {
		return hlsSegment{URL: media.InitURL, RangeOff: media.InitOff, RangeLen: media.InitLen, Seq: -1}
	}
	return media.Segments[i]
}

// open starts fetching media into f, which already holds have bytes of it: the part file is
// synced whenever progress is saved, and a resumed MP4 track reads its init piece back.
func (r *hlsRun) open(media *hlsMedia, f *os.File, have int64) {
	t := r.track(media)
	if have > 0 && media.InitURL != "" {
		b := make([]byte, min(have, 4<<20))
		if n, _ := f.ReadAt(b, 0); n > 0 {
			tracks := initTracks(b[:n])
			r.mu.Lock()
			t.mp4 = tracks
			r.mu.Unlock()
		}
	}
	r.d.mu.Lock()
	r.d.file = f
	r.d.mu.Unlock()
}

func (r *hlsRun) close(f *os.File) {
	r.d.mu.Lock()
	if r.d.file == f {
		r.d.file = nil
	}
	r.d.mu.Unlock()
}

// run is the stream run d's pieces belong to (a standalone one outside downloadHLS).
func (d *Download) run(m *Manager) *hlsRun {
	if d.hs == nil {
		d.hs = m.newRun(d, &hlsInfo{})
	}
	return d.hs
}

// recorded: d is a live recording with pieces on disk. It doesn't carry on (the stream has moved
// on meanwhile): what it holds is packaged.
func (m *Manager) recorded(d *Download) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return d.Live && d.streamStarted()
}

// streamGone explains a playlist that can't be loaded: when the download had got somewhere, a
// refusal means its link ran out (the browser extension or a click on the link renews it).
func (m *Manager) streamGone(d *Download, err error) error {
	var se statusErr
	m.mu.Lock()
	started := d.streamStarted() && !d.Live
	m.mu.Unlock()
	if started && errors.As(err, &se) && expiredStatus(se.code) {
		return permanentErr{errLinkExpired}
	}
	return err
}

// streamRefused: encryption Zipline can't undo. SAMPLE-AES in MP4 pieces is CENC ("cbcs").
func streamRefused(info *hlsInfo) error {
	for _, media := range []*hlsMedia{info.Media, info.Audio} {
		if media == nil || media.InitURL == "" {
			continue
		}
		for _, s := range media.Segments {
			if s.SampleAES {
				return permanentErr{errors.New("this video's MP4 pieces use SAMPLE-AES encryption, which Zipline can't decrypt")}
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- connections

// budget is how many connections all downloads together may hold to host.
func (r *hlsRun) budget(u string) int {
	if l := siteConns(u); l > 0 {
		return l
	}
	return r.m.hosts.perDownload()
}

// limit is how many pieces host takes at once now (0 = no limit seen). A learned limit goes up
// by one a while after it last changed, as a file download's does. Caller holds r.mu.
func (r *hlsRun) limit(h *runHost, now time.Time) int {
	if h.learned > 0 && now.Sub(h.learnedAt) >= learnedCreep {
		h.learned++
		h.learnedAt = now
		if h.learned >= r.workers {
			h.learned = 0
		}
	}
	return h.learned
}

// acquire waits for a connection to the server of u for piece i: until the server's Retry-After,
// within what it showed it accepts, and within this download's fair share of it. The lowest
// piece waiting goes first, so the file keeps being written in order.
func (r *hlsRun) acquire(ctx context.Context, u string, i int) (func(), error) {
	host, budget := connHost(u), r.budget(u)
	r.mu.Lock()
	h := r.hosts[host]
	if h == nil {
		h = &runHost{}
		r.hosts[host] = h
	}
	h.waiting = append(h.waiting, i)
	leave := func() {
		for k, x := range h.waiting {
			if x == i {
				h.waiting = append(h.waiting[:k], h.waiting[k+1:]...)
				break
			}
		}
	}
	for {
		now := time.Now()
		wait := 250 * time.Millisecond
		if q := r.m.hosts.until(host); q.After(now) {
			wait = min(q.Sub(now), 2*time.Second)
		} else if lowest(h.waiting) == i {
			if l := r.limit(h, now); (l == 0 || h.active < l) && r.m.hosts.take(host, r.d, budget) {
				h.active++
				leave()
				r.mu.Unlock()
				var once sync.Once
				return func() {
					once.Do(func() {
						r.mu.Lock()
						h.active--
						r.m.hosts.put(host, r.d)
						close(r.wake)
						r.wake = make(chan struct{})
						r.mu.Unlock()
					})
				}, nil
			}
		}
		wake := r.wake
		r.mu.Unlock()
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-t.C:
		}
		t.Stop()
		r.mu.Lock()
		if ctx.Err() != nil {
			leave()
			r.mu.Unlock()
			return nil, ctx.Err()
		}
	}
}

func lowest(v []int) int {
	m := v[0]
	for _, x := range v[1:] {
		m = min(m, x)
	}
	return m
}

// turnedAway notes that the server of u refused a piece while others pieces of ours were coming
// from it: it's turning away extra connections, so fewer are used (for a while).
func (r *hlsRun) turnedAway(u string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.hosts[connHost(u)]
	if h == nil || h.active <= 1 {
		return false
	}
	if others := h.active - 1; h.learned == 0 || h.learned > others {
		h.learned, h.learnedAt = others, time.Now()
	}
	return true
}

// ---------------------------------------------------------------- one piece

// piece fetches piece i of media (-1: its init piece), trying again as the engine's connections
// do. It returns the bytes worth keeping and what they hold.
func (r *hlsRun) piece(ctx context.Context, media *hlsMedia, i int) ([]byte, pieceCheck, error) {
	d := r.d
	tries, fails, soft := 0, 0, 0
	reloaded := -1 // the playlist generation this piece last asked for
	limit := maxTries
	if r.live {
		limit = min(maxTries, liveTries)
	}
	t := r.track(media)
	for {
		r.mu.Lock()
		gen := r.gen
		r.mu.Unlock()
		s := r.seg(media, i)
		release, err := r.acquire(ctx, s.URL, i)
		if err != nil {
			return nil, pieceCheck{}, err
		}
		b, got, err := r.m.fetchPieceOnce(ctx, d, s, r.keys)
		var c pieceCheck
		if err == nil {
			if i < 0 {
				err = checkInit(b)
				c = pieceCheck{keep: len(b), span: -1}
			} else {
				r.mu.Lock()
				tracks := t.mp4
				r.mu.Unlock()
				c, err = checkPiece(b, s.Chunk, tracks, s.Dur)
			}
		}
		var se statusErr
		// turned away while other pieces of ours were coming from it (without asking for a wait):
		// it takes fewer connections at once
		away := err != nil && errors.As(err, &se) && (se.code == 429 || se.code == 503) && se.wait == 0 && r.turnedAway(s.URL)
		release()
		if err == nil && c.short && !r.isKept() && soft < pieceTries-1 {
			soft++ // cut short: worth another try or two before keeping what came
			atomic.AddInt64(&d.done, -got)
			if !backoffSleep(ctx, retryDelay(soft)) {
				return nil, c, ctx.Err()
			}
			continue
		}
		if err == nil {
			r.m.countDown(got) // (only now: a piece that wasn't kept doesn't count)
			if drop := int64(len(b) - c.keep); drop > 0 {
				atomic.AddInt64(&d.done, -drop)
			}
			r.mu.Lock()
			r.worked = true
			r.kept = r.kept || c.short
			r.mu.Unlock()
			return b[:c.keep], c, nil
		}
		atomic.AddInt64(&d.done, -got)
		if ctx.Err() != nil {
			return nil, c, ctx.Err()
		}
		var pe permanentErr
		switch {
		case errors.Is(err, errCapReconnect):
			continue // the data cap held it a long while: reconnect, not a failure
		case errors.As(err, &pe):
			return nil, c, err
		case away:
			continue // it waits for a free connection instead (not a failure)
		case se.code != 0:
			r.m.hosts.retryAfter(connHost(s.URL), se.wait)
			if expiredStatus(se.code) && r.reload != nil && gen > reloaded {
				ng, lerr := r.reloadLinks(ctx, gen)
				if errors.Is(lerr, errLinkExpired) || errors.As(lerr, &pe) {
					return nil, c, lerr
				}
				if lerr == nil {
					reloaded = ng
					continue // fresh links: try at once
				}
			}
			switch {
			case !r.everWorked() && !retryStatus(se.code):
				return nil, c, permanentErr{err} // a link that never worked: a 404 shouldn't take minutes
			case (se.code == 403 || se.code == 410) && reloaded >= 0:
				return nil, c, permanentErr{errLinkExpired} // fresh piece links didn't help: the stream's own link needs renewing
			}
		}
		if got >= 64<<10 { // dropped after real progress: a flaky line, not a dead server
			tries, fails = 0, 0
		}
		fails++
		if !netFailure(err) || online() { // while offline, waiting isn't trying
			tries++
		}
		d.mu.Lock()
		d.lastErr = err
		d.mu.Unlock()
		if tries >= limit {
			return nil, c, err
		}
		wait := retryDelay(fails)
		if q := time.Until(r.m.hosts.until(connHost(s.URL))); q > wait {
			wait = 0 // the connection waits out the server's Retry-After itself
		}
		if !backoffSleep(ctx, wait) {
			return nil, c, ctx.Err()
		}
	}
}

func (r *hlsRun) isKept() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.kept
}

func (r *hlsRun) everWorked() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.worked
}

// reloadLinks loads the playlist again for fresh piece links, unless another piece did since gen.
// It returns the generation now in use; errLinkExpired when the playlist's own link ran out.
func (r *hlsRun) reloadLinks(ctx context.Context, gen int) (int, error) {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	r.mu.Lock()
	cur := r.gen
	r.mu.Unlock()
	if cur != gen {
		return cur, nil
	}
	info, err := r.reload(ctx)
	if err != nil {
		var se statusErr
		if errors.As(err, &se) && expiredStatus(se.code) {
			return cur, permanentErr{errLinkExpired}
		}
		return cur, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for media, t := range r.tracks {
		fresh := info.Media
		if t.audio {
			fresh = info.Audio
		} else if t.extra > 0 {
			fresh = nil
			if t.extra <= len(info.Extras) {
				fresh = info.Extras[t.extra-1].media
			}
		}
		if fresh == nil || len(fresh.Segments) != len(media.Segments) {
			return cur, errStreamChanged
		}
		media.InitURL, media.InitOff, media.InitLen = fresh.InitURL, fresh.InitOff, fresh.InitLen
		copy(media.Segments, fresh.Segments)
	}
	r.gen++
	logf("%s: the stream's links ran out; loaded the playlist again for fresh ones", r.d.FileName)
	return r.gen, nil
}

var errStreamChanged = permanentErr{errors.New("the stream changed on the server since this download started — use Redownload")}

// fetchPieceOnce makes one request for a piece and decrypts it. got is what it received, counted
// in the download's progress as it came (the caller takes it back when the piece isn't kept).
func (m *Manager) fetchPieceOnce(ctx context.Context, d *Download, s hlsSegment, keys *keyCache) ([]byte, int64, error) {
	rng := ""
	if s.RangeLen > 0 {
		rng = rangeHeader(s.RangeOff, s.RangeOff+s.RangeLen-1)
	}
	req, err := buildRequest(ctx, d, s.URL, rng)
	if err != nil {
		return nil, 0, permanentErr{err}
	}
	resp, err := doReqUA(d, req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if challenged(resp) {
		drainClose(resp.Body)
		return nil, 0, permanentErr{errChallenge}
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusUnauthorized:
		drainClose(resp.Body)
		return nil, 0, loginError(resp.Request)
	case http.StatusProxyAuthRequired:
		drainClose(resp.Body)
		return nil, 0, permanentErr{errors.New("the proxy needs a user name and password (Options → Proxy)")}
	default:
		drainClose(resp.Body)
		return nil, 0, statusErr{resp.StatusCode, resp.Status, retryAfterHeader(resp.Header)}
	}
	if pageType(resp.Header.Get("Content-Type")) {
		drainClose(resp.Body)
		if strings.Contains(resp.Header.Get("Content-Type"), "json") {
			return nil, 0, errNotPiece{"a message"}
		}
		return nil, 0, errNotPiece{"a web page"}
	}
	if resp.StatusCode == http.StatusPartialContent && s.RangeLen > 0 && !rightPart(resp.Header.Get("Content-Range"), s.RangeOff, s.RangeOff+s.RangeLen-1) {
		drainClose(resp.Body)
		return nil, 0, errWrongPart // (not permanent here: the next try may get it right)
	}
	buf := make([]byte, 0, min(max(resp.ContentLength, 64<<10), 64<<20))
	tmp := make([]byte, 64*1024)
	stall := time.AfterFunc(stallLimit, func() { resp.Body.Close() })
	defer stall.Stop()
	var got int64
	for {
		stall.Stop()
		want, long := m.pace(ctx, d, len(tmp))
		if ctx.Err() != nil || long {
			m.refund(d, want, 0)
			if long {
				return nil, got, errCapReconnect
			}
			return nil, got, ctx.Err()
		}
		stall.Reset(stallLimit)
		n, rerr := resp.Body.Read(tmp[:want])
		m.refund(d, want, n)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			got += int64(n)
			atomic.AddInt64(&d.done, int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, got, rerr
		}
	}
	if s.RangeLen > 0 && resp.StatusCode == http.StatusOK && int64(len(buf)) > s.RangeLen {
		if end := s.RangeOff + s.RangeLen; end <= int64(len(buf)) {
			buf = buf[s.RangeOff:end]
		}
	}
	if s.RangeLen > 0 && int64(len(buf)) < s.RangeLen && resp.StatusCode == http.StatusPartialContent {
		return nil, got, errClosedEarly
	}
	if s.KeyURL == "" || keys == nil {
		return buf, got, nil
	}
	key, err := keys.get(ctx, m, d, s.KeyURL)
	if err != nil {
		return nil, got, err
	}
	if s.SampleAES {
		iv := s.IV
		if iv == nil {
			iv = seqIV(s.Seq)
		}
		b, err := decryptSampleAES(buf, key, iv)
		return b, got, err
	}
	b, err := decryptSegment(buf, key, s)
	return b, got, err
}

func seqIV(seq int64) []byte {
	iv := make([]byte, 16)
	for i := 0; i < 8; i++ {
		iv[15-i] = byte(seq >> (8 * i))
	}
	return iv
}

// refund gives back the bandwidth a read reserved but didn't use. (A stream's bytes are counted
// in the data meter once its piece is kept.)
func (m *Manager) refund(d *Download, reserved, got int) {
	if got < reserved {
		m.limiter.Refund(reserved - got)
		d.lim.Refund(reserved - got)
		m.politeLim.Refund(reserved - got)
	}
}

// doReqUA is doReq for a server that may turn Zipline's own name away but take a browser's: it
// asks once more as a browser, and remembers the server for the rest of the session.
func doReqUA(d *Download, req *http.Request) (*http.Response, error) {
	resp, err := doReq(d, req)
	if err != nil || (resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotAcceptable) || req.Header.Get("User-Agent") != defaultUA {
		return resp, err
	}
	drainClose(resp.Body)
	again := req.Clone(req.Context())
	again.Header.Set("User-Agent", browserUA)
	if resp, err = doReq(d, again); err != nil {
		return nil, err
	}
	if resp.StatusCode < 400 {
		browserHosts.Store(strings.ToLower(req.URL.Host), true)
		browserHosts.Store(strings.ToLower(resp.Request.URL.Host), true)
	}
	return resp, nil
}

// ---------------------------------------------------------------- many pieces

// fetchList fetches pieces [from, to) of media on several connections and appends them to f in
// order, calling progress after each with the pieces now written and the bytes it added.
func (r *hlsRun) fetchList(ctx context.Context, media *hlsMedia, from, to int, f *os.File, progress func(done int, written int64)) error {
	if from >= to {
		return nil
	}
	d := r.d
	first := r.seg(media, from)
	workers := r.m.dlConns(d)
	if l := siteConns(first.URL); l > 0 && l < workers {
		workers = l
	}
	workers = max(min(workers, 12, to-from), 1)
	r.mu.Lock()
	r.workers = max(r.workers, workers)
	r.mu.Unlock()
	window := workers * 3

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	type got struct {
		b []byte
		c pieceCheck
	}
	ready := map[int]got{}
	nextWrite := from
	var firstErr error
	next := int64(from)
	atomic.StoreInt32(&d.workers, int32(workers))
	defer atomic.StoreInt32(&d.workers, 0)

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1) - 1)
				if i >= to {
					return
				}
				mu.Lock()
				for i >= nextWrite+window && firstErr == nil && ctx.Err() == nil {
					cond.Wait()
				}
				stop := firstErr != nil || ctx.Err() != nil
				mu.Unlock()
				if stop {
					return
				}
				b, c, err := r.piece(ctx, media, i)
				var pe permanentErr
				if err != nil && r.live && ctx.Err() == nil && !errors.As(err, &pe) {
					logf("%s: piece %d of the recording couldn't be fetched (%v); going on without it", d.FileName, i+1, err)
					b, c, err = nil, pieceCheck{span: -1}, nil
				}
				mu.Lock()
				if err != nil {
					if firstErr == nil && ctx.Err() == nil {
						firstErr = fmt.Errorf("piece %d of %d: %w", i+1, to, err)
						cancel()
					}
					cond.Broadcast()
					mu.Unlock()
					return
				}
				ready[i] = got{b, c}
				for { // write everything that is now in order
					p, ok := ready[nextWrite]
					if !ok {
						break
					}
					delete(ready, nextWrite)
					if _, werr := f.Write(p.b); werr != nil {
						if firstErr == nil {
							firstErr = permanentErr{fmt.Errorf("disk: %w", werr)}
							cancel()
						}
						break
					}
					r.measured(media, nextWrite, p.b, p.c)
					nextWrite++
					progress(nextWrite, int64(len(p.b)))
				}
				cond.Broadcast()
				mu.Unlock()
			}
		}()
	}
	// a stopped download's workers may sit waiting for room in the window: wake them
	go func() {
		<-ctx.Done()
		mu.Lock()
		cond.Broadcast()
		mu.Unlock()
	}()
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if nextWrite < to {
		return errors.New("stream ended early")
	}
	return nil
}

// measured books a written piece: the playlist's length for it, and how much of it was missing.
// The first piece of the main track also names the quality (MPEG-TS: its stream types).
func (r *hlsRun) measured(media *hlsMedia, i int, b []byte, c pieceCheck) {
	s := r.seg(media, i)
	audio := r.isAudio(media)
	if r.isExtra(media) { // (the length check follows the main video and audio)
		return
	}
	missing := 0.0
	if s.Dur > 0 && c.short && c.span >= 0 {
		missing = max(s.Dur-c.span, 0)
	} else if s.Dur > 0 && len(b) == 0 {
		missing = s.Dur // skipped (a recording that went on without it)
	}
	m, d := r.m, r.d
	m.mu.Lock()
	defer m.mu.Unlock()
	if audio {
		d.AWantSecs += s.Dur
		d.AShortSecs += missing
		return
	}
	d.WantSecs += s.Dur
	d.ShortSecs += missing
	if i == 0 && media.InitURL == "" && d.VarInit == "" {
		d.VarInit = tsID(b)
	}
	m.dirty = true
}

// fetchInit fetches media's init piece (fMP4), notes its tracks, and names the quality by it.
func (r *hlsRun) fetchInit(ctx context.Context, media *hlsMedia) ([]byte, error) {
	b, _, err := r.piece(ctx, media, -1)
	if err != nil {
		return nil, err
	}
	t := r.track(media)
	r.mu.Lock()
	t.mp4 = initTracks(b)
	audio := t.audio
	r.mu.Unlock()
	if !audio {
		r.m.mu.Lock()
		if r.d.VarInit == "" {
			r.d.VarInit = initID(b)
		}
		r.m.mu.Unlock()
	}
	return b, nil
}
