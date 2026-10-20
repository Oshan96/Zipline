package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"math/bits"
	"os"
)

// Matroska / WebM, for DASH streams that come as WebM (VP8/VP9/AV1 video, Opus/Vorbis audio):
// the reader, and the EBML helpers the writer (mkvout.go) uses. As with MP4, the frames are
// copied unchanged.

const (
	mkvEBML        = 0x1A45DFA3
	mkvDocType     = 0x4282
	mkvSegment     = 0x18538067
	mkvSeekHead    = 0x114D9B74
	mkvSeek        = 0x4DBB
	mkvSeekID      = 0x53AB
	mkvSeekPos     = 0x53AC
	mkvInfo        = 0x1549A966
	mkvScale       = 0x2AD7B1
	mkvDuration    = 0x4489
	mkvMuxingApp   = 0x4D80
	mkvWritingApp  = 0x5741
	mkvTracks      = 0x1654AE6B
	mkvTrackEntry  = 0xAE
	mkvTrackNumber = 0xD7
	mkvTrackUID    = 0x73C5
	mkvTrackType   = 0x83
	mkvFlagDefault = 0x88
	mkvEncodings   = 0x6D80
	mkvDefaultDur  = 0x23E383
	mkvCluster     = 0x1F43B675
	mkvTimecode    = 0xE7
	mkvSimpleBlock = 0xA3
	mkvBlockGroup  = 0xA0
	mkvBlockID     = 0xA1
	mkvRefBlock    = 0xFB
	mkvCues        = 0x1C53BB6B
	mkvCuePoint    = 0xBB
	mkvCueTime     = 0xB3
	mkvCuePos      = 0xB7
	mkvCueTrack    = 0xF7
	mkvCueCluster  = 0xF1
	mkvVoid        = 0xEC
)

// level-1 elements: seeing one of these ends a cluster of unknown size
var mkvTopLevel = map[uint64]bool{mkvEBML: true, mkvSegment: true, mkvSeekHead: true, mkvInfo: true, mkvTracks: true,
	mkvCluster: true, mkvCues: true, 0x1254C367: true, 0x1043A770: true, 0x1941A469: true}

type mkvBlock struct {
	time   int64 // absolute, in the file's timecode units
	off    int64 // where the block data (track number onwards) starts
	size   int64
	hdr    int8 // length of the track number at the start
	simple bool
	key    bool
	extra  []byte // other BlockGroup children, copied as they are
	part   uint16 // which part of the stream (discont.go)
}

type mkvTrack struct {
	num    uint64
	video  bool
	entry  []byte // TrackEntry children except number and UID
	defDur int64  // ns per frame, if given
	blocks []mkvBlock
}

type mkvFile struct {
	path    string
	scale   int64 // ns per timecode unit
	docType string
	tracks  []*mkvTrack
	part    uint16 // the part being read (readMKVParts)
}

type ebmlReader struct {
	r     *bufio.Reader
	pos   int64
	limit int64 // total length, when known: element bodies can't be longer than what's left
}

func (e *ebmlReader) byte() (byte, error) {
	b, err := e.r.ReadByte()
	if err == nil {
		e.pos++
	}
	return b, err
}

// vint reads a variable-length integer. With marker it keeps the length bits (element IDs).
func (e *ebmlReader) vint(marker bool) (v uint64, n int, unknown bool, err error) {
	b, err := e.byte()
	if err != nil {
		return 0, 0, false, err
	}
	if b == 0 {
		return 0, 0, false, errBadWebM
	}
	n = bits.LeadingZeros8(b) + 1
	v = uint64(b)
	if !marker {
		v &= 1<<(8-n) - 1
	}
	for i := 1; i < n; i++ {
		c, err := e.byte()
		if err != nil {
			return 0, 0, false, err
		}
		v = v<<8 | uint64(c)
	}
	unknown = !marker && v == 1<<(7*n)-1
	return v, n, unknown, nil
}

func (e *ebmlReader) element() (id uint64, size int64, err error) {
	id, _, _, err = e.vint(true)
	if err != nil {
		return 0, 0, err
	}
	s, _, unknown, err := e.vint(false)
	if err != nil {
		return 0, 0, err
	}
	if unknown {
		return id, -1, nil
	}
	if s > 1<<50 {
		return 0, 0, errBadWebM
	}
	return id, int64(s), nil
}

func (e *ebmlReader) peekID() (uint64, bool) {
	b, err := e.r.Peek(1)
	if err != nil || b[0] == 0 {
		return 0, false
	}
	n := bits.LeadingZeros8(b[0]) + 1
	if n > 4 {
		return 0, false
	}
	p, err := e.r.Peek(n)
	if err != nil {
		return 0, false
	}
	var v uint64
	for _, c := range p {
		v = v<<8 | uint64(c)
	}
	return v, true
}

func (e *ebmlReader) skip(n int64) error {
	for n > 0 {
		k := int(min(n, 1<<30))
		d, err := e.r.Discard(k)
		e.pos += int64(d)
		if err != nil {
			return err
		}
		n -= int64(d)
	}
	return nil
}

func (e *ebmlReader) bytes(n int64) ([]byte, error) {
	if n < 0 || n > 64<<20 || (e.limit > 0 && n > e.limit-e.pos) {
		return nil, errBadWebM
	}
	b := make([]byte, n)
	k, err := io.ReadFull(e.r, b)
	e.pos += int64(k)
	return b, err
}

var errBadWebM = unsupported("the WebM pieces couldn't be read")

func ebmlUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// eachEBML calls fn for each child element in an in-memory body.
func eachEBML(b []byte, fn func(id uint64, body, raw []byte) error) error {
	for len(b) > 0 {
		r := &ebmlReader{r: bufio.NewReaderSize(bytesReader(b), 16), limit: int64(len(b))}
		id, size, err := r.element()
		if err != nil || size < 0 || int64(r.pos)+size > int64(len(b)) {
			return errBadWebM
		}
		h := int(r.pos)
		if err := fn(id, b[h:h+int(size)], b[:h+int(size)]); err != nil {
			return err
		}
		b = b[h+int(size):]
	}
	return nil
}

type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

func bytesReader(b []byte) io.Reader { return &sliceReader{b} }

func readMKV(ctx context.Context, path string, progress func(float64)) (*mkvFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size := fileSize(path)
	e := &ebmlReader{r: bufio.NewReaderSize(f, 1<<20), limit: size}
	mf := &mkvFile{path: path, scale: 1000000, docType: "webm"}
	byNum := map[uint64]*mkvTrack{}
	sawTracks := false
	for n := 0; ; n++ {
		if n&63 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if progress != nil && size > 0 {
				progress(float64(e.pos) / float64(size))
			}
		}
		id, sz, err := e.element()
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			if sawTracks {
				break // junk after the last cluster
			}
			return nil, err
		}
		switch id {
		case mkvSegment: // read its children in place (its stated size may not cover later pieces)
			continue
		case mkvEBML:
			body, err := e.bytes(sz)
			if err != nil {
				return nil, errBadWebM
			}
			eachEBML(body, func(id uint64, b, _ []byte) error {
				if id == mkvDocType {
					mf.docType = string(b)
				}
				return nil
			})
		case mkvInfo:
			body, err := e.bytes(sz)
			if err != nil {
				return nil, errBadWebM
			}
			eachEBML(body, func(id uint64, b, _ []byte) error {
				if id == mkvScale {
					if v := int64(ebmlUint(b)); v > 0 {
						mf.scale = v
					}
				}
				return nil
			})
		case mkvTracks:
			body, err := e.bytes(sz)
			if err != nil {
				return nil, errBadWebM
			}
			if !sawTracks {
				if err := mf.parseTracks(body, byNum); err != nil {
					return nil, err
				}
			}
			sawTracks = true
		case mkvCluster:
			if !sawTracks {
				return nil, errBadWebM
			}
			if err := mf.parseCluster(e, sz, byNum, size); err != nil {
				return nil, err
			}
		default:
			if sz < 0 {
				return nil, errBadWebM
			}
			if err := e.skip(sz); err != nil {
				break
			}
		}
	}
	if !sawTracks {
		return nil, errBadWebM
	}
	for _, t := range mf.tracks { // a cut-off last block
		for len(t.blocks) > 0 && t.blocks[len(t.blocks)-1].off+t.blocks[len(t.blocks)-1].size > size {
			t.blocks = t.blocks[:len(t.blocks)-1]
		}
	}
	return mf, nil
}

func (mf *mkvFile) parseTracks(body []byte, byNum map[uint64]*mkvTrack) error {
	return eachEBML(body, func(id uint64, b, _ []byte) error {
		if id != mkvTrackEntry {
			return nil
		}
		t := &mkvTrack{}
		typ := uint64(0)
		var perr error
		eachEBML(b, func(id uint64, v, raw []byte) error {
			switch id {
			case mkvTrackNumber:
				t.num = ebmlUint(v)
				return nil
			case mkvTrackUID:
				return nil
			case mkvTrackType:
				typ = ebmlUint(v)
			case mkvEncodings:
				perr = unsupported("the stream is encrypted (DRM)")
			case mkvDefaultDur:
				t.defDur = int64(ebmlUint(v))
			}
			t.entry = append(t.entry, raw...)
			return nil
		})
		if typ != 1 && typ != 2 {
			return nil // subtitles and such
		}
		if perr != nil {
			return perr
		}
		t.video = typ == 1
		if t.num != 0 && byNum[t.num] == nil {
			byNum[t.num] = t
			mf.tracks = append(mf.tracks, t)
		}
		return nil
	})
}

// blockHeader reads a block's track number, relative time and flags.
func (e *ebmlReader) blockHeader() (track uint64, hdr int, tc int16, flags byte, err error) {
	track, hdr, _, err = e.vint(false)
	if err != nil {
		return
	}
	var b [3]byte
	for i := range b {
		if b[i], err = e.byte(); err != nil {
			return
		}
	}
	return track, hdr, int16(binary.BigEndian.Uint16(b[:])), b[2], nil
}

func (mf *mkvFile) parseCluster(e *ebmlReader, sz int64, byNum map[uint64]*mkvTrack, fsize int64) error {
	end := int64(-1)
	if sz >= 0 {
		end = e.pos + sz
	}
	var ctc int64
	for end < 0 || e.pos < end {
		if end < 0 {
			if id, ok := e.peekID(); !ok || mkvTopLevel[id] {
				return nil
			}
		}
		id, size, err := e.element()
		if err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		if size < 0 {
			return errBadWebM
		}
		start := e.pos
		switch id {
		case mkvTimecode:
			b, err := e.bytes(size)
			if err != nil {
				return nil
			}
			ctc = int64(ebmlUint(b))
		case mkvSimpleBlock:
			tn, hdr, tc, flags, err := e.blockHeader()
			if err != nil {
				return nil
			}
			if t := byNum[tn]; t != nil {
				t.blocks = append(t.blocks, mkvBlock{time: ctc + int64(tc), off: start, size: size, hdr: int8(hdr), simple: true, key: flags&0x80 != 0, part: mf.part})
			}
		case mkvBlockGroup:
			body, err := e.bytes(size)
			if err != nil {
				return nil
			}
			var blk *mkvBlock
			var tr *mkvTrack
			key := true
			var extra []byte
			eachEBML(body, func(id uint64, v, raw []byte) error {
				switch id {
				case mkvBlockID:
					r := &ebmlReader{r: bufio.NewReaderSize(bytesReader(v), 16)}
					tn, hdr, tc, _, err := r.blockHeader()
					if err != nil {
						return nil
					}
					tr = byNum[tn]
					off := start + int64(len(raw)-len(v)) + int64(cap(body)-cap(raw))
					blk = &mkvBlock{time: ctc + int64(tc), off: off, size: int64(len(v)), hdr: int8(hdr), part: mf.part}
				case mkvRefBlock:
					key = false
					extra = append(extra, raw...)
				default:
					extra = append(extra, raw...)
				}
				return nil
			})
			if blk != nil && tr != nil {
				blk.key, blk.extra = key, extra
				tr.blocks = append(tr.blocks, *blk)
			}
			continue
		}
		if err := e.skip(start + size - e.pos); err != nil {
			return nil
		}
	}
	return nil
}

// ---------------------------------------------------------------- writer

func appendID(b []byte, id uint64) []byte {
	switch {
	case id > 0xFFFFFF:
		return append(b, byte(id>>24), byte(id>>16), byte(id>>8), byte(id))
	case id > 0xFFFF:
		return append(b, byte(id>>16), byte(id>>8), byte(id))
	case id > 0xFF:
		return append(b, byte(id>>8), byte(id))
	}
	return append(b, byte(id))
}

func appendSize(b []byte, n uint64) []byte {
	l := 1
	for l < 8 && n >= 1<<(7*l)-1 {
		l++
	}
	v := n | 1<<(7*l)
	for i := l - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

func appendSize8(b []byte, n uint64) []byte { // fixed 8-byte size, so it can be patched later
	return append(b, 0x01, byte(n>>48), byte(n>>40), byte(n>>32), byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
}

func ebmlElem(b []byte, id uint64, body []byte) []byte {
	return append(appendSize(appendID(b, id), uint64(len(body))), body...)
}

func ebmlUintElem(b []byte, id, v uint64) []byte {
	n := max((bits.Len64(v)+7)/8, 1)
	body := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		body[i] = byte(v)
		v >>= 8
	}
	return ebmlElem(b, id, body)
}

func ebmlUint8Elem(b []byte, id, v uint64) []byte { // fixed width, for patching
	return ebmlElem(b, id, binary.BigEndian.AppendUint64(nil, v))
}

// isWebMFile reports whether a file starts with an EBML (Matroska/WebM) header.
func isWebMFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [4]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false
	}
	return binary.BigEndian.Uint32(b[:]) == mkvEBML
}
