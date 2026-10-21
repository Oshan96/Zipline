package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"net/url"
)

// SAMPLE-AES with a plain key (no KEYFORMAT or "identity", the key at an ordinary web address)
// isn't DRM: Apple's "MPEG-2 Stream Encryption Format for HTTP Live Streaming" encrypts parts of
// each H.264 picture and each audio frame with AES-128 in CBC mode, and the key is handed out like
// an AES-128 stream's. Each piece is decrypted as it arrives, so what's saved is a plain stream:
//   - H.264: NAL units of type 1 and 5 longer than 48 bytes. Start code emulation prevention was
//     applied again over the encrypted unit, so it's removed first (once); then after the first
//     32 bytes, one 16-byte block in ten is encrypted (16 encrypted, up to 144 clear). What's left
//     is the original unit, its own emulation prevention bytes included.
//   - AAC (ADTS): after the header and 16 clear bytes, the whole 16-byte blocks; the rest is clear.
//   - AC-3: the same from the start of each syncframe. E-AC-3: per syncframe too, but the chain
//     runs on across the syncframes of one audio frame (1536 samples).
// The CBC chain starts again from the IV for each NAL unit and each audio frame. The IV comes
// from the playlist, else the media sequence number, as for AES-128.

var errSampleAES = errors.New("couldn't decrypt the piece (SAMPLE-AES)")

// sampleCipher runs the CBC chains. enc encrypts instead (the tests' encrypter uses it).
type sampleCipher struct {
	blk cipher.Block
	iv  [16]byte
	enc bool
}

func newSampleCipher(key, iv []byte) (*sampleCipher, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	c := &sampleCipher{blk: blk}
	copy(c.iv[:], iv)
	return c, nil
}

// chain decrypts (or encrypts) the 16-byte blocks in b in place, chaining from iv, and returns
// the chain's state for the next block.
func (c *sampleCipher) chain(b []byte, iv [16]byte) [16]byte {
	var tmp [16]byte
	for len(b) >= 16 {
		blk := b[:16]
		if c.enc {
			for i := range tmp {
				tmp[i] = blk[i] ^ iv[i]
			}
			c.blk.Encrypt(blk, tmp[:])
			copy(iv[:], blk)
		} else {
			copy(tmp[:], blk)
			c.blk.Decrypt(blk, blk)
			for i := range blk {
				blk[i] ^= iv[i]
			}
			iv = tmp
		}
		b = b[16:]
	}
	return iv
}

// plainKeyURL: a key handed out at an ordinary web address (FairPlay's are "skd://").
func plainKeyURL(u string) bool {
	p, err := url.Parse(u)
	return err == nil && (p.Scheme == "http" || p.Scheme == "https") && p.Host != ""
}

// decryptSampleAES decrypts one piece: MPEG-TS, or packed audio (raw frames after an ID3 tag).
func decryptSampleAES(b, key, iv []byte) ([]byte, error) {
	c, err := newSampleCipher(key, iv)
	if err != nil {
		return nil, err
	}
	if len(b) > 0 && b[0] == 0x47 {
		return rewriteTS(b, clearType, c.video, c.audio)
	}
	at := skipID3(b)
	switch {
	case at < len(b)-1 && b[at] == 0xFF:
		c.audio("aac", b[at:])
	case at < len(b)-1 && b[at] == 0x0B && b[at+1] == 0x77:
		c.audio("ac3", b[at:])
	default:
		return nil, errSampleAES
	}
	return b, nil
}

// clearType is the stream type an encrypted stream has once decrypted (and the codec it's
// decrypted as): the encrypted types are 0xDB (H.264), 0xCF (AAC), 0xC1 (AC-3), 0xC2 (E-AC-3).
func clearType(t byte) (byte, string) {
	switch t {
	case 0x1B, 0xDB:
		return 0x1B, "h264"
	case 0x0F, 0xCF:
		return 0x0F, "aac"
	case 0x81, 0xC1:
		return 0x81, "ac3"
	case 0x87, 0xC2:
		return 0x87, "ac3"
	}
	return t, ""
}

// video decrypts the NAL units of one H.264 PES payload; their lengths can change.
func (c *sampleCipher) video(es []byte) []byte {
	out := make([]byte, 0, len(es))
	eachNAL(es, func(gap, nal []byte) {
		out = append(out, gap...)
		if len(nal) == 0 {
			return
		}
		if t := nal[0] & 0x1f; (t == 1 || t == 5) && len(nal) > 48 {
			nal = unescapeEP(nal)
			c.pattern(nal[32:])
		}
		out = append(out, nal...)
	})
	return out
}

// pattern runs one NAL unit's chain over its encrypted blocks: 16 bytes, then up to 144 clear.
func (c *sampleCipher) pattern(b []byte) {
	iv := c.iv
	for len(b) > 0 {
		if len(b) > 16 {
			iv = c.chain(b[:16], iv)
			b = b[16:]
		}
		b = b[min(144, len(b)):]
	}
}

// eachNAL calls fn with each NAL unit of an Annex B stream and the bytes before it (start code,
// zero padding); a last call has what follows the last unit (nal nil). A unit ends where the
// zeros of the next start code begin.
func eachNAL(b []byte, fn func(gap, nal []byte)) {
	from, start := 0, -1
	emit := func(end int) {
		e := end
		for e > start && b[e-1] == 0 {
			e--
		}
		fn(b[from:start], b[start:e])
		from = e
	}
	for i := 0; i+2 < len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				emit(i)
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 {
		emit(len(b))
	}
	fn(b[from:], nil)
}

// unescapeEP removes start code emulation prevention: the 0x03 in each 00 00 03 (as FFmpeg
// does, one that ends the unit stays).
func unescapeEP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if i+3 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 3 {
			out = append(out, 0, 0)
			i += 2
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// audio decrypts the frames of one audio stream in place (their lengths don't change). A frame
// cut off at the end of the piece is left as it is.
func (c *sampleCipher) audio(codec string, b []byte) {
	blocks := 0 // E-AC-3: audio blocks so far in this audio frame
	var iv [16]byte
	for i := 0; i+7 <= len(b); {
		var hdr, size, n int
		if codec == "aac" {
			hdr, size = adtsHeader(b[i:])
		} else if s, k, _, err := ac3Frame(b[i:]); err == nil {
			size, n = s, k
		}
		if size == 0 {
			i++
			continue
		}
		if i+size > len(b) {
			break
		}
		if codec == "aac" || blocks == 0 { // a new audio frame
			iv = c.iv
		}
		if f := b[i+hdr : i+size]; len(f) > 16 {
			body := f[16:]
			iv = c.chain(body[:len(body)/16*16], iv)
		}
		if codec != "aac" { // AC-3 has one syncframe per audio frame, E-AC-3 up to six
			if blocks += n; blocks >= 6 || b[i+5]>>3 <= 8 {
				blocks = 0
			}
		}
		i += size
	}
}

// ---------------------------------------------------------------- rewriting MPEG-TS

// rewriteTS changes the payload of a piece's audio and video streams: retype maps each stream type
// in the program map to the one to write and the codec it holds; video is called with each H.264
// PES payload and returns the new one (its length may change: the PES is then packed into
// packets again); audio gets each audio stream's payload bytes, all of them, to change in place.
func rewriteTS(b []byte, retype func(byte) (byte, string), video func([]byte) []byte, audio func(string, []byte)) ([]byte, error) {
	n := len(b) / 188
	if n == 0 {
		return nil, errSampleAES
	}
	b = append([]byte(nil), b...)
	codecs := map[int]string{}
	pmt := -1
	payload := func(p []byte) int { // where a packet's payload starts (188: none)
		at := 4
		if p[3]&0x20 != 0 {
			at += 1 + int(p[4])
		}
		if p[3]&0x10 == 0 || at > 188 {
			return 188
		}
		return at
	}
	type unit struct{ pkts []int }
	units := map[int][]*unit{}
	for i := 0; i < n; i++ {
		p := b[i*188 : i*188+188]
		if p[0] != 0x47 {
			return nil, errSampleAES
		}
		pid := int(p[1]&0x1f)<<8 | int(p[2])
		pusi := p[1]&0x40 != 0
		at := payload(p)
		switch {
		case at >= 188:
		case pid == 0 && pusi:
			if sec := psiSection(p[at:]); len(sec) >= 12 {
				for k := 8; k+4 <= len(sec)-4; k += 4 {
					if prog := int(sec[k])<<8 | int(sec[k+1]); prog != 0 {
						pmt = int(sec[k+2]&0x1f)<<8 | int(sec[k+3])
						break
					}
				}
			}
		case pid == pmt && pusi:
			sec := psiSection(p[at:])
			changed := false
			for _, es := range pmtStreams(sec) {
				t, codec := retype(es.typ)
				if codec != "" {
					codecs[es.pid] = codec
				}
				if t != es.typ {
					sec[es.at], changed = t, true
				}
			}
			if changed {
				binary.BigEndian.PutUint32(sec[len(sec)-4:], crc32MPEG(sec[:len(sec)-4]))
			}
		case codecs[pid] != "":
			u := units[pid]
			if pusi || len(u) == 0 {
				u = append(u, &unit{})
				units[pid] = u
			}
			u[len(u)-1].pkts = append(u[len(u)-1].pkts, i)
		}
	}
	// audio: its payload bytes, gathered across packets, changed in place and put back
	for pid, us := range units {
		if codecs[pid] == "h264" {
			continue
		}
		var es []byte
		var where [][2]int
		for _, u := range us {
			skip := -1 // PES header bytes still to skip
			for _, i := range u.pkts {
				p := b[i*188 : i*188+188]
				at := payload(p)
				if skip < 0 {
					skip = 0
					if pl := p[at:]; p[1]&0x40 != 0 && len(pl) >= 9 && pl[0] == 0 && pl[1] == 0 && pl[2] == 1 {
						skip = 9 + int(pl[8])
					}
				}
				d := min(skip, 188-at)
				at, skip = at+d, skip-d
				if at < 188 {
					es = append(es, p[at:]...)
					where = append(where, [2]int{i*188 + at, i*188 + 188})
				}
			}
		}
		audio(codecs[pid], es)
		for _, w := range where {
			es = es[copy(b[w[0]:w[1]], es):]
		}
	}
	// video: each PES on its own; one whose length changed is packed again
	redo := map[int][]byte{} // first packet of a PES → its new packets
	drop := map[int]bool{}
	for pid, us := range units {
		if codecs[pid] != "h264" {
			continue
		}
		for _, u := range us {
			first := b[u.pkts[0]*188 : u.pkts[0]*188+188]
			if first[1]&0x40 == 0 {
				continue // the piece starts part-way through a PES: leave it
			}
			var pes []byte
			for _, i := range u.pkts {
				p := b[i*188 : i*188+188]
				pes = append(pes, p[payload(p):]...)
			}
			if len(pes) < 9 || pes[0] != 0 || pes[1] != 0 || pes[2] != 1 || 9+int(pes[8]) > len(pes) {
				continue
			}
			hl := 9 + int(pes[8])
			body := pes[hl:]
			if plen := int(pes[4])<<8 | int(pes[5]); plen > 0 && 6+plen < len(pes) {
				body = pes[hl : 6+plen]
			}
			nb := video(append([]byte(nil), body...))
			if len(nb) == len(body) {
				copy(body, nb)
				at := 0
				for _, i := range u.pkts {
					p := b[i*188 : i*188+188]
					at += copy(p[payload(p):], pes[at:])
				}
				continue
			}
			np := append(append([]byte(nil), pes[:hl]...), nb...)
			if plen := int(pes[4])<<8 | int(pes[5]); plen > 0 {
				l := len(np) - 6
				if l > 0xFFFF {
					l = 0 // allowed for video
				}
				binary.BigEndian.PutUint16(np[4:], uint16(l))
			}
			redo[u.pkts[0]] = packPES(first, np)
			for _, i := range u.pkts[1:] {
				drop[i] = true
			}
		}
	}
	if len(redo) == 0 {
		return b, nil
	}
	out := make([]byte, 0, len(b)+len(b)/8)
	cc := map[int]byte{}
	for i := 0; i < n; i++ {
		p := b[i*188 : i*188+188]
		pid := int(p[1]&0x1f)<<8 | int(p[2])
		var pkts []byte
		switch {
		case redo[i] != nil:
			pkts = redo[i]
		case drop[i]:
			if p[3]&0x20 == 0 || p[4] == 0 || p[5]&0x10 == 0 {
				continue
			}
			// it carried a clock reference: keep that, in a packet of its own with no payload
			q := append([]byte(nil), p...)
			q[1] &^= 0x40
			q[3] = q[3]&0xC0 | 0x20 | q[3]&0x0F
			q[4], q[5] = 183, q[5]&0xF0 // (only the clock reference is kept among its fields)
			for k := 12; k < 188; k++ {
				q[k] = 0xFF
			}
			pkts = q
		default:
			pkts = p
		}
		if codecs[pid] == "h264" { // the number of packets changed: count them again
			for k := 0; k < len(pkts); k += 188 {
				q := pkts[k : k+188]
				if q[3]&0x10 == 0 {
					q[3] = q[3]&0xF0 | cc[pid]
					continue
				}
				v, ok := cc[pid]
				if !ok {
					v = (q[3] & 0x0F) + 15
				}
				cc[pid] = (v + 1) & 0x0F
				q[3] = q[3]&0xF0 | cc[pid]
			}
		}
		out = append(out, pkts...)
	}
	return append(out, b[n*188:]...), nil
}

// packPES packs a PES into packets like first (the PES's original first packet): same PID, and
// the same adaptation field fields (clock reference, random access) on the first packet.
func packPES(first, pes []byte) []byte {
	var af []byte // the first packet's adaptation field without its stuffing
	if first[3]&0x20 != 0 && first[4] > 0 {
		f := first[5]
		l := 1
		if f&0x10 != 0 {
			l += 6
		}
		if f&0x08 != 0 {
			l += 6
		}
		if f&0x04 != 0 {
			l++
		}
		if f&0x02 != 0 && 5+l < 188 {
			l += 1 + int(first[5+l])
		}
		if f&0x01 != 0 && 5+l < 188 {
			l += 1 + int(first[5+l])
		}
		l = min(l, int(first[4]))
		af = append([]byte(nil), first[5:5+l]...)
	}
	var out []byte
	for k := 0; k == 0 || len(pes) > 0; k++ {
		p := make([]byte, 188)
		p[0], p[1], p[2] = 0x47, first[1]&0x1F, first[2]
		var field []byte // the adaptation field after its length byte
		hasAF := false
		if k == 0 {
			p[1] |= 0x40
			field, hasAF = af, len(af) > 0
		}
		room := 184
		if hasAF {
			room -= 1 + len(field)
		}
		if s := room - len(pes); s > 0 { // the last packet: stuffing fills it up
			switch {
			case hasAF:
				field = append(field, bytes.Repeat([]byte{0xFF}, s)...)
			case s == 1:
				hasAF = true // just the length byte
			default:
				hasAF, field = true, append([]byte{0}, bytes.Repeat([]byte{0xFF}, s-2)...)
			}
		}
		at := 4
		p[3] = 0x10
		if hasAF {
			p[3], p[4] = 0x30, byte(len(field))
			copy(p[5:], field)
			at = 5 + len(field)
		}
		pes = pes[copy(p[at:], pes):]
		out = append(out, p...)
	}
	return out
}

// crc32MPEG is the CRC that ends each program table (MPEG-2: polynomial 0x04C11DB7, not reflected).
func crc32MPEG(b []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, x := range b {
		crc ^= uint32(x) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04C11DB7
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
