package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"sort"
	"strings"
)

// Matroska / WebM writer for any mix of downloaded tracks: WebM pieces, MP4 pieces and MPEG-TS
// (VP9 video with AAC sound, several audio languages, WebVTT subtitles). Like the MP4 writer it
// works in two passes: the first lays out every block (the frames keep their size, so their
// places are known), the second copies the frames into place. The tracks keep their timing on
// the shared timeline the MP4 writer uses (mp4mux.go), at 1 ms resolution.

const (
	mkvCodecID      = 0x86
	mkvCodecPrivate = 0x63A2
	mkvLanguage     = 0x22B59C
	mkvLanguageBCP  = 0x22B59D
	mkvName         = 0x536E
	mkvFlagForced   = 0x55AA
	mkvFlagLacing   = 0x9C
	mkvVideo        = 0xE0
	mkvAudio        = 0xE1
	mkvPixelW       = 0xB0
	mkvPixelH       = 0xBA
	mkvDisplayW     = 0x54B0
	mkvDisplayH     = 0x54BA
	mkvSampleRate   = 0xB5
	mkvChannels     = 0x9F
	mkvBlockDur     = 0x9B
)

// ---------------------------------------------------------------- reading WebM pieces

// mkvSource reads a WebM/Matroska file for the writers, as the other sources do.
type mkvSource struct {
	path  string
	marks []PartMark
	file  *mkvFile
}

func (s *mkvSource) size() int64 { return fileSize(s.path) }

func (s *mkvSource) scan(ctx context.Context, progress func(float64)) ([]*muxTrack, error) {
	mf, err := readMKVParts(ctx, s.path, s.marks, progress)
	if err != nil {
		return nil, err
	}
	s.file = mf
	var out []*muxTrack
	for _, t := range mf.tracks {
		mt := &muxTrack{video: t.video, mkv: t, mkvScale: mf.scale, timescale: uint32(max(1e9/mf.scale, 1))}
		mt.samples = mkvSamples(t)
		mt.lang, mt.name = mkvLabels(t.entry)
		if t.video {
			mt.width, mt.height = mkvSize(t.entry)
		}
		if t.defDur > 0 {
			mt.lastDur = max(t.defDur/mf.scale, 1)
		}
		out = append(out, mt)
	}
	return out, nil
}

// mkvSamples lists a Matroska track's blocks as frames: the data is the block's flags byte and
// its frames (copied as they are, lacing and all).
func mkvSamples(t *mkvTrack) []rawSample {
	out := make([]rawSample, 0, len(t.blocks))
	for _, b := range t.blocks {
		s := rawSample{dts: b.time, size: uint32(b.size - int64(b.hdr) - 2), part: b.part}
		if b.key || !t.video {
			s.flags = sfSync
		}
		out = append(out, s)
	}
	return out
}

func (s *mkvSource) copy(ctx context.Context, progress func(float64), write func(int, []byte) error) error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := newSeqReader(f)
	total := s.size()
	var done int64
	for ti, t := range s.file.tracks {
		for i, b := range t.blocks {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				if progress != nil && total > 0 {
					progress(float64(done) / float64(total))
				}
			}
			data, err := r.read(b.off+int64(b.hdr)+2, int(b.size)-int(b.hdr)-2)
			if err != nil {
				return err
			}
			done += b.size
			if err := write(ti, data); err != nil {
				return err
			}
		}
	}
	return nil
}

// mkvFamily names a Matroska track's format from its CodecID.
func mkvFamily(entry []byte) string {
	id := ""
	eachEBML(entry, func(eid uint64, b, _ []byte) error {
		if eid == mkvCodecID {
			id = string(b)
		}
		return nil
	})
	switch {
	case id == "V_VP8":
		return "vp8"
	case id == "V_VP9":
		return "vp9"
	case id == "V_AV1":
		return "av1"
	case id == "A_OPUS":
		return "opus"
	case id == "A_VORBIS":
		return "vorbis"
	case strings.HasPrefix(id, "V_MPEG4/ISO/AVC"):
		return "h264"
	case strings.HasPrefix(id, "V_MPEGH/ISO/HEVC"):
		return "hevc"
	case strings.HasPrefix(id, "A_AAC"):
		return "aac"
	case id == "A_MPEG/L3":
		return "mp3"
	case id == "A_AC3":
		return "ac3"
	case id == "A_EAC3":
		return "eac3"
	case id == "A_FLAC":
		return "flac"
	case strings.HasPrefix(id, "S_TEXT/WEBVTT"), strings.HasPrefix(id, "D_WEBVTT"):
		return "wvtt"
	}
	return strings.ToLower(id)
}

func mkvLabels(entry []byte) (lang, name string) {
	eachEBML(entry, func(id uint64, b, _ []byte) error {
		switch id {
		case mkvLanguage:
			lang = lang639(string(b), false)
		case mkvName:
			name = string(b)
		}
		return nil
	})
	return
}

func mkvSize(entry []byte) (w, h int) {
	eachEBML(entry, func(id uint64, b, _ []byte) error {
		if id == mkvVideo {
			eachEBML(b, func(id uint64, v, _ []byte) error {
				switch id {
				case mkvPixelW:
					w = int(ebmlUint(v))
				case mkvPixelH:
					h = int(ebmlUint(v))
				}
				return nil
			})
		}
		return nil
	})
	return
}

// ---------------------------------------------------------------- MP4 sample entries → Matroska

// entryKids returns a sample entry's four-character type and its child boxes.
func entryKids(e []byte, video bool) (string, []byte) {
	if len(e) < 8 {
		return "", nil
	}
	at := 36 // AudioSampleEntry
	if video {
		at = 86 // VisualSampleEntry
	} else if len(e) >= 18 {
		switch binary.BigEndian.Uint16(e[16:]) { // QuickTime sound entry versions
		case 1:
			at += 16
		case 2:
			at += 36
		}
	}
	if at > len(e) {
		return string(e[4:8]), nil
	}
	return string(e[4:8]), e[at:]
}

func childBox(b []byte, typ string) []byte {
	var out []byte
	eachBox(b, func(t string, body, _ []byte) error {
		if t == typ && out == nil {
			out = body
		}
		return nil
	})
	return out
}

// esdsConfig reads an esds box's object type and decoder-specific info (the AAC config).
func esdsConfig(esds []byte) (oti byte, dsi []byte) {
	b := esds
	if len(b) < 4 {
		return 0, nil
	}
	b = b[4:]
	desc := func() (tag byte, body []byte, rest []byte) {
		if len(b) < 2 {
			return 0, nil, nil
		}
		tag = b[0]
		n, i := 0, 1
		for ; i < 5 && i < len(b); i++ {
			n = n<<7 | int(b[i]&0x7F)
			if b[i]&0x80 == 0 {
				i++
				break
			}
		}
		if i+n > len(b) {
			return 0, nil, nil
		}
		return tag, b[i : i+n], b[i+n:]
	}
	tag, body, _ := desc()
	if tag != 3 || len(body) < 3 {
		return 0, nil
	}
	fl := body[2]
	body = body[3:]
	if fl&0x80 != 0 {
		body = body[min(2, len(body)):]
	}
	if fl&0x40 != 0 && len(body) > 0 {
		body = body[min(1+int(body[0]), len(body)):]
	}
	if fl&0x20 != 0 {
		body = body[min(2, len(body)):]
	}
	b = body
	tag, body, _ = desc()
	if tag != 4 || len(body) < 13 {
		return 0, nil
	}
	oti = body[0]
	b = body[13:]
	if tag, body, _ = desc(); tag == 5 {
		dsi = body
	}
	return oti, dsi
}

// mp4aMP3 reports whether an mp4a entry holds MPEG audio (MP3) rather than AAC.
func mp4aMP3(e []byte) bool {
	_, kids := entryKids(e, false)
	oti, _ := esdsConfig(childBox(kids, "esds"))
	return oti == 0x6B || oti == 0x69
}

// opusHead turns an MP4 dOps box into the OpusHead header Matroska and Ogg carry.
func opusHead(dops []byte) []byte {
	if len(dops) < 11 {
		return nil
	}
	h := []byte("OpusHead")
	h = append(h, 1, dops[1])
	h = binary.LittleEndian.AppendUint16(h, binary.BigEndian.Uint16(dops[2:]))
	h = binary.LittleEndian.AppendUint32(h, binary.BigEndian.Uint32(dops[4:]))
	h = binary.LittleEndian.AppendUint16(h, binary.BigEndian.Uint16(dops[8:]))
	h = append(h, dops[10])
	return append(h, dops[11:]...)
}

// paramSets reads the parameter sets (SPS, PPS, VPS) out of an avcC or hvcC box, as
// length-prefixed NAL units: what goes in front of a frame when the settings change part-way.
func paramSets(typ string, cfg []byte) []byte {
	var nals [][]byte
	lenSize := 4
	switch typ {
	case "avcC":
		if len(cfg) < 7 {
			return nil
		}
		lenSize = int(cfg[4]&3) + 1
		b := cfg[5:]
		for _, mask := range []byte{0x1F, 0xFF} {
			if len(b) < 1 {
				break
			}
			n := int(b[0] & mask)
			b = b[1:]
			for i := 0; i < n && len(b) >= 2; i++ {
				l := int(binary.BigEndian.Uint16(b))
				if 2+l > len(b) {
					return nil
				}
				nals, b = append(nals, b[2:2+l]), b[2+l:]
			}
		}
	case "hvcC":
		if len(cfg) < 23 {
			return nil
		}
		lenSize = int(cfg[21]&3) + 1
		b := cfg[23:]
		for a := 0; a < int(cfg[22]) && len(b) >= 3; a++ {
			n := int(binary.BigEndian.Uint16(b[1:]))
			b = b[3:]
			for i := 0; i < n && len(b) >= 2; i++ {
				l := int(binary.BigEndian.Uint16(b))
				if 2+l > len(b) {
					return nil
				}
				nals, b = append(nals, b[2:2+l]), b[2+l:]
			}
		}
	default:
		return nil
	}
	var out []byte
	for _, n := range nals {
		for i := lenSize - 1; i >= 0; i-- {
			out = append(out, byte(len(n)>>(8*i)))
		}
		out = append(out, n...)
	}
	return out
}

// mkvCodec maps an MP4 sample entry to a Matroska codec ID and private data.
func mkvCodec(e []byte, video bool) (id string, priv []byte, err error) {
	typ, kids := entryKids(e, video)
	switch typ {
	case "avc1", "avc3":
		return "V_MPEG4/ISO/AVC", childBox(kids, "avcC"), nil
	case "hvc1", "hev1":
		return "V_MPEGH/ISO/HEVC", childBox(kids, "hvcC"), nil
	case "av01":
		return "V_AV1", childBox(kids, "av1C"), nil
	case "vp09":
		return "V_VP9", nil, nil
	case "vp08":
		return "V_VP8", nil, nil
	case "mp4a":
		oti, dsi := esdsConfig(childBox(kids, "esds"))
		switch oti {
		case 0x6B, 0x69:
			return "A_MPEG/L3", nil, nil
		case 0x40, 0x66, 0x67, 0x68:
			return "A_AAC", dsi, nil
		}
	case "ac-3":
		return "A_AC3", nil, nil
	case "ec-3":
		return "A_EAC3", nil, nil
	case "Opus":
		return "A_OPUS", opusHead(childBox(kids, "dOps")), nil
	case "fLaC":
		if b := childBox(kids, "dfLa"); len(b) > 4 {
			return "A_FLAC", append([]byte("fLaC"), b[4:]...), nil
		}
	case "alac":
		if b := childBox(kids, "alac"); len(b) > 4 {
			return "A_ALAC", b[4:], nil
		}
	case "dtsc", "dtse", "dtsh", "dtsl":
		return "A_DTS", nil, nil
	}
	return "", nil, unsupported("this format (%s) can't go into a Matroska file", strings.TrimSpace(typ))
}

// ---------------------------------------------------------------- the writer

type mkvMuxer struct {
	tracks []*muxTrack // in output order; skipped or empty ones are left out
	webm   bool
	text   func(global int64, secs map[int]section)

	nums     []uint64   // track numbers (0 = not in the file)
	pts      [][]int64  // per track and frame: presentation time (ms)
	lay      [][]mkvPos // per track and frame: where its block goes
	pre      [][][]byte // per track and sample entry: parameter sets to put in front (MKV keeps one set)
	header   []byte
	clusters []mkvClusterPos
	cuesPos  int64
	cues     []byte
	segStart int64
	duration int64
	written  []int
}

type mkvPos struct {
	off int64
	rel int16
	pre int16 // 1 + the sample entry whose parameter sets go first, 0 for none
}

type mkvClusterPos struct {
	off, size int64 // size of the body
	time      int64
}

// blockBody is the size of a frame's block (track number to the end of its data).
func (mm *mkvMuxer) blockLen(ti, i int) int64 {
	t := mm.tracks[ti]
	n := int64(len(appendSize(nil, mm.nums[ti]))) + 2 + int64(t.samples[i].size)
	if t.mkv == nil {
		n++ // the flags byte (Matroska input has its own in the data)
	}
	if p := mm.lay[ti][i].pre; p > 0 {
		n += int64(len(mm.pre[ti][p-1]))
	}
	return n
}

// group reports whether a frame needs a BlockGroup (a duration, or the input's own extras).
func (mm *mkvMuxer) group(ti, i int) bool {
	t := mm.tracks[ti]
	return t.text || t.mkv != nil && !t.mkv.blocks[i].simple
}

func (mm *mkvMuxer) elemLen(ti, i int) int64 {
	body := mm.blockLen(ti, i)
	if !mm.group(ti, i) {
		return 1 + int64(len(appendSize(nil, uint64(body)))) + body
	}
	g := mm.groupBody(ti, i, body)
	return 1 + int64(len(appendSize(nil, uint64(g)))) + g
}

func (mm *mkvMuxer) groupBody(ti, i int, body int64) int64 {
	t := mm.tracks[ti]
	g := 1 + int64(len(appendSize(nil, uint64(body)))) + body
	if t.text {
		g += int64(len(ebmlUintElem(nil, mkvBlockDur, uint64(t.blockDur[i]))))
	} else {
		g += int64(len(t.mkv.blocks[i].extra))
	}
	return g
}

func (mm *mkvMuxer) prepare() error {
	var live []*muxTrack
	for _, t := range mm.tracks {
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
	secs := alignEpochs(live)
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
		if len(t.aCfgs) > 0 && t.mkv == nil {
			avg, peak, maxSize := t.audioStats()
			t.entries = t.entries[:0]
			for _, c := range t.aCfgs {
				t.entries = append(t.entries, audioEntry(c, avg, peak, maxSize))
			}
		}
	}
	if mm.text != nil {
		mm.text(global, secs)
		for _, t := range mm.tracks {
			if t.text { // its cues are already in ms from the start of the file
				t.dts0, t.mediaTime, t.start90 = 0, 0, global
			}
		}
	}
	n := len(mm.tracks)
	mm.nums, mm.pts, mm.lay, mm.pre, mm.written = make([]uint64, n), make([][]int64, n), make([][]mkvPos, n), make([][][]byte, n), make([]int, n)
	num := uint64(0)
	minPTS := int64(math.MaxInt64)
	for ti, t := range mm.tracks {
		if t.skip || len(t.samples) == 0 {
			continue
		}
		num++
		mm.nums[ti] = num
		ts := int64(t.timescale)
		p := make([]int64, len(t.samples))
		for i, s := range t.samples {
			p90 := t.start90 + floorDiv((s.dts+int64(s.cts)-t.mediaTime)*90000, ts)
			p[i] = floorDiv(p90-global+45, 90)
			minPTS = min(minPTS, p[i])
		}
		mm.pts[ti] = p
		mm.lay[ti] = make([]mkvPos, len(t.samples))
		if t.mkv == nil && !t.text && len(t.entries) > 1 { // the settings change part-way
			for _, e := range t.entries {
				typ, kids := entryKids(e, t.video)
				cfg := "avcC"
				if typ == "hvc1" || typ == "hev1" {
					cfg = "hvcC"
				}
				mm.pre[ti] = append(mm.pre[ti], paramSets(cfg, childBox(kids, cfg)))
			}
			prev := uint8(0)
			for i, s := range t.samples {
				if s.entry != prev && int(s.entry) < len(mm.pre[ti]) && len(mm.pre[ti][s.entry]) > 0 {
					mm.lay[ti][i].pre = int16(s.entry) + 1
				}
				prev = s.entry
			}
		}
	}
	if minPTS < 0 { // nothing may start before 0
		for ti := range mm.pts {
			for i := range mm.pts[ti] {
				mm.pts[ti][i] -= minPTS
			}
		}
	}
	header, err := mm.headerBytes(0, 0)
	if err != nil {
		return err
	}
	pos := int64(len(header))
	mm.segStart = int64(len(mm.ebmlHeader())) + 12
	mm.layout(pos)
	mm.header, err = mm.headerBytes(mm.cuesPos-mm.segStart, mm.cuesPos+int64(len(mm.cues))-mm.segStart)
	if err != nil {
		return err
	}
	if int64(len(mm.header)) != pos {
		return errors.New("remux: header size changed")
	}
	return nil
}

func (mm *mkvMuxer) ebmlHeader() []byte {
	docType := "matroska"
	if mm.webm {
		docType = "webm"
	}
	hdr := ebmlUintElem(nil, 0x4286, 1)
	hdr = ebmlUintElem(hdr, 0x42F7, 1)
	hdr = ebmlUintElem(hdr, 0x42F2, 4)
	hdr = ebmlUintElem(hdr, 0x42F3, 8)
	hdr = ebmlElem(hdr, mkvDocType, []byte(docType))
	hdr = ebmlUintElem(hdr, 0x4287, 4)
	hdr = ebmlUintElem(hdr, 0x4285, 2)
	return ebmlElem(nil, mkvEBML, hdr)
}

// layout places the clusters and blocks from pos on, then the cues.
func (mm *mkvMuxer) layout(pos int64) {
	type head struct{ ti, i int }
	var heads []head
	hasVideo := false
	for ti, t := range mm.tracks {
		if mm.nums[ti] > 0 {
			heads = append(heads, head{ti, 0})
			hasVideo = hasVideo || t.video
		}
	}
	// decode times decide the order of blocks (presentation times may go back with B-frames)
	dts := func(ti, i int) int64 {
		t := mm.tracks[ti]
		return floorDiv(t.start90, 90) + floorDiv((t.samples[i].dts-t.mediaTime)*1000, int64(t.timescale))
	}
	var cl *mkvClusterPos
	var cues []byte
	var end int64
	for {
		best := -1
		for k, h := range heads {
			if h.i < len(mm.tracks[h.ti].samples) && (best < 0 || dts(h.ti, h.i) < dts(heads[best].ti, heads[best].i)) {
				best = k
			}
		}
		if best < 0 {
			break
		}
		ti, i := heads[best].ti, heads[best].i
		heads[best].i++
		t := mm.tracks[ti]
		p := mm.pts[ti][i]
		key := t.video && t.samples[i].flags&sfSync != 0
		if cl != nil {
			rel := p - cl.time
			if rel < -30000 || rel > 30000 || cl.size > 5<<20 || key && rel >= 1000 || !hasVideo && rel >= 5000 {
				pos += 12 + cl.size // (cluster ID, 8-byte size)
				mm.clusters = append(mm.clusters, *cl)
				cl = nil
			}
		}
		if cl == nil {
			cl = &mkvClusterPos{off: pos, time: max(p, 0)}
			cl.size = int64(len(ebmlUintElem(nil, mkvTimecode, uint64(cl.time))))
			if key || !hasVideo {
				cp := ebmlUintElem(nil, mkvCueTrack, mm.nums[ti])
				cp = ebmlUintElem(cp, mkvCueCluster, uint64(pos-mm.segStart))
				c := ebmlUintElem(nil, mkvCueTime, uint64(cl.time))
				c = ebmlElem(c, mkvCuePos, cp)
				cues = ebmlElem(cues, mkvCuePoint, c)
			}
		}
		mm.lay[ti][i].off = pos + 12 + cl.size
		mm.lay[ti][i].rel = int16(p - cl.time)
		cl.size += mm.elemLen(ti, i)
		dur := int64(0)
		if t.text {
			dur = t.blockDur[i]
		} else if i+1 < len(t.samples) {
			dur = max(mm.pts[ti][i+1]-p, 0)
		} else {
			dur = t.lastDur * 1000 / int64(t.timescale)
		}
		end = max(end, p+dur)
	}
	if cl != nil {
		pos += 12 + cl.size
		mm.clusters = append(mm.clusters, *cl)
	}
	mm.duration = end
	mm.cuesPos = pos
	mm.cues = ebmlElem(nil, mkvCues, cues)
}

// headerBytes builds the EBML header, the segment start, the seek index, Info and Tracks.
func (mm *mkvMuxer) headerBytes(cuesRel, segSize int64) ([]byte, error) {
	b := mm.ebmlHeader()
	b = appendSize8(appendID(b, mkvSegment), uint64(segSize))
	var info []byte
	info = ebmlUintElem(info, mkvScale, 1000000)
	info = ebmlElem(info, mkvMuxingApp, []byte("Zipline"))
	info = ebmlElem(info, mkvWritingApp, []byte("Zipline"))
	info = ebmlElem(info, mkvDuration, binary.BigEndian.AppendUint64(nil, math.Float64bits(float64(mm.duration))))
	infoEl := ebmlElem(nil, mkvInfo, info)
	var tracks []byte
	seenDef := map[string]bool{}
	for ti, t := range mm.tracks {
		if mm.nums[ti] == 0 {
			continue
		}
		e, err := mm.trackEntry(ti, t, seenDef)
		if err != nil {
			return nil, err
		}
		tracks = ebmlElem(tracks, mkvTrackEntry, e)
	}
	tracksEl := ebmlElem(nil, mkvTracks, tracks)
	seek := func(info, tracks, cues int64) []byte {
		var body []byte
		for _, s := range [][2]uint64{{mkvInfo, uint64(info)}, {mkvTracks, uint64(tracks)}, {mkvCues, uint64(cues)}} {
			e := ebmlElem(nil, mkvSeekID, appendID(nil, s[0]))
			e = ebmlUint8Elem(e, mkvSeekPos, s[1])
			body = ebmlElem(body, mkvSeek, e)
		}
		return ebmlElem(nil, mkvSeekHead, body)
	}
	sh := seek(0, 0, 0)
	infoRel := int64(len(sh))
	b = append(b, seek(infoRel, infoRel+int64(len(infoEl)), cuesRel)...)
	b = append(b, infoEl...)
	return append(b, tracksEl...), nil
}

func (mm *mkvMuxer) trackEntry(ti int, t *muxTrack, seenDef map[string]bool) ([]byte, error) {
	kind := "audio"
	typ := uint64(2)
	switch {
	case t.video:
		kind, typ = "video", 1
	case t.text:
		kind, typ = "text", 0x11
	}
	e := ebmlUintElem(nil, mkvTrackNumber, mm.nums[ti])
	e = ebmlUintElem(e, mkvTrackUID, mm.nums[ti])
	def := uint64(0) // the first video, the chosen audio (else the first), a chosen subtitle track
	switch {
	case t.text:
		if t.def {
			def = 1
		}
	case !seenDef[kind] && (t.video || t.def || !mm.anyDef()):
		def, seenDef[kind] = 1, true
	}
	e = ebmlUintElem(e, mkvFlagDefault, def)
	if t.forced {
		e = ebmlUintElem(e, mkvFlagForced, 1)
	}
	labelled := t.lang != "" && t.lang != "und"
	if labelled {
		code := t.lang
		if b, ok := iso639B[code]; ok {
			code = b
		}
		e = ebmlElem(e, mkvLanguage, []byte(code))
	}
	if t.name != "" {
		e = ebmlElem(e, mkvName, []byte(t.name))
	}
	if t.mkv != nil { // the input's own entry, apart from what's set above
		eachEBML(t.mkv.entry, func(id uint64, _, raw []byte) error {
			switch id {
			case mkvFlagDefault, mkvFlagForced, mkvName:
			case mkvLanguage, mkvLanguageBCP:
				if !labelled {
					e = append(e, raw...)
				}
			default:
				e = append(e, raw...)
			}
			return nil
		})
		return e, nil
	}
	e = ebmlUintElem(e, mkvTrackType, typ)
	e = ebmlUintElem(e, mkvFlagLacing, 0)
	if t.text {
		// WebM's ID for WebVTT, in .mkv too: it's what ffmpeg writes there, and ffmpeg before 7.0
		// (most players) doesn't know the newer S_TEXT/WEBVTT
		return ebmlElem(e, mkvCodecID, []byte("D_WEBVTT/SUBTITLES")), nil
	}
	if len(t.entries) == 0 {
		return nil, errors.New("remux: track without a sample description")
	}
	id, priv, err := mkvCodec(t.entries[0], t.video)
	if err != nil {
		return nil, err
	}
	e = ebmlElem(e, mkvCodecID, []byte(id))
	if len(priv) > 0 {
		e = ebmlElem(e, mkvCodecPrivate, priv)
	}
	en := t.entries[0]
	if t.video {
		var v []byte
		w, h := 0, 0
		if len(en) >= 36 {
			w, h = int(binary.BigEndian.Uint16(en[32:])), int(binary.BigEndian.Uint16(en[34:]))
		}
		v = ebmlUintElem(v, mkvPixelW, uint64(w))
		v = ebmlUintElem(v, mkvPixelH, uint64(h))
		if t.width > 0 && t.height > 0 && (t.width != w || t.height != h) {
			v = ebmlUintElem(v, mkvDisplayW, uint64(t.width))
			v = ebmlUintElem(v, mkvDisplayH, uint64(t.height))
		}
		return ebmlElem(e, mkvVideo, v), nil
	}
	var a []byte
	rate, ch := float64(t.timescale), 2
	if len(en) >= 36 {
		ch = int(binary.BigEndian.Uint16(en[24:]))
		if r := binary.BigEndian.Uint32(en[32:]) >> 16; r > 0 {
			rate = float64(r)
		}
	}
	if len(t.aCfgs) > 0 {
		rate, ch = float64(t.aCfgs[0].rate), t.aCfgs[0].channels
	}
	a = ebmlElem(a, mkvSampleRate, binary.BigEndian.AppendUint64(nil, math.Float64bits(rate)))
	a = ebmlUintElem(a, mkvChannels, uint64(max(ch, 1)))
	return ebmlElem(e, mkvAudio, a), nil
}

func (mm *mkvMuxer) anyDef() bool {
	for ti, t := range mm.tracks {
		if mm.nums[ti] > 0 && t.def && !t.video && !t.text {
			return true
		}
	}
	return false
}

// writeFile writes the header, the cluster headers and the cues, and lets copy fill in the frames.
func (mm *mkvMuxer) writeFile(out string, copy func(write func(ti int, data []byte) error) error) (err error) {
	f, err := os.OpenFile(out, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(out)
		}
	}()
	w := &offWriter{f: f}
	if err = w.at(0, mm.header); err != nil {
		return err
	}
	for _, c := range mm.clusters {
		b := appendSize8(appendID(nil, mkvCluster), uint64(c.size))
		b = ebmlUintElem(b, mkvTimecode, uint64(c.time))
		if err = w.at(c.off, b); err != nil {
			return err
		}
	}
	err = copy(func(ti int, data []byte) error {
		if ti >= len(mm.tracks) || mm.nums[ti] == 0 {
			return nil
		}
		t := mm.tracks[ti]
		i := mm.written[ti]
		if i >= len(t.samples) {
			return errors.New("remux: the input changed while converting")
		}
		if int(t.samples[i].size) != len(data) {
			return errors.New("remux: a frame changed size")
		}
		mm.written[ti]++
		return w.at(mm.lay[ti][i].off, mm.blockBytes(ti, i, data))
	})
	if err != nil {
		return err
	}
	for ti, t := range mm.tracks {
		if mm.nums[ti] > 0 && mm.written[ti] != len(t.samples) {
			return errors.New("remux: the input changed while converting")
		}
	}
	if err = w.at(mm.cuesPos, mm.cues); err != nil {
		return err
	}
	return w.flush()
}

// blockBytes builds a frame's whole block element.
func (mm *mkvMuxer) blockBytes(ti, i int, data []byte) []byte {
	t := mm.tracks[ti]
	body := mm.blockLen(ti, i)
	var b []byte
	if mm.group(ti, i) {
		b = appendSize(appendID(b, mkvBlockGroup), uint64(mm.groupBody(ti, i, body)))
		b = appendSize(appendID(b, mkvBlockID), uint64(body))
	} else {
		b = appendSize(appendID(b, mkvSimpleBlock), uint64(body))
	}
	b = appendSize(b, mm.nums[ti])
	b = binary.BigEndian.AppendUint16(b, uint16(mm.lay[ti][i].rel))
	if t.mkv == nil {
		flags := byte(0)
		if !mm.group(ti, i) && (t.samples[i].flags&sfSync != 0 || !t.video) {
			flags = 0x80
		}
		b = append(b, flags)
	}
	if p := mm.lay[ti][i].pre; p > 0 {
		b = append(b, mm.pre[ti][p-1]...)
	}
	b = append(b, data...)
	if mm.group(ti, i) {
		if t.text {
			b = ebmlUintElem(b, mkvBlockDur, uint64(t.blockDur[i]))
		} else {
			b = append(b, t.mkv.blocks[i].extra...)
		}
	}
	return b
}

// offWriter collects writes that follow on from each other into larger ones.
type offWriter struct {
	f   *os.File
	off int64
	buf []byte
}

func (w *offWriter) at(off int64, b []byte) error {
	if len(w.buf) > 0 && (off != w.off+int64(len(w.buf)) || len(w.buf) > 4<<20) {
		if err := w.flush(); err != nil {
			return err
		}
	}
	if len(w.buf) == 0 {
		w.off = off
	}
	w.buf = append(w.buf, b...)
	return nil
}

func (w *offWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	_, err := w.f.WriteAt(w.buf, w.off)
	w.buf = w.buf[:0]
	return err
}

// mkvTextSamples turns placed cues into a Matroska text track's frames (ms), each with its own
// duration. A D_WEBVTT frame starts with the cue's identifier and settings lines (left empty).
func mkvTextSamples(t *muxTrack, cues []placedCue) {
	t.samples, t.textData, t.blockDur, t.lastDur = nil, nil, nil, 0
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].start < cues[j].start })
	for _, c := range cues {
		b := append([]byte("\n\n"), c.text...)
		t.samples = append(t.samples, rawSample{dts: c.start, size: uint32(len(b)), flags: sfSync})
		t.textData = append(t.textData, b)
		t.blockDur = append(t.blockDur, c.end-c.start)
	}
	if n := len(cues); n > 0 {
		t.lastDur = max(cues[n-1].end-cues[n-1].start, 1)
	}
}

// ---------------------------------------------------------------- reading with parts

// readMKVParts is readMKV for a file whose parts start at marks: a part may bring its own
// header and track list (a DASH period's init section), and its blocks are marked with it.
func readMKVParts(ctx context.Context, path string, marks []PartMark, progress func(float64)) (*mkvFile, error) {
	if len(marks) == 0 {
		return readMKV(ctx, path, progress)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size := fileSize(path)
	e := &ebmlReader{r: bufio.NewReaderSize(f, 1<<20), limit: size}
	mf := &mkvFile{path: path, scale: 1000000, docType: "webm"}
	byNum := map[uint64]*mkvTrack{}
	cur := newPartCursor(marks)
	mf.part = uint16(cur.part)
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
		if p, changed := cur.at(e.pos); changed {
			mf.part = uint16(p)
		}
		id, sz, err := e.element()
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			if sawTracks {
				break
			}
			return nil, err
		}
		switch id {
		case mkvSegment:
			continue
		case mkvTracks:
			body, err := e.bytes(sz)
			if err != nil {
				return nil, errBadWebM
			}
			if !sawTracks {
				if err := mf.parseTracks(body, byNum); err != nil {
					return nil, err
				}
			} else { // a part's own track list: match its tracks to the first ones by kind and order
				q := &mkvFile{}
				if err := q.parseTracks(body, map[uint64]*mkvTrack{}); err != nil {
					return nil, err
				}
				next := map[uint64]*mkvTrack{}
				used := map[*mkvTrack]bool{}
				for _, nt := range q.tracks {
					for _, t := range mf.tracks {
						if !used[t] && t.video == nt.video && mkvFamily(t.entry) == mkvFamily(nt.entry) {
							next[nt.num], used[t] = t, true
							break
						}
					}
				}
				byNum = next
			}
			sawTracks = true
		case mkvCluster:
			if !sawTracks {
				return nil, errBadWebM
			}
			if err := mf.parseCluster(e, sz, byNum, size); err != nil {
				return nil, err
			}
		case mkvEBML, mkvInfo:
			if _, err := e.bytes(sz); err != nil {
				return nil, errBadWebM
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
	for _, t := range mf.tracks {
		for len(t.blocks) > 0 && t.blocks[len(t.blocks)-1].off+t.blocks[len(t.blocks)-1].size > size {
			t.blocks = t.blocks[:len(t.blocks)-1]
		}
	}
	return mf, nil
}
