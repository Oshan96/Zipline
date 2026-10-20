package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Choosing DASH tracks. Every adaptation set is a track: video qualities, audio languages
// (with their role and accessibility marks), WebVTT subtitles. A manifest with several periods
// (usually the programme with ads between) is followed period by period: each chosen track
// continues in the next period in the adaptation set with the same id, else the one with the
// same kind, language and codec, and the periods become the parts of the download (discont.go).
// A period is an ad break when it carries SCTE-35 splice events, or when it's a short one
// between two longer ones.

// dashTextTrack says how a DASH subtitle track comes: one WebVTT file per period, WebVTT
// segments, or WebVTT in MP4 pieces (wvtt). TTML (stpp) isn't read.
type dashTextTrack struct {
	wvtt bool
}

type dashPeriodInfo struct {
	p          *mpdPeriod
	start, dur float64
	ad         bool
	reps       []*dashRep
}

func hasScheme(ds []mpdDesc, sub string, value string) bool {
	for _, x := range ds {
		if strings.Contains(strings.ToLower(x.Scheme), sub) && (value == "" || strings.EqualFold(strings.TrimSpace(x.Value), value)) {
			return true
		}
	}
	return false
}

// dashPeriods works out each period's place and length, and which are ad breaks.
func dashPeriods(doc *mpdDoc) []*dashPeriodInfo {
	total := isoDuration(doc.Duration)
	var out []*dashPeriodInfo
	var at float64
	for i := range doc.Periods {
		p := &doc.Periods[i]
		pi := &dashPeriodInfo{p: p, start: at}
		if p.Start != "" {
			pi.start = isoDuration(p.Start)
		}
		pi.dur = isoDuration(p.Duration)
		if pi.dur == 0 {
			if i+1 < len(doc.Periods) && doc.Periods[i+1].Start != "" {
				pi.dur = isoDuration(doc.Periods[i+1].Start) - pi.start
			} else if total > 0 {
				pi.dur = total - pi.start
			}
		}
		pi.dur = max(pi.dur, 0)
		pi.ad = hasScheme(p.Events, "scte35", "") || hasScheme(p.Events, "scte-35", "")
		at = pi.start + pi.dur
		out = append(out, pi)
	}
	for i := 1; i+1 < len(out); i++ { // a short period between two long ones
		a, b, c := out[i-1], out[i], out[i+1]
		if !b.ad && b.dur > 0 && b.dur <= 120 && b.dur*3 <= a.dur && b.dur*3 <= c.dur {
			b.ad = true
		}
	}
	return out
}

// periodReps lists a period's representations with what's needed to fetch them.
func periodReps(base *url.URL, pi *dashPeriodInfo) []*dashRep {
	p := pi.p
	pbase := joinBase(base, p.BaseURL)
	var out []*dashRep
	for si := range p.Sets {
		set := &p.Sets[si]
		for _, rep := range set.Reps {
			mime := strings.ToLower(rep.MimeType)
			if mime == "" {
				mime = strings.ToLower(set.MimeType)
			}
			codecs := rep.Codecs
			if codecs == "" {
				codecs = set.Codecs
			}
			kind := strings.ToLower(set.ContentType)
			if kind == "" {
				kind, _, _ = strings.Cut(mime, "/")
			}
			if kind == "application" && (codecFamily(codecs) == "wvtt" || codecFamily(codecs) == "ttml") || mime == "text/vtt" {
				kind = "text"
			}
			if kind != "video" && kind != "audio" && kind != "text" {
				continue // thumbnails
			}
			r := &dashRep{rep: rep, set: set, video: kind == "video", webm: strings.Contains(mime, "webm"),
				base: joinBase(joinBase(pbase, set.BaseURL), rep.BaseURL), periodS: pi.dur, codecs: codecs,
				drm: len(set.Protection) > 0 || len(rep.Protection) > 0, kind: kind, mime: mime}
			r.tpl = mergeTemplate(p.Template, set.Template, rep.Template)
			r.list = rep.List
			if r.list == nil {
				r.list = set.List
			}
			if r.list == nil {
				r.list = p.List
			}
			for _, role := range set.Roles {
				if strings.EqualFold(role.Value, "main") {
					r.priority = 1
				}
			}
			out = append(out, r)
		}
	}
	return out
}

func (r *dashRep) frameRate() float64 {
	s := r.rep.FrameRate
	if s == "" {
		s = r.set.FrameRate
	}
	a, b, ok := strings.Cut(s, "/")
	x, _ := strconv.ParseFloat(a, 64)
	if ok {
		if y, _ := strconv.ParseFloat(b, 64); y > 0 {
			x /= y
		}
	}
	return math.Round(x*1000) / 1000
}

// videoRange reads HDR from the transfer characteristics property (16 = PQ, 18 = HLG).
func (r *dashRep) videoRange() string {
	for _, ds := range [][]mpdDesc{r.rep.Props, r.set.Props, r.set.EProps} {
		for _, x := range ds {
			if strings.Contains(x.Scheme, "TransferCharacteristics") {
				switch strings.TrimSpace(x.Value) {
				case "16":
					return "PQ"
				case "18":
					return "HLG"
				}
				return "SDR"
			}
		}
	}
	return ""
}

func (r *dashRep) variant() hlsVariant {
	return hlsVariant{URL: r.rep.ID, Bandwidth: r.rep.Bandwidth, Resolution: r.resolution(), Codecs: r.codecs,
		FrameRate: r.frameRate(), VideoRange: r.videoRange(), Webm: r.webm}
}

// channels reads AudioChannelConfiguration: a count, or a CICP layout number.
func (r *dashRep) channels() string {
	for _, ds := range [][]mpdDesc{r.rep.Channels, r.set.Channels} {
		for _, x := range ds {
			v := strings.TrimSpace(x.Value)
			n, err := strconv.Atoi(v)
			switch {
			case strings.Contains(x.Scheme, "cicp:ChannelConfiguration") && err == nil:
				cicp := map[int]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 8, 9: 3, 10: 4, 11: 7, 12: 8, 13: 24, 14: 8, 16: 10, 17: 12, 19: 12, 20: 14}
				if c := cicp[n]; c > 0 {
					return strconv.Itoa(c)
				}
			case strings.Contains(x.Scheme, "dolby.com") && len(v) == 4: // a speaker bit mask (pairs count twice)
				if m, err := strconv.ParseUint(v, 16, 16); err == nil {
					pairs := uint16(0x0674) // the bits that stand for a pair of speakers (Lc/Rc, Lrs/Rrs, Lsd/Rsd, Lw/Rw, Vhl/Vhr, Lts/Rts)
					return strconv.Itoa(bits.OnesCount16(uint16(m)) + bits.OnesCount16(uint16(m)&pairs))
				}
			case err == nil && n > 0:
				return v
			}
		}
	}
	return ""
}

func setLabel(s *mpdSet) string {
	for _, l := range s.Labels {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func setRole(s *mpdSet, value string) bool {
	return hasScheme(s.Roles, "role", value)
}

// setDesc reports an audio description set (Role/Accessibility "description", or the TV-Anytime
// audio purpose 1: for the visually impaired).
func setDesc(s *mpdSet) bool {
	return setRole(s, "description") || hasScheme(s.Access, "role", "description") || hasScheme(s.Access, "audiopurposecs", "1")
}

// dashTrack makes the dialog's entry for an audio or text adaptation set (its best quality).
func dashTrack(r *dashRep, idx int) *streamTrack {
	s := r.set
	t := &streamTrack{Kind: "audio", Lang: s.Lang, Name: setLabel(s), Codec: r.codecs, Default: r.priority > 0, Webm: r.webm, Bandwidth: r.rep.Bandwidth}
	if r.kind == "text" {
		t.Kind, t.Codec = "subs", "wvtt"
		t.Forced = setRole(s, "forced-subtitle") || setRole(s, "forced_subtitle")
		t.SDH = setRole(s, "caption") || hasScheme(s.Access, "audiopurposecs", "2")
		t.text = &dashTextTrack{wvtt: strings.Contains(r.mime, "mp4") || codecFamily(r.codecs) == "wvtt" && r.mime != "text/vtt"}
	} else {
		t.Channels, t.Desc = r.channels(), setDesc(s)
	}
	key := s.ID
	if key == "" {
		key = fmt.Sprintf("%d", idx)
	}
	t.Key = r.kind + ":" + key + ":" + codecFamily(r.codecs)
	return t
}

// matchRep finds the representation that continues want's track in another period.
func matchRep(reps []*dashRep, want *dashRep) *dashRep {
	if want == nil {
		return nil
	}
	fam := codecFamily(want.codecs)
	score := func(r *dashRep) int {
		if r.kind != want.kind || r.drm {
			return -1
		}
		s := 0
		if want.set.ID != "" && r.set.ID == want.set.ID {
			s += 8
		}
		if codecFamily(r.codecs) == fam && r.webm == want.webm {
			s += 4
		} else if want.kind == "video" {
			s -= 2 // another format: still better than nothing (it's saved apart, see streamout.go)
		}
		if sameLang(r.set.Lang, want.set.Lang) || r.set.Lang == want.set.Lang {
			s += 2
		}
		if setDesc(r.set) == setDesc(want.set) {
			s++
		}
		return s
	}
	// the best score; then the same representation id; then the nearest bandwidth
	gap := func(r *dashRep) int {
		return max(r.rep.Bandwidth-want.rep.Bandwidth, want.rep.Bandwidth-r.rep.Bandwidth)
	}
	var best *dashRep
	bestScore := -1
	for _, r := range reps {
		sc := score(r)
		switch {
		case sc < 0:
		case best == nil || sc > bestScore:
			best, bestScore = r, sc
		case sc < bestScore:
		case (r.rep.ID == want.rep.ID) != (best.rep.ID == want.rep.ID):
			if r.rep.ID == want.rep.ID {
				best = r
			}
		case gap(r) < gap(best):
			best = r
		}
	}
	if best != nil && want.kind == "text" && !(sameLang(best.set.Lang, want.set.Lang) || best.set.Lang == want.set.Lang) {
		return nil // no subtitles in that language there
	}
	return best
}

// dashMedia lists one track's pieces across the periods, each period a part.
func (m *Manager) dashMedia(ctx context.Context, d *Download, periods []*dashPeriodInfo, want *dashRep, main int) (*hlsMedia, error) {
	out := &hlsMedia{Ended: true}
	for i, pi := range periods {
		r := want
		if i != main {
			r = matchRep(pi.reps, want)
		}
		if r == nil {
			continue
		}
		md, err := m.dashPieces(ctx, d, r)
		if err != nil {
			if i == main {
				return nil, err
			}
			continue // a broken ad period isn't worth failing the programme for
		}
		var mp *hlsMap
		if md.InitURL != "" {
			mp = &hlsMap{URL: md.InitURL, Off: md.InitOff, Len: md.InitLen}
			if out.InitURL == "" && len(out.Segments) == 0 {
				out.InitURL, out.InitOff, out.InitLen = md.InitURL, md.InitOff, md.InitLen
			}
		}
		n, timed := len(md.Segments), false
		for _, s := range md.Segments {
			timed = timed || s.Dur > 0
		}
		for k, s := range md.Segments {
			s.Part, s.Map, s.Ad, s.Disc = i, mp, pi.ad, k == 0 && len(out.Segments) > 0
			if !timed { // (the manifest's own timing when it gives one: the end check measures pieces against it)
				s.Dur = pi.dur / float64(n)
			}
			out.Segments = append(out.Segments, s)
		}
		out.Duration += pi.dur
	}
	if len(out.Segments) == 0 {
		return nil, errors.New("the stream has no pieces")
	}
	return out, nil
}

// dashChoose picks the tracks of a DASH manifest and lists their pieces.
func (m *Manager) dashChoose(ctx context.Context, d *Download, info *hlsInfo, doc *mpdDoc, base *url.URL, bandwidth int) (*hlsInfo, error) {
	periods := dashPeriods(doc)
	main := 0
	for i, pi := range periods { // the programme: the longest period that isn't an ad
		pi.reps = periodReps(base, pi)
		if !pi.ad && (periods[main].ad || pi.dur > periods[main].dur) {
			main = i
		}
	}
	reps := periods[main].reps
	var videos, audios, texts []*dashRep
	for _, r := range reps {
		switch r.kind {
		case "video":
			videos = append(videos, r)
		case "audio":
			audios = append(audios, r)
		case "text":
			if codecFamily(r.codecs) != "ttml" && !strings.Contains(r.mime, "ttml") {
				texts = append(texts, r)
			}
		}
	}
	if len(videos) == 0 && len(audios) == 0 {
		return nil, errors.New("the DASH manifest lists no video")
	}
	unprotected := func(list []*dashRep) []*dashRep {
		var out []*dashRep
		for _, r := range list {
			if !r.drm {
				out = append(out, r)
			}
		}
		return out
	}
	if len(videos) > 0 && len(unprotected(videos)) == 0 || len(audios) > 0 && len(unprotected(audios)) == 0 {
		info.DRM = true
		return info, nil
	}
	videos, audios = unprotected(videos), unprotected(audios) // some qualities protected, others not: those that aren't
	sort.SliceStable(videos, func(i, j int) bool { return videos[i].rep.Bandwidth > videos[j].rep.Bandwidth })
	for _, v := range videos {
		info.Variants = append(info.Variants, v.variant())
	}
	info.Variants = shownVariants(info.Variants)
	// audio: one track per adaptation set, its best quality
	best := map[*mpdSet]*dashRep{}
	var sets []*mpdSet
	for _, r := range audios {
		if b, ok := best[r.set]; !ok || r.rep.Bandwidth > b.rep.Bandwidth {
			if !ok {
				sets = append(sets, r.set)
			}
			best[r.set] = r
		}
	}
	trackRep := map[*streamTrack]*dashRep{}
	for i, s := range sets {
		t := dashTrack(best[s], i)
		info.Audios = append(info.Audios, t)
		trackRep[t] = best[s]
	}
	sort.SliceStable(info.Audios, func(i, j int) bool { // the main track first, then by quality
		a, b := info.Audios[i], info.Audios[j]
		if a.Default != b.Default {
			return a.Default
		}
		return a.Bandwidth > b.Bandwidth
	})
	bestText := map[*mpdSet]*dashRep{}
	var tsets []*mpdSet
	for _, r := range texts {
		if _, ok := bestText[r.set]; !ok {
			tsets = append(tsets, r.set)
			bestText[r.set] = r
		}
	}
	for i, s := range tsets {
		t := dashTrack(bestText[s], i)
		info.Subs = append(info.Subs, t)
		trackRep[t] = bestText[s]
	}
	p := d.StreamPick
	// the video, the audio to go with it (the same kind of pieces when there's a choice)
	var vrep *dashRep
	if len(videos) > 0 {
		cand := info.Variants
		if bandwidth == 0 && d.VarRes == "" && d.VarCodecs == "" { // no choice made: MP4 pieces when there are any (they make an MP4)
			var mp4 []hlsVariant
			for _, v := range cand {
				if !v.Webm {
					mp4 = append(mp4, v)
				}
			}
			if len(mp4) > 0 {
				cand = mp4
			}
		}
		ch, err := m.chooseVariant(d, cand, bandwidth) // a download under way keeps its quality (hlsident.go)
		if err != nil {
			return nil, err
		}
		for _, v := range videos {
			if v.rep.ID == ch.URL && v.rep.Bandwidth == ch.Bandwidth {
				vrep = v
				break
			}
		}
		if vrep == nil {
			vrep = videos[0]
		}
	}
	var amain *streamTrack
	if len(info.Audios) > 0 {
		amain = pickTrack(info.Audios, p.AudioKey, p.AudioLang, p.AudioName)
		if p.AudioKey == "" && p.AudioLang == "" && p.AudioName == "" && vrep != nil && amain.Webm != vrep.webm {
			// no choice made: the same language in the video's kind of pieces, so it stays one MP4 or WebM
			for _, t := range info.Audios {
				if t.Webm == vrep.webm && !t.Desc && t.Lang == amain.Lang {
					amain = t
					break
				}
			}
		}
	}
	mainRep := vrep
	if vrep == nil || p.AudioOnly {
		if amain == nil {
			return nil, errors.New("the DASH manifest lists no audio")
		}
		mainRep = trackRep[amain]
	}
	var err error
	if info.Media, err = m.dashMedia(ctx, d, periods, mainRep, main); err != nil {
		return nil, err
	}
	if vrep != nil {
		info.Chosen = vrep.variant()
	} else {
		info.Chosen = hlsVariant{URL: mainRep.rep.ID, Bandwidth: mainRep.rep.Bandwidth, Codecs: mainRep.codecs}
	}
	info.VideoWebm = mainRep.webm
	info.FMP4 = !mainRep.webm
	info.AudioTrack = amain
	if amain != nil && mainRep != trackRep[amain] {
		ar := trackRep[amain]
		if info.Audio, err = m.dashMedia(ctx, d, periods, ar, main); err != nil {
			return nil, fmt.Errorf("audio track: %w", err)
		}
		info.AudioURL, info.AudioWebm = ar.rep.ID, ar.webm
	}
	if p.AudioAll {
		seen := map[string]bool{}
		if amain != nil {
			seen[amain.Lang+"/"+amain.Name+"/"+strconv.FormatBool(amain.Desc)] = true
		}
		for _, t := range info.Audios {
			k := t.Lang + "/" + t.Name + "/" + strconv.FormatBool(t.Desc)
			if t == amain || seen[k] { // one per language and name (the first, best one)
				continue
			}
			seen[k] = true
			md, err := m.dashMedia(ctx, d, periods, trackRep[t], main)
			if err != nil {
				return nil, fmt.Errorf("audio track %s: %w", trackLabel(t), err)
			}
			info.MoreAudio = append(info.MoreAudio, t)
			info.Extras = append(info.Extras, &extraTrack{track: t, media: md})
		}
	}
	for _, t := range chooseSubs(info.Subs, "", p) {
		md, err := m.dashMedia(ctx, d, periods, trackRep[t], main)
		if err != nil {
			info.SubErr = err
			continue
		}
		info.SubMedia = append(info.SubMedia, &extraTrack{track: t, media: md})
	}
	var total float64
	for _, pi := range periods {
		total += pi.dur
	}
	info.Duration = total
	settleParts(info, p.NoAds)
	if p.NoAds {
		info.Duration = info.Media.Duration
	}
	info.Segments = len(info.Media.Segments)
	if info.Audio != nil {
		info.Segments += len(info.Audio.Segments)
	}
	for _, x := range info.Extras {
		info.Segments += len(x.media.Segments)
	}
	info.Ext = streamExt(info, d)
	if len(info.Variants) == 1 {
		info.Variants = nil
	}
	return info, nil
}
