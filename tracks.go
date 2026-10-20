package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Choosing what to take from a stream: the video quality (codec, frame rate, HDR), the audio
// track (language, audio description) and subtitles. The Add dialog lists what a stream offers;
// the choice is stored on the download (Variant, VarRes…, StreamPick) so a resumed download
// picks the same tracks again.

// StreamPick is what the Add dialog chose for a video stream besides its quality.
type StreamPick struct {
	AudioOnly bool   `json:"audioOnly,omitempty"` // just the sound (.m4a, .opus, .mka)
	AudioKey  string `json:"audioKey,omitempty"`  // the audio track ("" = the stream's default); its language
	AudioLang string `json:"audioLang,omitempty"` // and name find it again if the key doesn't
	AudioName string `json:"audioName,omitempty"`
	AudioAll  bool   `json:"audioAll,omitempty"` // keep every audio track, the chosen one first
	SubKey    string `json:"subKey,omitempty"`   // one subtitle track…
	SubLang   string `json:"subLang,omitempty"`
	SubName   string `json:"subName,omitempty"`
	SubAll    bool   `json:"subAll,omitempty"` // …or all of them
	NoAds     bool   `json:"noAds,omitempty"`  // leave out the ad breaks the stream marks
}

// TrackPart is the download state of one more audio track (Keep all languages).
type TrackPart struct {
	Lang  string `json:"lang,omitempty"`
	Name  string `json:"name,omitempty"`
	Parts int    `json:"parts"`
	Done  int    `json:"done"`
	Bytes int64  `json:"bytes"`
}

// extraPartPath is where the i-th extra track waits until it's merged in.
func (d *Download) extraPartPath(i int) string { return d.partPath() + ".x" + strconv.Itoa(i+1) }

// hlsRendition is one EXT-X-MEDIA entry of a master playlist.
type hlsRendition struct {
	typ, group, lang, name, channels, uri string
	def, auto, forced, desc, sdh          bool
}

// streamTrack is one audio or subtitle track as the Add dialog lists it.
type streamTrack struct {
	Key       string `json:"key"`            // tells it apart from the others (HLS: group/name; DASH: the adaptation set)
	Kind      string `json:"kind"`           // "audio" or "subs"
	Lang      string `json:"lang,omitempty"` // as the stream gives it (BCP 47)
	Name      string `json:"name,omitempty"`
	Channels  string `json:"channels,omitempty"` // "2", "6", "16/JOC"
	Codec     string `json:"codec,omitempty"`
	Default   bool   `json:"default,omitempty"`
	Auto      bool   `json:"autoselect,omitempty"`
	Desc      bool   `json:"desc,omitempty"`    // audio description (for the blind)
	Forced    bool   `json:"forced,omitempty"`  // subtitles for foreign-language parts only
	SDH       bool   `json:"sdh,omitempty"`     // subtitles for the deaf and hard of hearing
	Builtin   bool   `json:"builtin,omitempty"` // inside the video stream: nothing extra to fetch
	Group     string `json:"group,omitempty"`   // HLS rendition group
	Webm      bool   `json:"webm,omitempty"`
	Bandwidth int    `json:"bandwidth,omitempty"`

	url  string         // HLS: its playlist
	reps []*dashRep     // DASH: its representation in each period (nil = missing there)
	text *dashTextTrack // DASH subtitles
}

// is reports whether t is the stored choice: by key, else by language and name.
func (t *streamTrack) is(key, lang, name string) bool {
	if key != "" {
		return t.Key == key
	}
	return (lang != "" || name != "") && t.Lang == lang && t.Name == name
}

// parseRenditions reads the master playlist's EXT-X-MEDIA tags.
func parseRenditions(baseURL, body string) []hlsRendition {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	var out []hlsRendition
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1<<16), 1<<22)
	for sc.Scan() {
		tag, val, _ := strings.Cut(strings.TrimSpace(sc.Text()), ":")
		if tag != "#EXT-X-MEDIA" {
			continue
		}
		a := attrs(val)
		ch := strings.ToLower(a["CHARACTERISTICS"])
		r := hlsRendition{typ: strings.ToUpper(a["TYPE"]), group: a["GROUP-ID"], lang: a["LANGUAGE"], name: a["NAME"], channels: a["CHANNELS"],
			def: strings.EqualFold(a["DEFAULT"], "YES"), auto: strings.EqualFold(a["AUTOSELECT"], "YES"), forced: strings.EqualFold(a["FORCED"], "YES"),
			desc: strings.Contains(ch, "describes-video"), sdh: strings.Contains(ch, "transcribes-spoken-dialog") || strings.Contains(ch, "describes-music-and-sound")}
		if a["URI"] != "" {
			r.uri = resolveRef(base, a["URI"])
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------- codecs

// codecFamily names a codec string's format: h264, hevc, dv, av1, vp9, vp8, aac, mp3, ac3,
// eac3, opus, vorbis, flac, alac, dts, wvtt, ttml… ("" when unknown).
func codecFamily(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	p, rest, _ := strings.Cut(c, ".")
	switch p {
	case "avc1", "avc2", "avc3", "avc4", "h264":
		return "h264"
	case "hvc1", "hev1", "hevc", "h265":
		return "hevc"
	case "dvh1", "dvhe", "dva1", "dvav":
		return "dv"
	case "av01", "av1":
		return "av1"
	case "vp09", "vp9":
		return "vp9"
	case "vp08", "vp8":
		return "vp8"
	case "mp4a":
		switch rest {
		case "69", "6b", "40.34", "6B":
			return "mp3"
		}
		return "aac"
	case "mp3", "mp4a.6b":
		return "mp3"
	case "ac-3", "ac3":
		return "ac3"
	case "ec-3", "eac3", "ec+3":
		return "eac3"
	case "opus":
		return "opus"
	case "vorbis":
		return "vorbis"
	case "flac":
		return "flac"
	case "alac":
		return "alac"
	case "dtsc", "dtse", "dtsh", "dtsl", "dtsx":
		return "dts"
	case "ac-4":
		return "ac4"
	case "mhm1", "mha1":
		return "mpegh"
	case "wvtt":
		return "wvtt"
	case "stpp":
		return "ttml"
	case "mp4v":
		return "mpeg4"
	case "vvc1", "vvi1":
		return "vvc"
	}
	return ""
}

var videoFamilies = map[string]bool{"h264": true, "hevc": true, "dv": true, "av1": true, "vp9": true, "vp8": true, "mpeg4": true, "vvc": true}

// splitCodecs separates a CODECS list into video and audio codec strings.
func splitCodecs(codecs string) (video, audio []string) {
	for _, c := range strings.Split(codecs, ",") {
		c = strings.TrimSpace(c)
		f := codecFamily(c)
		switch {
		case c == "":
		case videoFamilies[f]:
			video = append(video, c)
		case f == "wvtt" || f == "ttml":
		default:
			audio = append(audio, c)
		}
	}
	return
}

// unholdable lists video formats the built-in converter can't turn into a playable file.
var unholdable = map[string]bool{"mpeg4": true, "vvc": true}

func holdable(codecs string) bool {
	v, _ := splitCodecs(codecs)
	for _, c := range v {
		if unholdable[codecFamily(c)] {
			return false
		}
	}
	return true
}

// isHDR reports whether a quality is HDR: PQ or HLG, Dolby Vision, or a VP9/AV1 profile with
// an HDR transfer function in its codec string.
func isHDR(v hlsVariant) bool {
	switch strings.ToUpper(v.VideoRange) {
	case "PQ", "HLG":
		return true
	}
	vc, _ := splitCodecs(v.Codecs)
	for _, c := range vc {
		switch codecFamily(c) {
		case "dv":
			return true
		case "vp9", "av1": // vp09.PP.LL.DD.CC.cp.tc… / av01.P.LLT.DD.M.CCC.cp.tc…: transfer 16 = PQ, 18 = HLG
			f := strings.Split(c, ".")
			at := 6
			if codecFamily(c) == "av1" {
				at = 7
			}
			if len(f) > at && (f[at] == "16" || f[at] == "18") {
				return true
			}
		}
	}
	return false
}

func audioOnlyVariant(v hlsVariant) bool {
	if v.Resolution != "" {
		return false
	}
	vc, ac := splitCodecs(v.Codecs)
	return len(vc) == 0 && len(ac) > 0
}

// pickVariant finds the stored quality again, or picks the best SDR one that can be saved.
func pickVariant(vs []hlsVariant, d *Download, bandwidth int) hlsVariant {
	if len(vs) == 0 {
		return hlsVariant{}
	}
	if d != nil && (d.VarRes != "" || d.VarCodecs != "" || d.VarFPS > 0) {
		same := func(v hlsVariant, bw bool) bool {
			return (!bw || v.Bandwidth == bandwidth) && v.Resolution == d.VarRes && v.Codecs == d.VarCodecs && (d.VarFPS == 0 || v.FrameRate == d.VarFPS)
		}
		for _, bw := range []bool{true, false} {
			for _, v := range vs {
				if same(v, bw) {
					return v
				}
			}
		}
	}
	if bandwidth > 0 {
		for _, v := range vs {
			if v.Bandwidth == bandwidth {
				return v
			}
		}
	}
	// (sorted by bandwidth, best first) the best SDR one that can be saved, else the best that can
	video := func(v hlsVariant) bool { return holdable(v.Codecs) && !audioOnlyVariant(v) }
	for _, ok := range []func(hlsVariant) bool{func(v hlsVariant) bool { return video(v) && !isHDR(v) }, video} {
		for _, v := range vs {
			if ok(v) {
				return v
			}
		}
	}
	return vs[0]
}

// pickTrack finds the stored audio or subtitle choice; with none stored it returns the
// stream's default: DEFAULT=YES (not audio description), then the built-in audio, then the first.
func pickTrack(list []*streamTrack, key, lang, name string) *streamTrack {
	if len(list) == 0 {
		return nil
	}
	if key != "" || lang != "" || name != "" {
		for _, k := range []string{key, ""} {
			for _, t := range list {
				if t.is(k, lang, name) {
					return t
				}
			}
		}
		for _, t := range list {
			if !t.Desc && sameLang(t.Lang, lang) {
				return t
			}
		}
	}
	for _, t := range list {
		if t.Default && !t.Desc {
			return t
		}
	}
	for _, t := range list {
		if t.Builtin {
			return t
		}
	}
	for _, t := range list {
		if !t.Desc {
			return t
		}
	}
	return list[0]
}

// ---------------------------------------------------------------- HLS

// hlsTracks lists the audio and subtitle tracks of a master playlist. Audio comes from every
// group the qualities use (a group with another codec is another track); the codec of a group
// is read from the CODECS of the qualities that use it.
func hlsTracks(vs []hlsVariant, rends []hlsRendition) (audios, subs []*streamTrack) {
	groupCodec := map[string]string{}
	used := map[string]bool{}
	for _, v := range vs {
		if v.AudioGroup != "" {
			used["AUDIO/"+v.AudioGroup] = true
			if _, ac := splitCodecs(v.Codecs); len(ac) > 0 && groupCodec[v.AudioGroup] == "" {
				groupCodec[v.AudioGroup] = ac[0]
			}
		}
		// subtitles in WebVTT only: IMSC/TTML ones (stpp in CODECS) can't be read
		if v.SubGroup != "" && !strings.Contains(strings.ToLower(v.Codecs), "stpp") {
			used["SUBTITLES/"+v.SubGroup] = true
		}
	}
	for _, r := range rends {
		if !used[r.typ+"/"+r.group] {
			continue
		}
		t := &streamTrack{Key: r.group + "/" + r.name, Lang: r.lang, Name: r.name, Default: r.def, Auto: r.auto, Group: r.group, url: r.uri}
		switch r.typ {
		case "AUDIO":
			t.Kind, t.Channels, t.Desc, t.Codec, t.Builtin = "audio", r.channels, r.desc, groupCodec[r.group], r.uri == ""
			audios = append(audios, t)
		case "SUBTITLES":
			if r.uri == "" {
				continue
			}
			t.Kind, t.Forced, t.SDH, t.Codec = "subs", r.forced, r.sdh, "wvtt"
			subs = append(subs, t)
		}
	}
	return
}

// chooseHLSAudio settles the audio for the chosen quality: which rendition of its group, and
// with Keep all languages the group's other renditions. Audio only takes the rendition's
// playlist as the main media (the audio inside the video, when there's no separate one).
func chooseHLSAudio(info *hlsInfo, vs []hlsVariant, d *Download) {
	inGroup := func(g string) (out []*streamTrack) {
		for _, t := range info.Audios {
			if t.Group == g {
				out = append(out, t)
			}
		}
		return out
	}
	group := inGroup(info.Chosen.AudioGroup)
	p := d.StreamPick
	exact := func() bool {
		for _, k := range []string{p.AudioKey, ""} {
			for _, t := range group {
				if t.is(k, p.AudioLang, p.AudioName) {
					return true
				}
			}
		}
		return false
	}
	if (p.AudioKey != "" || p.AudioLang != "" || p.AudioName != "") && !exact() {
		// the stored track is in another group (another codec): take the same video with that group
		for _, t := range info.Audios {
			if !t.is(p.AudioKey, p.AudioLang, p.AudioName) {
				continue
			}
			for _, v := range vs {
				if v.URL == info.Chosen.URL && v.AudioGroup == t.Group {
					info.Chosen, group = v, inGroup(t.Group)
				}
			}
		}
	}
	main := pickTrack(group, p.AudioKey, p.AudioLang, p.AudioName)
	info.AudioTrack = main
	info.Chosen.AudioURL = ""
	if main != nil {
		info.Chosen.AudioURL = main.url
	}
	if p.AudioAll {
		for _, t := range group {
			if t != main {
				info.MoreAudio = append(info.MoreAudio, t)
			}
		}
	}
	if p.AudioOnly && info.Chosen.AudioURL == "" {
		for _, v := range vs { // a quality that is only sound
			if audioOnlyVariant(v) {
				info.Chosen = v
				break
			}
		}
	}
}

// chooseSubs picks the subtitle tracks to keep: one (the stored choice) or all.
func chooseSubs(list []*streamTrack, group string, p StreamPick) []*streamTrack {
	if !p.SubAll && p.SubKey == "" && p.SubLang == "" && p.SubName == "" {
		return nil
	}
	var in []*streamTrack
	for _, t := range list {
		if group == "" || t.Group == "" || t.Group == group {
			in = append(in, t)
		}
	}
	if len(in) == 0 {
		in = list
	}
	if p.SubAll {
		return in
	}
	for _, k := range []string{p.SubKey, ""} {
		for _, t := range in {
			if t.is(k, p.SubLang, p.SubName) {
				return []*streamTrack{t}
			}
		}
	}
	for _, t := range in {
		if sameLang(t.Lang, p.SubLang) && !t.Forced {
			return []*streamTrack{t}
		}
	}
	return nil
}

// resolveHLSTracks loads the playlists of the extra tracks chosen: more audio languages and
// subtitles. Ad breaks are dropped from every track when asked.
func (m *Manager) resolveHLSTracks(ctx context.Context, d *Download, info *hlsInfo) error {
	load := func(u string) (*hlsMedia, error) {
		body, final, err := m.fetchText(ctx, d, u)
		if err != nil {
			return nil, err
		}
		_, md, err := parseM3U8(final, body)
		if err != nil || md == nil {
			return nil, errors.New("couldn't read the track's playlist")
		}
		return md, nil
	}
	for _, t := range info.MoreAudio {
		if t.url == "" { // the audio inside the video: kept with it
			continue
		}
		md, err := load(t.url)
		if err != nil {
			return fmt.Errorf("audio track %s: %w", trackLabel(t), err)
		}
		info.Extras = append(info.Extras, &extraTrack{track: t, media: md})
		info.Segments += len(md.Segments)
		info.Live = info.Live || !md.Ended
		info.DRM = info.DRM || md.DRM
	}
	for _, t := range chooseSubs(info.Subs, info.Chosen.SubGroup, d.StreamPick) {
		md, err := load(t.url)
		if err != nil {
			info.SubErr = err // subtitles never stop the video
			continue
		}
		info.SubMedia = append(info.SubMedia, &extraTrack{track: t, media: md})
	}
	return nil
}

// trackLabel names a track in notes and errors ("Deutsch", "de").
func trackLabel(t *streamTrack) string {
	if t == nil {
		return ""
	}
	if t.Name != "" {
		return t.Name
	}
	if t.Lang != "" {
		return t.Lang
	}
	return t.Kind
}

// extraTrack is a chosen track besides the main video and audio: more audio, or subtitles.
type extraTrack struct {
	track *streamTrack
	media *hlsMedia
}

// ---------------------------------------------------------------- the other audio tracks' files

// settleExtras matches the download's state of its other audio tracks to the ones chosen now
// (m.mu held). It reports which start from nothing.
func settleExtras(d *Download, info *hlsInfo, fresh bool) []bool {
	out := make([]bool, len(info.Extras))
	if len(d.Extra) != len(info.Extras) { // other tracks than last time: start them again
		removeExtras(d)
		d.Extra = nil
	}
	for i, x := range info.Extras {
		if i >= len(d.Extra) {
			d.Extra = append(d.Extra, &TrackPart{})
		}
		tp, n := d.Extra[i], len(x.media.Segments)
		if fresh || tp.Parts != n || tp.Lang != x.track.Lang || tp.Name != x.track.Name {
			*tp = TrackPart{Lang: x.track.Lang, Name: x.track.Name, Parts: n}
			out[i], d.Packaged = true, false
		}
	}
	return out
}

func extrasDone(d *Download, fresh []bool) bool {
	for i, x := range d.Extra {
		if i < len(fresh) && fresh[i] || x.Done < x.Parts {
			return false
		}
	}
	return true
}

func extraBytes(d *Download) int64 {
	var n int64
	for _, x := range d.Extra {
		n += x.Bytes
	}
	return n
}

// fetchExtras downloads (or resumes) the other audio tracks.
func (m *Manager) fetchExtras(ctx context.Context, d *Download, info *hlsInfo, fresh []bool) error {
	for i, x := range info.Extras {
		if i >= len(d.Extra) {
			break
		}
		if err := m.fetchTrack(ctx, d, x.media, d.extraPartPath(i), fresh[i], &d.Extra[i].Done, &d.Extra[i].Bytes); err != nil {
			return fmt.Errorf("audio track %s: %w", trackLabel(x.track), err)
		}
	}
	return nil
}

func (d *Download) extraPartPaths() []string {
	var out []string
	for i := range d.Extra {
		out = append(out, d.extraPartPath(i))
	}
	return out
}

// moveExtras follows a rename: the other tracks' files move with the main one.
func moveExtras(d *Download, old []string) {
	for i, p := range old {
		os.Rename(p, d.extraPartPath(i))
	}
}

// keepExtrasBeside keeps the other audio tracks next to the video when they couldn't be merged.
func (m *Manager) keepExtrasBeside(d *Download) {
	base := strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName))
	for i, x := range d.Extra {
		p := d.extraPartPath(i)
		label := x.Name
		if label == "" {
			label = x.Lang
		}
		m.mu.Lock()
		name := m.uniqueName(d.Dir, base+" (audio "+sanitizeName(label)+")"+audioExt(p), nil)
		m.mu.Unlock()
		os.Rename(p, filepath.Join(d.Dir, name))
	}
}

// containerName names a file type for the "Converting to …" stage.
func containerName(ext string) string {
	switch strings.ToLower(ext) {
	case ".webm":
		return "WebM"
	case ".opus":
		return "Opus"
	case "", ".mp4":
		return "MP4"
	}
	return strings.ToUpper(strings.TrimPrefix(ext, "."))
}

// nameFits reports whether a file name's extension suits the file type written: the types this
// writes must match; a name the user gave another extension (.mov, .m4v) keeps it for MP4.
func nameFits(name, ext string) bool {
	cur := strings.ToLower(filepath.Ext(name))
	if ext == "" || cur == ext {
		return true
	}
	switch cur {
	case "", ".mp4", ".m4a", ".webm", ".weba", ".mkv", ".mka", ".opus", ".ogg", ".ts":
		return false
	}
	return ext == ".mp4" || ext == ".m4a"
}

// streamChoices adds what the Add dialog offers for a stream to its probe answer: the qualities
// (with codecs, frame rate and HDR, in hlsVariant), the audio and subtitle tracks, the marked ad
// breaks, and the defaults picked.
func streamChoices(res map[string]any, info *hlsInfo) {
	if len(info.Audios) > 0 {
		res["audios"] = info.Audios
	}
	if len(info.Subs) > 0 {
		res["subs"] = info.Subs
	}
	if len(info.Ads) > 0 {
		res["ads"] = info.Ads
	}
	res["chosen"] = info.Chosen
	if info.AudioTrack != nil {
		res["audioKey"] = info.AudioTrack.Key
	}
	res["ext"] = info.Ext
}
