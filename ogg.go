package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"os"
)

// Ogg Opus (.opus), for a stream's sound alone when it's Opus: the file every player knows for
// Opus (RFC 7845). The packets are copied unchanged; Ogg only needs to know how many samples
// each one holds, which its first byte says (RFC 6716 §3.1).

var oggCRC = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for k := 0; k < 8; k++ {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return
}()

// opusSamples is how many 48 kHz samples an Opus packet decodes to.
func opusSamples(p []byte) int {
	if len(p) == 0 {
		return 0
	}
	cfg := int(p[0] >> 3)
	var frame int // in 1/400 s units of 120 samples
	switch {
	case cfg < 12:
		frame = []int{480, 960, 1920, 2880}[cfg%4]
	case cfg < 16:
		frame = []int{480, 960}[cfg%2]
	default:
		frame = []int{120, 240, 480, 960}[cfg%4]
	}
	switch p[0] & 3 {
	case 0:
		return frame
	case 1, 2:
		return 2 * frame
	}
	if len(p) < 2 {
		return 0
	}
	return frame * int(p[1]&0x3F)
}

type oggWriter struct {
	w      *bufio.Writer
	serial uint32
	seq    uint32
	segs   []byte
	data   []byte
	gran   int64
	first  bool
}

func (o *oggWriter) packet(p []byte) {
	for n := len(p); ; n -= 255 {
		if n < 255 {
			o.segs = append(o.segs, byte(n))
			break
		}
		o.segs = append(o.segs, 255)
	}
	o.data = append(o.data, p...)
}

// page writes what's collected as one page. flags: 2 = first, 4 = last.
func (o *oggWriter) page(flags byte) error {
	h := []byte("OggS")
	h = append(h, 0, flags)
	h = binary.LittleEndian.AppendUint64(h, uint64(o.gran))
	h = binary.LittleEndian.AppendUint32(h, o.serial)
	h = binary.LittleEndian.AppendUint32(h, o.seq)
	h = append(h, 0, 0, 0, 0, byte(len(o.segs)))
	h = append(h, o.segs...)
	var crc uint32
	for _, part := range [][]byte{h, o.data} {
		for _, c := range part {
			crc = crc<<8 ^ oggCRC[byte(crc>>24)^c]
		}
	}
	binary.LittleEndian.PutUint32(h[22:], crc)
	o.seq++
	if _, err := o.w.Write(h); err != nil {
		return err
	}
	_, err := o.w.Write(o.data)
	o.segs, o.data = o.segs[:0], o.data[:0]
	return err
}

// writeOggOpus writes the one Opus track of the job as an Ogg Opus file.
func writeOggOpus(ctx context.Context, out string, order []*muxTrack, srcs []muxSource, idx [][]int, progress func(float64)) (err error) {
	ti := -1
	for i, t := range order {
		if !t.video && !t.text && !t.skip && len(t.samples) > 0 {
			ti = i
			break
		}
	}
	if ti < 0 {
		return unsupported("no audio was found")
	}
	t := order[ti]
	var head []byte
	if t.mkv != nil {
		eachEBML(t.mkv.entry, func(id uint64, b, _ []byte) error {
			if id == mkvCodecPrivate {
				head = b
			}
			return nil
		})
	} else if len(t.entries) > 0 {
		_, kids := entryKids(t.entries[0], false)
		head = opusHead(childBox(kids, "dOps"))
	}
	if len(head) < 19 || string(head[:8]) != "OpusHead" {
		return unsupported("the Opus settings couldn't be read")
	}
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
	o := &oggWriter{w: bufio.NewWriterSize(f, 1<<20), serial: 0x5A4C4F47}
	o.packet(head)
	if err = o.page(2); err != nil {
		return err
	}
	tags := append([]byte("OpusTags"), 7, 0, 0, 0)
	tags = append(append(tags, "Zipline"...), 0, 0, 0, 0)
	o.packet(tags)
	if err = o.page(0); err != nil {
		return err
	}
	var pending []byte // the last packet waits, so the last page can say it's the last
	for i, s := range srcs {
		err = s.copy(ctx, stageProgress(progress, 0.4+0.6*float64(i)/float64(len(srcs)), 0.4+0.6*float64(i+1)/float64(len(srcs))), func(k int, data []byte) error {
			if k >= len(idx[i]) || idx[i][k] != ti {
				return nil
			}
			if t.mkv != nil { // the block's flags byte comes first
				if len(data) < 1 || data[0]&0x06 != 0 {
					return unsupported("laced Opus blocks aren't supported")
				}
				data = data[1:]
			}
			if pending != nil {
				o.packet(pending)
				o.gran += int64(opusSamples(pending))
				if len(o.segs) > 200 || len(o.data) > 32<<10 {
					if err := o.page(0); err != nil {
						return err
					}
				}
			}
			pending = append(pending[:0], data...)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if pending == nil {
		return errors.New("remux: no Opus packets")
	}
	o.packet(pending)
	o.gran += int64(opusSamples(pending))
	if err = o.page(4); err != nil {
		return err
	}
	return o.w.Flush()
}
