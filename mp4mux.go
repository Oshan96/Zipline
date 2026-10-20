package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

// MP4 writer. It works in two passes over the input: the first collects every frame's size
// and timing, the second copies the frame data into place. The index (moov) goes at the
// front of the file so it plays and seeks straight away ("fast start").

const movieTimescale = 1000

type muxTrack struct {
	video     bool
	timescale uint32
	width     int // display size (video)
	height    int
	entries   [][]byte   // sample entry boxes (avc1, hvc1, mp4a, …)
	aCfgs     []audioCfg // when set, the audio entries are built from these
	samples   []rawSample
	lastDur   int64
	lang      string
	src       int   // which input file it came from
	skip      bool  // left out of the output
	clock33   bool  // timestamps come from the 33-bit 90 kHz MPEG clock (TS, packed audio)
	floating  bool  // no timestamps of its own: starts with the other tracks
	hasEdit   bool  // the input had an edit list (fMP4): presentation starts at editMedia,
	editMedia int64 // after editEmpty of nothing (both in the track's timescale)
	editEmpty int64

	// what it is and how it's labelled (streamout.go)
	name      string
	def       bool     // plays by default (the first audio, a chosen subtitle)
	forced    bool     // subtitles: foreign-language parts only
	text      bool     // subtitles: tx3g in MP4, WebVTT in Matroska
	cues      []subCue // (a text track's cues, placed once the timing is known)
	textData  [][]byte // the samples built from them
	blockDur  []int64  // (Matroska: each cue's duration, ms)
	family    string   // codec family (h264, aac…)
	mkv       *mkvTrack
	mkvScale  int64            // Matroska input: ns per timecode unit
	partShift map[uint16]int64 // per part: where presentation starts (fmp4.go)

	// layout
	dts0      int64 // first decode time before rebasing (track timescale)
	start90   int64 // presentation start on the shared 90 kHz clock
	mediaTime int64
	mediaDur  int64
	presDur   int64
	emptyEdit int64 // movie timescale
	editDur   int64 // movie timescale
	chunks    []mp4Chunk

	// second pass
	ci, si int
	buf    []byte
}

type mp4Chunk struct {
	track int
	first int
	count int
	size  int64
	off   int64
	t90   int64
	entry uint8
}

type mp4Muxer struct {
	tracks   []*muxTrack
	header   []byte                                   // ftyp + moov + mdat header
	duration int64                                    // movie timescale
	text     func(global int64, secs map[int]section) // builds the text tracks once the timing is known
}

// section is one stretch of a stream's timeline: where its first part started on the 90 kHz
// clock, and how far it was moved to follow the one before.
type section struct{ start, off int64 }

// alignEpochs closes the timestamp jumps at stream discontinuities (ad breaks, joined clips,
// the parts of discont.go). Sections are ordered by part, then by the jumps inside a part;
// each starts where the previous one ended, and audio and video keep their relative timing
// inside one. It returns, per part, where it started and how far it moved (for subtitles).
func alignEpochs(tracks []*muxTrack) map[int]section {
	type key struct{ part, sub int }
	// epochs count from wherever each file's reader was: line them up per file and part
	type fp struct{ src, part int }
	minE := map[fp]int{}
	for _, t := range tracks {
		for i := range t.samples {
			k := fp{t.src, int(t.samples[i].part)}
			if e, ok := minE[k]; !ok || int(t.samples[i].epoch) < e {
				minE[k] = int(t.samples[i].epoch)
			}
		}
	}
	keyOf := func(t *muxTrack, s *rawSample) key {
		return key{int(s.part), int(s.epoch) - minE[fp{t.src, int(s.part)}]}
	}
	start, end := map[key]int64{}, map[key]int64{}
	for _, t := range tracks {
		ts := int64(t.timescale)
		n := len(t.samples)
		for i := range t.samples {
			s := &t.samples[i]
			dur := t.lastDur
			if i+1 < n && t.samples[i+1].epoch == s.epoch && t.samples[i+1].part == s.part {
				dur = max(t.samples[i+1].dts-s.dts, 0)
			}
			p := floorDiv((s.dts+int64(s.cts)-t.partShift[s.part])*90000, ts)
			k := keyOf(t, s)
			if v, ok := start[k]; !ok || p < v {
				start[k] = p
			}
			if v, ok := end[k]; !ok || p+dur*90000/ts > v {
				end[k] = p + dur*90000/ts
			}
		}
	}
	keys := make([]key, 0, len(start))
	for k := range start {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].part < keys[j].part || keys[i].part == keys[j].part && keys[i].sub < keys[j].sub
	})
	off := map[key]int64{}
	secs := map[int]section{}
	var prevEnd int64
	for i, k := range keys {
		if i > 0 {
			off[k] = prevEnd - start[k]
		}
		prevEnd = max(prevEnd, end[k]+off[k])
		if i == 0 {
			prevEnd = end[k]
		}
		if _, ok := secs[k.part]; !ok {
			secs[k.part] = section{start: start[k], off: off[k]}
		}
	}
	if len(keys) <= 1 {
		return secs
	}
	for _, t := range tracks {
		ts := int64(t.timescale)
		for i := range t.samples {
			if o := off[keyOf(t, &t.samples[i])]; o != 0 {
				t.samples[i].dts += floorDiv(o*ts+45000, 90000)
			}
		}
	}
	return secs
}

// alignClock33 lines up files whose 33-bit MPEG clocks were unwrapped separately: if one file
// starts on the far side of the ~26.5-hour wrap from another, it's moved by one wrap.
func alignClock33(tracks []*muxTrack) {
	const wrap = int64(1) << 33
	ref, have := int64(0), false
	shift := map[int]int64{}
	for _, t := range tracks {
		if !t.clock33 || t.floating || len(t.samples) == 0 {
			continue
		}
		t0 := floorDiv(t.samples[0].dts*90000, int64(t.timescale))
		if !have {
			ref, have = t0, true
			shift[t.src] = 0
			continue
		}
		if _, ok := shift[t.src]; !ok {
			shift[t.src] = floorDiv(ref-t0+wrap/2, wrap) * wrap
		}
	}
	for _, t := range tracks {
		if k := shift[t.src]; k != 0 && t.clock33 {
			d := floorDiv(k*int64(t.timescale), 90000)
			for i := range t.samples {
				t.samples[i].dts += d
			}
		}
	}
}

// fixTiming makes decode times strictly increasing and works out the track's edit list values.
func (t *muxTrack) fixTiming() {
	s := t.samples
	minCTS := int32(0)
	for i := range s {
		minCTS = min(minCTS, s[i].cts)
	}
	if minCTS < 0 { // negative composition offsets (fMP4 version 1 trun): shift them up
		for i := range s {
			s[i].cts -= minCTS
		}
	}
	for i := 1; i < len(s); i++ {
		if s[i].dts <= s[i-1].dts {
			d := s[i-1].dts + 1 - s[i].dts
			s[i].dts += d
			if t.video {
				s[i].cts = int32(max(int64(s[i].cts)-d, 0))
			}
		}
	}
	n := len(s)
	if t.lastDur <= 0 {
		t.lastDur = int64(t.timescale) / 25
		if n >= 2 && s[n-1].dts > s[n-2].dts {
			t.lastDur = s[n-1].dts - s[n-2].dts
		}
	}
	t.dts0 = s[0].dts
	minPTS, presEnd := int64(math.MaxInt64), int64(math.MinInt64)
	for i := range s {
		s[i].dts -= t.dts0
		dur := t.lastDur
		if i+1 < n {
			dur = s[i+1].dts - s[i].dts - t.dts0 // next one isn't rebased yet
		}
		p := s[i].dts + int64(s[i].cts)
		minPTS = min(minPTS, p)
		presEnd = max(presEnd, p+dur)
	}
	t.mediaDur = s[n-1].dts + t.lastDur
	// where presentation starts, in the input's media time, and how that maps to the shared clock
	start, shift := t.dts0+minPTS, int64(0)
	if t.hasEdit && t.editMedia >= 0 && t.editMedia < t.dts0+presEnd {
		start, shift = max(t.editMedia, start), t.editMedia-t.editEmpty
	}
	t.mediaTime = start - t.dts0
	t.presDur = max(presEnd-t.mediaTime, 1)
	t.start90 = floorDiv((start-shift)*90000, int64(t.timescale))
}

func (t *muxTrack) makeChunks(ti int) {
	ts := int64(t.timescale)
	n := len(t.samples)
	t.chunks = t.chunks[:0]
	for i := 0; i < n; {
		c := mp4Chunk{track: ti, first: i, entry: t.samples[i].entry, t90: floorDiv((t.dts0+t.samples[i].dts)*90000, ts)}
		j := i
		for j < n && t.samples[j].entry == c.entry && (j == i || (t.samples[j].dts-t.samples[i].dts < ts && c.size < 4<<20)) {
			c.size += int64(t.samples[j].size)
			j++
		}
		c.count = j - i
		t.chunks = append(t.chunks, c)
		i = j
	}
}

// audioStats returns the average and peak bitrate and the largest frame, for the esds box.
func (t *muxTrack) audioStats() (avg, peak, maxSize uint32) {
	var total, win int64
	var winStart int64
	ts := int64(t.timescale)
	for i := range t.samples {
		s := &t.samples[i]
		total += int64(s.size)
		maxSize = max(maxSize, s.size)
		for s.dts-winStart >= ts {
			peak = max(peak, uint32(min(win*8, math.MaxUint32)))
			win = 0
			winStart += ts
			if s.dts-winStart >= 10*ts { // skip long gaps quickly
				winStart = s.dts - s.dts%ts
			}
		}
		win += int64(s.size)
	}
	peak = max(peak, uint32(min(win*8, math.MaxUint32)))
	secs := float64(t.mediaDur) / float64(ts)
	if secs > 0 {
		avg = uint32(min(float64(total*8)/secs, math.MaxUint32))
	}
	return avg, max(peak, avg), maxSize
}

// prepare lays out the file and builds the index. align closes timestamp jumps (MPEG-TS input).
func (mx *mp4Muxer) prepare(align bool) error {
	var live []*muxTrack
	for _, t := range mx.tracks {
		if t.skip {
			t.samples = nil
		}
		if len(t.samples) > 0 && !t.text {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		return unsupported("no audio or video was found")
	}
	alignClock33(live)
	var secs map[int]section
	if align {
		secs = alignEpochs(live)
	}
	global, anchored := int64(math.MaxInt64), false
	for _, t := range live {
		t.fixTiming()
		if !t.floating {
			global, anchored = min(global, t.start90), true
		}
	}
	for _, t := range live {
		if t.floating {
			if anchored {
				t.start90 = global
			} else {
				global = min(global, t.start90)
			}
		}
	}
	if mx.text != nil { // subtitles go on the timeline the audio and video ended up with
		mx.text(global, secs)
		for _, t := range mx.tracks {
			if t.text && !t.skip && len(t.samples) > 0 {
				t.fixTiming()
				t.start90 = global
				live = append(live, t)
			}
		}
	}
	for _, t := range live {
		t.emptyEdit = (t.start90 - global) * movieTimescale / 90000
		t.editDur = (t.presDur*movieTimescale + int64(t.timescale)/2) / int64(t.timescale)
		mx.duration = max(mx.duration, t.emptyEdit+t.editDur)
		if len(t.aCfgs) > 0 {
			avg, peak, maxSize := t.audioStats()
			t.entries = t.entries[:0]
			for _, c := range t.aCfgs {
				t.entries = append(t.entries, audioEntry(c, avg, peak, maxSize))
			}
		}
		if len(t.entries) == 0 {
			return errors.New("remux: track without a sample description")
		}
	}

	var all []*mp4Chunk
	var payload int64
	for ti, t := range mx.tracks {
		if len(t.samples) == 0 {
			continue
		}
		t.makeChunks(ti)
		for i := range t.chunks {
			all = append(all, &t.chunks[i])
			payload += t.chunks[i].size
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].t90 != all[j].t90 {
			return all[i].t90 < all[j].t90
		}
		return all[i].track < all[j].track
	})
	co64 := payload > math.MaxUint32-(256<<20)
	mdatHdr := int64(8)
	if payload+8 > math.MaxUint32 {
		mdatHdr = 16
	}
	ftyp := ftypBox()
	moov := mx.moov(co64)
	off := int64(len(ftyp)+len(moov)) + mdatHdr
	for _, c := range all {
		c.off = off
		off += c.size
	}
	moov2 := mx.moov(co64)
	if len(moov2) != len(moov) {
		return errors.New("remux: index size changed")
	}
	w := &boxWriter{b: append(ftyp, moov2...)}
	if mdatHdr == 16 {
		w.u32(1)
		w.raw([]byte("mdat"))
		w.u64(uint64(payload + 16))
	} else {
		w.u32(uint32(payload + 8))
		w.raw([]byte("mdat"))
	}
	mx.header = w.b
	return nil
}

// write stores one frame during the second pass. Frames of a track must come in order.
func (mx *mp4Muxer) write(f io.WriterAt, ti int, data []byte) error {
	if ti >= len(mx.tracks) {
		return errors.New("remux: unknown track")
	}
	t := mx.tracks[ti]
	if t.skip {
		return nil
	}
	if t.si >= len(t.samples) {
		return errors.New("remux: the input changed while converting")
	}
	if int(t.samples[t.si].size) != len(data) {
		return fmt.Errorf("remux: frame %d of track %d changed size", t.si, ti+1)
	}
	t.buf = append(t.buf, data...)
	t.si++
	if c := &t.chunks[t.ci]; t.si == c.first+c.count {
		if _, err := f.WriteAt(t.buf, c.off); err != nil {
			return err
		}
		t.buf = t.buf[:0]
		t.ci++
	}
	return nil
}

func (mx *mp4Muxer) complete() error {
	for _, t := range mx.tracks {
		if t.si != len(t.samples) {
			return errors.New("remux: the input changed while converting")
		}
	}
	return nil
}

func ftypBox() []byte {
	w := &boxWriter{}
	p := w.start("ftyp")
	w.raw([]byte("isom"))
	w.u32(0x200)
	for _, b := range []string{"isom", "iso2", "avc1", "mp41"} {
		w.raw([]byte(b))
	}
	w.end(p)
	return w.b
}

var unityMatrix = []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

func (mx *mp4Muxer) moov(co64 bool) []byte {
	w := &boxWriter{b: make([]byte, 0, 1<<16)}
	mv := w.start("moov")
	p := w.full("mvhd", 0, 0)
	w.u32(0)
	w.u32(0)
	w.u32(movieTimescale)
	w.u32(uint32(min(mx.duration, math.MaxUint32)))
	w.u32(0x00010000)
	w.u16(0x0100)
	w.zeros(10)
	for _, v := range unityMatrix {
		w.u32(v)
	}
	w.zeros(24)
	w.u32(uint32(len(mx.tracks) + 1))
	w.end(p)
	nAudio, firstAudio, defAudio := 0, -1, -1
	for i, t := range mx.tracks {
		if !t.video && !t.text && len(t.samples) > 0 {
			if nAudio == 0 {
				firstAudio = i
			}
			if t.def && defAudio < 0 {
				defAudio = i
			}
			nAudio++
		}
	}
	if defAudio < 0 {
		defAudio = firstAudio
	}
	for i, t := range mx.tracks {
		if len(t.samples) == 0 {
			continue
		}
		alt, enabled := 0, true
		switch {
		case t.text: // subtitles: one group, shown only when chosen
			alt, enabled = 2, t.def
		case !t.video && nAudio > 1:
			alt, enabled = 1, i == defAudio // extra languages: switchable, off by default
		}
		mx.trak(w, i+1, t, co64, alt, enabled)
	}
	w.end(mv)
	return w.b
}

func lang3(s string) uint16 {
	if len(s) != 3 {
		s = "und"
	}
	return uint16(s[0]-0x60)<<10 | uint16(s[1]-0x60)<<5 | uint16(s[2]-0x60)
}

func (mx *mp4Muxer) trak(w *boxWriter, id int, t *muxTrack, co64 bool, alt int, enabled bool) {
	tr := w.start("trak")
	flags := uint32(2)
	if enabled {
		flags = 3
	}
	p := w.full("tkhd", 0, flags)
	w.u32(0)
	w.u32(0)
	w.u32(uint32(id))
	w.u32(0)
	w.u32(uint32(min(t.emptyEdit+t.editDur, math.MaxUint32)))
	w.zeros(8)
	w.u16(0)
	w.u16(uint16(alt))
	if t.video || t.text {
		w.u16(0)
	} else {
		w.u16(0x0100)
	}
	w.u16(0)
	for _, v := range unityMatrix {
		w.u32(v)
	}
	w.u32(uint32(t.width) << 16)
	w.u32(uint32(t.height) << 16)
	w.end(p)

	if t.emptyEdit > 0 || t.mediaTime != 0 {
		ed := w.start("edts")
		v1 := t.mediaTime > math.MaxInt32 || t.editDur > math.MaxUint32 || t.emptyEdit > math.MaxUint32
		ver := byte(0)
		if v1 {
			ver = 1
		}
		el := w.full("elst", ver, 0)
		n := uint32(1)
		if t.emptyEdit > 0 {
			n = 2
		}
		w.u32(n)
		put := func(dur, mt int64) {
			if v1 {
				w.u64(uint64(dur))
				w.u64(uint64(mt))
			} else {
				w.u32(uint32(dur))
				w.u32(uint32(int32(mt)))
			}
			w.u32(0x00010000)
		}
		if t.emptyEdit > 0 {
			put(t.emptyEdit, -1)
		}
		put(t.editDur, t.mediaTime)
		w.end(el)
		w.end(ed)
	}

	md := w.start("mdia")
	v1 := t.mediaDur > math.MaxUint32
	if v1 {
		p = w.full("mdhd", 1, 0)
		w.u64(0)
		w.u64(0)
		w.u32(t.timescale)
		w.u64(uint64(t.mediaDur))
	} else {
		p = w.full("mdhd", 0, 0)
		w.u32(0)
		w.u32(0)
		w.u32(t.timescale)
		w.u32(uint32(t.mediaDur))
	}
	w.u16(lang3(t.lang))
	w.u16(0)
	w.end(p)
	p = w.full("hdlr", 0, 0)
	w.u32(0)
	name := "SoundHandler"
	switch {
	case t.video:
		w.raw([]byte("vide"))
		name = "VideoHandler"
	case t.text:
		w.raw([]byte("sbtl")) // (what ffmpeg and Apple use for tx3g subtitles)
		name = "SubtitleHandler"
	default:
		w.raw([]byte("soun"))
	}
	if t.name != "" { // players show the handler name as the track's title
		name = t.name
	}
	w.zeros(12)
	w.raw(append([]byte(name), 0))
	w.end(p)

	mi := w.start("minf")
	switch {
	case t.video:
		p = w.full("vmhd", 0, 1)
		w.zeros(8)
	case t.text:
		p = w.full("nmhd", 0, 0)
	default:
		p = w.full("smhd", 0, 0)
		w.zeros(4)
	}
	w.end(p)
	di := w.start("dinf")
	dr := w.full("dref", 0, 0)
	w.u32(1)
	u := w.full("url ", 0, 1)
	w.end(u)
	w.end(dr)
	w.end(di)
	mx.stbl(w, t, co64)
	w.end(mi)
	w.end(md)
	if t.name != "" { // the track's title, as ffmpeg writes it (udta/name)
		ud := w.start("udta")
		n := w.start("name")
		w.raw([]byte(t.name))
		w.end(n)
		w.end(ud)
	}
	w.end(tr)
}

func (mx *mp4Muxer) stbl(w *boxWriter, t *muxTrack, co64 bool) {
	st := w.start("stbl")
	s := t.samples
	n := len(s)

	p := w.full("stsd", 0, 0)
	w.u32(uint32(len(t.entries)))
	for _, e := range t.entries {
		w.raw(e)
	}
	w.end(p)

	// stts: decode time deltas, run-length coded
	p = w.full("stts", 0, 0)
	cnt := w.len()
	w.u32(0)
	runs := uint32(0)
	var runN uint32
	var runD int64 = -1
	for i := 0; i < n; i++ {
		d := t.lastDur
		if i+1 < n {
			d = s[i+1].dts - s[i].dts
		}
		if d != runD {
			if runN > 0 {
				w.u32(runN)
				w.u32(uint32(runD))
				runs++
			}
			runN, runD = 0, d
		}
		runN++
	}
	w.u32(runN)
	w.u32(uint32(runD))
	runs++
	w.put32(cnt, runs)
	w.end(p)

	// ctts: composition offsets (B-frames)
	hasCTS := false
	for i := range s {
		if s[i].cts != 0 {
			hasCTS = true
			break
		}
	}
	if hasCTS {
		p = w.full("ctts", 0, 0)
		cnt = w.len()
		w.u32(0)
		runs, runN = 0, 0
		runC := int32(-1)
		for i := range s {
			if s[i].cts != runC {
				if runN > 0 {
					w.u32(runN)
					w.u32(uint32(runC))
					runs++
				}
				runN, runC = 0, s[i].cts
			}
			runN++
		}
		w.u32(runN)
		w.u32(uint32(runC))
		runs++
		w.put32(cnt, runs)
		w.end(p)
	}

	// stss: keyframes (video only; omitted when every frame is one)
	if t.video {
		mask := uint8(sfISlice)
		for i := range s {
			if s[i].flags&sfSync != 0 {
				mask = sfSync
				break
			}
		}
		var keys []uint32
		for i := range s {
			if s[i].flags&mask != 0 || i == 0 {
				keys = append(keys, uint32(i+1))
			}
		}
		if len(keys) < n {
			p = w.full("stss", 0, 0)
			w.u32(uint32(len(keys)))
			for _, k := range keys {
				w.u32(k)
			}
			w.end(p)
		}
	}

	// stsc: samples per chunk and which sample entry they use
	p = w.full("stsc", 0, 0)
	cnt = w.len()
	w.u32(0)
	runs = 0
	lastCount, lastEntry := -1, -1
	for i, c := range t.chunks {
		if c.count != lastCount || int(c.entry) != lastEntry {
			w.u32(uint32(i + 1))
			w.u32(uint32(c.count))
			w.u32(uint32(c.entry) + 1)
			runs++
			lastCount, lastEntry = c.count, int(c.entry)
		}
	}
	w.put32(cnt, runs)
	w.end(p)

	// stsz: frame sizes
	p = w.full("stsz", 0, 0)
	same := true
	for i := 1; i < n; i++ {
		if s[i].size != s[0].size {
			same = false
			break
		}
	}
	if same {
		w.u32(s[0].size)
		w.u32(uint32(n))
	} else {
		w.u32(0)
		w.u32(uint32(n))
		for i := range s {
			w.u32(s[i].size)
		}
	}
	w.end(p)

	// stco / co64: where each chunk starts
	if co64 {
		p = w.full("co64", 0, 0)
		w.u32(uint32(len(t.chunks)))
		for _, c := range t.chunks {
			w.u64(uint64(c.off))
		}
	} else {
		p = w.full("stco", 0, 0)
		w.u32(uint32(len(t.chunks)))
		for _, c := range t.chunks {
			w.u32(uint32(c.off))
		}
	}
	w.end(p)
	w.end(st)
}

// ---------------------------------------------------------------- sample entries

func visualEntry(typ string, width, height int, cfgType string, cfg []byte, sarW, sarH int) []byte {
	w := &boxWriter{}
	p := w.start(typ)
	w.zeros(6)
	w.u16(1) // data_reference_index
	w.zeros(16)
	w.u16(uint16(width))
	w.u16(uint16(height))
	w.u32(0x00480000)
	w.u32(0x00480000)
	w.u32(0)
	w.u16(1)
	w.zeros(32)
	w.u16(0x0018)
	w.u16(0xFFFF)
	c := w.start(cfgType)
	w.raw(cfg)
	w.end(c)
	if sarW > 0 && sarH > 0 && sarW != sarH {
		c = w.start("pasp")
		w.u32(uint32(sarW))
		w.u32(uint32(sarH))
		w.end(c)
	}
	w.end(p)
	return w.b
}

func sortedIDs(m map[int][]byte) []int {
	ids := make([]int, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// videoEntry builds an avc1 or hvc1 sample entry; it returns the entry and the display size.
func videoEntry(codec string, p *videoParams) (entry []byte, dispW, dispH int, err error) {
	sps := sortedIDs(p.sps)
	pps := sortedIDs(p.pps)
	if len(sps) == 0 || len(pps) == 0 {
		return nil, 0, 0, errors.New("remux: missing video parameter sets")
	}
	c := &boxWriter{}
	if codec == "h264" {
		first := p.sps[sps[0]]
		s, err := parseAVCSPS(first)
		if err != nil {
			return nil, 0, 0, err
		}
		if len(sps) > 31 {
			sps = sps[:31]
		}
		c.u8(1)
		c.u8(first[1])
		c.u8(first[2])
		c.u8(first[3])
		c.u8(0xFF)
		c.u8(0xE0 | byte(len(sps)))
		for _, id := range sps {
			c.u16(uint16(len(p.sps[id])))
			c.raw(p.sps[id])
		}
		c.u8(byte(min(len(pps), 255)))
		for _, id := range pps[:min(len(pps), 255)] {
			c.u16(uint16(len(p.pps[id])))
			c.raw(p.pps[id])
		}
		switch s.profile {
		case 100, 110, 122, 144:
			c.u8(0xFC | byte(s.chroma&3))
			c.u8(0xF8 | byte((s.depthL-8)&7))
			c.u8(0xF8 | byte((s.depthC-8)&7))
			c.u8(0)
		}
		dw := s.width
		if s.sarW != s.sarH {
			dw = s.width * s.sarW / s.sarH
		}
		return visualEntry("avc1", s.width, s.height, "avcC", c.b, s.sarW, s.sarH), dw, s.height, nil
	}
	s, err := parseHEVCSPS(p.sps[sps[0]])
	if err != nil {
		return nil, 0, 0, err
	}
	c.u8(1)
	c.raw(s.ptl[:])
	c.u16(0xF000) // min_spatial_segmentation_idc = 0
	c.u8(0xFC)    // parallelismType = 0
	c.u8(0xFC | byte(s.chroma&3))
	c.u8(0xF8 | byte((s.depthL-8)&7))
	c.u8(0xF8 | byte((s.depthC-8)&7))
	c.u16(0) // avgFrameRate
	nested := byte(0)
	if s.nesting {
		nested = 1
	}
	c.u8(byte(s.subLayers&7)<<3 | nested<<2 | 3)
	c.u8(3)
	for _, arr := range []struct {
		typ byte
		m   map[int][]byte
	}{{32, p.vps}, {33, p.sps}, {34, p.pps}} {
		ids := sortedIDs(arr.m)
		c.u8(0x80 | arr.typ)
		c.u16(uint16(len(ids)))
		for _, id := range ids {
			c.u16(uint16(len(arr.m[id])))
			c.raw(arr.m[id])
		}
	}
	return visualEntry("hvc1", s.width, s.height, "hvcC", c.b, 0, 0), s.width, s.height, nil
}

func esdsBox(oti byte, dsi []byte, avg, peak, bufSize uint32) []byte {
	w := &boxWriter{}
	p := w.full("esds", 0, 0)
	dcd := 13
	if len(dsi) > 0 {
		dcd += 2 + len(dsi)
	}
	w.u8(3)
	w.u8(byte(3 + 2 + dcd + 3))
	w.u16(0)
	w.u8(0)
	w.u8(4)
	w.u8(byte(dcd))
	w.u8(oti)
	w.u8(0x15) // audio stream
	w.u24(bufSize)
	w.u32(peak)
	w.u32(avg)
	if len(dsi) > 0 {
		w.u8(5)
		w.u8(byte(len(dsi)))
		w.raw(dsi)
	}
	w.u8(6)
	w.u8(1)
	w.u8(2)
	w.end(p)
	return w.b
}

func audioEntry(c audioCfg, avg, peak, bufSize uint32) []byte {
	typ := "mp4a"
	switch c.codec {
	case "ac3":
		typ = "ac-3"
	case "eac3":
		typ = "ec-3"
	}
	w := &boxWriter{}
	p := w.start(typ)
	w.zeros(6)
	w.u16(1)
	w.zeros(8)
	ch := c.channels
	if c.codec == "ac3" || c.codec == "eac3" {
		ch = 2 // the real layout is in dac3/dec3
	}
	w.u16(uint16(ch))
	w.u16(16)
	w.u32(0)
	w.u32(uint32(min(c.rate, 65535)) << 16)
	switch c.codec {
	case "aac":
		w.raw(esdsBox(0x40, c.asc, avg, peak, bufSize))
	case "mp3":
		w.raw(esdsBox(c.mpegOTI, nil, avg, peak, bufSize))
	case "ac3":
		b := w.start("dac3")
		w.raw(c.ac3[:])
		w.end(b)
	case "eac3":
		b := w.start("dec3")
		rate := uint32(c.frameSize * 8 * c.rate / 1536 / 1000)
		w.u16(uint16(rate&0x1FFF) << 3) // data_rate, num_ind_sub = 0 (one substream)
		v := uint32(c.fscod)<<22 | uint32(c.bsid)<<17 | uint32(c.bsmod)<<12 | uint32(c.acmod)<<9 | uint32(c.lfe)<<8
		w.u24(v)
		w.end(b)
	}
	w.end(p)
	return w.b
}
