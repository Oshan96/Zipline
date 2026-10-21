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

// Repackaging ("remuxing") of downloaded streams into MP4, done natively so Zipline doesn't
// need ffmpeg. The audio and video data is copied unchanged; only the container changes.
// Several inputs can be combined, e.g. a video-only stream and its separate audio track.

type remuxResult struct {
	video, audio bool
	ext          string // the file type written (streamout.go), "" = MP4
}

// muxSource is one downloaded file: MPEG-TS, fragmented MP4, or raw ("packed") audio.
type muxSource interface {
	// scan lists the tracks and frames (first pass).
	scan(ctx context.Context, progress func(float64)) ([]*muxTrack, error)
	// copy hands every frame's data to write, track by track in order (second pass).
	copy(ctx context.Context, progress func(float64), write func(track int, data []byte) error) error
	size() int64
}

func fileSize(path string) int64 {
	if st, err := os.Stat(path); err == nil {
		return st.Size()
	}
	return 0
}

// openSource looks at the start of a file to pick the right reader.
func openSource(path string) (muxSource, error) { return openSourceParts(path, nil) }

// openSourceParts is openSource for a file whose parts start at marks (discont.go).
func openSourceParts(path string, marks []PartMark) (muxSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, 192*4)
	n, _ := io.ReadFull(f, b)
	b = b[:n]
	switch {
	case n >= 8 && bytes.Contains([]byte("ftyp styp moov moof sidx free"), b[4:8]):
		return &fmp4Source{path: path, marks: marks}, nil
	case n >= 4 && binary.BigEndian.Uint32(b) == mkvEBML:
		return &mkvSource{path: path, marks: marks}, nil
	case n >= 3 && string(b[:3]) == "ID3", n >= 2 && (b[0] == 0xFF && b[1]&0xE0 == 0xE0 || b[0] == 0x0B && b[1] == 0x77):
		return &packedSource{path: path, marks: marks}, nil
	}
	return &tsSource{path: path, marks: marks}, nil // the TS reader finds the sync itself or says it isn't TS
}

// ---------------------------------------------------------------- MPEG-TS

type tsSource struct {
	path  string
	marks []PartMark
	keep  func(part int) bool // the parts to copy (nil: all)
}

func (s *tsSource) size() int64 { return fileSize(s.path) }

func scanTS(ctx context.Context, path string, marks []PartMark, progress func(float64), emit func(*tsTrack, rawSample, []byte) error) ([]*tsTrack, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	d := newTSDemuxer(ctx, f, st.Size(), progress, emit)
	if len(marks) > 0 {
		d.parts = newPartCursor(marks)
		d.part = uint16(d.parts.part)
	}
	err = d.run()
	return d.tracks, err
}

func (s *tsSource) scan(ctx context.Context, progress func(float64)) ([]*muxTrack, error) {
	tracks, err := scanTS(ctx, s.path, s.marks, progress, func(t *tsTrack, smp rawSample, _ []byte) error {
		t.samples = append(t.samples, smp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tsMuxTracks(tracks)
}

func tsMuxTracks(tracks []*tsTrack) ([]*muxTrack, error) {
	var out []*muxTrack
	for _, t := range tracks {
		mt := &muxTrack{video: t.video, samples: t.samples, clock33: true}
		if t.video {
			mt.timescale = 90000
			for i := range t.entries {
				e, w, h, err := videoEntry(t.codec, &t.entries[i])
				if err != nil {
					return nil, unsupported("the video settings couldn't be read (%v)", err)
				}
				if i == 0 {
					mt.width, mt.height = w, h
				}
				mt.entries = append(mt.entries, e)
			}
		} else if len(t.cfgs) > 0 {
			mt.timescale = uint32(t.cfgs[0].rate)
			mt.aCfgs = t.cfgs
			mt.lastDur = int64(t.cfgs[0].samples)
		} else {
			mt.samples = nil
		}
		out = append(out, mt)
	}
	return out, nil
}

func (s *tsSource) copy(ctx context.Context, progress func(float64), write func(int, []byte) error) error {
	_, err := scanTS(ctx, s.path, s.marks, progress, func(t *tsTrack, smp rawSample, data []byte) error {
		if s.keep != nil && !s.keep(int(smp.part)) {
			return nil
		}
		return write(t.idx, data)
	})
	return err
}

// ---------------------------------------------------------------- fragmented MP4

type fmp4Source struct {
	path   string
	marks  []PartMark
	tracks []*fmp4Track
}

func (s *fmp4Source) size() int64 { return fileSize(s.path) }

func (s *fmp4Source) scan(ctx context.Context, progress func(float64)) ([]*muxTrack, error) {
	tracks, err := readFMP4Parts(ctx, s.path, s.marks, progress)
	if err != nil {
		return nil, err
	}
	s.tracks = tracks
	var out []*muxTrack
	for _, t := range tracks {
		out = append(out, &muxTrack{video: t.video, timescale: t.timescale, width: t.width, height: t.height,
			entries: t.entries, samples: t.samples, lastDur: t.lastDur, lang: t.lang,
			hasEdit: t.hasEdit, editEmpty: t.editEmpty, editMedia: t.editMedia, partShift: t.partShift})
	}
	return out, nil
}

func (s *fmp4Source) copy(ctx context.Context, progress func(float64), write func(int, []byte) error) error {
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	defer f.Close()
	total := s.size()
	r := newSeqReader(f)
	var done int64
	for ti, t := range s.tracks {
		for i := range t.samples {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
				if progress != nil && total > 0 {
					progress(float64(done) / float64(total))
				}
			}
			b, err := r.read(t.offs[i], int(t.samples[i].size))
			if err != nil {
				return err
			}
			done += int64(len(b))
			if err := write(ti, b); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- packed audio

// packedSource reads HLS "packed audio": raw AAC/MP3/AC-3 frames, each piece starting with an
// ID3 tag that holds its timestamp on the same 90 kHz clock as MPEG-TS.
type packedSource struct {
	path  string
	marks []PartMark
	keep  func(part int) bool
}

func (s *packedSource) size() int64 { return fileSize(s.path) }

const id3TimestampOwner = "com.apple.streaming.transportStreamTimestamp\x00"

func (s *packedSource) run(ctx context.Context, progress func(float64), emit func(*tsTrack, rawSample, []byte) error) (*tsTrack, bool, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	size := s.size()
	br := bufio.NewReaderSize(f, 1<<20)
	d := newTSDemuxer(ctx, nil, size, progress, emit)
	t := &tsTrack{}
	floating := false
	parts := newPartCursor(s.marks)
	d.part = uint16(parts.part)
	var buf []byte
	var pos int
	var consumed int64
	eof := false
	fill := func(need int) bool { // make sure buf[pos:] has need bytes
		for len(buf)-pos < need && !eof {
			if pos > 0 && pos > len(buf)/2 {
				consumed += int64(pos)
				buf = append(buf[:0], buf[pos:]...)
				pos = 0
			}
			chunk := make([]byte, 256<<10)
			n, err := br.Read(chunk)
			buf = append(buf, chunk[:n]...)
			if err != nil {
				eof = true
			}
		}
		return len(buf)-pos >= need
	}
	for steps := 1; ; steps++ {
		if steps&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
			if progress != nil && size > 0 {
				progress(float64(consumed+int64(pos)) / float64(size))
			}
		}
		fill(10)
		if len(buf)-pos < 2 {
			break
		}
		if p, changed := parts.at(consumed + int64(pos)); changed { // a new part: its own timestamps
			if err := d.flushEAC3(t); err != nil {
				return nil, false, err
			}
			d.newEpoch(t)
			t.havePend, d.part = false, uint16(p)
			if t.aStarted {
				t.aStarted, t.rate = false, 0
			}
		}
		b := buf[pos:]
		if len(b) >= 10 && string(b[:3]) == "ID3" { // a new piece: read its timestamp
			n := 10 + (int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f))
			if b[5]&0x10 != 0 {
				n += 10
			}
			if !fill(n) {
				break
			}
			tag := buf[pos : pos+n]
			if i := bytes.Index(tag, []byte(id3TimestampOwner)); i >= 0 && i+len(id3TimestampOwner)+8 <= len(tag) {
				ts := int64(binary.BigEndian.Uint64(tag[i+len(id3TimestampOwner):]) & (1<<33 - 1))
				t.pend, t.havePend = d.unwrap(ts), true
			}
			pos += n
			continue
		}
		if t.codec == "" {
			switch {
			case b[0] == 0xFF && b[1]&0xF6 == 0xF0:
				t.codec = "aac"
			case b[0] == 0xFF && b[1]&0xE0 == 0xE0:
				t.codec = "mp3"
			case b[0] == 0x0B && b[1] == 0x77:
				t.codec = "ac3"
			default:
				pos++
				continue
			}
		}
		hdr, size, n, cfg, err := t.audioFrame(b)
		if err == errShort {
			if !fill(len(b) + 1) {
				break
			}
			continue
		}
		if err == nil && !fill(size+2) && len(buf)-pos < size {
			break // the last frame is cut short
		}
		b = buf[pos:]
		if err == errNoSync || (err == nil && len(b) >= size+3 && !t.syncAt(b[size:]) && string(b[size:size+3]) != "ID3") {
			pos++
			continue
		}
		if err != nil {
			return nil, false, err
		}
		if t.havePend {
			d.audioPTS(t, t.pend, cfg.rate)
			t.havePend = false
		}
		if !t.aStarted && len(t.cfgs) == 0 { // no timestamps at all: line it up with the video later
			t.aStarted, t.rate, floating = true, cfg.rate, true
		} else if !t.aStarted { // a part without a timestamp of its own: carry on
			t.aStarted, t.rate = true, cfg.rate
		}
		if err := d.addAudio(t, cfg, b[hdr:size], n); err != nil {
			return nil, false, err
		}
		pos += size
	}
	if err := d.flushEAC3(t); err != nil {
		return nil, false, err
	}
	if len(t.cfgs) == 0 {
		return nil, false, unsupported("no audio was found in the audio track")
	}
	return t, floating, nil
}

func (s *packedSource) scan(ctx context.Context, progress func(float64)) ([]*muxTrack, error) {
	t, floating, err := s.run(ctx, progress, func(t *tsTrack, smp rawSample, _ []byte) error {
		t.samples = append(t.samples, smp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	out, err := tsMuxTracks([]*tsTrack{t})
	if err == nil {
		out[0].floating = floating
	}
	return out, err
}

func (s *packedSource) copy(ctx context.Context, progress func(float64), write func(int, []byte) error) error {
	_, _, err := s.run(ctx, progress, func(_ *tsTrack, smp rawSample, data []byte) error {
		if s.keep != nil && !s.keep(int(smp.part)) {
			return nil
		}
		return write(0, data)
	})
	return err
}

// ---------------------------------------------------------------- putting it together

func stageProgress(p func(float64), from, to float64) func(float64) {
	return func(x float64) {
		if p != nil {
			p(from + (to-from)*min(max(x, 0), 1))
		}
	}
}

// remuxFiles combines the inputs into one MP4 at out. When skipAudio[i] is set, audio
// tracks from input i are left out (a separate audio track replaces them).
func remuxFiles(ctx context.Context, out string, paths []string, skipAudio []bool, progress func(float64)) (res remuxResult, err error) {
	var srcs []muxSource
	var total int64
	for _, p := range paths {
		s, err := openSource(p)
		if err != nil {
			return res, err
		}
		srcs = append(srcs, s)
		total += max(s.size(), 1)
	}
	share := func(i int) (float64, float64) { // this input's slice of a pass
		var before int64
		for _, s := range srcs[:i] {
			before += max(s.size(), 1)
		}
		return float64(before) / float64(total), float64(before+max(srcs[i].size(), 1)) / float64(total)
	}
	mx := &mp4Muxer{}
	base := make([]int, len(srcs))
	for i, s := range srcs {
		a, b := share(i)
		tracks, err := s.scan(ctx, stageProgress(progress, 0.4*a, 0.4*b))
		if err != nil {
			return res, err
		}
		base[i] = len(mx.tracks)
		for _, t := range tracks {
			t.src = i
			if i < len(skipAudio) && skipAudio[i] && !t.video {
				t.skip = true
			}
			mx.tracks = append(mx.tracks, t)
		}
	}
	for _, t := range mx.tracks {
		if !t.skip && len(t.samples) > 0 {
			res.video = res.video || t.video
			res.audio = res.audio || !t.video
		}
	}
	if err := mx.prepare(true); err != nil {
		return res, err
	}
	return res, writeMP4(out, mx, func(f *os.File) error {
		for i, s := range srcs {
			a, b := share(i)
			err := s.copy(ctx, stageProgress(progress, 0.4+0.6*a, 0.4+0.6*b), func(ti int, data []byte) error {
				return mx.write(f, base[i]+ti, data)
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// remuxTS converts one MPEG-TS file into MP4.
func remuxTS(ctx context.Context, in, out string, progress func(float64)) (remuxResult, error) {
	return remuxFiles(ctx, out, []string{in}, nil, progress)
}

// writeMP4 creates out, writes the header and lets copy fill in the frames. On failure
// the half-written file is removed.
func writeMP4(out string, mx *mp4Muxer, copy func(f *os.File) error) (err error) {
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
	if _, err = f.WriteAt(mx.header, 0); err != nil {
		return err
	}
	if err = copy(f); err != nil {
		return err
	}
	return mx.complete()
}

// isMP4File reports whether the file starts with an MP4 header.
func isMP4File(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [8]byte
	if _, err := io.ReadFull(f, b[:]); err != nil {
		return false
	}
	return string(b[4:8]) == "ftyp"
}

// friendlyRemuxErr turns a conversion error into a short note for the download list.
func friendlyRemuxErr(err error) string {
	switch {
	case errors.Is(err, errUnsupportedStream):
		msg := err.Error()
		if i := len(errUnsupportedStream.Error()) + 2; len(msg) > i {
			msg = msg[i:]
		}
		return msg
	case isDiskFull(err):
		return "there isn't enough free disk space"
	}
	return err.Error()
}

func isDiskFull(err error) bool {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	s := err.Error()
	return s == "no space left on device" || s == "There is not enough space on the disk."
}
