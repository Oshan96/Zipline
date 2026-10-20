package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Recording live and event streams (RFC 8216 §6.3). Recording starts three target durations
// before the live edge (an event stream can be recorded from its start instead), the playlist
// is loaded again as the RFC says — a target duration after it changed, half of one when it
// hadn't — and each piece is kept once, by its media sequence number. Pieces are written in
// order as they come, so what's recorded so far plays. Recording stops at the end of the stream
// (EXT-X-ENDLIST), after the length asked for, when the stream stops updating for a while, or
// on Pause; then what was recorded is packaged like any other stream. Live DASH isn't recorded.

// liveIdle is how many target durations (online) a stream may go without a new piece before
// the recording stops.
var liveIdle = 3.0

// recordLive records a live or event stream into d.
func (m *Manager) recordLive(ctx context.Context, d *Download, info *hlsInfo) error {
	if info.Format == "DASH" || info.Media == nil {
		return permanentErr{errors.New("this is a live DASH stream; Zipline records live HLS streams only")}
	}
	m.mu.Lock()
	d.Kind, d.Resumable, d.FinalURL, d.Live = "hls", true, info.MediaURL, true
	if d.Variant == 0 {
		d.Variant = info.Chosen.Bandwidth
	}
	d.setQuality(info.Chosen)
	d.Parts, d.PartsDone, d.PartBytes, d.Packaged = 0, 0, 0, false
	d.AParts, d.ADone, d.ABytes = 0, 0, 0
	d.WantSecs, d.ShortSecs, d.AWantSecs, d.AShortSecs = 0, 0, 0, 0
	if d.FileName == "" {
		d.FileName = hlsName(d.URL, "", ".mp4")
	}
	if ext := strings.ToLower(filepath.Ext(d.FileName)); ext == "" {
		d.FileName += ".mp4"
	} else if ext == ".ts" && (info.FMP4 || info.Audio != nil) {
		d.FileName = strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName)) + ".mp4"
	}
	if d.Category == "" {
		d.Category = "Video"
	}
	if d.Dir == "" {
		d.Dir = m.dirFor(d.Category)
	}
	d.FileName = m.uniqueName(d.Dir, d.FileName, d)
	d.Status, d.Note = StDownloading, ""
	from, stopAfter := d.LiveFrom, float64(d.StopAfter)
	m.dirty = true
	m.mu.Unlock()
	d.lim.SetRate(int64(d.LimitKB) * 1024)
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return err
	}
	atomic.StoreInt64(&d.done, 0)
	logf("%s: recording a live stream (%s)", d.FileName, d.URL)

	r := m.newRun(d, info)
	video := &recTrack{url: info.MediaURL, pl: info.Media, path: d.partPath(), parts: &d.Parts, done: &d.PartsDone, bytes: &d.PartBytes}
	tracks := []*recTrack{video}
	if info.Audio != nil {
		tracks = append(tracks, &recTrack{url: info.AudioURL, pl: info.Audio, path: d.audioPartPath(), audio: true, parts: &d.AParts, done: &d.ADone, bytes: &d.ABytes})
	} else {
		os.Remove(d.audioPartPath())
	}
	// where to start: three target durations from the live edge (RFC 8216 §6.3.3), or the start
	// of an event stream when asked; the audio from the same point before the end as the video
	back := 3 * max(info.Media.Target, 1)
	if from == "start" {
		back = 1e18
	}
	video.next = liveStart(info.Media, back)
	if len(tracks) > 1 {
		tracks[1].next = liveStart(info.Audio, fromEnd(info.Media, video.next)-0.05)
	}

	stop := &recStop{ch: make(chan struct{})} // the main track stopped: the audio catches up and stops too
	errs := make([]error, len(tracks))
	var wg sync.WaitGroup
	for i, t := range tracks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i == 0 {
				defer func() {
					stop.secs = t.queued
					close(stop.ch)
				}()
			}
			errs[i] = m.recordTrack(ctx, r, t, stop, stopAfter)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		var pe permanentErr
		if err != nil && ctx.Err() == nil && errors.As(err, &pe) {
			return err // (a disk error: what's recorded is packaged when the download is started again)
		}
	}
	m.mu.Lock()
	gone := m.byID[d.ID] != d
	recorded := d.PartsDone > 0
	m.mu.Unlock()
	if gone || !recorded {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("the live stream sent no pieces to record")
	}
	return m.packRecording(ctx, d)
}

// recTrack is one playlist being recorded.
type recTrack struct {
	url         string
	pl          *hlsMedia // the playlist as last loaded
	path        string
	audio       bool
	next        int64   // the media sequence number of the next piece to keep
	queued      float64 // seconds of pieces taken so far
	parts, done *int    // the download's counters for this track (guarded by m.mu)
	bytes       *int64
}

// recStop is closed when the main track stops recording; secs is how long it got by then.
type recStop struct {
	ch   chan struct{}
	secs float64
}

// liveStart is the media sequence number to start from: the last piece that starts at least back
// seconds before the end of the playlist (or the first).
func liveStart(pl *hlsMedia, back float64) int64 {
	if len(pl.Segments) == 0 {
		return 0
	}
	sum := 0.0
	for i := len(pl.Segments) - 1; i >= 0; i-- {
		sum += pl.Segments[i].Dur
		if sum >= back {
			return pl.Segments[i].Seq
		}
	}
	return pl.Segments[0].Seq
}

// fromEnd is how many seconds of the playlist come from the piece numbered seq on.
func fromEnd(pl *hlsMedia, seq int64) float64 {
	sum := 0.0
	for _, s := range pl.Segments {
		if s.Seq >= seq {
			sum += s.Dur
		}
	}
	return sum
}

// recordTrack records one playlist until the stream ends, stops updating, the length asked for
// is reached (the main track), or ctx ends. The separate audio, once the main track stopped,
// records on up to the same length (for a little while at most) and stops too.
func (m *Manager) recordTrack(ctx context.Context, r *hlsRun, t *recTrack, stop *recStop, stopAfter float64) error {
	d := r.d
	f, err := os.OpenFile(t.path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return permanentErr{err}
	}
	defer f.Close()
	// what's recorded: the pieces are added as the playlist lists them
	rec := &hlsMedia{InitURL: t.pl.InitURL, InitOff: t.pl.InitOff, InitLen: t.pl.InitLen, Target: t.pl.Target}
	rt := r.track(rec)
	r.mu.Lock()
	rt.audio = t.audio
	r.mu.Unlock()
	r.open(rec, f, 0)
	defer r.close(f)
	if rec.InitURL != "" {
		b, err := r.fetchInit(ctx, rec)
		if err != nil {
			return fmt.Errorf("init segment: %w", err)
		}
		if _, err := f.Write(b); err != nil {
			return permanentErr{fmt.Errorf("disk: %w", err)}
		}
		m.mu.Lock()
		*t.bytes += int64(len(b))
		m.mu.Unlock()
	}
	progress := func(n int, written int64) {
		m.mu.Lock()
		*t.done, *t.bytes = n, *t.bytes+written
		m.dirty = true
		m.mu.Unlock()
	}
	limit := stopAfter // the main track's length limit; the audio's is the main track's length once it stopped
	if t.audio {
		limit = 0
	}
	var stopped time.Time
	pl := t.pl
	target := max(pl.Target, 1)
	loaded := time.Now() // (the playlist was loaded just before recording started)
	var idle time.Duration
	lastCheck := time.Now()
	for written := 0; ; {
		added := 0
		r.mu.Lock()
		for _, s := range pl.Segments {
			if s.Seq < t.next {
				continue
			}
			if limit > 0 && t.queued >= limit-0.05 {
				break // as long as asked for
			}
			rec.Segments = append(rec.Segments, s)
			t.next, t.queued = s.Seq+1, t.queued+s.Dur
			added++
		}
		n := len(rec.Segments)
		r.mu.Unlock()
		if added > 0 {
			idle = 0
		}
		if n > written {
			m.mu.Lock()
			*t.parts = n
			m.mu.Unlock()
			if err := r.fetchList(ctx, rec, written, n, f, progress); err != nil {
				return err
			}
			written = n
		}
		switch {
		case pl.Ended, limit > 0 && t.queued >= limit-0.05:
			return nil
		case t.audio && !stopped.IsZero() && time.Since(stopped) > time.Duration(2*target*float64(time.Second)):
			return nil // the main track stopped a while ago: don't wait for more
		}
		// a target duration after a change, half of one when nothing came (RFC 8216 §6.3.4)
		wait := time.Duration(target * float64(time.Second))
		if added == 0 {
			wait /= 2
		}
		tm := time.NewTimer(time.Until(loaded.Add(wait)))
		select {
		case <-ctx.Done():
			tm.Stop()
			return ctx.Err()
		case <-stop.ch:
			tm.Stop()
			if !t.audio {
				return nil
			}
			if stopped.IsZero() { // catch up with the main track, then stop
				stopped, limit = time.Now(), max(stop.secs, 0.1)
				if t.queued >= limit-0.05 {
					return nil
				}
			}
		case <-tm.C:
		}
		if now := time.Now(); added == 0 && online() { // offline, the stream isn't to blame
			idle += now.Sub(lastCheck)
		}
		lastCheck = time.Now()
		if idle >= time.Duration(liveIdle*target*float64(time.Second)) {
			logf("%s: the live stream stopped updating; recording stops", d.FileName)
			return nil
		}
		loaded = time.Now()
		body, final, err := m.fetchText(ctx, d, t.url)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logf("%s: couldn't reload the live playlist (%v)", d.FileName, err)
			continue // tried again in a moment; it counts as no news meanwhile
		}
		_, npl, err := parseM3U8(final, body)
		if err != nil || npl == nil {
			continue
		}
		if npl.Target > 0 {
			target = npl.Target
		}
		if len(npl.Segments) > 0 && npl.Segments[0].Seq > t.next {
			logf("%s: %d pieces left the live playlist before they could be recorded", d.FileName, npl.Segments[0].Seq-t.next)
		}
		pl = npl
	}
}

// packRecording packages what a recording holds into the final file, even after Pause (it's
// how a recording is stopped), unless the download was removed meanwhile.
func (m *Manager) packRecording(ctx context.Context, d *Download) error {
	pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-pctx.Done():
				return
			case <-t.C:
			}
			m.mu.Lock()
			gone := m.byID[d.ID] != d
			m.mu.Unlock()
			if gone {
				cancel()
				return
			}
		}
	}()
	m.mu.Lock()
	d.Status = StDownloading // (Pause stops a recording: it isn't paused while it's packaged)
	packaged, audio, secs, short := d.Packaged, d.ADone > 0, d.WantSecs, d.ShortSecs
	m.dirty = true
	m.mu.Unlock()
	info := &hlsInfo{FMP4: isMP4File(d.partPath()) || packaged && isMP4File(d.partPath()+".mp4tmp")}
	if audio {
		info.Audio = &hlsMedia{}
	} else {
		os.Remove(d.audioPartPath())
	}
	if packaged {
		if _, err := os.Stat(d.partPath() + ".mp4tmp"); err == nil {
			if err := os.Rename(d.partPath()+".mp4tmp", d.partPath()); err != nil {
				return err
			}
		}
		os.Remove(d.audioPartPath())
	} else if err := m.packageHLS(pctx, d, info); err != nil {
		return err
	}
	if st, err := os.Stat(d.partPath()); err == nil {
		m.mu.Lock()
		d.Size = st.Size()
		if d.Note == "" {
			d.Note = "Recorded " + clock(secs) + " of the live stream"
			if tooShort(secs, short) {
				d.Note += " (some pieces were incomplete)"
			}
		}
		m.mu.Unlock()
	}
	return m.finish(pctx, d)
}
