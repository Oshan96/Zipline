package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// MPEG-DASH (.mpd) streams. The manifest is turned into the same piece lists HLS uses, so
// downloading, resuming and merging are shared: the best (or chosen) video quality and the
// best audio track are fetched on many connections and combined into one file.

type mpdDoc struct {
	Type     string      `xml:"type,attr"`
	Duration string      `xml:"mediaPresentationDuration,attr"`
	BaseURL  []string    `xml:"BaseURL"`
	Periods  []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	ID       string       `xml:"id,attr"`
	Events   []mpdDesc    `xml:"EventStream"` // SCTE-35 splice signals mark an ad break (dashtracks.go)
	Start    string       `xml:"start,attr"`
	Duration string       `xml:"duration,attr"`
	BaseURL  []string     `xml:"BaseURL"`
	Sets     []mpdSet     `xml:"AdaptationSet"`
	Template *mpdTemplate `xml:"SegmentTemplate"`
	List     *mpdList     `xml:"SegmentList"`
	Base     *mpdBase     `xml:"SegmentBase"`
}

type mpdSet struct {
	ID          string       `xml:"id,attr"`
	Labels      []string     `xml:"Label"`
	Access      []mpdDesc    `xml:"Accessibility"`
	Channels    []mpdDesc    `xml:"AudioChannelConfiguration"`
	Props       []mpdDesc    `xml:"SupplementalProperty"`
	EProps      []mpdDesc    `xml:"EssentialProperty"`
	FrameRate   string       `xml:"frameRate,attr"`
	ContentType string       `xml:"contentType,attr"`
	MimeType    string       `xml:"mimeType,attr"`
	Codecs      string       `xml:"codecs,attr"`
	Lang        string       `xml:"lang,attr"`
	Width       int          `xml:"width,attr"`
	Height      int          `xml:"height,attr"`
	BaseURL     []string     `xml:"BaseURL"`
	Protection  []struct{}   `xml:"ContentProtection"`
	Roles       []mpdDesc    `xml:"Role"`
	Template    *mpdTemplate `xml:"SegmentTemplate"`
	List        *mpdList     `xml:"SegmentList"`
	Base        *mpdBase     `xml:"SegmentBase"`
	Reps        []mpdRep     `xml:"Representation"`
}

type mpdDesc struct {
	Scheme string `xml:"schemeIdUri,attr"`
	Value  string `xml:"value,attr"`
}

type mpdRep struct {
	FrameRate  string       `xml:"frameRate,attr"`
	Channels   []mpdDesc    `xml:"AudioChannelConfiguration"`
	Props      []mpdDesc    `xml:"SupplementalProperty"`
	ID         string       `xml:"id,attr"`
	Bandwidth  int          `xml:"bandwidth,attr"`
	Width      int          `xml:"width,attr"`
	Height     int          `xml:"height,attr"`
	MimeType   string       `xml:"mimeType,attr"`
	Codecs     string       `xml:"codecs,attr"`
	BaseURL    []string     `xml:"BaseURL"`
	Protection []struct{}   `xml:"ContentProtection"`
	Template   *mpdTemplate `xml:"SegmentTemplate"`
	List       *mpdList     `xml:"SegmentList"`
	Base       *mpdBase     `xml:"SegmentBase"`
}

type mpdTemplate struct {
	Media       string       `xml:"media,attr"`
	Init        string       `xml:"initialization,attr"`
	StartNumber *int64       `xml:"startNumber,attr"`
	Timescale   *int64       `xml:"timescale,attr"`
	Duration    *int64       `xml:"duration,attr"`
	Timeline    *mpdTimeline `xml:"SegmentTimeline"`
}

type mpdTimeline struct {
	S []struct {
		T *int64 `xml:"t,attr"`
		D int64  `xml:"d,attr"`
		R int64  `xml:"r,attr"`
	} `xml:"S"`
}

type mpdURL struct {
	Source string `xml:"sourceURL,attr"`
	Range  string `xml:"range,attr"`
}

type mpdList struct {
	Timescale *int64  `xml:"timescale,attr"`
	Duration  *int64  `xml:"duration,attr"`
	Init      *mpdURL `xml:"Initialization"`
	URLs      []struct {
		Media string `xml:"media,attr"`
		Range string `xml:"mediaRange,attr"`
	} `xml:"SegmentURL"`
}

// oneFile reports whether the list only names byte ranges of the representation's own file.
// Then the whole file is fetched instead: some packagers leave its last bytes out of the list.
func (l *mpdList) oneFile() bool {
	if l.Init != nil && l.Init.Source != "" {
		return false
	}
	for _, u := range l.URLs {
		if u.Media != "" {
			return false
		}
	}
	return true
}

type mpdBase struct {
	IndexRange string  `xml:"indexRange,attr"`
	Init       *mpdURL `xml:"Initialization"`
}

func looksLikeDASH(u string) bool {
	p := strings.ToLower(u)
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	return strings.HasSuffix(p, ".mpd")
}

func isMPD(body string) bool {
	head := body
	if len(head) > 2048 {
		head = head[:2048]
	}
	return strings.Contains(head, "<MPD")
}

var isoDurRE = regexp.MustCompile(`^P(?:(\d+(?:\.\d+)?)D)?(?:T(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

// isoDuration parses ISO 8601 durations like PT1H2M3.5S (seconds; 0 when empty or invalid).
func isoDuration(s string) float64 {
	m := isoDurRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	var out float64
	for i, mul := range []float64{86400, 3600, 60, 1} {
		if m[i+1] != "" {
			v, _ := strconv.ParseFloat(m[i+1], 64)
			out += v * mul
		}
	}
	return out
}

func joinBase(base *url.URL, refs []string) *url.URL {
	if len(refs) == 0 || strings.TrimSpace(refs[0]) == "" {
		return base
	}
	r, err := url.Parse(strings.TrimSpace(refs[0]))
	if err != nil {
		return base
	}
	return base.ResolveReference(r)
}

var tplVarRE = regexp.MustCompile(`\$(RepresentationID|Number|Time|Bandwidth)(%0(\d+)d)?\$|\$\$`)

func expandTemplate(tpl, repID string, bandwidth int, number, t int64) string {
	return tplVarRE.ReplaceAllStringFunc(tpl, func(v string) string {
		if v == "$$" {
			return "$"
		}
		m := tplVarRE.FindStringSubmatch(v)
		var n int64
		switch m[1] {
		case "RepresentationID":
			return repID
		case "Number":
			n = number
		case "Time":
			n = t
		case "Bandwidth":
			n = int64(bandwidth)
		}
		if m[3] != "" {
			w, _ := strconv.Atoi(m[3])
			return fmt.Sprintf("%0*d", w, n)
		}
		return strconv.FormatInt(n, 10)
	})
}

// mergeTemplate fills in attributes missing at a lower level from the level above.
func mergeTemplate(levels ...*mpdTemplate) *mpdTemplate {
	var out *mpdTemplate
	for _, l := range levels {
		if l == nil {
			continue
		}
		if out == nil {
			out = &mpdTemplate{}
		}
		if l.Media != "" {
			out.Media = l.Media
		}
		if l.Init != "" {
			out.Init = l.Init
		}
		if l.StartNumber != nil {
			out.StartNumber = l.StartNumber
		}
		if l.Timescale != nil {
			out.Timescale = l.Timescale
		}
		if l.Duration != nil {
			out.Duration = l.Duration
		}
		if l.Timeline != nil {
			out.Timeline = l.Timeline
		}
	}
	return out
}

func parseRange(s string) (off, n int64, ok bool) {
	a, b, found := strings.Cut(strings.TrimSpace(s), "-")
	x, err1 := strconv.ParseInt(a, 10, 64)
	y, err2 := strconv.ParseInt(b, 10, 64)
	if !found || err1 != nil || err2 != nil || y < x {
		return 0, 0, false
	}
	return x, y - x + 1, true
}

// dashRep is one quality of one track, with everything needed to list its pieces.
type dashRep struct {
	rep      mpdRep
	set      *mpdSet
	video    bool
	webm     bool
	base     *url.URL
	tpl      *mpdTemplate
	list     *mpdList
	sbase    *mpdBase
	periodS  float64 // period length in seconds
	drm      bool
	codecs   string
	priority int    // audio: prefer the main/default track
	kind     string // video, audio or text
	mime     string
}

func (r *dashRep) resolution() string {
	w, h := r.rep.Width, r.rep.Height
	if w == 0 {
		w, h = r.set.Width, r.set.Height
	}
	if w == 0 || h == 0 {
		return ""
	}
	return fmt.Sprintf("%dx%d", w, h)
}

// pieces lists the representation's init piece and media pieces.
func (m *Manager) dashPieces(ctx context.Context, d *Download, r *dashRep) (*hlsMedia, error) {
	media := &hlsMedia{Ended: true, Duration: r.periodS}
	res := func(ref string) string {
		u, err := url.Parse(ref)
		if err != nil {
			return ref
		}
		return r.base.ResolveReference(u).String()
	}
	switch {
	case r.tpl != nil && r.tpl.Media != "":
		t := r.tpl
		ts := int64(1)
		if t.Timescale != nil && *t.Timescale > 0 {
			ts = *t.Timescale
		}
		start := int64(1)
		if t.StartNumber != nil {
			start = *t.StartNumber
		}
		if t.Init != "" {
			media.InitURL = res(expandTemplate(t.Init, r.rep.ID, r.rep.Bandwidth, 0, 0))
		}
		add := func(num, tm int64, dur float64) {
			media.Segments = append(media.Segments, hlsSegment{URL: res(expandTemplate(t.Media, r.rep.ID, r.rep.Bandwidth, num, tm)), Seq: num, Dur: dur})
		}
		if t.Timeline != nil && len(t.Timeline.S) > 0 {
			var tm int64
			num := start
			end := int64(math.Ceil(r.periodS * float64(ts)))
			for i, s := range t.Timeline.S {
				if s.T != nil {
					tm = *s.T
				}
				if s.D <= 0 {
					return nil, errors.New("the stream's piece list is broken")
				}
				reps := s.R
				if reps < 0 { // repeat until the next entry, or the end of the period
					limit := end
					if i+1 < len(t.Timeline.S) && t.Timeline.S[i+1].T != nil {
						limit = *t.Timeline.S[i+1].T
					}
					reps = max((limit-tm+s.D-1)/s.D-1, 0)
				}
				for k := int64(0); k <= reps && len(media.Segments) < 200000; k++ {
					add(num, tm, float64(s.D)/float64(ts))
					num++
					tm += s.D
				}
			}
		} else if t.Duration != nil && *t.Duration > 0 {
			if r.periodS <= 0 {
				return nil, errors.New("the stream doesn't say how long it is")
			}
			n := int64(math.Ceil(r.periodS*float64(ts)/float64(*t.Duration) - 1e-9))
			if n > 200000 {
				return nil, errors.New("the stream has too many pieces")
			}
			step := float64(*t.Duration) / float64(ts)
			for i := int64(0); i < n; i++ {
				add(start+i, i**t.Duration, min(step, r.periodS-float64(i)*step)) // (the last may be shorter)
			}
			// the stated length is often rounded: a track can run a little past it (audio
			// priming), into one more piece. Look for up to two extra pieces.
			for i := n; i < n+2; i++ {
				u := res(expandTemplate(t.Media, r.rep.ID, r.rep.Bandwidth, start+i, i**t.Duration))
				if !m.pieceExists(ctx, d, u) {
					break
				}
				add(start+i, i**t.Duration, 0)
			}
		} else {
			return nil, errors.New("the stream's piece list is incomplete")
		}
	case r.list != nil && len(r.list.URLs) > 0 && !r.list.oneFile():
		if in := r.list.Init; in != nil {
			media.InitURL = r.base.String()
			if in.Source != "" {
				media.InitURL = res(in.Source)
			}
			if off, n, ok := parseRange(in.Range); ok {
				media.InitOff, media.InitLen = off, n
			}
		}
		var step float64
		if l := r.list; l.Duration != nil && *l.Duration > 0 {
			step = float64(*l.Duration)
			if l.Timescale != nil && *l.Timescale > 0 {
				step /= float64(*l.Timescale)
			}
		}
		for i, su := range r.list.URLs {
			s := hlsSegment{URL: r.base.String(), Seq: int64(i), Dur: step}
			if su.Media != "" {
				s.URL = res(su.Media)
			}
			if off, n, ok := parseRange(su.Range); ok {
				s.RangeOff, s.RangeLen = off, n
			}
			media.Segments = append(media.Segments, s)
		}
	default:
		// one file per quality (SegmentBase or just a BaseURL): fetch it in ranges so it
		// still comes down on many connections
		u := r.base.String()
		size, ranged, err := m.remoteSize(ctx, d, u)
		if err != nil {
			return nil, err
		}
		if !ranged || size <= 0 {
			media.Segments = []hlsSegment{{URL: u}}
			break
		}
		for off, i := int64(0), int64(0); off < size; off, i = off+dashPieceSize, i+1 {
			media.Segments = append(media.Segments, hlsSegment{URL: u, Seq: i, RangeOff: off, RangeLen: min(dashPieceSize, size-off), Chunk: true})
		}
	}
	if len(media.Segments) == 0 {
		return nil, errors.New("the stream has no pieces")
	}
	return media, nil
}

// pieceExists checks a piece the manifest doesn't strictly promise.
func (m *Manager) pieceExists(ctx context.Context, d *Download, u string) bool {
	req, err := buildRequest(ctx, d, u, "bytes=0-0")
	if err != nil {
		return false
	}
	resp, err := doReq(d, req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	ok := resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent
	return ok && !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html")
}

// dashPieceSize is how much of a one-file track each connection fetches at a time.
var dashPieceSize int64 = 4 << 20

// remoteSize asks for the first byte to learn the file's size and whether ranges work.
func (m *Manager) remoteSize(ctx context.Context, d *Download, u string) (int64, bool, error) {
	req, err := buildRequest(ctx, d, u, "bytes=0-0")
	if err != nil {
		return 0, false, err
	}
	resp, err := doReq(d, req)
	if err != nil {
		return 0, false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndexByte(cr, '/'); i >= 0 {
				if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
					return n, true, nil
				}
			}
		}
		return 0, false, nil
	case http.StatusOK:
		return resp.ContentLength, false, nil
	}
	return 0, false, fmt.Errorf("server replied %s", resp.Status)
}

// resolveDASH reads an MPD and picks the qualities to download.
func (m *Manager) resolveDASH(ctx context.Context, d *Download, mpdURL, body string, bandwidth int) (*hlsInfo, error) {
	var doc mpdDoc
	if err := xml.Unmarshal([]byte(body), &doc); err != nil {
		return nil, errors.New("couldn't read the DASH manifest")
	}
	info := &hlsInfo{Format: "DASH", MediaURL: mpdURL, Live: strings.EqualFold(doc.Type, "dynamic")}
	if info.Live {
		return info, nil
	}
	if len(doc.Periods) == 0 {
		return nil, errors.New("the DASH manifest lists no video")
	}
	base, err := url.Parse(mpdURL)
	if err != nil {
		return nil, err
	}
	base = joinBase(base, doc.BaseURL)
	return m.dashChoose(ctx, d, info, &doc, base, bandwidth)
}
