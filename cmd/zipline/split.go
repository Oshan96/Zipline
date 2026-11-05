package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"zipline/internal/mediahead"
)

// Videos whose player fetches the picture and the sound as two separate files, each in pieces (byte
// ranges, asked for in the address or in a Range header), with no playlist to say which file is which.
// The extension hands over the files it saw a tab's player fetch that way, newest first (Download.Split);
// here each one's header says what it holds (internal/mediahead): picture or sound, how long it is, how
// big its picture. The newest picture's video is the one: its qualities are the picture files of the
// same length, its sound the sound file of that length (a page that plays one video after another gets
// each one's sound right). They're then downloaded as a DASH stream's one-file tracks are, in ranges on
// many connections, and combined into one file. An encrypted file is DRM, and refused as such.

const maxSplitFiles = 8

// rangeParams pick a piece of a file in its address (bytestart=0&byteend=99, range=0-99).
var rangeParams = map[string]bool{"bytestart": true, "byteend": true, "range": true, "start_byte": true, "end_byte": true}

// wholeFile is a piece's address without the part that picks the piece. The rest stays exactly as it
// was: a signed address can't take its query written another way.
func wholeFile(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		return raw
	}
	parts := strings.Split(u.RawQuery, "&")
	keep := parts[:0]
	for _, p := range parts {
		k, _, _ := strings.Cut(p, "=")
		if dk, err := url.QueryUnescape(k); err == nil && rangeParams[strings.ToLower(dk)] {
			continue
		}
		keep = append(keep, p)
	}
	u.RawQuery = strings.Join(keep, "&")
	return u.String()
}

// cleanSplit keeps the distinct web addresses (at most maxSplitFiles), each for its whole file.
func cleanSplit(list []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range list {
		u := wholeFile(strings.TrimSpace(raw))
		pu, err := url.Parse(u)
		if err != nil || pu.Host == "" || pu.Scheme != "http" && pu.Scheme != "https" || seen[u] {
			continue
		}
		seen[u] = true
		if out = append(out, u); len(out) == maxSplitFiles {
			break
		}
	}
	return out
}

// protectedSplit: is any of the files, or the page they were played on, a protected site's?
func protectedSplit(list []string, hdr map[string]string) bool {
	for _, u := range list {
		if protectedURL(u) {
			return true
		}
	}
	for k, v := range hdr {
		if strings.EqualFold(k, "Referer") && protectedURL(v) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- reading a file's header over the network

// headReader reads the start of a file over HTTP for mediahead, in blocks; what's missing from a read
// is asked for in one request.
type headReader struct {
	ctx  context.Context
	d    *Download
	u    string
	data map[int64][]byte // blocks read
	size int64            // the whole file's size (-1 until a reply says)
	asks int
}

const headBlock = 64 << 10

func (r *headReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	end := off + int64(len(p))
	if r.size >= 0 {
		end = min(end, r.size)
	}
	if end <= off {
		return 0, io.EOF
	}
	for b := off / headBlock; b*headBlock < end; {
		if _, ok := r.data[b]; ok {
			b++
			continue
		}
		last, e := (end-1)/headBlock, b
		for e < last {
			if _, ok := r.data[e+1]; ok {
				break
			}
			e++
		}
		if err := r.fetch(b, e); err != nil {
			return 0, err
		}
		if r.size >= 0 {
			end = min(end, r.size)
		}
		b = e + 1
	}
	n := 0
	for off+int64(n) < end {
		at := off + int64(n)
		blk := r.data[at/headBlock]
		i := int(at % headBlock)
		if i >= len(blk) {
			break
		}
		n += copy(p[n:end-off], blk[i:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *headReader) fetch(from, to int64) error {
	if r.asks++; r.asks > 12 {
		return errors.New("the file's header is spread too far through it")
	}
	start, want := from*headBlock, (to-from+1)*headBlock
	req, err := buildRequest(r.ctx, r.d, r.u, fmt.Sprintf("bytes=%d-%d", start, start+want-1))
	if err != nil {
		return err
	}
	resp, err := doReq(r.d, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		r.size = start // (past its end)
		return nil
	case resp.StatusCode == http.StatusOK && start > 0:
		return errors.New("the server sends the file only from its start")
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent:
		return httpError(resp)
	case strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html"):
		return errors.New("the address gave a web page, not a video")
	}
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				r.size = n
			}
		}
	} else if resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 {
		r.size = resp.ContentLength
	}
	buf := make([]byte, want)
	got, err := io.ReadFull(resp.Body, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	if int64(got) < want && r.size < 0 {
		r.size = start + int64(got)
	}
	for i := int64(0); i*headBlock < int64(got); i++ {
		r.data[from+i] = buf[i*headBlock : min((i+1)*headBlock, int64(got))]
	}
	if got == 0 {
		r.data[from] = nil // (nothing there: don't ask again)
	}
	return nil
}

// httpError is a refusal in plain words.
func httpError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusUnauthorized, http.StatusGone:
		return fmt.Errorf("the server refused the file (%s): its address may have expired, so play the video again", resp.Status)
	case http.StatusNotFound:
		return fmt.Errorf("the file isn't there any more (%s)", resp.Status)
	}
	return fmt.Errorf("server replied %s", resp.Status)
}

// ---------------------------------------------------------------- telling the files apart

type splitFile struct {
	idx  int
	url  string
	info mediahead.Info
	size int64
	err  error
}

// readSplit reads each file's header, all at once.
func (m *Manager) readSplit(ctx context.Context, d *Download, list []string) []*splitFile {
	out := make([]*splitFile, len(list))
	var wg sync.WaitGroup
	for i, u := range list {
		f := &splitFile{idx: i, url: u, size: -1}
		out[i] = f
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			r := &headReader{ctx: ctx, d: d, u: f.url, data: map[int64][]byte{}, size: -1}
			if _, f.err = r.ReadAt(make([]byte, 1), 0); f.err != nil { // (the first reply says how big the file is)
				return
			}
			f.info, f.err = mediahead.Read(r, r.size)
			f.size = r.size
		}()
	}
	wg.Wait()
	return out
}

// sameLength: could a and b be the picture and sound of one video? (A sound track often runs a little
// longer or shorter than its picture.) A file that doesn't say how long it is can't be told apart.
func sameLength(a, b float64) bool {
	return a == 0 || b == 0 || math.Abs(a-b) <= max(1, a*0.02)
}

// splitParts picks the video's files: the newest picture's qualities, and the sound that goes with them.
func splitParts(files []*splitFile) (videos, sounds []*splitFile, err error) {
	var main *splitFile
	for _, f := range files {
		if f.err == nil && f.info.Video {
			main = f
			break
		}
	}
	if main == nil { // no picture at all: a sound recording
		for _, f := range files {
			if f.err == nil && f.info.Audio {
				main = f
				break
			}
		}
	}
	if main == nil {
		for _, f := range files {
			if f.err != nil && !errors.Is(f.err, mediahead.ErrUnknown) && !errors.Is(f.err, mediahead.ErrNoIndex) && !errors.Is(f.err, mediahead.ErrBroken) {
				return nil, nil, f.err
			}
		}
		return nil, nil, permanentErr{errors.New("the files the player fetched aren't a video Zipline can read")}
	}
	whole := main.info.Video && main.info.Audio // picture and sound in one file: nothing to add
	for _, f := range files {
		if f.err != nil || !sameLength(main.info.Duration, f.info.Duration) {
			continue
		}
		switch {
		case f.info.Video && f.info.Audio == whole:
			videos = append(videos, f)
		case !f.info.Video && f.info.Audio && !whole:
			sounds = append(sounds, f)
		}
	}
	return videos, sounds, nil
}

// splitBandwidth is a file's bits per second, for telling its qualities apart.
func splitBandwidth(f *splitFile, secs float64) int {
	if f.size > 0 && secs > 0 {
		return int(float64(f.size) * 8 / secs)
	}
	return f.info.Width * f.info.Height // (a file that doesn't say its size: by its picture)
}

// resolveSplit describes d's files as a DASH stream with one file per quality, for dashChoose.
func (m *Manager) resolveSplit(ctx context.Context, d *Download, bandwidth int) (*hlsInfo, error) {
	videos, sounds, err := splitParts(m.readSplit(ctx, d, d.Split))
	if err != nil {
		return nil, err
	}
	secs := 0.0
	for _, f := range append(videos, sounds...) {
		secs = max(secs, f.info.Duration)
	}
	rep := func(f *splitFile, kind string) mpdRep {
		mime := kind + "/mp4"
		if f.info.Container != "mp4" {
			mime = kind + "/webm"
		}
		r := mpdRep{ID: "file" + strconv.Itoa(f.idx+1), Bandwidth: splitBandwidth(f, secs), Width: f.info.Width, Height: f.info.Height,
			MimeType: mime, Codecs: strings.Join(f.info.Codecs, ","), BaseURL: []string{f.url}}
		if f.info.Encrypted {
			r.Protection = []struct{}{{}}
		}
		return r
	}
	var p mpdPeriod
	if len(videos) > 0 {
		set := mpdSet{ContentType: "video"}
		for _, f := range videos {
			set.Reps = append(set.Reps, rep(f, "video"))
		}
		p.Sets = append(p.Sets, set)
	}
	if len(sounds) > 0 {
		set := mpdSet{ContentType: "audio"}
		for _, f := range sounds {
			set.Reps = append(set.Reps, rep(f, "audio"))
		}
		p.Sets = append(p.Sets, set)
	}
	doc := &mpdDoc{Periods: []mpdPeriod{p}}
	if secs > 0 {
		doc.Duration = fmt.Sprintf("PT%.3fS", secs)
	}
	base, err := url.Parse(d.URL)
	if err != nil {
		return nil, permanentErr{err}
	}
	info := &hlsInfo{Format: "DASH", MediaURL: d.URL, Split: true}
	return m.dashChoose(ctx, d, info, doc, base, bandwidth)
}
