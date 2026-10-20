package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"mime"
	"slices"
	"strings"
)

// Every piece of a stream is checked before it's kept: a server that answers 200 with an error
// page, a sign-in page or a JSON message mustn't end up in the middle of the video, and a piece
// that arrives cut short is fetched again. Once the stream is in, the playlist's length is
// compared with what the pieces held, so a video that came out short fails clearly instead of
// being called complete.

// errNotPiece: the reply isn't a piece of the video at all. It's retried like a network error.
type errNotPiece struct{ what string }

func (e errNotPiece) Error() string {
	return "the server sent " + e.what + " instead of a piece of the video"
}

// pieceCheck is what a piece's bytes say about it.
type pieceCheck struct {
	keep  int     // bytes worth keeping (all of them unless it was cut short)
	short bool    // cut short: part of a packet, a box or a frame is missing, or less media than the playlist says
	span  float64 // seconds of media in it (-1: can't tell)
}

// pageType: a reply whose type says it's a web page or a message, not media.
func pageType(ctype string) bool {
	t, _, _ := mime.ParseMediaType(ctype)
	switch t {
	case "text/html", "application/xhtml+xml", "application/json", "application/problem+json", "text/javascript", "application/javascript":
		return true
	}
	return false
}

// sniffPiece names what a piece is ("ts", "mp4", "webm", "audio", "text" for subtitles), or says
// why it isn't one.
func sniffPiece(b []byte) (string, error) {
	b = bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))
	switch {
	case len(b) == 0:
		return "", errNotPiece{"an empty reply"}
	case bytes.HasPrefix(b, []byte("WEBVTT")):
		return "text", nil
	case tsSync(b, 0) || len(b) > 4 && tsSync(b, 4) && len(b)%192 == 0:
		return "ts", nil
	case len(b) >= 8 && mp4Top[string(b[4:8])]:
		return "mp4", nil
	case len(b) >= 4 && webmTop[binary.BigEndian.Uint32(b)]:
		return "webm", nil
	case bytes.HasPrefix(b, []byte("ID3")), len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0, len(b) >= 2 && b[0] == 0x0B && b[1] == 0x77:
		return "audio", nil // ADTS, MP3, AC-3 / E-AC-3, or packed audio with its ID3 timestamp
	}
	t := bytes.TrimLeft(b[:min(len(b), 512)], " \t\r\n")
	switch {
	case len(t) > 0 && t[0] == '<':
		if ttml(b) {
			return "text", nil
		}
		return "", errNotPiece{"a web page"}
	case len(t) > 0 && (t[0] == '{' || t[0] == '['):
		return "", errNotPiece{"a message"}
	}
	for skip := 1; skip < 192 && skip+376 < len(b); skip++ { // a few stray bytes before the first packet
		if tsSync(b, skip) {
			return "ts", nil
		}
	}
	return "", errNotPiece{"something else"}
}

// tsSync: MPEG-TS packets start at off (the sync byte 0x47 every 188 bytes, for the first few).
func tsSync(b []byte, off int) bool {
	if off >= len(b) || b[off] != 0x47 {
		return false
	}
	for k := 1; k < 4 && off+k*188 < len(b); k++ {
		if b[off+k*188] != 0x47 {
			return false
		}
	}
	return true
}

// ttml: an XML subtitle piece (TTML/IMSC), the one kind of XML that belongs in a stream.
func ttml(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 1024)]))
	return strings.Contains(head, "<tt ") || strings.Contains(head, "<tt>") || strings.Contains(head, "<tt:tt")
}

var mp4Top = map[string]bool{"ftyp": true, "styp": true, "moof": true, "mdat": true, "sidx": true, "emsg": true, "prft": true,
	"free": true, "skip": true, "moov": true, "uuid": true, "ssix": true}

// EBML header, Segment, Cluster, Cues, SeekHead, Info, Tracks: what WebM pieces start with
var webmTop = map[uint32]bool{0x1A45DFA3: true, 0x18538067: true, 0x1F43B675: true, 0x1C53BB6B: true, 0x114D9B74: true, 0x1549A966: true, 0x1654AE6B: true}

// checkInit checks an init piece (fMP4: ftyp/moov; WebM: its header).
func checkInit(b []byte) error {
	kind, err := sniffPiece(b)
	if err != nil {
		return err
	}
	switch kind {
	case "webm":
		return nil
	case "mp4":
		ok := false
		if err := eachBox(b, func(typ string, _, _ []byte) error {
			ok = ok || typ == "moov"
			return nil
		}); err != nil {
			return errNotPiece{"an incomplete start of the video"}
		}
		if ok {
			return nil
		}
	}
	return errNotPiece{"something that isn't the start of the video"}
}

// mp4Track is what measuring a fragmented MP4 piece needs from the init piece.
type mp4Track struct {
	scale          uint32
	defDur, defLen uint32
}

// checkPiece looks at a media piece: what it is, whether it arrived whole, and how much media it
// holds. tracks are the init piece's (fMP4), dur the playlist's length for it (0 = not given).
func checkPiece(b []byte, chunk bool, tracks map[uint32]mp4Track, dur float64) (pieceCheck, error) {
	c := pieceCheck{keep: len(b), span: -1}
	if chunk { // a plain byte range of one file: its first bytes can be anything
		return c, nil
	}
	kind, err := sniffPiece(b)
	if err != nil {
		return c, err
	}
	switch kind {
	case "ts":
		size := 188
		if !tsSync(b, 0) {
			size = 192
		}
		if r := len(b) % size; r != 0 && b[0] == 0x47 {
			c.keep, c.short = len(b)-r, true // the last packet is cut off
		}
		c.span = tsSpan(b[:c.keep])
	case "mp4":
		c.keep, c.span = mp4Piece(b, tracks)
		c.short = c.keep < len(b)
	case "audio":
		c.keep, c.span = audioPiece(b)
		c.short = c.keep < len(b)
	}
	if dur > 0 && c.span >= 0 && c.span < 0.8*dur-0.1 {
		c.short = true // fewer seconds than the playlist says: cut at a packet or frame boundary
	}
	return c, nil
}

// ---------------------------------------------------------------- MPEG-TS

// tsSpan is how many seconds of media a piece of MPEG-TS holds: per stream, from its first to its
// last timestamp plus one frame (AAC: the frames it carries); the longest stream counts.
func tsSpan(b []byte) float64 {
	pmt := -1
	types := map[int]byte{}
	pts := map[int][]int64{}
	aac := map[int][]byte{}
	open := map[int]bool{} // a PES whose header is still to come in the next packet (rare)
	for off := 0; off+188 <= len(b); off += 188 {
		p := b[off : off+188]
		if p[0] != 0x47 || p[1]&0x80 != 0 {
			continue
		}
		pid := int(p[1]&0x1f)<<8 | int(p[2])
		pusi := p[1]&0x40 != 0
		at := 4
		if p[3]&0x20 != 0 {
			at += 1 + int(p[4])
		}
		if p[3]&0x10 == 0 || at >= 188 {
			continue
		}
		pl := p[at:]
		switch {
		case pid == 0 && pusi:
			if sec := psiSection(pl); len(sec) >= 12 && sec[0] == 0 {
				for i := 8; i+4 <= len(sec)-4; i += 4 {
					if prog := int(sec[i])<<8 | int(sec[i+1]); prog != 0 {
						pmt = int(sec[i+2]&0x1f)<<8 | int(sec[i+3])
						break
					}
				}
			}
		case pid == pmt && pusi:
			for _, es := range pmtStreams(psiSection(pl)) {
				types[es.pid] = es.typ
			}
		case types[pid] != 0:
			if pusi {
				open[pid] = false
				if len(pl) >= 9 && pl[0] == 0 && pl[1] == 0 && pl[2] == 1 {
					hl := int(pl[8])
					if pl[7]&0x80 != 0 && len(pl) >= 14 {
						pts[pid] = append(pts[pid], readPTS(pl[9:]))
					}
					if 9+hl <= len(pl) {
						pl = pl[9+hl:]
					} else {
						pl = nil
					}
				}
			}
			if t := types[pid]; t == 0x0F || t == 0xCF {
				aac[pid] = append(aac[pid], pl...)
			}
		}
	}
	best := -1.0
	for pid, v := range pts {
		s := ptsSpan(v)
		if a, ok := aac[pid]; ok && adtsSpan(a) > 0 {
			s = adtsSpan(a) // exact: audio PES often hold many frames, so their timestamps tell less
		}
		best = max(best, s)
	}
	return best
}

// psiSection is the section a PSI packet's payload starts (when it fits in the packet).
func psiSection(pl []byte) []byte {
	if len(pl) < 1 || 1+int(pl[0]) >= len(pl) {
		return nil
	}
	s := pl[1+int(pl[0]):]
	if len(s) < 3 {
		return nil
	}
	n := 3 + (int(s[1]&0x0f)<<8 | int(s[2]))
	if n > len(s) {
		return nil
	}
	return s[:n]
}

type pmtES struct {
	pid int
	typ byte
	at  int // offset of its stream type in the section
}

// pmtStreams lists a PMT section's elementary streams.
func pmtStreams(sec []byte) []pmtES {
	if len(sec) < 16 || sec[0] != 2 {
		return nil
	}
	var out []pmtES
	end := len(sec) - 4
	for i := 12 + (int(sec[10]&0x0f)<<8 | int(sec[11])); i+5 <= end; {
		out = append(out, pmtES{pid: int(sec[i+1]&0x1f)<<8 | int(sec[i+2]), typ: sec[i], at: i})
		i += 5 + (int(sec[i+3]&0x0f)<<8 | int(sec[i+4]))
	}
	return out
}

// ptsSpan: seconds from the first timestamp to the last, plus the usual step between them.
func ptsSpan(v []int64) float64 {
	if len(v) < 2 {
		return -1
	}
	v = slices.Clone(v)
	slices.Sort(v)
	if v[len(v)-1]-v[0] > 1<<32 { // the 33-bit clock wrapped inside the piece
		for i := range v {
			if v[i] < 1<<32 {
				v[i] += 1 << 33
			}
		}
		slices.Sort(v)
	}
	var steps []int64
	for i := 1; i < len(v); i++ {
		if d := v[i] - v[i-1]; d > 0 {
			steps = append(steps, d)
		}
	}
	if len(steps) == 0 {
		return -1
	}
	slices.Sort(steps)
	return float64(v[len(v)-1]-v[0]+steps[len(steps)/2]) / 90000
}

// ---------------------------------------------------------------- raw audio

// adtsHeader reads an ADTS frame's header length and frame length (0, 0 when b isn't one).
func adtsHeader(b []byte) (hdr, size int) {
	if len(b) < 7 || b[0] != 0xFF || b[1]&0xF6 != 0xF0 {
		return 0, 0
	}
	hdr = 7
	if b[1]&1 == 0 {
		hdr = 9
	}
	size = int(b[3]&3)<<11 | int(b[4])<<3 | int(b[5]>>5)
	if size < hdr || int(b[2]>>2&0xF) >= len(aacRates) {
		return 0, 0
	}
	return hdr, size
}

// adtsSpan counts the whole ADTS frames in b (1024 samples each), in seconds.
func adtsSpan(b []byte) float64 {
	n, rate := adtsFrames(b)
	if n == 0 {
		return -1
	}
	return float64(n) * 1024 / float64(rate)
}

// adtsFrames counts whole ADTS frames, skipping bytes that don't start one.
func adtsFrames(b []byte) (n, rate int) {
	for i := 0; i+7 <= len(b); {
		_, size := adtsHeader(b[i:])
		if size == 0 {
			i++
			continue
		}
		if i+size > len(b) {
			break
		}
		n, rate = n+1, aacRates[b[i+2]>>2&0xF]
		i += size
	}
	return
}

// skipID3 steps over the ID3 tags packed audio pieces start with.
func skipID3(b []byte) int {
	at := 0
	for len(b)-at >= 10 && string(b[at:at+3]) == "ID3" {
		n := 10 + (int(b[at+6]&0x7f)<<21 | int(b[at+7]&0x7f)<<14 | int(b[at+8]&0x7f)<<7 | int(b[at+9]&0x7f))
		if b[at+5]&0x10 != 0 {
			n += 10
		}
		if at+n > len(b) {
			return len(b)
		}
		at += n
	}
	return at
}

// audioPiece measures a piece of raw ("packed") audio. Only AAC can be told apart frame by frame
// here; a cut-off last AAC frame is left out.
func audioPiece(b []byte) (keep int, span float64) {
	at := skipID3(b)
	if _, size := adtsHeader(b[at:]); size == 0 {
		return len(b), -1
	}
	i := at
	for i+7 <= len(b) {
		_, size := adtsHeader(b[i:])
		if size == 0 || i+size > len(b) {
			break
		}
		i += size
	}
	keep = len(b)
	if i < len(b) && !bytes.HasPrefix(b[i:], []byte("ID3")) {
		if _, size := adtsHeader(b[i:]); size > 0 { // a frame that starts but doesn't end
			keep = i
		}
	}
	return keep, adtsSpan(b[at:keep])
}

// ---------------------------------------------------------------- fragmented MP4

// initTracks reads an fMP4 init piece's tracks: timescale and default sample length, by track ID.
func initTracks(b []byte) map[uint32]mp4Track {
	out := map[uint32]mp4Track{}
	eachBox(b, func(typ string, body, _ []byte) error {
		if typ != "moov" {
			return nil
		}
		return eachBox(body, func(typ string, body, _ []byte) error {
			switch typ {
			case "trak":
				var id, scale uint32
				eachBox(body, func(typ string, body, _ []byte) error {
					switch typ {
					case "tkhd":
						at := 12
						if len(body) > 0 && body[0] == 1 {
							at = 20
						}
						id, _ = be32(body, at)
					case "mdia":
						eachBox(body, func(typ string, body, _ []byte) error {
							if typ == "mdhd" {
								at := 12
								if len(body) > 0 && body[0] == 1 {
									at = 20
								}
								scale, _ = be32(body, at)
							}
							return nil
						})
					}
					return nil
				})
				if id != 0 && scale != 0 {
					t := out[id]
					t.scale = scale
					out[id] = t
				}
			case "mvex":
				eachBox(body, func(typ string, body, _ []byte) error {
					if typ == "trex" && len(body) >= 24 {
						id := binary.BigEndian.Uint32(body[4:])
						t := out[id]
						t.defDur, t.defLen = binary.BigEndian.Uint32(body[12:]), binary.BigEndian.Uint32(body[16:])
						out[id] = t
					}
					return nil
				})
			}
			return nil
		})
	})
	return out
}

// mp4Piece checks a piece of fragmented MP4: its boxes must fit it exactly. A box that runs past
// the end means the piece was cut off: only the whole fragments before it are worth keeping
// (a moof without its mdat would mislead whatever reads the file). It also measures the media in
// the fragments, counting only frames whose data is there.
func mp4Piece(b []byte, tracks map[uint32]mp4Track) (keep int, span float64) {
	keep = len(b)
	lastMoof := -1 // a moof whose mdat hasn't come yet
	for at := 0; at < len(b); {
		if at+8 > len(b) {
			keep = at
			break
		}
		size, hdr := uint64(binary.BigEndian.Uint32(b[at:])), uint64(8)
		switch size {
		case 1:
			if at+16 > len(b) {
				size = math.MaxUint64
				break
			}
			size, hdr = binary.BigEndian.Uint64(b[at+8:]), 16
		case 0:
			size = uint64(len(b) - at)
		}
		if size < hdr || size > uint64(len(b)-at) {
			keep = at
			break
		}
		switch string(b[at+4 : at+8]) {
		case "moof":
			lastMoof = at
		case "mdat":
			lastMoof = -1
		}
		at += int(size)
	}
	if lastMoof >= 0 && lastMoof < keep {
		keep = lastMoof
	}
	if len(tracks) == 0 {
		return keep, -1
	}
	secs := map[uint32]float64{}
	found, moofs := false, 0
	at := 0
	eachBox(b[:keep], func(typ string, body, raw []byte) error {
		moof := at
		at += len(raw)
		if typ != "moof" {
			return nil
		}
		moofs++
		prevEnd, first := int64(moof), true
		return eachBox(body, func(typ string, body, _ []byte) error {
			if typ != "traf" {
				return nil
			}
			id, d, end, ok := trafDur(body, tracks, int64(moof), prevEnd, first, keep)
			prevEnd, first = end, false
			if ok {
				secs[id] += d
				found = true
			}
			return nil
		})
	})
	if !found {
		if moofs == 0 && keep < len(b) {
			return keep, 0 // its only fragment was cut away
		}
		return keep, -1
	}
	span = -1
	for _, s := range secs {
		span = max(span, s)
	}
	return keep, span
}

// trafDur adds up a track fragment's frame durations (seconds), stopping at the first frame whose
// data lies past end (the bytes of the piece kept). Its data starts where fmp4.go says it does:
// at the moof, or where the previous track fragment's ended. It returns where its data ends.
func trafDur(b []byte, tracks map[uint32]mp4Track, moof, prevEnd int64, first bool, end int) (uint32, float64, int64, bool) {
	var id, defDur, defLen uint32
	var t mp4Track
	baseKnown := true
	base := prevEnd
	if first {
		base = moof
	}
	pos := base
	var total uint64
	known := false
	err := eachBox(b, func(typ string, body, _ []byte) error {
		switch typ {
		case "tfhd":
			fl, _ := be32(body, 0)
			id, _ = be32(body, 4)
			t = tracks[id]
			defDur, defLen = t.defDur, t.defLen
			at := 8
			switch {
			case fl&1 != 0: // a position in the whole file: can't be checked within the piece
				baseKnown = false
				at += 8
			case fl&0x20000 != 0:
				base = moof
			}
			pos = base
			if fl&2 != 0 {
				at += 4
			}
			if fl&8 != 0 {
				defDur, _ = be32(body, at)
				at += 4
			}
			if fl&0x10 != 0 {
				defLen, _ = be32(body, at)
			}
		case "trun":
			fl, ok := be32(body, 0)
			n, ok2 := be32(body, 4)
			if !ok || !ok2 {
				return errBadMP4
			}
			fl &= 0xFFFFFF
			at := 8
			if fl&1 != 0 {
				v, _ := be32(body, at)
				pos = base + int64(int32(v))
				at += 4
			}
			if fl&4 != 0 {
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
				d, sz := defDur, defLen
				p := at
				if fl&0x100 != 0 {
					d = binary.BigEndian.Uint32(body[p:])
					p += 4
				}
				if fl&0x200 != 0 {
					sz = binary.BigEndian.Uint32(body[p:])
				}
				at += per
				pos += int64(sz)
				if baseKnown && sz > 0 && pos > int64(end) {
					return nil // its data isn't in the piece
				}
				total += uint64(d)
				known = true
			}
		}
		return nil
	})
	if err != nil || !known || t.scale == 0 {
		return id, 0, pos, false
	}
	return id, float64(total) / float64(t.scale), pos, true
}

// ---------------------------------------------------------------- which quality

// initID fingerprints an fMP4 init piece by what tells one encoding from another: each track's
// timescale and sample description (codec settings), not times or names a packager may change.
func initID(b []byte) string {
	h := sha256.New()
	n := 0
	eachBox(b, func(typ string, body, _ []byte) error {
		if typ != "moov" {
			return nil
		}
		return eachBox(body, func(typ string, body, _ []byte) error {
			if typ != "trak" {
				return nil
			}
			return eachBox(body, func(typ string, body, _ []byte) error {
				if typ != "mdia" {
					return nil
				}
				return eachBox(body, func(typ string, body, _ []byte) error {
					switch typ {
					case "mdhd":
						at := 12
						if len(body) > 0 && body[0] == 1 {
							at = 20
						}
						s, _ := be32(body, at)
						binary.Write(h, binary.BigEndian, s)
					case "minf":
						return eachBox(body, func(typ string, body, _ []byte) error {
							if typ == "stbl" {
								return eachBox(body, func(typ string, _, raw []byte) error {
									if typ == "stsd" {
										h.Write(raw)
										n++
									}
									return nil
								})
							}
							return nil
						})
					}
					return nil
				})
			})
		})
	})
	if n == 0 {
		return ""
	}
	return "mp4:" + hex.EncodeToString(h.Sum(nil)[:8])
}

// tsID names the stream types in a TS piece's program map ("ts:1b,0f"): H.264 and AAC, say.
func tsID(b []byte) string {
	pmt := -1
	for off := 0; off+188 <= len(b) && off < 64*188; off += 188 {
		p := b[off : off+188]
		if p[0] != 0x47 || p[1]&0x40 == 0 {
			continue
		}
		pid := int(p[1]&0x1f)<<8 | int(p[2])
		at := 4
		if p[3]&0x20 != 0 {
			at += 1 + int(p[4])
		}
		if at >= 188 {
			continue
		}
		sec := psiSection(p[at:])
		switch {
		case pid == 0 && len(sec) >= 12:
			for i := 8; i+4 <= len(sec)-4; i += 4 {
				if prog := int(sec[i])<<8 | int(sec[i+1]); prog != 0 {
					pmt = int(sec[i+2]&0x1f)<<8 | int(sec[i+3])
					break
				}
			}
		case pid == pmt:
			var types []string
			for _, es := range pmtStreams(sec) {
				types = append(types, fmt.Sprintf("%02x", es.typ))
			}
			if len(types) > 0 {
				return "ts:" + strings.Join(types, ",")
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------- the whole stream

// errStreamShort: the pieces held clearly less than the playlist promised.
func errStreamShort(got, want float64) error {
	return permanentErr{fmt.Errorf("the video stopped early: got %s of %s — some pieces were missing", clock(got), clock(want))}
}

// tooShort: what's missing (seconds) is more than a second, or 1% of the video, whichever is more.
func tooShort(want, missing float64) bool { return missing > max(1, want/100) }

// clock writes seconds as m:ss or h:mm:ss, the way the window writes lengths.
func clock(s float64) string {
	n := int(math.Round(max(s, 0)))
	if h := n / 3600; h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, n/60%60, n%60)
	}
	return fmt.Sprintf("%d:%02d", n/60, n%60)
}

// checkLength fails a finished stream whose pieces held clearly less than its playlist says,
// instead of calling it complete. It starts over next time: what's there isn't whole.
func (m *Manager) checkLength(d *Download) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range [][2]float64{{d.WantSecs, d.ShortSecs}, {d.AWantSecs, d.AShortSecs}} {
		if want, short := t[0], t[1]; tooShort(want, short) {
			logf("%s: the pieces hold %.1f s less than the playlist's %.1f s", d.FileName, short, want)
			d.Parts, m.dirty = 0, true
			return errStreamShort(want-short, want)
		}
	}
	return nil
}
