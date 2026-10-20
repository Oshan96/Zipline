package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Never mixing qualities. A download remembers the quality it started with by what it is
// (resolution, codecs, frame rate, bandwidth, and a fingerprint of its init piece or of its first
// piece's stream types), not by its bandwidth alone: a regenerated playlist may state bandwidths
// differently, and resuming then must still pick the same quality, or fail clearly instead of
// splicing two encodings together. The same check tells whether a fresh link (from the browser
// extension, or clicked again) is for the same stream.

var errQualityChanged = permanentErr{errors.New("the stream's qualities changed since this download started — use Redownload")}

// streamStarted: some of d's stream is on disk. Caller holds m.mu.
func (d *Download) streamStarted() bool {
	return d.PartsDone > 0 || d.ADone > 0 || d.PartBytes > 0 || d.ABytes > 0
}

// setQuality remembers the quality a stream download starts with. Caller holds m.mu.
func (d *Download) setQuality(v hlsVariant) {
	d.VarRes, d.VarCodecs, d.VarAvgBW, d.VarFPS, d.VarInit = v.Resolution, v.Codecs, v.AvgBandwidth, v.FrameRate, ""
}

// resetStream forgets what a stream download measured and recorded (it starts over). Caller
// holds m.mu.
func (d *Download) resetStream() {
	d.VarInit = ""
	d.WantSecs, d.ShortSecs, d.AWantSecs, d.AShortSecs = 0, 0, 0, 0
	d.Live = false
}

// sameCodecs compares CODECS lists whatever their order and case.
func sameCodecs(a, b string) bool {
	norm := func(s string) []string {
		var out []string
		for _, c := range strings.Split(strings.ToLower(s), ",") {
			if c = strings.TrimSpace(c); c != "" {
				out = append(out, c)
			}
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(norm(a), norm(b))
}

// chooseVariant picks the quality to fetch from a master playlist: for a download that has
// pieces already, the one it started with (none matching fails: errQualityChanged); otherwise
// the one asked for (by bandwidth), else the best.
func (m *Manager) chooseVariant(d *Download, vs []hlsVariant, bandwidth int) (hlsVariant, error) {
	m.mu.Lock()
	res, codecs, fps, avg, started := d.VarRes, d.VarCodecs, d.VarFPS, d.VarAvgBW, d.streamStarted()
	m.mu.Unlock()
	f := fps
	if res != "" || codecs != "" || f > 0 {
		var match []hlsVariant
		for _, v := range vs {
			if (res == "" || v.Resolution == "" || strings.EqualFold(res, v.Resolution)) &&
				(codecs == "" || v.Codecs == "" || sameCodecs(codecs, v.Codecs)) &&
				(f == 0 || v.FrameRate == 0 || math.Abs(f-v.FrameRate) < 0.01) {
				match = append(match, v)
			}
		}
		if len(match) > 0 { // the closest in bandwidth (a regenerated playlist may state it differently)
			best, bestD := match[0], math.Inf(1)
			for _, v := range match {
				dist := math.Abs(math.Log(float64(max(v.Bandwidth, 1)) / float64(max(bandwidth, 1))))
				if bandwidth <= 0 {
					dist = -float64(v.Bandwidth) // none asked for: the best
				}
				if avg > 0 && v.AvgBandwidth == avg || bandwidth > 0 && v.Bandwidth == bandwidth {
					dist = math.Inf(-1)
				}
				if dist < bestD {
					best, bestD = v, dist
				}
			}
			return best, nil
		}
		if started {
			return hlsVariant{}, errQualityChanged
		}
	} else if started && bandwidth > 0 { // begun before qualities were told apart by more than this
		for _, v := range vs {
			if v.Bandwidth == bandwidth {
				return v, nil
			}
		}
		return hlsVariant{}, errQualityChanged
	}
	return pickVariant(vs, nil, bandwidth), nil // the one asked for, else the best SDR quality that can be saved
}

// sameQuality checks, when a download carries on, that media still has the quality it began
// with: its init piece (fMP4) or its first piece's stream types (MPEG-TS) must be what they were.
// via makes the requests (a fresh link's own headers).
func (m *Manager) sameQuality(ctx context.Context, d, via *Download, media *hlsMedia) error {
	m.mu.Lock()
	want, started := d.VarInit, d.streamStarted()
	m.mu.Unlock()
	if want == "" || !started || media == nil || len(media.Segments) == 0 {
		return nil
	}
	var got string
	keys := &keyCache{m: map[string][]byte{}}
	if media.InitURL != "" {
		b, err := m.pieceHead(ctx, via, hlsSegment{URL: media.InitURL, RangeOff: media.InitOff, RangeLen: media.InitLen}, nil, 1<<20)
		if err != nil {
			return err
		}
		got = initID(b)
	} else {
		b, err := m.pieceHead(ctx, via, media.Segments[0], keys, 128*188)
		if err != nil {
			return err
		}
		got = tsIDClear(tsID(b))
	}
	if got != "" && got != want {
		logf("%s: the stream's quality is %s now, was %s", d.FileName, got, want)
		return errQualityChanged
	}
	return nil
}

// pieceHead reads the start of a piece (up to n bytes, or the whole range it names), decrypting
// the part of an AES-128 piece it has. Nothing is written: only checked.
func (m *Manager) pieceHead(ctx context.Context, d *Download, s hlsSegment, keys *keyCache, n int64) ([]byte, error) {
	off, want := s.RangeOff, n
	if s.RangeLen > 0 {
		want = min(n, s.RangeLen)
	}
	req, err := buildRequest(ctx, d, s.URL, rangeHeader(off, off+want-1))
	if err != nil {
		return nil, err
	}
	resp, err := doReqUA(d, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK: // the whole file: skip to the piece
		if _, err := io.CopyN(io.Discard, resp.Body, off); err != nil {
			return nil, err
		}
	default:
		return nil, statusErr{resp.StatusCode, resp.Status, retryAfterHeader(resp.Header)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, want))
	m.countDown(int64(len(b)))
	if err != nil {
		return nil, err
	}
	if s.KeyURL != "" && keys != nil {
		key, err := keys.get(ctx, m, d, s.KeyURL)
		if err != nil {
			return nil, err
		}
		if s.SampleAES {
			return b, nil // (its program tables aren't encrypted; tsID reads them)
		}
		iv := s.IV
		if iv == nil {
			iv = seqIV(s.Seq)
		}
		blk, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		b = b[:len(b)/16*16]
		cipher.NewCBCDecrypter(blk, iv).CryptBlocks(b, b)
	}
	return b, nil
}

// tsIDClear is tsID with SAMPLE-AES stream types named as the plain ones they hold.
func tsIDClear(id string) string {
	if !strings.HasPrefix(id, "ts:") {
		return id
	}
	parts := strings.Split(id[3:], ",")
	for i, p := range parts {
		if v, err := strconv.ParseUint(p, 16, 8); err == nil {
			t, _ := clearType(byte(v))
			parts[i] = strconv.FormatUint(uint64(t), 16)
			if len(parts[i]) == 1 {
				parts[i] = "0" + parts[i]
			}
		}
	}
	return "ts:" + strings.Join(parts, ",")
}

// ---------------------------------------------------------------- a fresh link for a stream

// sameStream reports whether link (with headers) serves the stream d was downloading: the same
// quality, the same number of pieces, the same init piece or stream types.
func (m *Manager) sameStream(ctx context.Context, d *Download, headers map[string]string, link string) bool {
	m.mu.Lock()
	via := &Download{URL: link, Headers: headers, Variant: d.Variant, VarRes: d.VarRes, VarCodecs: d.VarCodecs, VarFPS: d.VarFPS,
		VarAvgBW: d.VarAvgBW, VarInit: d.VarInit, PartsDone: d.PartsDone, ADone: d.ADone, PartBytes: d.PartBytes, ABytes: d.ABytes}
	parts, aparts, live := d.Parts, d.AParts, d.Live
	m.mu.Unlock()
	if live {
		return false // a recording can't carry on from where it stopped
	}
	info, err := m.resolveHLS(ctx, via, link, via.Variant)
	if err != nil || info.Live || info.DRM || info.Media == nil || len(info.Media.Segments) != parts {
		return false
	}
	if (info.Audio == nil) != (aparts == 0) || info.Audio != nil && len(info.Audio.Segments) != aparts {
		return false
	}
	return m.sameQuality(ctx, d, via, info.Media) == nil && m.sameStart(ctx, d, via, info.Media)
}

// sameStart compares the start of the stream behind a fresh link (its init piece, then the start of
// its first piece) with what's on disk: the same files, not merely a stream that looks alike.
func (m *Manager) sameStart(ctx context.Context, d, via *Download, media *hlsMedia) bool {
	m.mu.Lock()
	path, have, pieces := d.partPath(), d.PartBytes, d.PartsDone
	m.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	same := func(b []byte, at int64) bool {
		disk := make([]byte, len(b))
		n, _ := f.ReadAt(disk, at)
		return n == len(b) && bytes.Equal(disk, b)
	}
	var at int64
	if media.InitURL != "" {
		init, err := m.pieceHead(ctx, via, hlsSegment{URL: media.InitURL, RangeOff: media.InitOff, RangeLen: media.InitLen}, nil, 4<<20)
		if err != nil || !same(init, 0) {
			return false
		}
		at = int64(len(init))
	}
	s := media.Segments[0]
	if pieces == 0 || s.SampleAES { // (a SAMPLE-AES piece is changed on its way to the disk)
		return true
	}
	head, err := m.pieceHead(ctx, via, s, &keyCache{m: map[string][]byte{}}, 16<<10)
	return err == nil && len(head) > 0 && at+int64(len(head)) <= have && same(head, at)
}

// renewStream switches a stream download to a fresh link for the same stream and carries on.
// Caller holds m.mu.
func (m *Manager) renewStream(d *Download, link string, headers map[string]string) {
	d.URL, d.FinalURL = link, ""
	if d.Headers == nil {
		d.Headers = map[string]string{}
	}
	for k, v := range headers {
		if v != "" {
			d.Headers[k] = v
		}
	}
	d.Link, d.Error, d.linkTries, d.linkWait = "", "", 0, time.Time{}
	delete(m.refresh, d.ID)
	m.dirty = true
	m.notify("linkRenewed", d.ID, "Got a fresh link for "+iso(d.FileName)+" — carrying on")
	if !d.running && d.Status != StCompleted {
		m.startLocked(d)
	}
}

// waitingStream finds a stopped stream download waiting for a fresh link that link is for.
func (m *Manager) waitingStream(ctx context.Context, link string, headers map[string]string) *Download {
	m.mu.Lock()
	var cands []*Download
	for _, d := range m.list {
		if !d.running && d.Kind == "hls" && !d.Provisional && !d.Live && d.Link != "" && d.streamStarted() && len(cands) < 5 {
			cands = append(cands, d)
		}
	}
	m.mu.Unlock()
	for _, d := range cands {
		if m.sameStream(ctx, d, headers, link) {
			return d
		}
	}
	return nil
}

// refreshName is the name the browser extension looks for on the page an expired download came
// from: a stream's is its playlist's (players load "master.m3u8" again, with a fresh signature).
func refreshName(d *Download) string {
	if d.Kind != "hls" {
		return d.FileName
	}
	if u, err := url.Parse(d.URL); err == nil && path.Base(u.Path) != "/" && path.Base(u.Path) != "." {
		return path.Base(u.Path)
	}
	return d.FileName
}

// streamProgress is how far a stream download got (bytes so far), for the Add dialog's "same
// download" box. Caller holds m.mu.
func (d *Download) streamProgress() (done, size int64) {
	p, est := hlsProgress(d)
	if d.Parts == 0 || est <= 0 {
		return atomic.LoadInt64(&d.done), 0
	}
	return int64(p * float64(est)), est
}
