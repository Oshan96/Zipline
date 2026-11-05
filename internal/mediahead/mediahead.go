// Package mediahead reads what the start of an MP4 or WebM file says about it: whether it holds a
// picture, sound or both, the picture's size, how long it is and whether it's encrypted. A player
// that streams a video's picture and its sound as two separate files gets them told apart this way,
// without downloading either.
package mediahead

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strings"
)

// Info is what a file's header says.
type Info struct {
	Container  string   // "mp4", "webm" or "mkv"
	Video      bool     // it has a picture
	Audio      bool     // it has sound
	Width      int      // the picture's size (0 when the file doesn't say)
	Height     int      //
	Duration   float64  // seconds (0 when the file doesn't say)
	Codecs     []string // as an MP4 names them (avc1, hvc1, av01, vp09, mp4a, opus…), a track each
	Encrypted  bool     // a track is encrypted (DRM)
	Fragmented bool     // an MP4 in fragments (moof), the kind a player streams
}

var (
	ErrUnknown = errors.New("not an MP4 or WebM file")
	ErrBroken  = errors.New("the file's header is broken")
	ErrNoIndex = errors.New("the file doesn't describe its tracks near its start")
)

// Most a header part may take: enough for a long film's MP4 index, small enough for memory.
const maxPart = 32 << 20

// Read reads r (size bytes long, or -1 when that isn't known) only as far as it needs to.
func Read(r io.ReaderAt, size int64) (Info, error) {
	var magic [8]byte
	if n, _ := r.ReadAt(magic[:], 0); n < 8 {
		return Info{}, ErrUnknown
	}
	if binary.BigEndian.Uint32(magic[:4]) == 0x1A45DFA3 {
		return readWebM(r, size)
	}
	switch string(magic[4:8]) {
	case "ftyp", "styp", "moov", "free", "skip", "sidx", "wide", "pdin":
		return readMP4(r, size)
	}
	return Info{}, ErrUnknown
}

// readFull reads n bytes at off, or fails.
func readFull(r io.ReaderAt, off, n int64) ([]byte, error) {
	if n < 0 || n > maxPart {
		return nil, ErrBroken
	}
	b := make([]byte, n)
	got, err := r.ReadAt(b, off)
	if int64(got) < n {
		if err == nil || err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}

// ---------------------------------------------------------------- MP4

// box is one MP4 box inside a parent's bytes.
type box struct {
	typ  string
	body []byte
}

// boxes lists b's boxes (a broken one ends the list).
func boxes(b []byte) []box {
	var out []box
	for len(b) >= 8 {
		size, hdr := int64(binary.BigEndian.Uint32(b)), int64(8)
		typ := string(b[4:8])
		switch size {
		case 0:
			size = int64(len(b))
		case 1:
			if len(b) < 16 {
				return out
			}
			size, hdr = int64(binary.BigEndian.Uint64(b[8:])), 16
		}
		if size < hdr || size > int64(len(b)) {
			return out
		}
		out = append(out, box{typ, b[hdr:size]})
		b = b[size:]
	}
	return out
}

func child(b []byte, path ...string) []byte {
	for _, want := range path {
		var found []byte
		for _, x := range boxes(b) {
			if x.typ == want {
				found = x.body
				break
			}
		}
		if found == nil {
			return nil
		}
		b = found
	}
	return b
}

func readMP4(r io.ReaderAt, size int64) (Info, error) {
	info := Info{Container: "mp4"}
	var off int64
	moov := false
	var mdhdDur float64
	for i := 0; i < 64; i++ {
		if size >= 0 && off+8 > size {
			break
		}
		var h [16]byte
		if n, _ := r.ReadAt(h[:8], off); n < 8 {
			break
		}
		bsize, hdr := int64(binary.BigEndian.Uint32(h[:4])), int64(8)
		typ := string(h[4:8])
		if bsize == 1 {
			if n, _ := r.ReadAt(h[8:16], off+8); n < 8 {
				return info, ErrBroken
			}
			bsize, hdr = int64(binary.BigEndian.Uint64(h[8:16])), 16
		}
		if bsize == 0 { // (to the end of the file)
			if size < 0 {
				bsize = math.MaxInt64 - off
			} else {
				bsize = size - off
			}
		}
		if bsize < hdr {
			return info, ErrBroken
		}
		switch typ {
		case "moov":
			body, err := readFull(r, off+hdr, bsize-hdr)
			if err != nil {
				return info, err
			}
			mdhdDur = parseMoov(body, &info)
			moov = true
		case "sidx":
			if moov && info.Duration == 0 && bsize <= 4<<20 {
				if body, err := readFull(r, off+hdr, bsize-hdr); err == nil {
					// an index of the whole file (DASH's one-file tracks have one); some writers put an
					// index before each fragment instead, and the first covers only the first
					dur, first, covered := sidxDuration(body)
					if end := off + bsize + first + covered; size < 0 || end >= size-max(64<<10, size/100) {
						info.Duration = dur
					}
				}
			}
		case "pssh":
			info.Encrypted = true
		case "moof":
			info.Fragmented = true
		}
		if moov && (typ == "moof" || typ == "mdat" || info.Duration > 0 && typ == "sidx") {
			break
		}
		if bsize > math.MaxInt64-off {
			break
		}
		off += bsize
	}
	if !moov {
		return info, ErrNoIndex
	}
	if info.Duration == 0 {
		info.Duration = mdhdDur
	}
	return info, nil
}

// parseMoov reads the tracks and the length; it returns the longest track's own length, for a file
// whose movie header doesn't say.
func parseMoov(moov []byte, info *Info) (trackDur float64) {
	var scale uint32
	if mvhd := child(moov, "mvhd"); len(mvhd) >= 20 {
		var dur uint64
		if mvhd[0] == 1 && len(mvhd) >= 32 {
			scale, dur = binary.BigEndian.Uint32(mvhd[20:]), binary.BigEndian.Uint64(mvhd[24:])
		} else {
			scale, dur = binary.BigEndian.Uint32(mvhd[12:]), uint64(binary.BigEndian.Uint32(mvhd[16:]))
		}
		if scale > 0 && dur != math.MaxUint32 && dur != math.MaxUint64 {
			info.Duration = float64(dur) / float64(scale)
		}
	}
	if mvex := child(moov, "mvex"); mvex != nil {
		info.Fragmented = true
		if mehd := child(mvex, "mehd"); info.Duration == 0 && scale > 0 && len(mehd) >= 8 {
			if mehd[0] == 1 && len(mehd) >= 12 {
				info.Duration = float64(binary.BigEndian.Uint64(mehd[4:])) / float64(scale)
			} else {
				info.Duration = float64(binary.BigEndian.Uint32(mehd[4:])) / float64(scale)
			}
		}
	}
	for _, x := range boxes(moov) {
		switch x.typ {
		case "pssh":
			info.Encrypted = true
		case "trak":
			if d := parseTrak(x.body, info); d > trackDur {
				trackDur = d
			}
		}
	}
	return trackDur
}

func parseTrak(trak []byte, info *Info) float64 {
	hdlr := child(trak, "mdia", "hdlr")
	if len(hdlr) < 12 {
		return 0
	}
	kind := string(hdlr[8:12])
	if kind != "vide" && kind != "soun" {
		return 0 // subtitles, timed metadata
	}
	var dur float64
	if mdhd := child(trak, "mdia", "mdhd"); len(mdhd) >= 20 {
		if mdhd[0] == 1 && len(mdhd) >= 32 {
			if s := binary.BigEndian.Uint32(mdhd[20:]); s > 0 {
				dur = float64(binary.BigEndian.Uint64(mdhd[24:])) / float64(s)
			}
		} else if s := binary.BigEndian.Uint32(mdhd[12:]); s > 0 {
			dur = float64(binary.BigEndian.Uint32(mdhd[16:])) / float64(s)
		}
	}
	codec := ""
	if stsd := child(trak, "mdia", "minf", "stbl", "stsd"); len(stsd) >= 8 {
		if es := boxes(stsd[8:]); len(es) > 0 {
			codec = es[0].typ
			if codec == "encv" || codec == "enca" {
				info.Encrypted = true
				// the original format is in its protection info: sinf/frma, after the entry's own fields
				skip := 78 // a visual entry's fields
				if codec == "enca" {
					skip = 28
				}
				if len(es[0].body) > skip {
					if frma := child(es[0].body[skip:], "sinf", "frma"); len(frma) >= 4 {
						codec = string(frma[:4])
					}
				}
			}
		}
	}
	if kind == "vide" {
		info.Video = true
		if tkhd := child(trak, "tkhd"); len(tkhd) >= 84 {
			at := 76 // width and height, 16.16 fixed point, end the box
			if tkhd[0] == 1 {
				at = 88
			}
			if len(tkhd) >= at+8 {
				info.Width = int(binary.BigEndian.Uint32(tkhd[at:]) >> 16)
				info.Height = int(binary.BigEndian.Uint32(tkhd[at+4:]) >> 16)
			}
		}
	} else {
		info.Audio = true
	}
	if codec != "" {
		info.Codecs = append(info.Codecs, strings.ToLower(strings.TrimRight(codec, " \x00")))
	}
	return dur
}

// sidxDuration adds up a segment index's lengths; it also says where the media it indexes starts
// (after the index) and how many bytes it covers.
func sidxDuration(b []byte) (secs float64, first, covered int64) {
	if len(b) < 20 {
		return 0, 0, 0
	}
	scale := binary.BigEndian.Uint32(b[8:])
	at := 20 // after earliest_presentation_time and first_offset (32 bits each)
	first = int64(binary.BigEndian.Uint32(b[16:]))
	if b[0] == 1 {
		if len(b) < 28 {
			return 0, 0, 0
		}
		at, first = 28, int64(binary.BigEndian.Uint64(b[20:])&math.MaxInt32)
	}
	if scale == 0 || len(b) < at+4 {
		return 0, 0, 0
	}
	n := int(binary.BigEndian.Uint16(b[at+2:]))
	at += 4
	var total uint64
	for i := 0; i < n && at+12 <= len(b); i, at = i+1, at+12 {
		covered += int64(binary.BigEndian.Uint32(b[at:]) & 0x7FFFFFFF)
		total += uint64(binary.BigEndian.Uint32(b[at+4:]))
	}
	return float64(total) / float64(scale), first, covered
}

// ---------------------------------------------------------------- WebM (Matroska)

const (
	idEBML      = 0x1A45DFA3
	idDocType   = 0x4282
	idSegment   = 0x18538067
	idInfo      = 0x1549A966
	idScale     = 0x2AD7B1
	idDuration  = 0x4489
	idTracks    = 0x1654AE6B
	idTrack     = 0xAE
	idTrackType = 0x83
	idCodecID   = 0x86
	idVideo     = 0xE0
	idWidth     = 0xB0
	idHeight    = 0xBA
	idEncodings = 0x6D80
	idEncoding  = 0x6240
	idEncrypt   = 0x5035
	idCluster   = 0x1F43B675
)

// vint reads an EBML number at b: an element's ID (marker kept) or its size (marker dropped;
// all ones means "not known").
func vint(b []byte, keepMarker bool) (v uint64, n int, unknown bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n = 1
	for m := byte(0x80); b[0]&m == 0; m >>= 1 {
		n++
	}
	if n > 8 || len(b) < n {
		return 0, 0, false
	}
	v = uint64(b[0])
	if !keepMarker {
		v &= uint64(0xFF >> n)
	}
	ones := v == uint64(0xFF>>n)
	for _, c := range b[1:n] {
		v = v<<8 | uint64(c)
		ones = ones && c == 0xFF
	}
	return v, n, ones && !keepMarker
}

// header reads the element header at off: its ID, where its body starts and how long it is (-1: not known).
func header(r io.ReaderAt, off int64) (id uint64, body, size int64, err error) {
	var h [12]byte
	n, _ := r.ReadAt(h[:], off)
	id, a, _ := vint(h[:n], true)
	if a == 0 || a > 4 {
		return 0, 0, 0, ErrBroken
	}
	sz, b, unknown := vint(h[a:n], false)
	if b == 0 {
		return 0, 0, 0, ErrBroken
	}
	if unknown {
		return id, off + int64(a+b), -1, nil
	}
	return id, off + int64(a+b), int64(sz), nil
}

// elems lists b's elements: ID and body.
func elems(b []byte, fn func(id uint64, body []byte)) {
	for len(b) > 0 {
		id, a, _ := vint(b, true)
		if a == 0 || a > 4 {
			return
		}
		sz, c, unknown := vint(b[a:], false)
		if c == 0 || unknown || uint64(len(b)-a-c) < sz {
			return
		}
		fn(id, b[a+c:a+c+int(sz)])
		b = b[a+c+int(sz):]
	}
}

func uintOf(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func readWebM(r io.ReaderAt, size int64) (Info, error) {
	info := Info{Container: "webm"}
	_, body, n, err := header(r, 0)
	if err != nil || n < 0 {
		return info, ErrBroken
	}
	ebml, err := readFull(r, body, n)
	if err != nil {
		return info, err
	}
	elems(ebml, func(id uint64, b []byte) {
		if id == idDocType && string(b) == "matroska" {
			info.Container = "mkv"
		}
	})
	id, off, segSize, err := header(r, body+n)
	if err != nil || id != idSegment {
		return info, ErrBroken
	}
	end := int64(math.MaxInt64)
	if segSize >= 0 {
		end = off + segSize
	}
	if size >= 0 {
		end = min(end, size)
	}
	tracks := false
	for i := 0; i < 64 && off < end; i++ {
		id, b, n, err := header(r, off)
		if err != nil || n < 0 || id == idCluster {
			break // (the media itself: what comes before it is all there is)
		}
		switch id {
		case idInfo:
			x, err := readFull(r, b, n)
			if err != nil {
				return info, err
			}
			scale, dur := uint64(1000000), 0.0
			elems(x, func(id uint64, v []byte) {
				switch {
				case id == idScale:
					scale = uintOf(v)
				case id == idDuration && len(v) == 4:
					dur = float64(math.Float32frombits(binary.BigEndian.Uint32(v)))
				case id == idDuration && len(v) == 8:
					dur = math.Float64frombits(binary.BigEndian.Uint64(v))
				}
			})
			info.Duration = dur * float64(scale) / 1e9
		case idTracks:
			x, err := readFull(r, b, n)
			if err != nil {
				return info, err
			}
			elems(x, func(id uint64, t []byte) {
				if id == idTrack {
					webmTrack(t, &info)
				}
			})
			tracks = true
		}
		off = b + n
	}
	if !tracks {
		return info, ErrNoIndex
	}
	return info, nil
}

var webmCodecs = map[string]string{"V_VP8": "vp08", "V_VP9": "vp09", "V_AV1": "av01", "V_MPEG4/ISO/AVC": "avc1", "V_MPEGH/ISO/HEVC": "hvc1",
	"A_OPUS": "opus", "A_VORBIS": "vorbis", "A_AAC": "mp4a", "A_FLAC": "flac", "A_AC3": "ac-3", "A_EAC3": "ec-3", "A_MPEG/L3": "mp3"}

func webmTrack(t []byte, info *Info) {
	var typ uint64
	codec, w, h := "", 0, 0
	elems(t, func(id uint64, v []byte) {
		switch id {
		case idTrackType:
			typ = uintOf(v)
		case idCodecID:
			codec = strings.TrimRight(string(v), "\x00")
		case idVideo:
			elems(v, func(id uint64, x []byte) {
				switch id {
				case idWidth:
					w = int(uintOf(x))
				case idHeight:
					h = int(uintOf(x))
				}
			})
		case idEncodings:
			elems(v, func(id uint64, e []byte) {
				if id == idEncoding {
					elems(e, func(id uint64, _ []byte) { info.Encrypted = info.Encrypted || id == idEncrypt })
				}
			})
		}
	})
	switch typ {
	case 1:
		info.Video, info.Width, info.Height = true, w, h
	case 2:
		info.Audio = true
	default:
		return
	}
	if c, ok := webmCodecs[codec]; ok {
		codec = c
	} else {
		codec = strings.ToLower(codec)
	}
	if codec != "" {
		info.Codecs = append(info.Codecs, codec)
	}
}
