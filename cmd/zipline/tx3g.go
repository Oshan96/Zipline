package main

import (
	"encoding/binary"
	"sort"
	"strings"
)

// 3GPP timed text (tx3g, TS 26.245): the subtitle track MP4 files carry that ffmpeg-based players,
// QuickTime and phones all read (WebVTT in MP4 isn't). Each sample is a 16-bit length and UTF-8
// text without styling; the samples cover the whole track, empty ones filling the gaps.

// tx3gEntry is the sample description of a 3GPP timed text track (TS 26.245): centred at the
// bottom, white serif text, as ffmpeg writes it.
func tx3gEntry() []byte {
	w := &boxWriter{}
	p := w.start("tx3g")
	w.zeros(6)
	w.u16(1)          // data_reference_index
	w.u32(0)          // display flags
	w.u8(1)           // horizontal: centre
	w.u8(0xFF)        // vertical: bottom
	w.u32(0)          // background (transparent)
	w.zeros(8)        // default text box
	w.u16(0)          // style record: start and end character,
	w.u16(0)          //
	w.u16(1)          // font 1,
	w.u8(0)           // plain,
	w.u8(0x12)        // 18 pt,
	w.u32(^uint32(0)) // white
	f := w.start("ftab")
	w.u16(1)
	w.u16(1)
	w.u8(5)
	w.raw([]byte("Serif"))
	w.end(f)
	w.end(p)
	return w.b
}

// textTimeline cuts the cues' times into back-to-back pieces: what shows in each (cues that
// overlap are shown together, one under the other). A text track covers its whole length, so
// the gaps are pieces too, with no text.
func textTimeline(cues []placedCue) (starts []int64, texts []string, end int64) {
	var edges []int64
	for _, c := range cues {
		if c.end > c.start {
			edges = append(edges, c.start, c.end)
		}
	}
	if len(edges) == 0 {
		return nil, nil, 0
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i] < edges[j] })
	if edges[0] > 0 {
		edges = append([]int64{0}, edges...)
	}
	for i := 0; i+1 < len(edges); i++ {
		a, b := edges[i], edges[i+1]
		if b <= a {
			continue
		}
		var on []string
		for _, c := range cues {
			if c.start <= a && c.end >= b {
				on = append(on, c.text)
			}
		}
		text := strings.Join(on, "\n")
		if n := len(texts); n > 0 && texts[n-1] == text { // the same text goes on
			continue
		}
		starts, texts = append(starts, a), append(texts, text)
	}
	return starts, texts, edges[len(edges)-1]
}

// tx3gSamples builds a text track's samples (ms timescale): a 16-bit length and the UTF-8 text.
func tx3gSamples(cues []placedCue) (samples []rawSample, data [][]byte, lastDur int64) {
	starts, texts, end := textTimeline(cues)
	for i, st := range starts {
		text := vttPlain(texts[i])
		if len(text) > 0xFFF0 {
			text = text[:0xFFF0]
		}
		b := binary.BigEndian.AppendUint16(nil, uint16(len(text)))
		b = append(b, text...)
		samples = append(samples, rawSample{dts: st, size: uint32(len(b)), flags: sfSync})
		data = append(data, b)
	}
	if n := len(starts); n > 0 {
		lastDur = max(end-starts[n-1], 1)
	}
	return samples, data, lastDur
}
