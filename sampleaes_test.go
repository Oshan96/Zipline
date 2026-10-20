package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests' SAMPLE-AES encrypter, written from Apple's "MPEG-2 Stream Encryption Format for HTTP
// Live Streaming" on its own (only the MPEG-TS packet handling is Zipline's): H.264 slices (NAL
// types 1 and 5, longer than 48 bytes) keep their first 32 bytes clear, then one 16-byte block in
// ten is encrypted, chained within the unit, and emulation prevention is applied again over the
// unit; AAC frames keep the header and 16 bytes clear and encrypt the whole blocks after; AC-3
// and E-AC-3 syncframes keep their first 16 bytes clear.
type saesEnc struct {
	blk cipher.Block
	iv  []byte
}

func (e *saesEnc) blocks(b []byte, iv []byte) []byte { // CBC over whole blocks, returns the chain
	prev := append([]byte(nil), iv...)
	for len(b) >= 16 {
		for i := 0; i < 16; i++ {
			b[i] ^= prev[i]
		}
		e.blk.Encrypt(b[:16], b[:16])
		copy(prev, b[:16])
		b = b[16:]
	}
	return prev
}

func (e *saesEnc) video(es []byte) []byte {
	var out []byte
	starts := []int{} // where each unit's start code begins
	for i := 0; i+2 < len(es); i++ {
		if es[i] == 0 && es[i+1] == 0 && es[i+2] == 1 {
			starts = append(starts, i)
			i += 2
		}
	}
	if len(starts) == 0 {
		return es
	}
	out = append(out, es[:starts[0]]...)
	for k, st := range starts {
		end := len(es)
		if k+1 < len(starts) {
			end = starts[k+1]
		}
		body := es[st+3 : end]
		tail := 0
		for tail < len(body) && body[len(body)-1-tail] == 0 {
			tail++
		}
		nal := append([]byte(nil), body[:len(body)-tail]...)
		if len(nal) > 48 && (nal[0]&0x1f == 1 || nal[0]&0x1f == 5) {
			prev := e.iv
			for p := 32; p < len(nal); {
				if len(nal)-p > 16 {
					prev = e.blocks(nal[p:p+16], prev)
					p += 16
				}
				p += min(144, len(nal)-p)
			}
			var esc []byte // emulation prevention, applied again over the whole unit
			zeros := 0
			for _, c := range nal {
				if zeros >= 2 && c <= 3 {
					esc = append(esc, 3)
					zeros = 0
				}
				esc = append(esc, c)
				if c == 0 {
					zeros++
				} else {
					zeros = 0
				}
			}
			nal = esc
		}
		out = append(append(append(out, 0, 0, 1), nal...), body[len(body)-tail:]...)
	}
	return out
}

func (e *saesEnc) audio(codec string, es []byte) {
	for i := 0; i+7 <= len(es); {
		hdr, size := 0, 0
		if codec == "aac" {
			if es[i] == 0xFF && es[i+1]&0xF6 == 0xF0 {
				hdr = 7
				if es[i+1]&1 == 0 {
					hdr = 9
				}
				size = int(es[i+3]&3)<<11 | int(es[i+4])<<3 | int(es[i+5]>>5)
			}
		} else if n, _, _, err := ac3Frame(es[i:]); err == nil {
			size = n
		}
		if size == 0 {
			i++
			continue
		}
		if i+size > len(es) {
			return
		}
		if f := es[i+hdr : i+size]; len(f) > 16 {
			e.blocks(f[16:16+(len(f)-16)/16*16], e.iv)
		}
		i += size
	}
}

func encType(t byte) (byte, string) {
	switch t {
	case 0x1B:
		return 0xDB, "h264"
	case 0x0F:
		return 0xCF, "aac"
	case 0x81:
		return 0xC1, "ac3"
	case 0x87:
		return 0xC2, "ac3"
	}
	return t, ""
}

func saesEncrypt(t *testing.T, ts, key, iv []byte) []byte {
	t.Helper()
	blk, _ := aes.NewCipher(key)
	e := &saesEnc{blk: blk, iv: iv}
	out, err := rewriteTS(ts, encType, e.video, e.audio)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSampleAESAudioRoundTrip: AAC, AC-3 and E-AC-3 in MPEG-TS, and packed AAC, come back
// byte for byte (audio is decrypted in place: nothing else in the piece changes).
func TestSampleAESAudioRoundTrip(t *testing.T) {
	needFFmpeg(t)
	key, iv := []byte("0123456789abcdef"), bytes.Repeat([]byte{7}, 16)
	for _, codec := range []string{"aac", "ac3", "eac3"} {
		in := filepath.Join(t.TempDir(), "a.ts")
		ffmpegRun(t, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "2", "-c:a", codec, "-f", "mpegts", in)
		plain, _ := os.ReadFile(in)
		enc := saesEncrypt(t, plain, key, iv)
		if bytes.Equal(enc, plain) || tsIDClear(tsID(enc)) != tsID(plain) {
			t.Fatalf("%s: not encrypted (%s)", codec, tsID(enc))
		}
		got, err := decryptSampleAES(enc, key, iv)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("%s: decrypted piece differs (%v)", codec, err)
		}
	}
	// packed audio: an ID3 timestamp, then ADTS frames
	adts := filepath.Join(t.TempDir(), "a.aac")
	ffmpegRun(t, "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "1", "-c:a", "aac", "-f", "adts", adts)
	raw, _ := os.ReadFile(adts)
	plain := append(id3Timestamp(900000), raw...)
	enc := append([]byte(nil), plain...)
	blk, _ := aes.NewCipher(key)
	(&saesEnc{blk: blk, iv: iv}).audio("aac", enc[skipID3(enc):])
	if bytes.Equal(enc, plain) {
		t.Fatal("packed audio wasn't encrypted")
	}
	if got, err := decryptSampleAES(enc, key, iv); err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("packed audio differs (%v)", err)
	}
}

// TestSampleAESStream: an H.264 stream with AAC, AC-3 or E-AC-3 audio, encrypted with SAMPLE-AES
// (a plain key), downloads and decodes to the very same frames as the clear stream. FFmpeg, which
// decrypts SAMPLE-AES itself, first vouches for the tests' encrypter.
func TestSampleAESStream(t *testing.T) {
	needFFmpeg(t)
	orig := ffmpegPath
	ffmpegPath = func() string { return "" }
	t.Cleanup(func() { ffmpegPath = orig })
	for _, audio := range []string{"aac", "ac3", "eac3"} {
		t.Run(audio, func(t *testing.T) { sampleAESStream(t, audio) })
	}
}

func sampleAESStream(t *testing.T, audio string) {
	clear := t.TempDir()
	ffmpegRun(t, "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "6",
		"-c:v", "libx264", "-preset", "fast", "-bf", "2", "-g", "50", "-c:a", audio, "-f", "hls", "-hls_time", "2", "-hls_playlist_type", "vod",
		"-hls_segment_filename", filepath.Join(clear, "s%03d.ts"), filepath.Join(clear, "index.m3u8"))
	pl, _ := os.ReadFile(filepath.Join(clear, "index.m3u8"))
	_, media, err := parseM3U8("http://x/index.m3u8", string(pl))
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("sixteen byte key")
	fixedIV := bytes.Repeat([]byte{0xA5}, 16)
	// write encrypts the stream into a folder: oracle uses one IV throughout and ends with a copy
	// of the first piece (FFmpeg decrypts a piece's last frame only once the next piece has begun,
	// with that piece's IV); otherwise the pieces use their sequence numbers, the last its own IV.
	write := func(oracle bool) string {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "key.bin"), key, 0o644)
		var b strings.Builder
		b.WriteString("#EXTM3U\n#EXT-X-VERSION:5\n#EXT-X-TARGETDURATION:2\n#EXT-X-PLAYLIST-TYPE:VOD\n")
		if oracle {
			b.WriteString("#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key.bin\",IV=0x" + hex.EncodeToString(fixedIV) + "\n")
		} else {
			b.WriteString("#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key.bin\",KEYFORMAT=\"identity\"\n")
		}
		for i, s := range media.Segments {
			iv := seqIV(int64(i))
			if oracle {
				iv = fixedIV
			} else if i == len(media.Segments)-1 {
				iv = fixedIV
				b.WriteString("#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key.bin\",IV=0x" + hex.EncodeToString(fixedIV) + "\n")
			}
			plain, _ := os.ReadFile(filepath.Join(clear, filepath.Base(s.URL)))
			e := saesEncrypt(t, plain, key, iv)
			if bytes.Equal(e, plain) {
				t.Fatal("not encrypted")
			}
			os.WriteFile(filepath.Join(dir, filepath.Base(s.URL)), e, 0o644)
			fmt.Fprintf(&b, "#EXTINF:%.6f,\n%s\n", s.Dur, filepath.Base(s.URL))
		}
		if oracle {
			fmt.Fprintf(&b, "#EXT-X-DISCONTINUITY\n#EXTINF:%.6f,\n%s\n", media.Segments[0].Dur, filepath.Base(media.Segments[0].URL))
		}
		b.WriteString("#EXT-X-ENDLIST\n")
		os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte(b.String()), 0o644)
		return dir
	}
	src := filepath.Join(clear, "index.m3u8")
	wantV, wantA := decodedHashes(t, src, "v:0"), decodedHashes(t, src, "a:0")

	// the oracle: FFmpeg's own SAMPLE-AES decryption gets the clear frames back from the encrypter
	osrv := httptest.NewServer(http.FileServer(http.Dir(write(true))))
	defer osrv.Close()
	for _, c := range []struct {
		sel  string
		want []string
	}{{"v:0", wantV}, {"a:0", wantA}} {
		got := decodedHashes(t, osrv.URL+"/index.m3u8", c.sel)
		if len(got) < len(c.want) {
			t.Fatalf("ffmpeg decoded %d frames of %s", len(got), c.sel)
		}
		sameHashes(t, "ffmpeg's decryption of the test stream ("+c.sel+")", c.want, got[:len(c.want)])
	}

	srv := httptest.NewServer(http.FileServer(http.Dir(write(false))))
	defer srv.Close()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/index.m3u8", Name: "secret.mp4", Start: true})
	waitFor(t, m, d, StCompleted, 30*time.Second)
	out := filepath.Join(d.Dir, d.FileName)
	if d.FileName != "secret.mp4" || d.Note != "" || !isRegularMP4(out) {
		t.Fatalf("%s note %q", d.FileName, d.Note)
	}
	sameHashes(t, "video", wantV, decodedHashes(t, out, "v:0"))
	sameHashes(t, "audio", wantA, decodedHashes(t, out, "a:0"))
	if e := decodeErrors(t, out); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
	if want := tsID(mustRead(t, filepath.Join(clear, "s000.ts"))); d.VarInit != want || !strings.HasPrefix(want, "ts:1b,") {
		t.Fatalf("stream types %q, want %q", d.VarInit, want)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestSampleAESKeepsClearTS: the decrypted pieces are plain MPEG-TS (named .ts, the stream is kept
// as it is): the stream types are the plain ones and every packet lines up.
func TestSampleAESKeepsClearTS(t *testing.T) {
	needFFmpeg(t)
	dir := makeHLS(t, "ts", 2, 2)
	plain, _ := os.ReadFile(filepath.Join(dir, "s000.ts"))
	key, iv := []byte("0123456789abcdef"), seqIV(0)
	got, err := decryptSampleAES(saesEncrypt(t, plain, key, iv), key, iv)
	if err != nil || tsID(got) != "ts:1b,0f" || len(got)%188 != 0 {
		t.Fatalf("%v %q %d", err, tsID(got), len(got))
	}
	for off := 0; off < len(got); off += 188 {
		if got[off] != 0x47 {
			t.Fatalf("packet at %d out of line", off)
		}
	}
	p := filepath.Join(t.TempDir(), "x.ts")
	os.WriteFile(p, got, 0o644)
	sameHashes(t, "video", decodedHashes(t, filepath.Join(dir, "s000.ts"), "v:0"), decodedHashes(t, p, "v:0"))
	if out, _ := exec.Command("ffmpeg", "-v", "error", "-i", p, "-f", "null", "-").CombinedOutput(); len(bytes.TrimSpace(out)) > 0 {
		t.Fatalf("ffmpeg complained about the decrypted piece:\n%s", out)
	}
}

// TestRewriteTSRepacks: when a video PES changes length, it's packed into packets again: still
// aligned, its packets numbered on without a gap, its clock references kept, and the frames the
// same (here each picture gets a filler unit, which decoders skip).
func TestRewriteTSRepacks(t *testing.T) {
	needFFmpeg(t)
	dir := makeHLS(t, "ts", 4, 2)
	src := filepath.Join(dir, "s000.ts")
	plain, _ := os.ReadFile(src)
	grown := 0
	out, err := rewriteTS(plain, clearType, func(es []byte) []byte {
		grown++
		filler := append([]byte{0, 0, 1, 0x0C}, bytes.Repeat([]byte{0xFF}, 100+grown%300)...)
		return append(append([]byte(nil), es...), append(filler, 0x80)...)
	}, func(string, []byte) {})
	if err != nil || len(out) <= len(plain) || len(out)%188 != 0 || grown < 40 {
		t.Fatalf("%v: %d → %d bytes, %d pictures", err, len(plain), len(out), grown)
	}
	pcrs := func(b []byte) (n int) {
		for off := 0; off+188 <= len(b); off += 188 {
			if p := b[off:]; p[3]&0x20 != 0 && p[4] > 0 && p[5]&0x10 != 0 {
				n++
			}
		}
		return
	}
	if pcrs(out) != pcrs(plain) {
		t.Fatalf("clock references: %d, were %d", pcrs(out), pcrs(plain))
	}
	last := map[int]int{}
	for off := 0; off < len(out); off += 188 {
		p := out[off : off+188]
		if p[0] != 0x47 {
			t.Fatalf("packet at %d out of line", off)
		}
		pid, cc := int(p[1]&0x1f)<<8|int(p[2]), int(p[3]&0x0f)
		if p[3]&0x10 == 0 {
			continue // (no payload: the counter stays)
		}
		if prev, ok := last[pid]; ok && cc != (prev+1)&15 {
			t.Fatalf("pid %d: counter %d after %d at %d", pid, cc, prev, off)
		}
		last[pid] = cc
	}
	p := filepath.Join(t.TempDir(), "x.ts")
	os.WriteFile(p, out, 0o644)
	sameHashes(t, "video", decodedHashes(t, src, "v:0"), decodedHashes(t, p, "v:0"))
	sameHashes(t, "audio", decodedHashes(t, src, "a:0"), decodedHashes(t, p, "a:0"))
	if e := decodeErrors(t, p); e != "" {
		t.Fatalf("decoder complained:\n%s", e)
	}
}
