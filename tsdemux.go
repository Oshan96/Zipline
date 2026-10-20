package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
)

// MPEG-TS demuxer. HLS streams usually arrive as .ts pieces; this reads the audio and
// video frames back out of them (H.264, HEVC, AAC, MP3, AC-3, E-AC-3) so they can be
// stored in an MP4 file as-is. Nothing is decoded or re-encoded.

const (
	sfSync   = 1 // keyframe (IDR / IRAP)
	sfISlice = 2 // intra frame without IDR, used when a stream has no IDR frames at all
)

// rawSample is one audio or video frame on its way into an MP4 file.
type rawSample struct {
	dts   int64 // decode time in the track's timescale
	cts   int32 // composition offset: presentation = dts + cts
	size  uint32
	flags uint8
	entry uint8 // MP4 sample description, 0-based
	epoch uint16
	part  uint16 // which part of the stream (discont.go)
}

// videoParams holds the parameter sets (VPS/SPS/PPS) one MP4 sample entry describes.
type videoParams struct{ vps, sps, pps map[int][]byte }

func setParam(m *map[int][]byte, id int, nal []byte) bool {
	if *m == nil {
		*m = map[int][]byte{}
	}
	if old, ok := (*m)[id]; ok && bytes.Equal(old, nal) {
		return false
	}
	(*m)[id] = append([]byte(nil), nal...)
	return true
}

func (p *videoParams) ready(codec string) bool {
	return len(p.sps) > 0 && len(p.pps) > 0 && (codec != "hevc" || len(p.vps) > 0)
}

func (p *videoParams) clone() videoParams {
	cp := func(m map[int][]byte) map[int][]byte {
		o := make(map[int][]byte, len(m))
		for k, v := range m {
			o[k] = v
		}
		return o
	}
	return videoParams{cp(p.vps), cp(p.sps), cp(p.pps)}
}

// absorb adds cur's parameter sets to p if none of them conflicts with one p already has.
func (p *videoParams) absorb(cur *videoParams) bool {
	pairs := [][2]map[int][]byte{{p.vps, cur.vps}, {p.sps, cur.sps}, {p.pps, cur.pps}}
	for _, pr := range pairs {
		for id, nal := range pr[1] {
			if old, ok := pr[0][id]; ok && !bytes.Equal(old, nal) {
				return false
			}
		}
	}
	for i, pr := range pairs {
		for id, nal := range pr[1] {
			if _, ok := pr[0][id]; !ok {
				switch i {
				case 0:
					setParam(&p.vps, id, nal)
				case 1:
					setParam(&p.sps, id, nal)
				default:
					setParam(&p.pps, id, nal)
				}
			}
		}
	}
	return true
}

type tsTrack struct {
	idx     int
	video   bool
	codec   string // h264, hevc, aac, mp3, ac3 (AC-3 or E-AC-3)
	epoch   uint16
	samples []rawSample // filled on the first pass

	// video
	au           []byte
	auPTS, auDTS int64
	auOpen       bool
	carry        []byte
	out          []byte
	started      bool
	lastDTS      int64
	haveDTS      bool
	ps           videoParams
	entries      []videoParams
	cur          int

	// audio
	left     []byte
	work     []byte
	cfgs     []audioCfg
	rate     int
	next     int64
	aStarted bool
	pend     int64
	havePend bool
	frameLen int
	eacBuf   []byte
	eacN     int
	eacDTS   int64
	eacEpoch uint16
	eacEntry uint8
}

type tsStream struct {
	track *tsTrack
	buf   []byte
	open  bool
}

type tsDemuxer struct {
	ctx      context.Context
	r        *bufio.Reader
	size     int64
	pos      int64
	pktSize  int
	pkt      [192]byte
	pmtPID   int
	lastPMT  []byte
	sec      map[int][]byte
	streams  map[int]*tsStream
	order    []*tsStream
	tracks   []*tsTrack
	byKey    map[string]*tsTrack
	lastTS   int64
	haveTS   bool
	epoch    uint16
	emit     func(t *tsTrack, s rawSample, data []byte) error
	progress func(float64)
	parts    *partCursor // where the stream's parts start (nil: one part)
	part     uint16

	avPackets, scrambled int
}

func newTSDemuxer(ctx context.Context, r io.Reader, size int64, progress func(float64), emit func(*tsTrack, rawSample, []byte) error) *tsDemuxer {
	return &tsDemuxer{ctx: ctx, r: bufio.NewReaderSize(r, 1<<20), size: size, pmtPID: -1, sec: map[int][]byte{},
		streams: map[int]*tsStream{}, byKey: map[string]*tsTrack{}, emit: emit, progress: progress}
}

// detect finds the packet size (188, or 192 for M2TS) and skips any leading junk.
func (d *tsDemuxer) detect() error {
	b, _ := d.r.Peek(192 * 4)
	for skip := 0; skip+188 < len(b) && skip < 192; skip++ {
		for _, size := range []int{188, 192} {
			off := size - 188
			ok := skip+off < len(b) && b[skip+off] == 0x47
			for k := 1; ok && k < 3 && skip+off+k*size < len(b); k++ {
				ok = b[skip+off+k*size] == 0x47
			}
			if ok {
				d.pktSize = size
				d.r.Discard(skip)
				d.pos += int64(skip)
				return nil
			}
		}
	}
	return unsupported("the file isn't an MPEG-TS stream")
}

func (d *tsDemuxer) packet() ([]byte, error) {
	off := d.pktSize - 188
	for {
		b, _ := d.r.Peek(d.pktSize)
		if len(b) < d.pktSize {
			return nil, io.EOF
		}
		if b[off] == 0x47 {
			break
		}
		if err := d.resync(); err != nil {
			return nil, err
		}
	}
	pkt := d.pkt[:d.pktSize]
	if _, err := io.ReadFull(d.r, pkt); err != nil {
		return nil, io.EOF
	}
	d.pos += int64(d.pktSize)
	return pkt[off:], nil
}

// resync skips damaged bytes up to the next place where two packets line up. It scans in
// large windows, so even a long run of junk is passed over quickly.
func (d *tsDemuxer) resync() error {
	n, off := d.pktSize, d.pktSize-188
	const window = 1 << 16
	for {
		b, _ := d.r.Peek(window)
		full := len(b) == window
		if len(b) < n {
			d.r.Discard(len(b))
			d.pos += int64(len(b))
			return io.EOF
		}
		for p := 1; p+n <= len(b); p++ {
			if b[p+off] != 0x47 {
				continue
			}
			if p+n+off < len(b) {
				if b[p+n+off] != 0x47 {
					continue
				}
			} else if full {
				break // can't confirm yet: move on and look again
			}
			d.r.Discard(p)
			d.pos += int64(p)
			return nil
		}
		if !full {
			d.r.Discard(len(b))
			d.pos += int64(len(b))
			return io.EOF
		}
		drop := len(b) - 2*n
		d.r.Discard(drop)
		d.pos += int64(drop)
	}
}

func (d *tsDemuxer) run() error {
	if err := d.detect(); err != nil {
		return err
	}
	for n := 1; ; n++ {
		pkt, err := d.packet()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if n&8191 == 0 {
			if err := d.ctx.Err(); err != nil {
				return err
			}
			if d.progress != nil && d.size > 0 {
				d.progress(float64(d.pos) / float64(d.size))
			}
		}
		if p, changed := d.parts.at(d.pos - int64(d.pktSize)); changed {
			if err := d.newPart(p); err != nil {
				return err
			}
		}
		if err := d.handle(pkt); err != nil {
			return err
		}
	}
	for _, st := range d.order {
		if st.open {
			if err := d.pes(st); err != nil {
				return err
			}
		}
	}
	for _, t := range d.tracks {
		if err := d.flushTrack(t); err != nil {
			return err
		}
	}
	if d.scrambled > 10 && d.scrambled*2 > d.avPackets {
		return unsupported("the stream is encrypted")
	}
	if len(d.tracks) == 0 {
		return unsupported("no audio or video was found")
	}
	return nil
}

func (d *tsDemuxer) handle(pkt []byte) error {
	if pkt[1]&0x80 != 0 { // transport error indicator
		return nil
	}
	pid := int(pkt[1]&0x1f)<<8 | int(pkt[2])
	pusi := pkt[1]&0x40 != 0
	afc := pkt[3] >> 4 & 3
	p := 4
	if afc&2 != 0 {
		p += 1 + int(pkt[4])
	}
	if afc&1 == 0 || p >= 188 {
		return nil
	}
	b := pkt[p:188]
	if pid == 0 || pid == d.pmtPID {
		return d.psi(pid, pusi, b)
	}
	st := d.streams[pid]
	if st == nil {
		return nil
	}
	d.avPackets++
	if pkt[3]>>6 != 0 { // scrambled: skip it; if most packets are, the stream is encrypted
		d.scrambled++
		return nil
	}
	if pusi {
		if st.open {
			if err := d.pes(st); err != nil {
				return err
			}
		}
		st.buf = append(st.buf[:0], b...)
		st.open = true
	} else if st.open {
		st.buf = append(st.buf, b...)
	}
	return nil
}

func (d *tsDemuxer) psi(pid int, pusi bool, b []byte) error {
	s := d.sec[pid]
	if pusi {
		ptr := int(b[0])
		if 1+ptr > len(b) {
			return nil
		}
		s = append(s[:0], b[1+ptr:]...)
	} else if len(s) > 0 {
		s = append(s, b...)
	} else {
		return nil
	}
	d.sec[pid] = s
	if len(s) < 3 {
		return nil
	}
	if s[0] == 0xFF {
		d.sec[pid] = s[:0]
		return nil
	}
	l := int(s[1]&0x0f)<<8 | int(s[2])
	if len(s) < 3+l {
		if len(s) > 4096 {
			d.sec[pid] = s[:0]
		}
		return nil
	}
	sec := s[:3+l]
	d.sec[pid] = s[:0]
	if pid == 0 {
		d.pat(sec)
		return nil
	}
	return d.pmt(sec)
}

func (d *tsDemuxer) pat(sec []byte) {
	if sec[0] != 0 || len(sec) < 12 {
		return
	}
	for i := 8; i+4 <= len(sec)-4; i += 4 {
		prog := int(sec[i])<<8 | int(sec[i+1])
		pid := int(sec[i+2]&0x1f)<<8 | int(sec[i+3])
		if prog != 0 {
			if pid != d.pmtPID {
				d.pmtPID, d.lastPMT = pid, nil
			}
			return
		}
	}
}

// classifyTS maps a PMT stream type (and descriptors) to a codec. "" means "not audio/video, ignore".
func classifyTS(typ byte, desc []byte) (video bool, codec string, err error) {
	switch typ {
	case 0x1B:
		return true, "h264", nil
	case 0x24:
		return true, "hevc", nil
	case 0x0F:
		return false, "aac", nil
	case 0x03, 0x04:
		return false, "mp3", nil
	case 0x81, 0x87:
		return false, "ac3", nil
	case 0x06: // private data: look at the descriptors
		for j := 0; j+2 <= len(desc); {
			tag, l := desc[j], int(desc[j+1])
			body := desc[j+2 : min(j+2+l, len(desc))]
			j += 2 + l
			switch tag {
			case 0x6A, 0x7A:
				return false, "ac3", nil
			case 0x7B:
				return false, "", unsupported("DTS audio isn't supported")
			case 0x05:
				if len(body) >= 4 {
					switch string(body[:4]) {
					case "AC-3", "EAC3":
						return false, "ac3", nil
					case "HEVC":
						return true, "hevc", nil
					case "Opus":
						return false, "", unsupported("Opus audio in .ts files isn't supported")
					case "DTS1", "DTS2", "DTS3":
						return false, "", unsupported("DTS audio isn't supported")
					}
				}
			}
		}
		return false, "", nil
	case 0x01, 0x02:
		return true, "", unsupported("MPEG-2 video isn't supported")
	case 0x10:
		return true, "", unsupported("MPEG-4 Part 2 video isn't supported")
	case 0x33:
		return true, "", unsupported("H.266 (VVC) video isn't supported")
	case 0x42, 0xD1, 0xD2, 0xEA:
		return true, "", unsupported("this video format isn't supported")
	case 0x11:
		return false, "", unsupported("AAC-LATM audio isn't supported")
	case 0x82, 0x85, 0x8A:
		return false, "", unsupported("DTS audio isn't supported")
	case 0xDB, 0xCF, 0xC1, 0xC2:
		return false, "", unsupported("the stream is encrypted")
	}
	return false, "", nil
}

func (d *tsDemuxer) pmt(sec []byte) error {
	if sec[0] != 2 || len(sec) < 16 || bytes.Equal(sec, d.lastPMT) {
		return nil
	}
	d.lastPMT = append(d.lastPMT[:0], sec...)
	end := len(sec) - 4
	i := 12 + (int(sec[10]&0x0f)<<8 | int(sec[11]))
	vOrd, aOrd := 0, 0
	for i+5 <= end {
		typ := sec[i]
		pid := int(sec[i+1]&0x1f)<<8 | int(sec[i+2])
		esl := int(sec[i+3]&0x0f)<<8 | int(sec[i+4])
		desc := sec[min(i+5, end):min(i+5+esl, end)]
		i += 5 + esl
		video, codec, err := classifyTS(typ, desc)
		if err != nil {
			return err
		}
		if codec == "" {
			continue
		}
		// tracks are matched by kind and order, so a PMT that moves streams to new PIDs
		// (common around ad breaks) keeps feeding the same MP4 tracks
		key := "a" + strconv.Itoa(aOrd)
		if video {
			key = "v" + strconv.Itoa(vOrd)
			vOrd++
		} else {
			aOrd++
		}
		t := d.byKey[key]
		if t != nil && t.codec != codec { // another format (an ad, say): a track of its own
			key += "/" + codec
			t = d.byKey[key]
		}
		if t == nil {
			t = &tsTrack{idx: len(d.tracks), video: video, codec: codec, epoch: d.epoch}
			d.tracks = append(d.tracks, t)
			d.byKey[key] = t
		}
		if st := d.streams[pid]; st == nil || st.track != t {
			if st != nil && st.open {
				if err := d.pes(st); err != nil {
					return err
				}
			}
			st = &tsStream{track: t}
			d.streams[pid] = st
			d.order = append(d.order, st)
		}
	}
	return nil
}

func readPTS(b []byte) int64 {
	return int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// unwrap turns 33-bit timestamps into a continuous clock across the ~26.5-hour wrap.
func (d *tsDemuxer) unwrap(v int64) int64 {
	const wrap = int64(1) << 33
	if d.haveTS {
		v += floorDiv(d.lastTS-v+wrap/2, wrap) * wrap
	}
	d.lastTS, d.haveTS = v, true
	return v
}

// newPart starts the next part of the stream (discont.go): what's pending is finished, and
// every track starts a new timeline section, as decoders reset at a discontinuity.
func (d *tsDemuxer) newPart(p int) error {
	for _, st := range d.order {
		if st.open {
			if err := d.pes(st); err != nil {
				return err
			}
		}
	}
	for _, t := range d.tracks {
		if err := d.flushTrack(t); err != nil {
			return err
		}
	}
	d.epoch++
	for _, t := range d.tracks {
		t.epoch = d.epoch
		t.haveDTS, t.started, t.carry, t.ps = false, false, t.carry[:0], videoParams{}
		t.aStarted, t.havePend, t.left = false, false, t.left[:0]
	}
	d.haveTS, d.part = false, uint16(p)
	return nil
}

// newEpoch starts a new timeline section after a timestamp jump (a discontinuity).
func (d *tsDemuxer) newEpoch(t *tsTrack) {
	if t.epoch == d.epoch {
		d.epoch++
	}
	t.epoch = d.epoch
}

func (d *tsDemuxer) pes(st *tsStream) error {
	st.open = false
	b := st.buf
	if len(b) < 9 || b[0] != 0 || b[1] != 0 || b[2] != 1 {
		return nil
	}
	if plen := int(b[4])<<8 | int(b[5]); plen > 0 && 6+plen < len(b) {
		b = b[:6+plen]
	}
	flags, hl := b[7]>>6, int(b[8])
	if 9+hl > len(b) {
		return nil
	}
	pts, dts := int64(-1), int64(-1)
	if flags&2 != 0 && hl >= 5 {
		pts = d.unwrap(readPTS(b[9:]))
		dts = pts
		if flags == 3 && hl >= 10 {
			dts = d.unwrap(readPTS(b[14:]))
		}
	}
	t := st.track
	if t.video {
		return d.videoPES(t, pts, dts, b[9+hl:])
	}
	return d.audioPES(t, pts, b[9+hl:])
}

// ---------------------------------------------------------------- video

func (d *tsDemuxer) videoPES(t *tsTrack, pts, dts int64, payload []byte) error {
	if pts >= 0 { // a new access unit (frame) starts
		if err := d.flushAU(t); err != nil {
			return err
		}
		t.au = append(t.au[:0], payload...)
		t.auPTS, t.auDTS, t.auOpen = pts, dts, true
	} else if t.auOpen { // the frame continues in another PES
		t.au = append(t.au, payload...)
	}
	return nil
}

func (d *tsDemuxer) flushAU(t *tsTrack) error {
	if !t.auOpen {
		return nil
	}
	t.auOpen = false
	out := append(t.out[:0], t.carry...)
	t.carry = t.carry[:0]
	var flags uint8
	vcl, changed := false, false
	hevc := t.codec == "hevc"
	splitAnnexB(t.au, func(nal []byte) {
		if !hevc {
			switch nal[0] & 0x1f {
			case 9, 12: // access unit delimiter, filler
				return
			case 7:
				if s, err := parseAVCSPS(nal); err == nil {
					changed = setParam(&t.ps.sps, s.id, nal) || changed
				}
			case 8:
				if id, ok := avcPPSID(nal); ok {
					changed = setParam(&t.ps.pps, id, nal) || changed
				}
			case 5:
				flags |= sfSync
				vcl = true
			case 1:
				vcl = true
				if flags&(sfSync|sfISlice) == 0 && avcIsISlice(nal) {
					flags |= sfISlice
				}
			case 2, 3, 4:
				vcl = true
			}
		} else {
			typ := (nal[0] >> 1) & 0x3f
			switch {
			case typ == 35 || typ == 38:
				return
			case typ >= 32 && typ <= 34: // VPS/SPS/PPS go into the sample entry (hvc1)
				if id, ok := hevcParamID(nal); ok {
					switch typ {
					case 32:
						changed = setParam(&t.ps.vps, id, nal) || changed
					case 33:
						changed = setParam(&t.ps.sps, id, nal) || changed
					default:
						changed = setParam(&t.ps.pps, id, nal) || changed
					}
				}
				return
			case typ >= 16 && typ <= 23:
				flags |= sfSync
				vcl = true
			case typ < 32:
				vcl = true
			}
		}
		out = binary.BigEndian.AppendUint32(out, uint32(len(nal)))
		out = append(out, nal...)
	})
	t.out = out
	if !vcl { // only headers/SEI: keep them for the next frame
		t.carry = append(t.carry, out...)
		return nil
	}
	if !t.started {
		if flags == 0 || !t.ps.ready(t.codec) {
			return nil // skip frames before the first keyframe
		}
		t.started, changed = true, true
	}
	if changed {
		if err := t.updateEntry(); err != nil {
			return err
		}
	}
	dts, pts := t.auDTS, t.auPTS
	if t.haveDTS {
		delta := dts - t.lastDTS
		if delta < -45000 || delta > 10*90000 {
			d.newEpoch(t)
		} else if delta <= 0 {
			dts = t.lastDTS + 1
		}
	}
	t.lastDTS, t.haveDTS = dts, true
	cts := pts - dts
	if cts < 0 || cts > math.MaxInt32 {
		cts = 0
	}
	return d.emit(t, rawSample{dts: dts, cts: int32(cts), size: uint32(len(out)), flags: flags, entry: uint8(t.cur), epoch: t.epoch, part: d.part}, out)
}

// updateEntry picks the MP4 sample entry for the current parameter sets, adding one when they change.
func (t *tsTrack) updateEntry() error {
	if n := len(t.entries); n > 0 && t.entries[t.cur].absorb(&t.ps) {
		return nil
	}
	for i := range t.entries {
		if t.entries[i].absorb(&t.ps) {
			t.cur = i
			return nil
		}
	}
	if len(t.entries) >= 255 {
		return unsupported("the video settings change too often")
	}
	t.entries = append(t.entries, t.ps.clone())
	t.cur = len(t.entries) - 1
	return nil
}

// ---------------------------------------------------------------- audio

var errShort = errors.New("short")

func (t *tsTrack) audioFrame(b []byte) (hdr, size, n int, cfg audioCfg, err error) {
	switch t.codec {
	case "aac":
		if len(b) < 9 {
			return 0, 0, 0, cfg, errShort
		}
		hdr, size, cfg, err = adtsFrame(b)
		n = 1024
	case "mp3":
		if len(b) < 4 {
			return 0, 0, 0, cfg, errShort
		}
		size, cfg, err = mpaFrame(b)
		n = cfg.samples
	default:
		if len(b) < 8 {
			return 0, 0, 0, cfg, errShort
		}
		var blocks int
		size, blocks, cfg, err = ac3Frame(b)
		n = blocks * 256
	}
	return
}

// syncAt reports whether a frame header could start at b (used to double-check a frame's length).
func (t *tsTrack) syncAt(b []byte) bool {
	if len(b) < 2 {
		return true // can't tell yet
	}
	switch t.codec {
	case "aac":
		return b[0] == 0xFF && b[1]&0xF6 == 0xF0
	case "mp3":
		return b[0] == 0xFF && b[1]&0xE0 == 0xE0
	}
	return b[0] == 0x0B && b[1] == 0x77
}

func (d *tsDemuxer) audioPES(t *tsTrack, pts int64, payload []byte) error {
	L := len(t.left)
	buf := append(append(t.work[:0], t.left...), payload...)
	t.work = buf
	if pts >= 0 {
		t.pend, t.havePend = pts, true
	}
	pos := 0
	for pos < len(buf) {
		hdr, size, n, cfg, err := t.audioFrame(buf[pos:])
		if err == errShort {
			break
		}
		if err == errNoSync || (err == nil && pos+size+2 <= len(buf) && !t.syncAt(buf[pos+size:])) {
			pos++
			continue
		}
		if err != nil {
			return err
		}
		if pos+size > len(buf) {
			break
		}
		if t.havePend && pos >= L {
			d.audioPTS(t, t.pend, cfg.rate)
			t.havePend = false
		}
		if err := d.addAudio(t, cfg, buf[pos+hdr:pos+size], n); err != nil {
			return err
		}
		pos += size
	}
	t.left = append(t.left[:0], buf[pos:]...)
	return nil
}

// audioPTS lines the audio clock up with a PES timestamp. Small jitter is ignored so frames
// stay back to back; a gap moves the clock forward; a big jump starts a new section.
func (d *tsDemuxer) audioPTS(t *tsTrack, p90 int64, rate int) {
	if !t.aStarted {
		t.rate, t.aStarted = rate, true
		t.next = floorDiv(p90*int64(rate)+45000, 90000)
		return
	}
	cand := floorDiv(p90*int64(t.rate)+45000, 90000)
	delta := p90 - floorDiv(t.next*90000, int64(t.rate))
	frame := int64(max(t.frameLen, 1))
	switch {
	case delta < -45000 || delta > 10*90000:
		d.newEpoch(t)
		t.next = cand
	case cand > t.next+frame/2:
		t.next = cand
	}
}

func (d *tsDemuxer) addAudio(t *tsTrack, cfg audioCfg, data []byte, n int) error {
	if !t.aStarted {
		return nil // no timestamp yet
	}
	if len(t.cfgs) == 0 || t.cfgs[t.cur].key != cfg.key {
		if len(t.cfgs) > 0 && (cfg.rate != t.cfgs[0].rate || cfg.codec != t.cfgs[0].codec) {
			return unsupported("the audio format changes mid-stream")
		}
		found := false
		for i := range t.cfgs {
			if t.cfgs[i].key == cfg.key {
				t.cur, found = i, true
			}
		}
		if !found {
			if len(t.cfgs) >= 255 {
				return unsupported("the audio format changes too often")
			}
			t.cfgs = append(t.cfgs, cfg)
			t.cur = len(t.cfgs) - 1
		}
		t.rate = cfg.rate
	}
	t.frameLen = n
	if cfg.codec == "eac3" { // MP4 wants 1536 samples (6 blocks) per E-AC-3 sample
		if t.eacN == 0 {
			t.eacDTS, t.eacEpoch, t.eacEntry = t.next, t.epoch, uint8(t.cur)
		}
		t.eacBuf = append(t.eacBuf, data...)
		t.eacN += n
		t.next += int64(n)
		if t.eacN >= 1536 {
			return d.flushEAC3(t)
		}
		return nil
	}
	s := rawSample{dts: t.next, size: uint32(len(data)), flags: sfSync, entry: uint8(t.cur), epoch: t.epoch, part: d.part}
	t.next += int64(n)
	return d.emit(t, s, data)
}

func (d *tsDemuxer) flushEAC3(t *tsTrack) error {
	if t.eacN == 0 {
		return nil
	}
	s := rawSample{dts: t.eacDTS, size: uint32(len(t.eacBuf)), flags: sfSync, entry: t.eacEntry, epoch: t.eacEpoch, part: d.part}
	err := d.emit(t, s, t.eacBuf)
	t.eacBuf, t.eacN = t.eacBuf[:0], 0
	return err
}

func (d *tsDemuxer) flushTrack(t *tsTrack) error {
	if t.video {
		return d.flushAU(t)
	}
	return d.flushEAC3(t)
}
