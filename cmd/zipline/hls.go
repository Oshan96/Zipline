package main

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HLS (.m3u8) video streams: the playlist lists many small segments. Zipline fetches
// them on several connections at once, decrypts AES-128 if needed, and writes them in
// order into one file. SAMPLE-AES with a plain key is decrypted too (sampleaes.go); DRM-protected
// streams (FairPlay / Widevine / PlayReady) are refused.

type hlsVariant struct {
	URL          string  `json:"url"`
	Bandwidth    int     `json:"bandwidth"`
	Resolution   string  `json:"resolution,omitempty"`
	AudioURL     string  `json:"-"`
	Codecs       string  `json:"codecs,omitempty"`
	FrameRate    float64 `json:"frameRate,omitempty"`
	VideoRange   string  `json:"videoRange,omitempty"` // SDR, PQ or HLG
	AvgBandwidth int     `json:"avgBandwidth,omitempty"`
	AudioGroup   string  `json:"audioGroup,omitempty"`
	SubGroup     string  `json:"-"`
	Webm         bool    `json:"webm,omitempty"` // DASH: WebM pieces
}

type hlsSegment struct {
	URL      string
	KeyURL   string
	IV       []byte // nil → derived from the sequence number
	Seq      int64
	RangeOff int64
	RangeLen int64 // 0 = whole resource

	Dur       float64 // seconds, as the playlist says (EXTINF; 0 = not given)
	SampleAES bool    // KeyURL's key decrypts it as SAMPLE-AES (sampleaes.go), not AES-128
	Chunk     bool    // a plain byte range of one file (DASH): its first bytes can be anything
	Disc      bool    // a discontinuity comes before it: new timestamps, maybe a new format
	Map       *hlsMap // the init section it needs (fMP4), nil for none
	Ad        bool    // inside a marked ad break
	Part      int     // which part of the programme (see discont.go)
}

// hlsMap is an init section (EXT-X-MAP, or a DASH period's initialization).
type hlsMap struct {
	URL      string
	Off, Len int64
}

type hlsMedia struct {
	Segments []hlsSegment
	InitURL  string
	InitOff  int64
	InitLen  int64
	Ended    bool
	Duration float64
	DRM      bool

	Target float64 // EXT-X-TARGETDURATION (seconds)
	Event  bool    // EXT-X-PLAYLIST-TYPE:EVENT: pieces are only ever added
}

// looksLikeHLS reports whether a link points at a stream manifest (HLS .m3u8 or DASH .mpd).
func looksLikeHLS(u string) bool {
	p := strings.ToLower(u)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return strings.HasSuffix(p, ".m3u8") || strings.HasSuffix(p, ".m3u") || looksLikeDASH(u)
}

func isHLSMime(m string) bool {
	m = strings.ToLower(m)
	return strings.Contains(m, "mpegurl") || strings.Contains(m, "dash+xml")
}

// attrs parses `KEY=VALUE,KEY="quoted,value"` attribute lists.
func attrs(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		k := strings.ToUpper(strings.TrimSpace(s[:eq]))
		s = s[eq+1:]
		var v string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				v, s = s[1:], ""
			} else {
				v, s = s[1:end+1], s[end+2:]
			}
			s = strings.TrimPrefix(s, ",")
		} else if c := strings.IndexByte(s, ','); c >= 0 {
			v, s = s[:c], s[c+1:]
		} else {
			v, s = s, ""
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

func resolveRef(base *url.URL, ref string) string {
	r, err := url.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	return base.ResolveReference(r).String()
}

func parseByteRange(s string, next int64) (off, length int64) {
	parts := strings.SplitN(s, "@", 2)
	length, _ = strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	off = next
	if len(parts) == 2 {
		off, _ = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	}
	return
}

// parseM3U8 returns either variants (master playlist) or a media playlist.
func parseM3U8(baseURL, body string) ([]hlsVariant, *hlsMedia, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, nil, err
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<16), 1<<22)
	if !sc.Scan() || !strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\uFEFF")), "#EXTM3U") {
		return nil, nil, errors.New("not an HLS playlist")
	}
	var variants []hlsVariant
	// group → the audio to use: the DEFAULT=YES rendition, else the first. One with no URI is the
	// audio already inside the video (RFC 8216 §4.3.4.1), so nothing extra is fetched.
	type rendition struct {
		uri  string
		def  bool
		desc bool // audio description for the blind
	}
	audio := map[string]rendition{}
	var pendingVar map[string]string
	media := &hlsMedia{}
	var seq int64
	var keyURL string
	var keyIV []byte
	var keySample bool
	var pendingSeg bool
	var rangeOff, rangeLen, nextOff int64
	var segDur float64
	var disc bool
	var curMap *hlsMap
	ads := &hlsAds{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			tag, val, _ := strings.Cut(line, ":")
			switch tag {
			case "#EXT-X-STREAM-INF":
				pendingVar = attrs(val)
			case "#EXT-X-MEDIA":
				a := attrs(val)
				if strings.EqualFold(a["TYPE"], "AUDIO") {
					r := rendition{def: strings.EqualFold(a["DEFAULT"], "YES"), desc: strings.Contains(a["CHARACTERISTICS"], "describes-video")}
					if a["URI"] != "" {
						r.uri = resolveRef(base, a["URI"])
					}
					cur, ok := audio[a["GROUP-ID"]]
					if !ok || (r.def && !cur.def && !r.desc) || (cur.desc && !r.desc) {
						audio[a["GROUP-ID"]] = r
					}
				}
			case "#EXT-X-MEDIA-SEQUENCE":
				seq, _ = strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			case "#EXT-X-KEY":
				a := attrs(val)
				switch method := strings.ToUpper(a["METHOD"]); method {
				case "NONE", "":
					keyURL, keyIV, keySample = "", nil, false
				case "AES-128", "SAMPLE-AES":
					if kf := a["KEYFORMAT"]; kf != "" && !strings.EqualFold(kf, "identity") {
						media.DRM = true
					}
					keyURL, keyIV, keySample = resolveRef(base, a["URI"]), nil, method == "SAMPLE-AES"
					if keySample && !plainKeyURL(keyURL) { // FairPlay (skd://) and the like
						media.DRM = true
					}
					if iv := a["IV"]; iv != "" {
						if b, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(iv, "0x"), "0X")); err == nil && len(b) == 16 {
							keyIV = b
						}
					}
				default: // SAMPLE-AES-CTR, SAMPLE-AES-CENC… = DRM
					media.DRM = true
				}
			case "#EXT-X-MAP":
				a := attrs(val)
				curMap = &hlsMap{URL: resolveRef(base, a["URI"])}
				if br := a["BYTERANGE"]; br != "" {
					curMap.Off, curMap.Len = parseByteRange(br, 0)
				}
				if media.InitURL == "" { // the first one; a later one starts a new part (discont.go)
					media.InitURL, media.InitOff, media.InitLen = curMap.URL, curMap.Off, curMap.Len
				}
			case "#EXT-X-DISCONTINUITY":
				disc = true
			case "#EXT-X-CUE-OUT", "#EXT-X-CUE-OUT-CONT", "#EXT-X-CUE-IN", "#EXT-X-DATERANGE", "#EXT-X-PROGRAM-DATE-TIME":
				ads.tag(tag, val)
			case "#EXTINF":
				pendingSeg, segDur = true, 0
				if d, err := strconv.ParseFloat(strings.TrimSpace(strings.Split(val, ",")[0]), 64); err == nil {
					media.Duration += d
					segDur = d
				}
			case "#EXT-X-TARGETDURATION":
				media.Target, _ = strconv.ParseFloat(strings.TrimSpace(val), 64)
			case "#EXT-X-PLAYLIST-TYPE":
				media.Event = strings.EqualFold(strings.TrimSpace(val), "EVENT")
			case "#EXT-X-BYTERANGE":
				rangeOff, rangeLen = parseByteRange(val, nextOff)
			case "#EXT-X-ENDLIST":
				media.Ended = true
			}
			continue
		}
		// URI line
		if pendingVar != nil {
			bw, _ := strconv.Atoi(pendingVar["BANDWIDTH"])
			v := hlsVariant{URL: resolveRef(base, line), Bandwidth: bw, Resolution: pendingVar["RESOLUTION"],
				Codecs: pendingVar["CODECS"], VideoRange: pendingVar["VIDEO-RANGE"], AudioGroup: pendingVar["AUDIO"], SubGroup: pendingVar["SUBTITLES"]}
			v.FrameRate, _ = strconv.ParseFloat(pendingVar["FRAME-RATE"], 64)
			v.AvgBandwidth, _ = strconv.Atoi(pendingVar["AVERAGE-BANDWIDTH"])
			if g := pendingVar["AUDIO"]; g != "" {
				v.AudioURL = g // resolved below once all EXT-X-MEDIA tags are known
			}
			variants = append(variants, v)
			pendingVar = nil
			continue
		}
		if pendingSeg {
			media.Segments = append(media.Segments, hlsSegment{URL: resolveRef(base, line), KeyURL: keyURL, IV: keyIV, Seq: seq, RangeOff: rangeOff, RangeLen: rangeLen,
				Dur: segDur, SampleAES: keySample, Disc: disc, Map: curMap, Ad: ads.segment(segDur)})
			disc = false
			if rangeLen > 0 {
				nextOff = rangeOff + rangeLen
			}
			seq++
			pendingSeg, rangeOff, rangeLen = false, 0, 0
		}
	}
	if len(variants) > 0 {
		for i := range variants {
			if g := variants[i].AudioURL; g != "" {
				variants[i].AudioURL = audio[g].uri // "" = the audio inside the video
			}
		}
		sort.SliceStable(variants, func(i, j int) bool { return variants[i].Bandwidth > variants[j].Bandwidth })
		return variants, nil, nil
	}
	if len(media.Segments) == 0 {
		return nil, nil, errors.New("the playlist has no video segments")
	}
	ads.finish(media)
	numberParts(media)
	return nil, media, nil
}

func (m *Manager) fetchText(ctx context.Context, d *Download, u string) (string, string, error) {
	req, err := buildRequest(ctx, d, u, "")
	if err != nil {
		return "", "", err
	}
	resp, err := doReqUA(d, req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		drainClose(resp.Body)
		return "", "", statusErr{resp.StatusCode, resp.Status, retryAfterHeader(resp.Header)}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	m.countDown(int64(len(b))) // playlists, robots.txt, sync checks: data used too
	return string(b), resp.Request.URL.String(), err
}

type hlsInfo struct {
	Variants []hlsVariant `json:"variants"`
	Chosen   hlsVariant   `json:"chosen"`
	Media    *hlsMedia    `json:"-"`
	MediaURL string       `json:"-"`
	FMP4     bool         `json:"fmp4"`
	Segments int          `json:"segments"`
	Duration float64      `json:"duration"`
	Live     bool         `json:"live"`
	DRM      bool         `json:"drm"`
	Ext      string       `json:"ext"`
	Audio    *hlsMedia    `json:"-"` // separate audio track, if the stream has one
	AudioURL string       `json:"-"`
	Format   string       `json:"format"` // HLS or DASH
	Event    bool         `json:"event"`  // a live event stream: everything since it began is listed
	Target   float64      `json:"target"` // a live stream's target duration (seconds)

	Audios     []*streamTrack `json:"audios,omitempty"` // the audio and subtitle tracks offered (tracks.go)
	Subs       []*streamTrack `json:"subs,omitempty"`
	Ads        []adBreak      `json:"ads,omitempty"` // marked ad breaks (discont.go)
	AudioTrack *streamTrack   `json:"-"`             // the chosen audio, and the other tracks to keep
	MoreAudio  []*streamTrack `json:"-"`
	Extras     []*extraTrack  `json:"-"`
	SubMedia   []*extraTrack  `json:"-"`
	SubErr     error          `json:"-"`
	VideoWebm  bool           `json:"-"` // DASH: the video (or main) pieces are WebM, the audio's too
	AudioWebm  bool           `json:"-"`
}

// resolveHLS loads the playlist (and the chosen variant if it's a master playlist).
func (m *Manager) resolveHLS(ctx context.Context, d *Download, u string, bandwidth int) (*hlsInfo, error) {
	body, final, err := m.fetchText(ctx, d, u)
	if err != nil {
		return nil, err
	}
	if isMPD(body) {
		return m.resolveDASH(ctx, d, final, body, bandwidth)
	}
	variants, media, err := parseM3U8(final, body)
	if err != nil {
		return nil, err
	}
	info := &hlsInfo{Variants: variants, MediaURL: final, Format: "HLS"}
	if media == nil {
		info.Audios, info.Subs = hlsTracks(variants, parseRenditions(final, body))
		info.Variants = shownVariants(variants)
		if info.Chosen, err = m.chooseVariant(d, info.Variants, bandwidth); err != nil {
			return nil, err
		}
		chooseHLSAudio(info, variants, d)
		if d.AudioOnly && info.Chosen.AudioURL != "" { // the sound alone: its playlist is the main one
			info.Chosen.URL, info.Chosen.AudioURL = info.Chosen.AudioURL, ""
		}
		body, final, err = m.fetchText(ctx, d, info.Chosen.URL)
		if err != nil {
			return nil, err
		}
		_, media, err = parseM3U8(final, body)
		if err != nil {
			return nil, err
		}
		if media == nil {
			return nil, errors.New("nested master playlists aren't supported")
		}
		info.MediaURL = final
	}
	info.Media = media
	info.FMP4 = media.InitURL != ""
	info.Segments = len(media.Segments)
	info.Duration = media.Duration
	info.Live = !media.Ended
	info.DRM = media.DRM
	info.Event, info.Target = media.Event, media.Target
	info.Ext = ".mp4" // .ts pieces are repackaged into MP4 once downloaded
	if u := info.Chosen.AudioURL; u != "" {
		body, final, err := m.fetchText(ctx, d, u)
		if err != nil {
			return nil, fmt.Errorf("audio track: %w", err)
		}
		_, am, err := parseM3U8(final, body)
		if err != nil || am == nil {
			return nil, errors.New("couldn't read the audio track's playlist")
		}
		info.Audio, info.AudioURL = am, final
		info.Segments += len(am.Segments)
		info.Live = info.Live || !am.Ended
		info.DRM = info.DRM || am.DRM
	}
	if err := m.resolveHLSTracks(ctx, d, info); err != nil {
		return nil, err
	}
	settleParts(info, d.NoAds)
	info.Ext = streamExt(info, d)
	return info, nil
}

// ffmpegPath finds an optional ffmpeg. Zipline doesn't need it; it's only tried for the rare
// formats the built-in converter can't put into MP4 (MPEG-2 video, DTS audio and similar).
var ffmpegPath = func() string {
	if exe, err := os.Executable(); err == nil {
		for _, n := range []string{"ffmpeg.exe", "ffmpeg"} {
			p := filepath.Join(filepath.Dir(exe), n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

func hlsName(u, title, ext string) string {
	n := strings.TrimSpace(title)
	if n == "" {
		p, _ := url.Parse(u)
		if p != nil {
			n = strings.TrimSuffix(path.Base(p.Path), path.Ext(p.Path))
		}
		if n == "" || n == "/" || n == "." || strings.EqualFold(n, "index") || strings.EqualFold(n, "master") || strings.EqualFold(n, "playlist") {
			n = "video-" + time.Now().Format("2006-01-02-150405")
		}
	}
	return sanitizeName(n) + ext
}

// downloadHLS downloads (or resumes) a stream into d.partPath, then finishes it.
func (m *Manager) downloadHLS(ctx context.Context, d *Download) error {
	m.setStatus(d, StConnecting)
	if m.recorded(d) { // a recording that was stopped (or cut off): package what it holds
		return m.packRecording(ctx, d)
	}
	info, err := m.resolveHLS(ctx, d, d.URL, d.Variant)
	if err != nil {
		return m.streamGone(d, err)
	}
	if info.DRM {
		return permanentErr{errors.New("this video is DRM-protected, so it can't be downloaded")}
	}
	if err := streamRefused(info); err != nil {
		return err
	}
	if info.Live {
		return m.recordLive(ctx, d, info)
	}
	media, amedia := info.Media, info.Audio
	fresh := d.Parts == 0 || d.Parts != len(media.Segments)
	freshA := amedia != nil && (fresh || d.AParts != len(amedia.Segments))

	m.mu.Lock()
	d.Kind, d.Resumable, d.FinalURL = "hls", true, info.MediaURL
	if d.Variant == 0 {
		d.Variant = info.Chosen.Bandwidth
	}
	if d.VarRes == "" && d.VarCodecs == "" { // (bandwidth alone can match two qualities)
		d.VarRes, d.VarCodecs, d.VarFPS = info.Chosen.Resolution, info.Chosen.Codecs, info.Chosen.FrameRate
	}
	if fresh {
		d.Parts, d.PartsDone, d.PartBytes, d.Packaged = len(media.Segments), 0, 0, false
		d.resetStream()
		d.setQuality(info.Chosen)
		if d.FileName == "" {
			d.FileName = hlsName(d.URL, "", info.Ext)
		}
		// match the extension to what we produce; naming it .ts keeps the stream as .ts
		switch ext := strings.ToLower(filepath.Ext(d.FileName)); {
		case ext == "":
			d.FileName += info.Ext
		case ext == ".ts" && (info.FMP4 || amedia != nil || len(info.Extras) > 0 || len(info.SubMedia) > 0):
			d.FileName = strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName)) + info.Ext
		}
		if d.Category == "" {
			d.Category = "Video"
		}
		if d.Dir == "" {
			d.Dir = m.dirFor(d.Category)
		}
		d.FileName = m.uniqueName(d.Dir, d.FileName, d)
	}
	if freshA {
		d.AParts, d.ADone, d.ABytes, d.Packaged = len(amedia.Segments), 0, 0, false
		d.AWantSecs, d.AShortSecs = 0, 0
	} else if amedia == nil {
		d.AParts, d.ADone, d.ABytes = 0, 0, 0
	}
	freshX := settleExtras(d, info, fresh)
	d.Status = StDownloading
	d.Note = ""
	m.dirty = true
	m.mu.Unlock()
	d.lim.SetRate(int64(d.LimitKB) * 1024)

	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return err
	}
	// a restart can find everything downloaded and already combined (it stopped just before or
	// after the final file replaced the pieces): finish that step and leave the file alone
	complete := !fresh && d.PartsDone >= len(media.Segments) && (amedia == nil || (!freshA && d.ADone >= len(amedia.Segments))) && extrasDone(d, freshX)
	packaged := complete && d.Packaged
	atomic.StoreInt64(&d.done, d.PartBytes+d.ABytes+extraBytes(d))
	if packaged {
		if _, err := os.Stat(d.partPath() + ".mp4tmp"); err == nil {
			if err := os.Rename(d.partPath()+".mp4tmp", d.partPath()); err != nil {
				return err
			}
		}
		os.Remove(d.audioPartPath())
		removeExtras(d)
	} else {
		r := m.newRun(d, info)
		d.hs = r
		defer func() { d.hs = nil }()
		if !fresh { // carrying on: the stream must still be the same quality
			if err := m.sameQuality(ctx, d, d, media); err != nil {
				return err
			}
		}
		// the separate audio comes first: it's small, and knowing its size early keeps the
		// progress estimate honest
		if amedia != nil {
			if err := m.fetchTrack(ctx, d, amedia, d.audioPartPath(), freshA, &d.ADone, &d.ABytes); err != nil {
				return err
			}
		} else {
			os.Remove(d.audioPartPath())
		}
		if err := m.fetchExtras(ctx, d, info, freshX); err != nil {
			return err
		}
		if err := m.fetchTrack(ctx, d, media, d.partPath(), fresh, &d.PartsDone, &d.PartBytes); err != nil {
			return err
		}
		if err := m.checkLength(d); err != nil { // a short video isn't complete
			return err
		}
		if err := m.packageHLS(ctx, d, info); err != nil {
			return err
		}
	}
	m.mu.Lock()
	d.Size = d.PartBytes + d.ABytes
	m.mu.Unlock()
	if st, err := os.Stat(d.partPath()); err == nil {
		m.mu.Lock()
		d.Size = st.Size()
		m.mu.Unlock()
	}
	return m.finish(ctx, d)
}

// fetchTrack downloads (or resumes) one playlist's pieces into path. done and bytes are the
// download's counters for that track (guarded by m.mu).
func (m *Manager) fetchTrack(ctx context.Context, d *Download, media *hlsMedia, path string, fresh bool, done *int, bytes *int64) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	m.mu.Lock()
	have, start := *bytes, *done
	m.mu.Unlock()
	if fresh {
		have = 0
	}
	if st, err := f.Stat(); err == nil && st.Size() < have { // the list says more than reached the disk (a crash): start the track again
		have, start = 0, 0
		m.mu.Lock()
		*done, *bytes = 0, 0
		m.mu.Unlock()
	}
	if err := f.Truncate(have); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	r := d.run(m)
	r.open(media, f, have)
	defer r.close(f)
	if have == 0 && media.InitURL != "" { // fragmented MP4: the init piece goes first
		b, err := r.fetchInit(ctx, media)
		if err != nil {
			return fmt.Errorf("init segment: %w", err)
		}
		if _, err := f.Write(b); err != nil {
			return permanentErr{fmt.Errorf("disk: %w", err)}
		}
		m.mu.Lock()
		*bytes += int64(len(b))
		m.dirty = true
		m.mu.Unlock()
		atomic.AddInt64(&d.done, int64(len(b)))
	}
	return m.fetchParts(ctx, d, media, path, start, have, f, func(n int, written int64) {
		m.mu.Lock()
		*done, *bytes = n, *bytes+written
		m.dirty = true
		m.mu.Unlock()
	})
}

// packageHLS turns the downloaded pieces into the final file: .ts pieces become MP4, MP4
// pieces become a regular MP4, and a separate audio track is merged in. Nothing is re-encoded.
// When something can't be combined, the playable parts are kept and the note says why.
func (m *Manager) packageHLS(ctx context.Context, d *Download, info *hlsInfo) error {
	part, ap := d.partPath(), ""
	if info.Audio != nil {
		ap = d.audioPartPath()
	}
	if ap == "" && !info.FMP4 && strings.EqualFold(filepath.Ext(d.FileName), ".ts") && len(info.Extras) == 0 && len(info.SubMedia) == 0 {
		return nil // asked to keep the original .ts
	}
	stage := "Converting to " + containerName(info.Ext)
	if ap != "" {
		stage = "Adding the audio track"
	}
	run := func(inputs []string, skip []bool) (remuxResult, error) {
		tmp := part + ".mp4tmp"
		m.setStage(d, stage, 0)
		defer m.setStage(d, "", 0)
		res, err := m.repackageStream(ctx, d, info, tmp, inputs, skip, func(p float64) { m.setStage(d, stage, int(p*100)) })
		if err != nil {
			os.Remove(tmp)
			return res, err
		}
		m.markPackaged(d)
		return res, os.Rename(tmp, part)
	}
	note := func(n string) {
		m.mu.Lock()
		d.Note = n
		m.dirty = true
		m.mu.Unlock()
	}
	inputs, skip := []string{part}, []bool{false}
	if ap != "" {
		inputs, skip = append(inputs, ap), []bool{true, false}
	}
	res, err := run(inputs, skip)
	if err == nil {
		os.Remove(ap)
		removeExtras(d)
		if !nameFits(d.FileName, res.ext) { // the file type follows the tracks (an audio-only stream: .m4a)
			return m.renameDownload(d, strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName))+res.ext)
		}
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err() // paused: this runs again on resume
	}
	// optional ffmpeg for formats MP4 can't hold here
	if ff := ffmpegPath(); ff != "" && errors.Is(err, errUnsupportedStream) {
		if ffErr := remux(ctx, ff, part, ap); ffErr == nil {
			os.Remove(ap)
			m.keepExtrasBeside(d)
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	reason := friendlyRemuxErr(err)
	if ap != "" { // keep the audio next to the video rather than losing it
		name := strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName)) + " (audio)" + audioExt(ap)
		m.mu.Lock()
		name = m.uniqueName(d.Dir, name, nil)
		m.mu.Unlock()
		if rerr := os.Rename(ap, filepath.Join(d.Dir, name)); rerr != nil {
			return rerr
		}
		m.keepExtrasBeside(d)
		if !info.FMP4 { // still turn the video into MP4 if it can be
			if _, verr := run([]string{part}, []bool{false}); verr != nil && ctx.Err() != nil {
				return ctx.Err()
			} else if verr != nil {
				if rerr := m.renameDownload(d, strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName))+".ts"); rerr != nil {
					return rerr
				}
			}
		}
		note("The audio couldn't be added (" + reason + "), so it's saved separately as " + name)
		m.markPackaged(d)
		return nil
	}
	m.keepExtrasBeside(d)
	if info.FMP4 {
		return nil // the downloaded MP4 pieces already play as they are
	}
	if rerr := m.renameDownload(d, strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName))+".ts"); rerr != nil {
		return rerr
	}
	note("Saved as .ts (plays in VLC): " + reason)
	return nil
}

// markPackaged records, on disk, that the final file is ready, before it replaces the pieces.
func (m *Manager) markPackaged(d *Download) {
	m.mu.Lock()
	d.Packaged = true
	m.dirty = true
	m.mu.Unlock()
	m.save()
}

// audioExt picks a file extension for a separate audio file that couldn't be merged.
func audioExt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ".audio"
	}
	defer f.Close()
	b := make([]byte, 4096)
	n, _ := io.ReadFull(f, b)
	b = b[:n]
	if n >= 10 && string(b[:3]) == "ID3" {
		skip := 10 + (int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f))
		if skip < n {
			b = b[skip:]
		}
	}
	switch {
	case len(b) >= 8 && string(b[4:8]) == "ftyp":
		return ".m4a"
	case len(b) >= 4 && binary.BigEndian.Uint32(b) == mkvEBML:
		return ".webm"
	case len(b) > 0 && b[0] == 0x47:
		return ".ts"
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xF6 == 0xF0:
		return ".aac"
	case len(b) >= 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0:
		return ".mp3"
	case len(b) >= 2 && b[0] == 0x0B && b[1] == 0x77:
		return ".ac3"
	}
	return ".audio"
}

// fetchSegments downloads segments from d.PartsDone on many connections and appends them in order.
func (m *Manager) fetchSegments(ctx context.Context, d *Download, media *hlsMedia, start int, f *os.File, progress func(done int, written int64)) error {
	return d.run(m).fetchList(ctx, media, start, len(media.Segments), f, progress)
}

type keyCache struct {
	mu sync.Mutex
	m  map[string][]byte
}

// fetchSegment fetches one piece once (hlsfetch.go has the retries and checks).
func (m *Manager) fetchSegment(ctx context.Context, d *Download, s hlsSegment, keys *keyCache) ([]byte, error) {
	b, got, err := m.fetchPieceOnce(ctx, d, s, keys)
	if err != nil {
		atomic.AddInt64(&d.done, -got)
		return nil, err
	}
	m.countDown(got)
	return b, nil
}

func (k *keyCache) get(ctx context.Context, m *Manager, d *Download, u string) ([]byte, error) {
	k.mu.Lock()
	if b, ok := k.m[u]; ok {
		k.mu.Unlock()
		return b, nil
	}
	k.mu.Unlock()
	req, err := buildRequest(ctx, d, u, "")
	if err != nil {
		return nil, err
	}
	resp, err := doReq(d, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("couldn't get the stream key: %w", statusErr{resp.StatusCode, resp.Status, retryAfterHeader(resp.Header)})
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	m.countDown(int64(len(b)))
	if err != nil {
		return nil, err
	}
	if len(b) != 16 {
		return nil, permanentErr{errors.New("this video uses protected keys (DRM), so it can't be downloaded")}
	}
	k.mu.Lock()
	k.m[u] = b
	k.mu.Unlock()
	return b, nil
}

func decryptSegment(b, key []byte, s hlsSegment) ([]byte, error) {
	if len(b) == 0 || len(b)%aes.BlockSize != 0 {
		return nil, errors.New("encrypted segment has a bad length")
	}
	iv := s.IV
	if iv == nil {
		iv = make([]byte, 16)
		binary.BigEndian.PutUint64(iv[8:], uint64(s.Seq))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(b))
	cipher.NewCBCDecrypter(blk, iv).CryptBlocks(out, b)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(out) {
		return nil, errors.New("couldn't decrypt the segment (wrong key?)")
	}
	return out[:len(out)-pad], nil
}

// remux is the optional ffmpeg fallback: it rewrites the part file in place as MP4, adding audio if given.
func remux(ctx context.Context, ff, video, audio string) error {
	out := video + ".mp4"
	args := []string{"-y", "-loglevel", "error", "-i", video}
	if audio != "" {
		args = append(args, "-i", audio, "-map", "0:v:0?", "-map", "1:a:0?")
	}
	args = append(args, "-c", "copy", "-movflags", "+faststart", "-f", "mp4", out)
	cmd := exec.CommandContext(ctx, ff, args...)
	hideConsole(cmd)
	if b, err := cmd.CombinedOutput(); err != nil {
		os.Remove(out)
		msg := strings.TrimSpace(string(b))
		if len(msg) > 160 {
			msg = msg[:160]
		}
		return fmt.Errorf("ffmpeg: %v %s", err, msg)
	}
	return os.Rename(out, video)
}

func (m *Manager) setStage(d *Download, stage string, pct int) {
	m.mu.Lock()
	if d.stage != stage || d.stagePct != pct {
		d.stage, d.stagePct = stage, pct
		m.dirty = true
	}
	m.mu.Unlock()
}

// isHLSResponse is used by the generic engine to hand off to the HLS path.
func isHLSResponse(u string, resp *http.Response) bool {
	return looksLikeHLS(u) || (resp != nil && isHLSMime(resp.Header.Get("Content-Type")))
}

// renameDownload changes an unfinished download's file name and moves its partial file with it.
func (m *Manager) renameDownload(d *Download, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, oldA, oldX := d.partPath(), d.audioPartPath(), d.extraPartPaths()
	d.FileName = m.uniqueName(d.Dir, name, d)
	if err := os.Rename(old, d.partPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	os.Rename(oldA, d.audioPartPath())
	moveExtras(d, oldX)
	m.dirty = true
	return nil
}
