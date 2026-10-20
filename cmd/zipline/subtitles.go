package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Subtitles. They are small, so a track's pieces are fetched into memory just before the file
// is put together (and again if that's interrupted), never into a part file. The cues are placed
// on the timeline the audio and video end up with, then saved beside the video as SubRip (.srt:
// every player and TV reads it, and finds "Movie.en.srt" next to "Movie.mp4" by itself) and
// put inside it: tx3g text in MP4 (what ffmpeg-based players and QuickTime read; WebVTT in MP4
// isn't), WebVTT in Matroska and WebM.

// subTrack is a subtitle track on its way into the file.
type subTrack struct {
	track *streamTrack
	cues  []subCue
}

// cue timing: on the media's 90 kHz clock (HLS with X-TIMESTAMP-MAP, WebVTT in MP4 pieces), in
// ms from the start of its part (a DASH period's WebVTT file), or in ms from the start of the
// programme (HLS without a map)
const (
	cueClock = iota
	cuePart
	cueProg
)

type subCue struct {
	start, end int64
	text       string
	part       int
	mode       uint8
}

// fetchSubtitles downloads and reads the chosen subtitle tracks. A track that fails is left out:
// subtitles never stop a video from being saved.
func (m *Manager) fetchSubtitles(ctx context.Context, d *Download, info *hlsInfo) ([]*subTrack, error) {
	var out []*subTrack
	var firstErr error
	for _, x := range info.SubMedia {
		segs, err := m.fetchSmall(ctx, d, x.media)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("subtitles %s: %w", trackLabel(x.track), err)
			}
			continue
		}
		st := &subTrack{track: x.track}
		if x.track.text != nil && x.track.text.wvtt || x.media.InitURL != "" { // WebVTT in MP4 pieces
			st.cues = wvttTrackCues(segs, x.media)
		} else {
			st.cues = vttTrackCues(segs, x.media, info, d.NoAds)
		}
		out = append(out, st)
	}
	if firstErr == nil {
		firstErr = info.SubErr
	}
	return out, firstErr
}

// fetchSmall fetches a short track's pieces into memory, a few at a time.
func (m *Manager) fetchSmall(ctx context.Context, d *Download, md *hlsMedia) ([][]byte, error) {
	out := make([][]byte, len(md.Segments))
	var next int64 = -1
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	keys := &keyCache{m: map[string][]byte{}}
	if md.InitURL != "" { // WebVTT in MP4 pieces: the init piece first
		b, err := m.fetchSegment(ctx, d, hlsSegment{URL: md.InitURL, RangeOff: md.InitOff, RangeLen: md.InitLen}, nil)
		if err != nil {
			return nil, err
		}
		atomic.AddInt64(&d.done, -int64(len(b)))
		out = append([][]byte{b}, out...)
	}
	base := len(out) - len(md.Segments)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(atomic.AddInt64(&next, 1))
				if i >= len(md.Segments) || ctx.Err() != nil {
					return
				}
				var b []byte
				var err error
				for try := 0; try < 4; try++ {
					if b, err = m.fetchSegment(ctx, d, md.Segments[i], keys); err == nil || ctx.Err() != nil {
						break
					}
					if !sleepCtx(ctx, time.Duration(500<<try)*time.Millisecond) {
						break
					}
				}
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				out[base+i] = b
				mu.Unlock()
				atomic.AddInt64(&d.done, -int64(len(b))) // not part of the download's size
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, firstErr
}

// vttTrackCues reads an HLS (or segmented DASH) WebVTT track's pieces.
func vttTrackCues(segs [][]byte, md *hlsMedia, info *hlsInfo, noAds bool) []subCue {
	var out []subCue
	for i := 0; i < len(segs) && i < len(md.Segments); i++ {
		b := segs[i]
		for i+1 < len(segs) && i+1 < len(md.Segments) && md.Segments[i+1].URL == md.Segments[i].URL && md.Segments[i+1].RangeLen > 0 {
			b = append(append([]byte(nil), b...), segs[i+1]...) // one file fetched in ranges
			i++
		}
		if len(b) == 0 {
			continue
		}
		f := parseVTT(b)
		for _, c := range f.cues {
			sc := subCue{text: c.text, part: md.Segments[i].Part, mode: cueProg, start: c.start, end: c.end}
			if f.mapTS >= 0 {
				sc.mode = cueClock
				sc.start = f.mapTS + (c.start-f.mapLocal)*90
				sc.end = f.mapTS + (c.end-f.mapLocal)*90
			} else if info != nil && info.Format == "DASH" {
				sc.mode = cuePart
			} else if noAds && info != nil { // times count from the playlist's start, breaks included
				var cut int64
				skip := false
				for _, a := range info.Ads {
					st, en := int64(a.Start*1000), int64((a.Start+a.Dur)*1000)
					if c.start >= st && c.start < en {
						skip = true
					}
					if st < c.start {
						cut += min(en, c.start) - st
					}
				}
				if skip {
					continue
				}
				sc.start, sc.end = c.start-cut, c.end-cut
			}
			out = append(out, sc)
		}
	}
	return out
}

// wvttTrackCues reads WebVTT carried in MP4 pieces: the first piece is the init section.
func wvttTrackCues(segs [][]byte, md *hlsMedia) []subCue {
	if len(segs) == 0 {
		return nil
	}
	f, err := os.CreateTemp("", "zl-wvtt-*.mp4")
	if err != nil {
		return nil
	}
	defer os.Remove(f.Name())
	var marks []PartMark
	var off int64
	base := len(segs) - len(md.Segments) // (the init piece comes first)
	for i, b := range segs {
		if k := i - base; k >= 0 && (len(marks) == 0 || marks[len(marks)-1].Part != md.Segments[k].Part) {
			marks = append(marks, PartMark{Off: off, Part: md.Segments[k].Part})
		}
		f.Write(b)
		off += int64(len(b))
	}
	f.Close()
	tracks, err := readFMP4Text(f.Name(), marks)
	if err != nil {
		return nil
	}
	return tracks
}

// placeCues puts a track's cues on the output timeline (ms from its start). global is where the
// output starts on the 90 kHz clock; secs says where each part started and how far it moved.
func placeCues(cues []subCue, global int64, secs map[int]section) []placedCue {
	const wrap = int64(1) << 33
	var out []placedCue
	for _, c := range cues {
		var st, en int64
		switch c.mode {
		case cueClock:
			sec, ok := secs[c.part]
			if !ok {
				if len(secs) > 0 {
					continue // its part isn't in the file (an ad break left out)
				}
				sec = section{start: global}
			}
			v := c.start + floorDiv(sec.start-c.start+wrap/2, wrap)*wrap // the 33-bit clock wraps every 26.5 hours
			st = floorDiv(v+sec.off-global, 90)
			en = st + (c.end-c.start)/90
		case cuePart:
			sec, ok := secs[c.part]
			if !ok {
				if len(secs) > 0 {
					continue
				}
				sec = section{start: global}
			}
			p0 := floorDiv(sec.start+sec.off-global, 90)
			st, en = p0+c.start, p0+c.end
		default:
			st, en = c.start, c.end
		}
		if en <= 0 || en <= st {
			continue
		}
		out = append(out, placedCue{start: max(st, 0), end: en, text: c.text})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	// a cue that runs across pieces is repeated in each: join the copies
	merged := out[:0]
	for _, c := range out {
		dup := false
		for k := len(merged) - 1; k >= 0 && k >= len(merged)-8; k-- {
			p := &merged[k]
			if p.text == c.text && c.start <= p.end+1 && c.end >= p.start {
				p.end = max(p.end, c.end)
				dup = true
				break
			}
		}
		if !dup {
			merged = append(merged, c)
		}
	}
	return merged
}

// sidecarName names a subtitle file beside the video: "Movie.en.srt", "Movie.en.forced.srt",
// "Movie.en.sdh.srt" (the names Plex, Jellyfin, Kodi and VLC look for).
func sidecarName(video string, t *streamTrack) string {
	base := strings.TrimSuffix(video, filepath.Ext(video))
	lang := strings.ToLower(t.Lang)
	if lang == "" {
		lang = "und"
	}
	n := base + "." + sanitizeName(lang)
	if t.Forced {
		n += ".forced"
	} else if t.SDH {
		n += ".sdh"
	}
	return n + ".srt"
}

// writeSidecars saves each subtitle track beside the video as .srt. The names follow the video's
// own (unique) name, so a file already there is from an earlier try and is replaced; a second
// track with the same language gets its name added ("Movie.en.Commentary.srt").
func (m *Manager) writeSidecars(d *Download, video string, subs []*subTrack, placed [][]placedCue) ([]string, error) {
	var names []string
	used := map[string]bool{}
	for i, s := range subs {
		if i >= len(placed) || len(placed[i]) == 0 {
			continue
		}
		name := sidecarName(video, s.track)
		for n := 2; used[strings.ToLower(name)]; n++ {
			extra := sanitizeName(s.track.Name)
			if extra == "" || n > 2 {
				extra = fmt.Sprint(n)
			}
			name = strings.TrimSuffix(sidecarName(video, s.track), ".srt") + "." + extra + ".srt"
		}
		used[strings.ToLower(name)] = true
		if err := os.WriteFile(filepath.Join(d.Dir, name), writeSRT(placed[i]), 0o644); err != nil {
			return names, err
		}
		names = append(names, name)
	}
	return names, nil
}
