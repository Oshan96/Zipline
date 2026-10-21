package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Parts of a stream. An HLS playlist can change its timestamps (EXT-X-DISCONTINUITY) or its init
// section (a new EXT-X-MAP) part-way, usually around ad breaks; a DASH manifest can have several
// periods. Each run of pieces sharing one timeline and one init section is a part. The pieces
// are still downloaded into one file per track, but where each part starts is remembered
// (Download.Marks), and the readers start a new timeline section there (RFC 8216 §6.3.3: reset
// the decoders), so the parts are joined end to end with continuous timestamps. A part whose
// init section differs gets that init written in front of it.

// partMarks are a stream's marks per file, by partsKey.
type partMarks map[string][]PartMark

// PartMark says that a track's file holds part Part from byte Off on.
type PartMark struct {
	Off  int64 `json:"o"`
	Part int   `json:"p"`
}

// adBreak is a marked ad break, for the Add dialog (seconds into the stream).
type adBreak struct {
	Start float64 `json:"start"`
	Dur   float64 `json:"dur"`
}

func sameMap(a, b *hlsMap) bool {
	return a == b || a != nil && b != nil && *a == *b
}

// numberParts gives each piece of a playlist its part: a new one starts at a discontinuity and
// at a new init section.
func numberParts(md *hlsMedia) {
	part := 0
	for i := range md.Segments {
		s := &md.Segments[i]
		if i > 0 && (s.Disc || !sameMap(s.Map, md.Segments[i-1].Map)) {
			part++
		}
		s.Part = part
	}
}

// ---------------------------------------------------------------- HLS ad breaks

// hlsAds follows the ad markers of a media playlist: #EXT-X-CUE-OUT … #EXT-X-CUE-IN, and
// #EXT-X-DATERANGE entries with SCTE-35 splice data (placed by #EXT-X-PROGRAM-DATE-TIME).
type hlsAds struct {
	inCue   bool
	cueLeft float64 // seconds left of a CUE-OUT that gave its length
	pdt     time.Time
	segPDT  []time.Time
	ranges  []adRange
}

// adClassRE matches DATERANGE classes that name an ad break ("…scte35…", "ad", "com.example.ads").
var adClassRE = regexp.MustCompile(`(?i)scte-?35|advert|(^|[.:/_-])ads?($|[.:/_-])`)

type adRange struct {
	id         string
	start, end time.Time
}

func (a *hlsAds) tag(tag, val string) {
	switch tag {
	case "#EXT-X-CUE-OUT":
		a.inCue, a.cueLeft = true, 0
		v := val
		if at := attrs(val); at["DURATION"] != "" {
			v = at["DURATION"]
		}
		if d, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && d > 0 {
			a.cueLeft = d
		}
	case "#EXT-X-CUE-OUT-CONT": // joined part-way through a break
		a.inCue = true
	case "#EXT-X-CUE-IN":
		a.inCue, a.cueLeft = false, 0
	case "#EXT-X-PROGRAM-DATE-TIME":
		if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(val)); err == nil {
			a.pdt = t
		} else if t, err := time.Parse("2006-01-02T15:04:05.999999999Z0700", strings.TrimSpace(val)); err == nil {
			a.pdt = t
		}
	case "#EXT-X-DATERANGE":
		at := attrs(val)
		if strings.Contains(strings.ToLower(at["CLASS"]), "interstitial") {
			return // the player inserts those itself; the programme goes on underneath
		}
		start, err := time.Parse(time.RFC3339Nano, at["START-DATE"])
		if at["SCTE35-IN"] != "" && at["SCTE35-OUT"] == "" { // the end of an earlier break
			for i := range a.ranges {
				if a.ranges[i].id == at["ID"] && err == nil {
					a.ranges[i].end = start
				}
			}
			return
		}
		if (at["SCTE35-OUT"] == "" && at["SCTE35-CMD"] == "" && !adClassRE.MatchString(at["CLASS"])) || err != nil {
			return
		}
		r := adRange{id: at["ID"], start: start}
		if e, err := time.Parse(time.RFC3339Nano, at["END-DATE"]); err == nil {
			r.end = e
		} else {
			for _, k := range []string{"DURATION", "PLANNED-DURATION"} {
				if d, err := strconv.ParseFloat(at[k], 64); err == nil && d > 0 {
					r.end = start.Add(time.Duration(d * float64(time.Second)))
					break
				}
			}
		}
		a.ranges = append(a.ranges, r)
	}
}

// segment is told about each piece in order; it reports whether the piece is inside a CUE break.
func (a *hlsAds) segment(dur float64) bool {
	a.segPDT = append(a.segPDT, a.pdt)
	if !a.pdt.IsZero() {
		a.pdt = a.pdt.Add(time.Duration(dur * float64(time.Second)))
	}
	ad := a.inCue
	if a.inCue && a.cueLeft > 0 {
		if a.cueLeft -= dur; a.cueLeft <= 0.001 {
			a.inCue = false
		}
	}
	return ad
}

// finish marks the pieces that fall inside a DATERANGE break.
func (a *hlsAds) finish(md *hlsMedia) {
	for _, r := range a.ranges {
		if r.end.IsZero() || !r.end.After(r.start) {
			continue
		}
		for i := range md.Segments {
			if i < len(a.segPDT) && !a.segPDT[i].IsZero() {
				mid := a.segPDT[i].Add(time.Duration(md.Segments[i].Dur * float64(time.Second) / 2))
				if !mid.Before(r.start) && mid.Before(r.end) {
					md.Segments[i].Ad = true
				}
			}
		}
	}
}

// ---------------------------------------------------------------- settling the parts

// segTimes returns each piece's start in the playlist (seconds), and the total.
func segTimes(md *hlsMedia) ([]float64, float64) {
	out := make([]float64, len(md.Segments))
	var t float64
	for i, s := range md.Segments {
		out[i] = t
		t += s.Dur
	}
	return out, t
}

// adBreaks lists the main track's marked breaks.
func adBreaks(md *hlsMedia) []adBreak {
	var out []adBreak
	ts, _ := segTimes(md)
	for i, s := range md.Segments {
		if !s.Ad {
			continue
		}
		if i > 0 && md.Segments[i-1].Ad && len(out) > 0 {
			out[len(out)-1].Dur += s.Dur
			continue
		}
		out = append(out, adBreak{Start: ts[i], Dur: s.Dur})
	}
	return out
}

func allAds(md *hlsMedia) bool {
	for _, s := range md.Segments {
		if !s.Ad {
			return false
		}
	}
	return true
}

// dropAds leaves out the pieces of ad breaks. The piece after a break starts a new part, so its
// timestamps are joined to what came before.
func dropAds(md *hlsMedia) {
	out := md.Segments[:0]
	cut := false
	md.Duration = 0
	for _, s := range md.Segments {
		if s.Ad {
			cut = true
			continue
		}
		if cut && len(out) > 0 {
			s.Disc = true
		}
		cut = false
		md.Duration += s.Dur
		out = append(out, s)
	}
	md.Segments = out
	if len(out) > 0 && out[0].Map != nil { // the first piece left may need another init section
		md.InitURL, md.InitOff, md.InitLen = out[0].Map.URL, out[0].Map.Off, out[0].Map.Len
	}
}

// settleParts runs once every track's playlist is known: it lists the ad breaks, leaves them
// out when asked (DASH: whole periods), and (HLS) numbers the other tracks' parts after the main
// track's: each run between a track's own discontinuities takes the part playing at its middle
// (a track's pieces don't line up exactly with the video's, and only the video may mark breaks).
func settleParts(info *hlsInfo, noAds bool) {
	main := info.Media
	if main == nil {
		return
	}
	others := []*hlsMedia{info.Audio}
	for _, x := range append(append([]*extraTrack{}, info.Extras...), info.SubMedia...) {
		others = append(others, x.media)
	}
	info.Ads = adBreaks(main)
	if allAds(main) { // the whole stream marked as a break: the marks can't be right
		noAds = false
	}
	if info.Format == "DASH" { // pieces carry their period's part and ad flag already
		if noAds && len(info.Ads) > 0 {
			for _, md := range append(others, main) {
				if md != nil {
					dropAds(md)
				}
			}
		}
		return
	}
	starts, total := segTimes(main)
	ranges := make([][2]float64, 0, len(info.Ads))
	for _, a := range info.Ads {
		ranges = append(ranges, [2]float64{a.Start, a.Start + a.Dur})
	}
	for _, md := range others {
		if md == nil {
			continue
		}
		ts, _ := segTimes(md)
		for i := range md.Segments {
			mid := ts[i] + md.Segments[i].Dur/2
			for _, r := range ranges {
				if mid >= r[0] && mid < r[1] {
					md.Segments[i].Ad = true
				}
			}
		}
	}
	if noAds && len(info.Ads) > 0 {
		for _, md := range append(others, main) {
			if md != nil {
				dropAds(md)
			}
		}
		numberParts(main)
		starts, total = segTimes(main)
	}
	subs := map[*hlsMedia]bool{}
	for _, x := range info.SubMedia {
		subs[x.media] = true
	}
	for _, md := range others { // each of its own runs takes the main track's part at its middle
		if md == nil {
			continue
		}
		numberParts(md)
		if subs[md] && len(md.Segments) > 0 && md.Segments[len(md.Segments)-1].Part == 0 { // subtitles that don't mark the breaks: piece by piece
			ts, _ := segTimes(md)
			for i := range md.Segments {
				md.Segments[i].Part = partAtTime(main, starts, total, ts[i]+md.Segments[i].Dur/2)
			}
			continue
		}
		ts, end := segTimes(md)
		for i := 0; i < len(md.Segments); {
			j := i
			for j < len(md.Segments) && md.Segments[j].Part == md.Segments[i].Part {
				j++
			}
			stop := end
			if j < len(md.Segments) {
				stop = ts[j]
			}
			p := partAtTime(main, starts, total, (ts[i]+stop)/2)
			for k := i; k < j; k++ {
				md.Segments[k].Part = p
			}
			i = j
		}
	}
	info.Duration = main.Duration
	info.Segments = len(main.Segments)
	if info.Audio != nil {
		info.Segments += len(info.Audio.Segments)
	}
	for _, x := range info.Extras {
		info.Segments += len(x.media.Segments)
	}
}

// partAtTime is the main track's part playing at t seconds.
func partAtTime(main *hlsMedia, starts []float64, total, t float64) int {
	n := len(main.Segments)
	if n == 0 {
		return 0
	}
	lo, hi := 0, n-1
	for lo < hi { // the last piece starting at or before t
		mid := (lo + hi + 1) / 2
		if starts[mid] <= t {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return main.Segments[lo].Part
}

// ---------------------------------------------------------------- fetching by part

type fetchPart struct {
	from, to, part int
	mp             *hlsMap
}

func splitFetchParts(md *hlsMedia) []fetchPart {
	var out []fetchPart
	for i, s := range md.Segments {
		if i == 0 || s.Part != md.Segments[i-1].Part {
			out = append(out, fetchPart{from: i, part: s.Part, mp: s.Map})
		}
		out[len(out)-1].to = i + 1
	}
	return out
}

// partsKey names a track's marks: "" for the main file, ".audio", ".x1"…
func partsKey(d *Download, path string) string { return strings.TrimPrefix(path, d.partPath()) }

// fetchParts downloads a track's pieces part by part (fetchSegments does the work), writing a
// part's own init section in front of it when it differs from the one before, and noting in
// d.Marks where each part starts in the file. have is the size the file was cut back to.
func (m *Manager) fetchParts(ctx context.Context, d *Download, media *hlsMedia, path string, start int, have int64, f *os.File, progress func(done int, written int64)) error {
	parts := splitFetchParts(media)
	key := partsKey(d, path)
	m.mu.Lock()
	marks := d.Marks[key]
	for len(marks) > 0 && marks[len(marks)-1].Off >= have && have >= 0 {
		marks = marks[:len(marks)-1] // their part's first piece isn't in the file any more
	}
	if len(parts) <= 1 && (len(parts) == 0 || parts[0].part == 0) {
		marks = nil
	}
	if d.Marks == nil && marks != nil {
		d.Marks = partMarks{}
	}
	if marks == nil {
		delete(d.Marks, key)
	} else {
		d.Marks[key] = marks
	}
	m.mu.Unlock()
	if len(parts) <= 1 && (len(parts) == 0 || parts[0].part == 0) {
		return m.fetchSegments(ctx, d, media, start, f, progress)
	}
	initFor := map[hlsMap][]byte{}
	for pi, p := range parts {
		if start >= p.to {
			continue
		}
		var prefix []byte
		first := start <= p.from
		if first && pi > 0 && p.mp != nil && !sameMap(p.mp, parts[pi-1].mp) {
			b, ok := initFor[*p.mp]
			if !ok {
				var err error
				if b, err = m.fetchSegment(ctx, d, hlsSegment{URL: p.mp.URL, RangeOff: p.mp.Off, RangeLen: p.mp.Len}, nil); err != nil {
					return fmt.Errorf("init segment: %w", err)
				}
				initFor[*p.mp] = b
			}
			prefix = b
		}
		off, err := f.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		if len(prefix) > 0 {
			if _, err := f.Write(prefix); err != nil {
				return permanentErr{fmt.Errorf("disk: %w", err)}
			}
		}
		part := p
		err = d.run(m).fetchList(ctx, media, max(start, p.from), p.to, f, func(n int, written int64) {
			if first && n == part.from+1 { // the part's first piece is in: it starts at off (the init counts with it)
				m.mu.Lock()
				if d.Marks == nil {
					d.Marks = partMarks{}
				}
				d.Marks[key] = append(d.Marks[key], PartMark{Off: off, Part: part.part})
				m.dirty = true
				m.mu.Unlock()
				written += int64(len(prefix))
			}
			progress(n, written)
		})
		if err != nil {
			return err
		}
		start = p.to
	}
	return nil
}

// partCursor walks a file's marks while a reader moves through it.
type partCursor struct {
	marks []PartMark
	i     int
	part  int
}

func newPartCursor(marks []PartMark) *partCursor {
	c := &partCursor{marks: marks}
	if len(marks) > 0 && marks[0].Off <= 0 {
		c.part, c.i = marks[0].Part, 1
	}
	return c
}

// at reports the part at byte off, and whether a new one started since the last call.
func (c *partCursor) at(off int64) (int, bool) {
	changed := false
	for c != nil && c.i < len(c.marks) && off >= c.marks[c.i].Off {
		c.part, changed = c.marks[c.i].Part, true
		c.i++
	}
	if c == nil {
		return 0, false
	}
	return c.part, changed
}
