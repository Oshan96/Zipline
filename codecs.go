package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Small parsers for the codec headers Zipline needs when it repackages video into MP4.
// Nothing here decodes audio or video; it only reads sizes, rates and settings.

var errUnsupportedStream = errors.New("unsupported stream")

func unsupported(format string, a ...any) error {
	return fmt.Errorf("%w: %s", errUnsupportedStream, fmt.Sprintf(format, a...))
}

// ---------------------------------------------------------------- bit reader

type bitReader struct {
	b   []byte
	pos int // in bits
	bad bool
}

func (r *bitReader) u(n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		if r.pos>>3 >= len(r.b) {
			r.bad = true
			return v << uint(n-i)
		}
		v = v<<1 | uint64(r.b[r.pos>>3]>>(7-uint(r.pos&7))&1)
		r.pos++
	}
	return v
}

func (r *bitReader) skip(n int) { r.pos += n }

func (r *bitReader) flag() bool { return r.u(1) == 1 }

// ue reads an unsigned Exp-Golomb code.
func (r *bitReader) ue() uint64 {
	zeros := 0
	for r.u(1) == 0 {
		if r.bad || zeros > 31 {
			r.bad = true
			return 0
		}
		zeros++
	}
	return (1<<uint(zeros) - 1) + r.u(zeros)
}

func (r *bitReader) se() int64 {
	v := r.ue()
	if v&1 == 1 {
		return int64(v+1) / 2
	}
	return -int64(v / 2)
}

// unescapeRBSP removes emulation-prevention bytes (00 00 03 → 00 00).
func unescapeRBSP(b []byte) []byte {
	if !bytes.Contains(b, []byte{0, 0, 3}) {
		return b
	}
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// splitAnnexB calls fn for each NAL unit in an Annex B byte stream (00 00 01 start codes).
func splitAnnexB(b []byte, fn func(nal []byte)) {
	sc := []byte{0, 0, 1}
	i := bytes.Index(b, sc)
	for i >= 0 {
		s := i + 3
		j := bytes.Index(b[s:], sc)
		end := len(b)
		if j >= 0 {
			end = s + j
		}
		nal := b[s:end]
		for len(nal) > 0 && nal[len(nal)-1] == 0 {
			nal = nal[:len(nal)-1] // trailing zeros belong to the next 4-byte start code
		}
		if len(nal) > 0 {
			fn(nal)
		}
		if j < 0 {
			break
		}
		i = end
	}
}

// ---------------------------------------------------------------- H.264

type avcSPS struct {
	id                     int
	profile, compat, lvl   byte
	chroma, depthL, depthC int
	width, height          int
	sarW, sarH             int
}

var sarTable = [][2]int{{0, 0}, {1, 1}, {12, 11}, {10, 11}, {16, 11}, {40, 33}, {24, 11}, {20, 11}, {32, 11}, {80, 33}, {18, 11}, {15, 11}, {64, 33}, {160, 99}, {4, 3}, {3, 2}, {2, 1}}

func skipScalingList(r *bitReader, size int) {
	last, next := int64(8), int64(8)
	for j := 0; j < size; j++ {
		if next != 0 {
			next = (last + r.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

func parseAVCSPS(nal []byte) (*avcSPS, error) {
	if len(nal) < 4 || nal[0]&0x1f != 7 {
		return nil, errors.New("not an SPS")
	}
	r := &bitReader{b: unescapeRBSP(nal[1:])}
	s := &avcSPS{chroma: 1, depthL: 8, depthC: 8, sarW: 1, sarH: 1}
	s.profile, s.compat, s.lvl = byte(r.u(8)), byte(r.u(8)), byte(r.u(8))
	s.id = int(r.ue())
	switch s.profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		s.chroma = int(r.ue())
		if s.chroma == 3 {
			r.skip(1)
		}
		s.depthL = int(r.ue()) + 8
		s.depthC = int(r.ue()) + 8
		r.skip(1)
		if r.flag() {
			n := 8
			if s.chroma == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.flag() {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(r, size)
				}
			}
		}
	}
	r.ue() // log2_max_frame_num_minus4
	switch r.ue() {
	case 0:
		r.ue()
	case 1:
		r.skip(1)
		r.se()
		r.se()
		n := r.ue()
		for i := uint64(0); i < n && !r.bad; i++ {
			r.se()
		}
	}
	r.ue()    // max_num_ref_frames
	r.skip(1) // gaps_in_frame_num_value_allowed_flag
	wmbs := int(r.ue()) + 1
	hmap := int(r.ue()) + 1
	frameMbsOnly := int(r.u(1))
	if frameMbsOnly == 0 {
		r.skip(1)
	}
	r.skip(1)
	s.width, s.height = wmbs*16, (2-frameMbsOnly)*hmap*16
	if r.flag() {
		l, rr, t, b := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		cx, cy := 1, 2-frameMbsOnly
		if s.chroma == 1 || s.chroma == 2 {
			cx = 2
		}
		if s.chroma == 1 {
			cy *= 2
		}
		s.width -= (l + rr) * cx
		s.height -= (t + b) * cy
	}
	if r.flag() && r.flag() { // vui → aspect_ratio_info
		idc := int(r.u(8))
		if idc == 255 {
			s.sarW, s.sarH = int(r.u(16)), int(r.u(16))
		} else if idc > 0 && idc < len(sarTable) {
			s.sarW, s.sarH = sarTable[idc][0], sarTable[idc][1]
		}
	}
	if r.bad || s.width <= 0 || s.height <= 0 || s.width > 16384 || s.height > 16384 {
		return nil, errors.New("couldn't read the video settings (SPS)")
	}
	if s.sarW <= 0 || s.sarH <= 0 {
		s.sarW, s.sarH = 1, 1
	}
	return s, nil
}

func avcPPSID(nal []byte) (int, bool) {
	if len(nal) < 2 {
		return 0, false
	}
	r := &bitReader{b: unescapeRBSP(nal[1:min(len(nal), 16)])}
	id := r.ue()
	return int(id), !r.bad && id < 256
}

// avcIsISlice reports whether a non-IDR slice is an I (or SI) slice.
func avcIsISlice(nal []byte) bool {
	if len(nal) < 2 {
		return false
	}
	r := &bitReader{b: unescapeRBSP(nal[1:min(len(nal), 24)])}
	r.ue() // first_mb_in_slice
	t := r.ue() % 5
	return !r.bad && (t == 2 || t == 4)
}

// ---------------------------------------------------------------- HEVC

type hevcSPS struct {
	id             int
	ptl            [12]byte // general profile/tier/level bytes, as hvcC wants them
	subLayers      int
	nesting        bool
	chroma         int
	depthL, depthC int
	width, height  int
}

func parseHEVCSPS(nal []byte) (*hevcSPS, error) {
	if len(nal) < 16 || (nal[0]>>1)&0x3f != 33 {
		return nil, errors.New("not an SPS")
	}
	rb := unescapeRBSP(nal[2:])
	if len(rb) < 14 {
		return nil, errors.New("short SPS")
	}
	s := &hevcSPS{}
	r := &bitReader{b: rb}
	r.skip(4) // sps_video_parameter_set_id
	maxSub := int(r.u(3))
	s.subLayers = maxSub + 1
	s.nesting = r.flag()
	copy(s.ptl[:], rb[1:13])
	r.skip(96) // general profile_tier_level
	subProf := make([]bool, maxSub)
	subLvl := make([]bool, maxSub)
	for i := 0; i < maxSub; i++ {
		subProf[i], subLvl[i] = r.flag(), r.flag()
	}
	if maxSub > 0 {
		r.skip(2 * (8 - maxSub))
	}
	for i := 0; i < maxSub; i++ {
		if subProf[i] {
			r.skip(88)
		}
		if subLvl[i] {
			r.skip(8)
		}
	}
	s.id = int(r.ue())
	s.chroma = int(r.ue())
	if s.chroma == 3 {
		r.skip(1)
	}
	s.width, s.height = int(r.ue()), int(r.ue())
	if r.flag() { // conformance window
		l, rr, t, b := int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
		sw, sh := 1, 1
		if s.chroma == 1 || s.chroma == 2 {
			sw = 2
		}
		if s.chroma == 1 {
			sh = 2
		}
		s.width -= (l + rr) * sw
		s.height -= (t + b) * sh
	}
	s.depthL = int(r.ue()) + 8
	s.depthC = int(r.ue()) + 8
	if r.bad || s.width <= 0 || s.height <= 0 || s.width > 16384 || s.height > 16384 {
		return nil, errors.New("couldn't read the video settings (SPS)")
	}
	return s, nil
}

func hevcParamID(nal []byte) (int, bool) {
	if len(nal) < 3 {
		return 0, false
	}
	switch (nal[0] >> 1) & 0x3f {
	case 32: // VPS: first 4 bits
		return int(nal[2] >> 4), true
	case 33:
		s, err := parseHEVCSPS(nal)
		if err != nil {
			return 0, false
		}
		return s.id, true
	case 34:
		r := &bitReader{b: unescapeRBSP(nal[2:min(len(nal), 16)])}
		id := r.ue()
		return int(id), !r.bad && id < 64
	}
	return 0, false
}

// ---------------------------------------------------------------- audio frames

// audioCfg identifies an audio format; a change in key means a new MP4 sample entry.
type audioCfg struct {
	codec    string // aac, mp3, ac3, eac3
	rate     int
	channels int
	samples  int // per MP4 sample
	key      string
	// codec details
	asc       []byte // AAC AudioSpecificConfig
	mpegOTI   byte   // MP3: 0x6B (MPEG-1) or 0x69 (MPEG-2)
	ac3       [3]byte
	fscod     int
	bsid      int
	bsmod     int
	acmod     int
	lfe       int
	frameSize int // bytes, for bitrate fields
}

var aacRates = []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// adtsFrame parses an ADTS header at b. It returns the header length, the full frame length and the format.
func adtsFrame(b []byte) (hdr, size int, cfg audioCfg, err error) {
	if len(b) < 7 || b[0] != 0xFF || b[1]&0xF6 != 0xF0 {
		return 0, 0, cfg, errNoSync
	}
	hdr = 7
	if b[1]&1 == 0 {
		hdr = 9
	}
	obj := int(b[2]>>6) + 1
	sfi := int(b[2]>>2) & 0xF
	ch := int(b[2]&1)<<2 | int(b[3]>>6)
	size = int(b[3]&3)<<11 | int(b[4])<<3 | int(b[5]>>5)
	if sfi >= len(aacRates) || size < hdr {
		return 0, 0, cfg, errNoSync
	}
	if b[6]&3 != 0 {
		return 0, 0, cfg, unsupported("AAC audio with several blocks per frame isn't supported")
	}
	if ch == 0 {
		return 0, 0, cfg, unsupported("AAC audio with a custom channel layout isn't supported")
	}
	asc := []byte{byte(obj<<3 | sfi>>1), byte((sfi&1)<<7 | ch<<3)}
	cfg = audioCfg{codec: "aac", rate: aacRates[sfi], channels: ch, samples: 1024, asc: asc, key: fmt.Sprintf("aac%x", asc)}
	if ch == 7 {
		cfg.channels = 8
	}
	return hdr, size, cfg, nil
}

var errNoSync = errors.New("no sync")

var mp3Bitrates = [5][15]int{
	{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448}, // MPEG-1 layer I
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384},    // MPEG-1 layer II
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320},     // MPEG-1 layer III
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256},    // MPEG-2 layer I
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160},         // MPEG-2 layers II & III
}

// mpaFrame parses an MPEG audio (MP1/MP2/MP3) frame header.
func mpaFrame(b []byte) (size int, cfg audioCfg, err error) {
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return 0, cfg, errNoSync
	}
	ver := int(b[1]>>3) & 3 // 3 = MPEG-1, 2 = MPEG-2, 0 = MPEG-2.5
	layer := 4 - int(b[1]>>1)&3
	bri, sri, pad := int(b[2]>>4), int(b[2]>>2)&3, int(b[2]>>1)&1
	if ver == 1 || layer == 4 || bri == 15 || sri == 3 {
		return 0, cfg, errNoSync
	}
	if bri == 0 {
		return 0, cfg, unsupported("free-format MP3 audio isn't supported")
	}
	rate := []int{44100, 48000, 32000}[sri]
	var br, spf int
	if ver == 3 {
		br = mp3Bitrates[layer-1][bri]
		spf = []int{384, 1152, 1152}[layer-1]
	} else {
		rate /= 2
		if ver == 0 {
			rate /= 2
		}
		if layer == 1 {
			br = mp3Bitrates[3][bri]
		} else {
			br = mp3Bitrates[4][bri]
		}
		spf = []int{384, 1152, 576}[layer-1]
	}
	br *= 1000
	switch {
	case layer == 1:
		size = (12*br/rate + pad) * 4
	case layer == 3 && ver != 3:
		size = 72*br/rate + pad
	default:
		size = 144*br/rate + pad
	}
	ch := 2
	if b[3]>>6 == 3 {
		ch = 1
	}
	oti := byte(0x6B)
	if ver != 3 {
		oti = 0x69
	}
	cfg = audioCfg{codec: "mp3", rate: rate, channels: ch, samples: spf, mpegOTI: oti, key: fmt.Sprintf("mpa%d/%d/%d/%d", ver, layer, rate, ch)}
	return size, cfg, nil
}

var ac3Bitrates = []int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}
var ac3Channels = []int{2, 1, 2, 3, 3, 4, 4, 5}

// ac3Frame parses an AC-3 or E-AC-3 sync frame header.
func ac3Frame(b []byte) (size, blocks int, cfg audioCfg, err error) {
	if len(b) < 8 || b[0] != 0x0B || b[1] != 0x77 {
		return 0, 0, cfg, errNoSync
	}
	bsid := int(b[5] >> 3)
	switch {
	case bsid <= 8: // AC-3
		fscod, frmsizecod := int(b[4]>>6), int(b[4]&0x3f)
		if fscod == 3 || frmsizecod >= 38 {
			return 0, 0, cfg, errNoSync
		}
		br := ac3Bitrates[frmsizecod>>1]
		switch fscod {
		case 0:
			size = br * 2 * 2
		case 1:
			size = (br*320/147 + frmsizecod&1) * 2
		case 2:
			size = br * 3 * 2
		}
		r := &bitReader{b: b[5:]}
		r.skip(5)
		bsmod := int(r.u(3))
		acmod := int(r.u(3))
		if acmod&1 != 0 && acmod != 1 {
			r.skip(2)
		}
		if acmod&4 != 0 {
			r.skip(2)
		}
		if acmod == 2 {
			r.skip(2)
		}
		lfe := int(r.u(1))
		cfg = audioCfg{codec: "ac3", rate: []int{48000, 44100, 32000}[fscod], channels: ac3Channels[acmod] + lfe, samples: 1536,
			fscod: fscod, bsid: bsid, bsmod: bsmod, acmod: acmod, lfe: lfe, frameSize: size}
		// dac3: fscod(2) bsid(5) bsmod(3) acmod(3) lfeon(1) bit_rate_code(5) reserved(5)
		v := uint32(fscod)<<22 | uint32(bsid)<<17 | uint32(bsmod)<<14 | uint32(acmod)<<11 | uint32(lfe)<<10 | uint32(frmsizecod>>1)<<5
		cfg.ac3 = [3]byte{byte(v >> 16), byte(v >> 8), byte(v)}
		cfg.key = fmt.Sprintf("ac3%x", cfg.ac3)
		return size, 6, cfg, nil
	case bsid >= 11 && bsid <= 16: // E-AC-3
		r := &bitReader{b: b[2:]}
		strmtyp := int(r.u(2))
		substream := int(r.u(3))
		size = (int(r.u(11)) + 1) * 2
		fscod := int(r.u(2))
		rate, nblk := 0, 6
		if fscod == 3 {
			fscod2 := int(r.u(2))
			if fscod2 == 3 {
				return 0, 0, cfg, errNoSync
			}
			rate = []int{24000, 22050, 16000}[fscod2]
		} else {
			rate = []int{48000, 44100, 32000}[fscod]
			nblk = []int{1, 2, 3, 6}[r.u(2)]
		}
		acmod := int(r.u(3))
		lfe := int(r.u(1))
		if strmtyp == 1 || substream != 0 {
			return 0, 0, cfg, unsupported("E-AC-3 audio with extra channels (7.1) isn't supported")
		}
		cfg = audioCfg{codec: "eac3", rate: rate, channels: ac3Channels[acmod] + lfe, samples: 1536,
			fscod: fscod, bsid: bsid, acmod: acmod, lfe: lfe, frameSize: size * 6 / nblk}
		cfg.key = fmt.Sprintf("eac3/%d/%d/%d/%d", fscod, bsid, acmod, lfe)
		return size, nblk, cfg, nil
	}
	return 0, 0, cfg, unsupported("this AC-3 audio variant (%d) isn't supported", bsid)
}

// ---------------------------------------------------------------- MP4 box writer

type boxWriter struct{ b []byte }

func (w *boxWriter) start(typ string) int {
	p := len(w.b)
	w.b = append(w.b, 0, 0, 0, 0)
	w.b = append(w.b, typ...)
	return p
}

func (w *boxWriter) full(typ string, ver byte, flags uint32) int {
	p := w.start(typ)
	w.u32(uint32(ver)<<24 | flags&0xFFFFFF)
	return p
}

func (w *boxWriter) end(p int) { binary.BigEndian.PutUint32(w.b[p:], uint32(len(w.b)-p)) }

func (w *boxWriter) u8(v byte)    { w.b = append(w.b, v) }
func (w *boxWriter) u16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *boxWriter) u24(v uint32) { w.b = append(w.b, byte(v>>16), byte(v>>8), byte(v)) }
func (w *boxWriter) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *boxWriter) u64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *boxWriter) raw(b []byte) { w.b = append(w.b, b...) }
func (w *boxWriter) zeros(n int)  { w.b = append(w.b, make([]byte, n)...) }

func (w *boxWriter) len() int               { return len(w.b) }
func (w *boxWriter) put32(at int, v uint32) { binary.BigEndian.PutUint32(w.b[at:], v) }
