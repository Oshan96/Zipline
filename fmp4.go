package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// Reader for fragmented MP4 (an init segment followed by moof+mdat pieces), the format of
// "MP4 pieces" HLS streams and of DASH. It lists every frame's timing, size and position so
// the frames can be copied into a regular MP4.

type fmp4Track struct {
	id        uint32
	video     bool
	av        bool
	timescale uint32
	width     int
	height    int
	lang      string
	entries   [][]byte
	defDesc   uint32
	defDur    uint32
	defSize   uint32
	defFlags  uint32
	samples   []rawSample
	offs      []int64
	lastDur   int64
	next      int64
	haveNext  bool
	epoch     uint16
	edits     [][2]int64 // edit list: segment duration (movie timescale), media time (-1 = empty)
	editEmpty int64
	editMedia int64
	hasEdit   bool
	// a later init section (a new part, discont.go) maps its sample descriptions onto entries
	// and may count time in another timescale: times are scaled by num/den
	descMap  []int
	num, den int64
	handler  string
	// where presentation starts in each part (its init's edit list: media time minus the empty
	// start), so parts are joined by what's shown, not by the decoder's lead-in
	shift     int64
	partShift map[uint16]int64
}

// out converts a time in the current part's timescale to the track's own.
func (t *fmp4Track) out(v int64) int64 {
	if t.den == 0 || t.num == t.den {
		return v
	}
	return floorDiv(v*t.num, t.den)
}

var errBadMP4 = unsupported("the MP4 pieces couldn't be read")

// eachBox calls fn for each box in b with its type, body and the whole box.
func eachBox(b []byte, fn func(typ string, body, raw []byte) error) error {
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		hdr := uint64(8)
		if size == 1 {
			if len(b) < 16 {
				return errBadMP4
			}
			size, hdr = binary.BigEndian.Uint64(b[8:]), 16
		} else if size == 0 {
			size = uint64(len(b))
		}
		if size < hdr || size > uint64(len(b)) {
			return errBadMP4
		}
		if err := fn(typ, b[hdr:size], b[:size]); err != nil {
			return err
		}
		b = b[size:]
	}
	return nil
}

func be32(b []byte, at int) (uint32, bool) {
	if at < 0 || at+4 > len(b) {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[at:]), true
}

func be64(b []byte, at int) (uint64, bool) {
	if at < 0 || at+8 > len(b) {
		return 0, false
	}
	return binary.BigEndian.Uint64(b[at:]), true
}

type fmp4File struct {
	tracks  []*fmp4Track
	byID    map[uint32]*fmp4Track
	epoch   uint16 // shared by the file's tracks, like the TS reader
	movieTS uint32
	part    uint16
	text    bool // keep text tracks too (WebVTT in MP4 pieces)
}

// newPart starts a new timeline section for every track (a discontinuity, a new period).
func (p *fmp4File) newPart(part int) {
	p.epoch++
	for _, t := range p.tracks {
		t.epoch, t.haveNext = p.epoch, false
	}
	p.part = uint16(part)
}

// sampleFamily names the format of a sample entry (avc1 and avc3 are both H.264, and so on).
func sampleFamily(entry []byte) string {
	if len(entry) < 8 {
		return ""
	}
	typ := string(entry[4:8])
	switch typ {
	case "mp4a":
		return "mp4a"
	case "enca", "encv":
		return typ
	}
	if f := codecFamily(typ); f != "" {
		return f
	}
	return typ
}

// addInit reads an init section that comes part-way through the file (a new part's). Its tracks
// are matched to the file's by kind and order; their sample descriptions become more entries of
// the same track. A track in another format becomes a track of its own (saved separately).
func (p *fmp4File) addInit(b []byte) error {
	q := &fmp4File{byID: map[uint32]*fmp4Track{}}
	if err := q.parseMoov(b); err != nil {
		return err
	}
	byID := map[uint32]*fmp4Track{}
	used := map[*fmp4Track]bool{}
	for _, nt := range q.tracks {
		if !nt.av {
			continue
		}
		var t *fmp4Track
		for _, o := range p.tracks {
			if !used[o] && o.av && o.video == nt.video && sampleFamily(o.entries[0]) == sampleFamily(nt.entries[0]) {
				t = o
				break
			}
		}
		if t == nil { // a format the file didn't have yet
			nt.epoch, nt.edits = p.epoch, nil // (it starts part-way: its own edit list doesn't apply)
			p.tracks = append(p.tracks, nt)
			byID[nt.id], used[nt] = nt, true
			continue
		}
		used[t] = true
		t.descMap = t.descMap[:0]
		for _, e := range nt.entries {
			at := -1
			for i, have := range t.entries {
				if bytes.Equal(have, e) {
					at = i
				}
			}
			if at < 0 {
				t.entries = append(t.entries, e)
				at = len(t.entries) - 1
			}
			t.descMap = append(t.descMap, at)
		}
		t.defDesc, t.defDur, t.defSize, t.defFlags = nt.defDesc, nt.defDur, nt.defSize, nt.defFlags
		t.num, t.den = int64(t.timescale), int64(nt.timescale)
		t.shift = t.out(nt.shift)
		t.haveNext = false
		byID[nt.id] = t
	}
	p.byID = byID
	return nil
}

// editStart reads the start of the track's edit list: how long it waits before it starts
// (in its own timescale) and the media time it starts from. ok is false without an edit list.
func (t *fmp4Track) editStart(movieTS uint32) (empty, media int64, ok bool) {
	if movieTS == 0 {
		return 0, 0, false
	}
	for _, e := range t.edits {
		if e[1] < 0 {
			empty += e[0] * int64(t.timescale) / int64(movieTS)
			continue
		}
		return empty, e[1], true
	}
	return 0, 0, false
}

func (p *fmp4File) parseMoov(b []byte) error {
	trex := map[uint32][4]uint32{}
	err := eachBox(b, func(typ string, body, _ []byte) error {
		switch typ {
		case "mvhd":
			at := 12
			if len(body) > 0 && body[0] == 1 {
				at = 20
			}
			p.movieTS, _ = be32(body, at)
		case "mvex":
			return eachBox(body, func(typ string, body, _ []byte) error {
				if typ == "trex" && len(body) >= 24 {
					id := binary.BigEndian.Uint32(body[4:])
					trex[id] = [4]uint32{binary.BigEndian.Uint32(body[8:]), binary.BigEndian.Uint32(body[12:]), binary.BigEndian.Uint32(body[16:]), binary.BigEndian.Uint32(body[20:])}
				}
				return nil
			})
		case "trak":
			t, err := parseTrak(body)
			if err != nil {
				return err
			}
			if t != nil && p.text && (t.handler == "text" || t.handler == "subt") && len(t.entries) > 0 && t.timescale > 0 {
				t.av = true
			}
			if t != nil {
				if _, dup := p.byID[t.id]; !dup {
					p.byID[t.id] = t
					p.tracks = append(p.tracks, t)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for id, v := range trex {
		if t := p.byID[id]; t != nil {
			t.defDesc, t.defDur, t.defSize, t.defFlags = v[0], v[1], v[2], v[3]
		}
	}
	for _, t := range p.tracks {
		if empty, media, ok := t.editStart(p.movieTS); ok {
			t.shift = media - empty
		}
	}
	return nil
}

func parseTrak(b []byte) (*fmp4Track, error) {
	t := &fmp4Track{lang: "und"}
	var handler string
	var perr error
	err := eachBox(b, func(typ string, body, _ []byte) error {
		switch typ {
		case "tkhd":
			at := 12
			if len(body) > 0 && body[0] == 1 {
				at = 20
			}
			id, ok := be32(body, at)
			if !ok {
				return errBadMP4
			}
			t.id = id
			if w, ok := be32(body, len(body)-8); ok {
				h, _ := be32(body, len(body)-4)
				t.width, t.height = int(w>>16), int(h>>16)
			}
		case "edts":
			return eachBox(body, func(typ string, body, _ []byte) error {
				if typ != "elst" || len(body) < 8 {
					return nil
				}
				n, _ := be32(body, 4)
				at, v1 := 8, body[0] == 1
				for i := uint32(0); i < n && i < 64; i++ {
					if v1 {
						d, ok := be64(body, at)
						m, ok2 := be64(body, at+8)
						if !ok || !ok2 {
							break
						}
						t.edits = append(t.edits, [2]int64{int64(d), int64(m)})
						at += 20
					} else {
						d, ok := be32(body, at)
						m, ok2 := be32(body, at+4)
						if !ok || !ok2 {
							break
						}
						t.edits = append(t.edits, [2]int64{int64(d), int64(int32(m))})
						at += 12
					}
				}
				return nil
			})
		case "mdia":
			return eachBox(body, func(typ string, body, _ []byte) error {
				switch typ {
				case "mdhd":
					at := 12
					if len(body) > 0 && body[0] == 1 {
						at = 20
					}
					ts, ok := be32(body, at)
					if !ok || ts == 0 {
						return errBadMP4
					}
					t.timescale = ts
					la := at + 8
					if body[0] == 1 {
						la = at + 12
					}
					if l, ok := be32(body, la-2); ok {
						if v := uint16(l); v != 0 && v != 0x7FFF {
							t.lang = string([]byte{byte(v>>10&31) + 0x60, byte(v>>5&31) + 0x60, byte(v&31) + 0x60})
						}
					}
				case "hdlr":
					if len(body) >= 12 {
						handler = string(body[8:12])
					}
				case "minf":
					return eachBox(body, func(typ string, body, _ []byte) error {
						if typ != "stbl" {
							return nil
						}
						return eachBox(body, func(typ string, body, _ []byte) error {
							if typ != "stsd" || len(body) < 8 {
								return nil
							}
							return eachBox(body[8:], func(typ string, _, raw []byte) error {
								switch typ {
								case "encv", "enca":
									perr = unsupported("the stream is encrypted (DRM)")
								}
								t.entries = append(t.entries, append([]byte(nil), raw...))
								return nil
							})
						})
					})
				}
				return nil
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	t.video, t.handler = handler == "vide", handler
	t.av = (handler == "vide" || handler == "soun") && len(t.entries) > 0 && t.timescale > 0
	if t.av && perr != nil {
		return nil, perr
	}
	if t.video && (t.width == 0 || t.height == 0) && len(t.entries) > 0 && len(t.entries[0]) >= 36 {
		t.width = int(binary.BigEndian.Uint16(t.entries[0][32:]))
		t.height = int(binary.BigEndian.Uint16(t.entries[0][34:]))
	}
	return t, nil
}

func (p *fmp4File) parseMoof(b []byte, moofOff int64) error {
	prevEnd := moofOff
	first := true
	return eachBox(b, func(typ string, body, _ []byte) error {
		if typ != "traf" {
			return nil
		}
		end, err := p.parseTraf(body, moofOff, prevEnd, first)
		first = false
		prevEnd = end
		return err
	})
}

func (p *fmp4File) parseTraf(b []byte, moofOff, prevEnd int64, first bool) (int64, error) {
	var t *fmp4Track
	var base int64
	desc, dur, size, flags := uint32(0), uint32(0), uint32(0), uint32(0)
	tfdt := int64(-1)
	// tfhd and tfdt come before the truns; read them first
	err := eachBox(b, func(typ string, body, _ []byte) error {
		switch typ {
		case "tfhd":
			fl, ok := be32(body, 0)
			id, ok2 := be32(body, 4)
			if !ok || !ok2 {
				return errBadMP4
			}
			fl &= 0xFFFFFF
			t = p.byID[id]
			if t == nil {
				return nil
			}
			desc, dur, size, flags = t.defDesc, t.defDur, t.defSize, t.defFlags
			at := 8
			switch {
			case fl&1 != 0:
				v, ok := be64(body, at)
				if !ok {
					return errBadMP4
				}
				base = int64(v)
				at += 8
			case fl&0x20000 != 0 || first:
				base = moofOff
			default:
				base = prevEnd
			}
			for _, f := range []struct {
				bit uint32
				dst *uint32
			}{{2, &desc}, {8, &dur}, {0x10, &size}, {0x20, &flags}} {
				if fl&f.bit != 0 {
					v, ok := be32(body, at)
					if !ok {
						return errBadMP4
					}
					*f.dst = v
					at += 4
				}
			}
		case "tfdt":
			if len(body) > 0 && body[0] == 1 {
				v, ok := be64(body, 4)
				if !ok {
					return errBadMP4
				}
				tfdt = int64(v)
			} else if v, ok := be32(body, 4); ok {
				tfdt = int64(v)
			}
		}
		return nil
	})
	if err != nil || t == nil || !t.av {
		return prevEnd, err
	}
	if tfdt >= 0 {
		if t.haveNext {
			ts := int64(t.timescale)
			if t.den > 0 {
				ts = t.den
			}
			if delta := tfdt - t.next; delta < -ts/2 || delta > 10*ts {
				if t.epoch == p.epoch {
					p.epoch++
				}
				t.epoch = p.epoch
			}
		}
		t.next, t.haveNext = tfdt, true
	}
	t.haveNext = true
	if desc == 0 {
		desc = 1
	}
	entry := int(desc) - 1
	if t.descMap != nil {
		if int(desc) > len(t.descMap) {
			return prevEnd, errBadMP4
		}
		entry = t.descMap[desc-1]
	}
	if entry >= len(t.entries) || entry > 254 {
		return prevEnd, errBadMP4
	}
	dataPos := base
	err = eachBox(b, func(typ string, body, _ []byte) error {
		if typ != "trun" {
			return nil
		}
		fl, ok := be32(body, 0)
		n, ok2 := be32(body, 4)
		if !ok || !ok2 {
			return errBadMP4
		}
		ver := fl >> 24
		fl &= 0xFFFFFF
		at := 8
		if fl&1 != 0 {
			v, ok := be32(body, at)
			if !ok {
				return errBadMP4
			}
			dataPos = base + int64(int32(v))
			at += 4
		}
		firstFlags, hasFirst := uint32(0), fl&4 != 0
		if hasFirst {
			if firstFlags, ok = be32(body, at); !ok {
				return errBadMP4
			}
			at += 4
		}
		per := 0
		for _, bit := range []uint32{0x100, 0x200, 0x400, 0x800} {
			if fl&bit != 0 {
				per += 4
			}
		}
		if uint64(at)+uint64(n)*uint64(per) > uint64(len(body)) {
			return errBadMP4
		}
		for i := uint32(0); i < n; i++ {
			sd, ss, sf, cts := dur, size, flags, int64(0)
			if i == 0 && hasFirst {
				sf = firstFlags
			}
			if fl&0x100 != 0 {
				sd = binary.BigEndian.Uint32(body[at:])
				at += 4
			}
			if fl&0x200 != 0 {
				ss = binary.BigEndian.Uint32(body[at:])
				at += 4
			}
			if fl&0x400 != 0 {
				sf = binary.BigEndian.Uint32(body[at:])
				at += 4
			}
			if fl&0x800 != 0 {
				v := binary.BigEndian.Uint32(body[at:])
				if ver == 0 {
					cts = int64(v)
				} else {
					cts = int64(int32(v))
				}
				at += 4
			}
			if t.den > 0 { // another timescale than the first init's
				cts = t.out(t.next+cts) - t.out(t.next)
			}
			s := rawSample{dts: t.out(t.next), cts: int32(max(min(cts, 1<<31-1), -1<<31)), size: ss, entry: uint8(entry), epoch: t.epoch, part: p.part}
			if _, ok := t.partShift[p.part]; !ok {
				if t.partShift == nil {
					t.partShift = map[uint16]int64{}
				}
				t.partShift[p.part] = t.shift
			}
			if !t.video || sf&0x10000 == 0 {
				s.flags = sfSync
			}
			t.samples = append(t.samples, s)
			t.offs = append(t.offs, dataPos)
			dataPos += int64(ss)
			t.next += int64(sd)
			t.lastDur = t.out(t.next) - s.dts
		}
		return nil
	})
	return dataPos, err
}

// readFMP4 lists the frames of a fragmented MP4 file. Tracks other than audio/video are dropped.
func readFMP4(ctx context.Context, path string, progress func(float64)) ([]*fmp4Track, error) {
	return readFMP4Parts(ctx, path, nil, progress)
}

// readFMP4Parts is readFMP4 for a file whose parts start at marks (discont.go).
func readFMP4Parts(ctx context.Context, path string, marks []PartMark, progress func(float64)) ([]*fmp4Track, error) {
	return readFMP4With(ctx, path, marks, progress, false)
}

func readFMP4With(ctx context.Context, path string, marks []PartMark, progress func(float64), text bool) ([]*fmp4Track, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()
	p := &fmp4File{byID: map[uint32]*fmp4Track{}, text: text}
	parts := newPartCursor(marks)
	p.part = uint16(parts.part)
	sawMoov := false
	var hdr [16]byte
	for off, n := int64(0), 0; off+8 <= size; n++ {
		if n&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if progress != nil {
				progress(float64(off) / float64(size))
			}
		}
		k, _ := f.ReadAt(hdr[:], off)
		if k < 8 {
			break
		}
		bsize := int64(binary.BigEndian.Uint32(hdr[:]))
		typ := string(hdr[4:8])
		h := int64(8)
		switch {
		case bsize == 1 && k == 16:
			bsize, h = int64(binary.BigEndian.Uint64(hdr[8:])), 16
		case bsize == 0:
			bsize = size - off
		}
		if bsize < h || bsize > size-off {
			if typ == "mdat" { // cut short: keep what's there
				break
			}
			if !sawMoov {
				return nil, errBadMP4
			}
			break
		}
		if part, changed := parts.at(off); changed && sawMoov {
			p.newPart(part)
		} else if changed {
			p.part = uint16(part)
		}
		if typ == "moov" || typ == "moof" {
			if bsize > 256<<20 {
				return nil, errBadMP4
			}
			body := make([]byte, bsize-h)
			if _, err := f.ReadAt(body, off+h); err != nil {
				return nil, err
			}
			if typ == "moov" {
				if !sawMoov {
					if err := p.parseMoov(body); err != nil {
						return nil, err
					}
				} else if err := p.addInit(body); err != nil {
					return nil, err
				}
				sawMoov = true
			} else if sawMoov {
				if err := p.parseMoof(body, off); err != nil {
					return nil, err
				}
			}
		}
		off += bsize
	}
	if !sawMoov {
		return nil, errBadMP4
	}
	var out []*fmp4Track
	for _, t := range p.tracks {
		if !t.av {
			continue
		}
		t.editEmpty, t.editMedia, t.hasEdit = t.editStart(p.movieTS)
		// drop frames whose data lies past the end of the file (an unfinished last piece)
		n := len(t.samples)
		for n > 0 && t.offs[n-1]+int64(t.samples[n-1].size) > size {
			n--
		}
		t.samples, t.offs = t.samples[:n], t.offs[:n]
		out = append(out, t)
	}
	return out, nil
}

// isRegularMP4 reports whether path is a finished, non-fragmented MP4 (its moov has no mvex).
// Used after a restart to tell a converted file from downloaded pieces.
func isRegularMP4(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var hdr [8]byte
	for off, i := int64(0), 0; i < 4; i++ {
		if _, err := f.ReadAt(hdr[:], off); err != nil {
			return false
		}
		size := int64(binary.BigEndian.Uint32(hdr[:]))
		typ := string(hdr[4:8])
		if typ == "moov" {
			if size < 8 || size > 256<<20 {
				return false
			}
			body := make([]byte, size-8)
			if _, err := f.ReadAt(body, off+8); err != nil {
				return false
			}
			frag := false
			eachBox(body, func(typ string, _, _ []byte) error {
				frag = frag || typ == "mvex"
				return nil
			})
			return !frag
		}
		if size < 8 {
			return false
		}
		off += size
	}
	return false
}

// seqReader reads frames by file position, mostly in order, with a large buffer.
type seqReader struct {
	f   *os.File
	br  *bufio.Reader
	pos int64
	buf []byte
}

func newSeqReader(f *os.File) *seqReader {
	return &seqReader{f: f, br: bufio.NewReaderSize(f, 1<<20), pos: 0}
}

func (r *seqReader) read(off int64, n int) ([]byte, error) {
	if off != r.pos {
		if gap := off - r.pos; gap > 0 && gap < 1<<20 {
			if _, err := r.br.Discard(int(gap)); err != nil {
				return nil, err
			}
		} else {
			if _, err := r.f.Seek(off, io.SeekStart); err != nil {
				return nil, err
			}
			r.br.Reset(r.f)
		}
		r.pos = off
	}
	if cap(r.buf) < n {
		r.buf = make([]byte, n)
	}
	b := r.buf[:n]
	if _, err := io.ReadFull(r.br, b); err != nil {
		if err == io.ErrUnexpectedEOF {
			err = errors.New("the file got shorter while converting")
		}
		return nil, err
	}
	r.pos += int64(n)
	return b, nil
}

// readFMP4Text reads the cues of WebVTT carried in MP4 pieces (ISO 14496-30): each sample holds
// the cues showing from its time for its duration.
func readFMP4Text(path string, marks []PartMark) ([]subCue, error) {
	tracks, err := readFMP4With(context.Background(), path, marks, nil, true)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []subCue
	for _, t := range tracks {
		if t.handler != "text" && t.handler != "subt" {
			continue
		}
		ts := int64(t.timescale)
		for i, s := range t.samples {
			dur := t.lastDur
			if i+1 < len(t.samples) && t.samples[i+1].epoch == s.epoch {
				dur = t.samples[i+1].dts - s.dts
			}
			b := make([]byte, s.size)
			if _, err := f.ReadAt(b, t.offs[i]); err != nil {
				return out, err
			}
			st := floorDiv((s.dts+int64(s.cts))*90000, ts)
			for _, text := range wvttCues(b) {
				out = append(out, subCue{start: st, end: st + dur*90000/ts, text: text, part: int(s.part), mode: cueClock})
			}
		}
		break
	}
	return out, nil
}
