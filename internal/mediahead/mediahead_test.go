package mediahead

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// ---- MP4 boxes

func bx(typ string, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	b := binary.BigEndian.AppendUint32(nil, uint32(8+len(body)))
	return append(append(b, typ...), body...)
}

func u32(vs ...uint32) []byte {
	var b []byte
	for _, v := range vs {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return b
}

func mvhd(scale, dur uint32) []byte { return bx("mvhd", u32(0, 0, 0, scale, dur), make([]byte, 80)) }

func trak(handler, entry string, w, h uint32, extra ...[]byte) []byte {
	tkhd := bx("tkhd", u32(0, 0, 0, 1, 0, 0), make([]byte, 52), u32(w<<16, h<<16))
	size := 78 // a visual sample entry's own fields
	if handler == "soun" {
		size = 28
	}
	e := bx(entry, append(make([]byte, size), bytes.Join(extra, nil)...))
	stsd := bx("stsd", u32(0, 1), e)
	return bx("trak", tkhd, bx("mdia", bx("mdhd", u32(0, 0, 0, 1000, 0, 0)), bx("hdlr", u32(0, 0), []byte(handler), make([]byte, 13)),
		bx("minf", bx("stbl", stsd))))
}

func sidx(scale uint32, durs ...uint32) []byte {
	b := u32(0, 1, scale, 0, 0)
	b = append(b, 0, 0, byte(len(durs)>>8), byte(len(durs)))
	for _, d := range durs {
		b = append(b, u32(1000, d, 0x90000000)...)
	}
	return bx("sidx", b)
}

var ftyp = bx("ftyp", []byte("iso6"), u32(0), []byte("iso6dash"))

func videoMP4() []byte {
	return bytes.Join([][]byte{ftyp, bx("moov", mvhd(1000, 0), bx("mvex", bx("trex", make([]byte, 24))), trak("vide", "avc1", 1280, 720)),
		sidx(1000, 2000, 2000, 2002), bx("moof", make([]byte, 40)), bx("mdat", make([]byte, 5000))}, nil)
}

// ---- WebM elements

func el(id uint32, parts ...[]byte) []byte {
	body := bytes.Join(parts, nil)
	var b []byte
	for s := 24; s >= 0; s -= 8 {
		if c := byte(id >> s); c != 0 || len(b) > 0 {
			b = append(b, c)
		}
	}
	b = append(b, 0x01) // an 8-byte size
	return append(append(b, binary.BigEndian.AppendUint64(nil, uint64(len(body)))[1:]...), body...)
}

func webm(tracks ...[]byte) []byte {
	ebml := el(idEBML, el(idDocType, []byte("webm")))
	info := el(idInfo, el(idScale, []byte{0x0F, 0x42, 0x40}), el(idDuration, binary.BigEndian.AppendUint64(nil, math.Float64bits(6000))))
	seg := append([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, // (a size not known: a live writer's)
		bytes.Join([][]byte{el(0xEC, make([]byte, 30)), info, el(idTracks, tracks...), el(idCluster, make([]byte, 100))}, nil)...)
	return append(ebml, seg...)
}

func webmTrackEl(typ byte, codec string, more ...[]byte) []byte {
	return el(idTrack, append([][]byte{el(idTrackType, []byte{typ}), el(idCodecID, []byte(codec))}, more...)...)
}

// limited is a reader that notes how far into the file it was asked to read.
type limited struct {
	*bytes.Reader
	far int64
}

func (l *limited) ReadAt(p []byte, off int64) (int, error) {
	l.far = max(l.far, off+int64(len(p)))
	return l.Reader.ReadAt(p, off)
}

func TestMP4(t *testing.T) {
	v := videoMP4()
	r := &limited{Reader: bytes.NewReader(v)}
	info, err := Read(r, int64(len(v)))
	if err != nil || !info.Video || info.Audio || info.Width != 1280 || info.Height != 720 || !info.Fragmented || info.Encrypted ||
		math.Abs(info.Duration-6.002) > 1e-9 || len(info.Codecs) != 1 || info.Codecs[0] != "avc1" || info.Container != "mp4" {
		t.Fatalf("picture only: %+v %v", info, err)
	}
	if r.far > int64(len(v))-5000 {
		t.Errorf("read into the media itself: up to %d of %d", r.far, len(v))
	}

	// an index before each fragment: the first covers only the first, so the length isn't known
	frags := bytes.Join([][]byte{ftyp, bx("moov", mvhd(1000, 0), bx("mvex"), trak("vide", "avc1", 640, 360)), sidx(1000, 2000),
		bx("moof", make([]byte, 40)), bx("mdat", make([]byte, 300<<10)), sidx(1000, 2000), bx("moof", make([]byte, 40)), bx("mdat", make([]byte, 300<<10))}, nil)
	if info, err := Read(bytes.NewReader(frags), int64(len(frags))); err != nil || info.Duration != 0 || !info.Video {
		t.Fatalf("a partial index: %+v %v", info, err)
	}

	a := bytes.Join([][]byte{ftyp, bx("moov", mvhd(1000, 6010), bx("mvex"), trak("soun", "mp4a", 0, 0))}, nil)
	if info, err := Read(bytes.NewReader(a), -1); err != nil || info.Video || !info.Audio || info.Duration != 6.01 || info.Codecs[0] != "mp4a" {
		t.Fatalf("sound only: %+v %v", info, err)
	}

	// a whole file whose index comes after the media; a 64-bit box size
	big := append(append(u32(1), "mdat"...), binary.BigEndian.AppendUint64(nil, 16+3000)...)
	big = append(big, make([]byte, 3000)...)
	whole := bytes.Join([][]byte{ftyp, big, bx("moov", mvhd(600, 3000), trak("vide", "hvc1", 1920, 1080), trak("soun", "Opus", 0, 0))}, nil)
	if info, err := Read(bytes.NewReader(whole), int64(len(whole))); err != nil || !info.Video || !info.Audio || info.Fragmented || info.Duration != 5 ||
		info.Height != 1080 || len(info.Codecs) != 2 || info.Codecs[1] != "opus" {
		t.Fatalf("a whole file: %+v %v", info, err)
	}

	// encrypted: the entry says encv, its sinf the original format
	enc := bytes.Join([][]byte{ftyp, bx("moov", mvhd(1000, 1000), trak("vide", "encv", 640, 360, bx("sinf", bx("frma", []byte("avc1")))))}, nil)
	if info, err := Read(bytes.NewReader(enc), -1); err != nil || !info.Encrypted || info.Codecs[0] != "avc1" {
		t.Fatalf("encrypted: %+v %v", info, err)
	}
	pssh := bytes.Join([][]byte{ftyp, bx("moov", mvhd(1000, 1000), bx("pssh", make([]byte, 20)), trak("soun", "mp4a", 0, 0))}, nil)
	if info, _ := Read(bytes.NewReader(pssh), -1); !info.Encrypted {
		t.Fatal("a DRM system's box wasn't seen")
	}

	// pieces without their index; something that isn't media
	if _, err := Read(bytes.NewReader(bytes.Join([][]byte{bx("styp", []byte("msdh")), bx("moof"), bx("mdat", make([]byte, 50))}, nil)), -1); !errors.Is(err, ErrNoIndex) {
		t.Errorf("a piece: %v", err)
	}
	if _, err := Read(bytes.NewReader([]byte("<!doctype html><html>…")), -1); !errors.Is(err, ErrUnknown) {
		t.Errorf("a page: %v", err)
	}
}

func TestWebM(t *testing.T) {
	video := webmTrackEl(1, "V_VP9", el(idVideo, el(idWidth, []byte{0x02, 0x80}), el(idHeight, []byte{0x01, 0x68})))
	b := webm(video)
	r := &limited{Reader: bytes.NewReader(b)}
	info, err := Read(r, int64(len(b)))
	if err != nil || !info.Video || info.Audio || info.Width != 640 || info.Height != 360 || info.Duration != 6 || info.Codecs[0] != "vp09" || info.Container != "webm" {
		t.Fatalf("picture only: %+v %v", info, err)
	}
	if r.far > int64(len(b))-90 {
		t.Errorf("read into the clusters: up to %d of %d", r.far, len(b))
	}
	if info, err := Read(bytes.NewReader(webm(webmTrackEl(2, "A_OPUS"))), -1); err != nil || info.Video || !info.Audio || info.Codecs[0] != "opus" {
		t.Fatalf("sound only: %+v %v", info, err)
	}
	enc := webmTrackEl(2, "A_VORBIS", el(idEncodings, el(idEncoding, el(idEncrypt, el(0x47E1, []byte{5})))))
	if info, _ := Read(bytes.NewReader(webm(enc)), -1); !info.Encrypted || info.Codecs[0] != "vorbis" {
		t.Fatalf("encrypted: %+v", info)
	}
}

// Cut short or scrambled, a header is an error, never a crash.
func TestDamaged(t *testing.T) {
	for _, b := range [][]byte{videoMP4(), webm(webmTrackEl(1, "V_VP8"), webmTrackEl(2, "A_OPUS"))} {
		for n := 0; n < len(b); n += 3 {
			Read(bytes.NewReader(b[:n]), int64(n))
			Read(bytes.NewReader(b[:n]), -1)
			x := bytes.Clone(b)
			x[n] ^= 0xA5
			Read(bytes.NewReader(x), int64(len(x)))
		}
	}
}
