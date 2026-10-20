package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Putting a stream's downloaded tracks into the final file. The file type follows the tracks
// (as yt-dlp chooses it): MP4 when every track fits MP4 (the pieces came as MPEG-TS or MP4),
// WebM when all are VP8/VP9/AV1 with Opus/Vorbis, Matroska (.mkv) otherwise, e.g. VP9 from WebM
// pieces with AAC sound. Sound alone becomes .m4a, .opus (Ogg) or .mka. Audio tracks are
// labelled with their language and name, the chosen one first and playing by default.
// Subtitles are added as text tracks. A part of the stream in another format (an ad in H.264
// in an HEVC programme) can't share the programme's tracks, so it's saved as a file of its own.

// muxIn is one downloaded file.
type muxIn struct {
	path      string
	marks     []PartMark
	skipAudio bool           // a separate audio track replaces its own
	audio     []*streamTrack // labels for its audio tracks, in order
	def       bool           // its first audio track is the chosen one
}

type muxJob struct {
	out       string
	ins       []muxIn
	subs      []*subTrack
	audioOnly bool
	keep      func(part int) bool // only these parts (nil: all)
}

type muxOut struct {
	remuxResult
	placed  [][]placedCue // the subtitles as placed in the file (for the .srt files)
	foreign [][]int       // runs of parts in another format, left out of this file
}

// partKeeper is a reader that can leave out parts of its file.
type partKeeper interface{ keepParts(func(int) bool) }

func (s *tsSource) keepParts(k func(int) bool)     { s.keep = k }
func (s *packedSource) keepParts(k func(int) bool) { s.keep = k }
func (s *fmp4Source) keepParts(k func(int) bool) {
	for _, t := range s.tracks {
		ns, no := t.samples[:0:0], t.offs[:0:0]
		for i, smp := range t.samples {
			if k(int(smp.part)) {
				ns, no = append(ns, smp), append(no, t.offs[i])
			}
		}
		t.samples, t.offs = ns, no
	}
}
func (s *mkvSource) keepParts(k func(int) bool) {
	for _, t := range s.file.tracks {
		nb := t.blocks[:0:0]
		for _, b := range t.blocks {
			if k(int(b.part)) {
				nb = append(nb, b)
			}
		}
		t.blocks = nb
	}
}

func filterSamples(s []rawSample, keep func(int) bool) []rawSample {
	out := s[:0:0]
	for _, x := range s {
		if keep(int(x.part)) {
			out = append(out, x)
		}
	}
	return out
}

// trackFamily names a scanned track's format.
func trackFamily(t *muxTrack) string {
	switch {
	case t.family != "":
		return t.family
	case t.mkv != nil:
		return mkvFamily(t.mkv.entry)
	case len(t.aCfgs) > 0:
		return t.aCfgs[0].codec
	case len(t.entries) > 0:
		f := sampleFamily(t.entries[0])
		if f == "mp4a" {
			if mp4aMP3(t.entries[0]) {
				return "mp3"
			}
			return "aac"
		}
		return f
	}
	return ""
}

var webmFamilies = map[string]bool{"vp8": true, "vp9": true, "av1": true, "opus": true, "vorbis": true}

// pickContainer chooses the file type for the tracks: mp4, webm or mkv; m4a, opus or mka for
// sound alone.
func pickContainer(tracks []*muxTrack, audioOnly bool) string {
	mp4ok, webmok, nAudio, opus := true, true, 0, false
	for _, t := range tracks {
		if t.skip || t.text {
			continue
		}
		f := trackFamily(t)
		if t.mkv != nil || f == "vorbis" {
			mp4ok = false
		}
		if !webmFamilies[f] {
			webmok = false
		}
		if !t.video {
			nAudio++
			opus = opus || f == "opus"
		}
	}
	switch {
	case audioOnly && nAudio == 1 && opus:
		return "opus"
	case audioOnly && mp4ok:
		return "m4a"
	case audioOnly:
		return "mka"
	case mp4ok:
		return "mp4"
	case webmok:
		return "webm"
	}
	return "mkv"
}

// muxStream writes the job's file. With several formats of one kind in an input (a codec change
// between parts), the format with the most frames stays and the other parts are left out and
// listed in foreign.
func muxStream(ctx context.Context, job *muxJob, progress func(float64)) (res muxOut, err error) {
	defer func() {
		if r := recover(); r != nil { // a malformed file must never bring the app down
			os.Remove(job.out)
			res, err = muxOut{}, unsupported("the stream's data is damaged (%v)", r)
		}
	}()
	return muxFiles(ctx, job, progress)
}

// muxFiles is muxStream without the safety net (the fuzz tests call it, so a crash shows).
func muxFiles(ctx context.Context, job *muxJob, progress func(float64)) (res muxOut, err error) {
	var srcs []muxSource
	var total int64
	for _, in := range job.ins {
		s, err := openSourceParts(in.path, in.marks)
		if err != nil {
			return res, err
		}
		srcs = append(srcs, s)
		total += max(s.size(), 1)
	}
	share := func(i int) (float64, float64) {
		var before int64
		for _, s := range srcs[:i] {
			before += max(s.size(), 1)
		}
		return float64(before) / float64(total), float64(before+max(srcs[i].size(), 1)) / float64(total)
	}
	scanned := make([][]*muxTrack, len(srcs))
	for i, s := range srcs {
		a, b := share(i)
		tracks, err := s.scan(ctx, stageProgress(progress, 0.4*a, 0.4*b))
		if err != nil {
			return res, err
		}
		nA := 0
		for _, t := range tracks {
			t.src = i
			if t.video {
				continue
			}
			if job.ins[i].skipAudio {
				t.skip = true
				continue
			}
			if nA < len(job.ins[i].audio) && job.ins[i].audio[nA] != nil {
				l := job.ins[i].audio[nA]
				if l.Lang != "" {
					t.lang = lang639(l.Lang, false)
				}
				t.name = l.Name
				if t.name == "" && l.Desc {
					t.name = "Audio description"
				}
			}
			t.def = job.ins[i].def && nA == 0
			nA++
		}
		scanned[i] = tracks
	}
	if job.audioOnly {
		for _, ts := range scanned {
			for _, t := range ts {
				if t.video {
					t.skip = true
				}
			}
		}
	}
	keep := job.keep
	if keep == nil {
		if foreign := foreignParts(scanned); len(foreign) > 0 {
			bad := map[int]bool{}
			for _, p := range foreign {
				bad[p] = true
			}
			keep = func(p int) bool { return !bad[p] }
			res.foreign = partRuns(foreign)
		}
	}
	if keep != nil {
		for i, s := range srcs {
			if pk, ok := s.(partKeeper); ok {
				pk.keepParts(keep)
			}
			for _, t := range scanned[i] {
				t.samples = filterSamples(t.samples, keep)
				if t.mkv != nil {
					t.samples = mkvSamples(t.mkv)
				}
			}
		}
	}
	// output order: video, the chosen audio, the other audio, subtitles
	var order []*muxTrack
	for _, ts := range scanned {
		for _, t := range ts {
			if t.video {
				order = append(order, t)
			}
		}
	}
	for _, first := range []bool{true, false} {
		for _, ts := range scanned {
			for _, t := range ts {
				if !t.video && t.def == first {
					order = append(order, t)
				}
			}
		}
	}
	var vw, vh int
	for _, t := range order {
		if t.video && !t.skip && len(t.samples) > 0 && vw == 0 {
			vw, vh = t.width, t.height
		}
	}
	for i, s := range job.subs { // (sound alone gets the .srt files only)
		order = append(order, &muxTrack{text: true, lang: lang639(s.track.Lang, false), name: s.track.Name, cues: s.cues, src: len(srcs) + i,
			def: len(job.subs) == 1, forced: s.track.Forced, width: vw, height: vh, timescale: 1000, skip: job.audioOnly})
	}
	for _, t := range order {
		if !t.skip && len(t.samples) > 0 {
			res.video = res.video || t.video
			res.audio = res.audio || !t.video && !t.text
		}
	}
	index := map[*muxTrack]int{}
	for i, t := range order {
		index[t] = i
	}
	idx := make([][]int, len(srcs))
	for i, ts := range scanned {
		for _, t := range ts {
			idx[i] = append(idx[i], index[t])
		}
	}
	kind := pickContainer(order, job.audioOnly || !res.video)
	res.ext = "." + kind
	placed := func(global int64, secs map[int]section) {
		for _, t := range order {
			if t.text {
				pc := placeCues(t.cues, global, secs)
				res.placed = append(res.placed, pc)
				t.cues = nil
				if t.skip {
					continue
				}
				if kind == "mp4" || kind == "m4a" {
					t.samples, t.textData, t.lastDur = tx3gSamples(pc)
					t.entries = [][]byte{tx3gEntry()}
				} else {
					mkvTextSamples(t, pc)
				}
			}
		}
	}
	copyAll := func(write func(ti int, data []byte) error) error {
		for i, s := range srcs {
			a, b := share(i)
			err := s.copy(ctx, stageProgress(progress, 0.4+0.6*a, 0.4+0.6*b), func(ti int, data []byte) error {
				if ti >= len(idx[i]) {
					return nil
				}
				return write(idx[i][ti], data)
			})
			if err != nil {
				return err
			}
		}
		for i, t := range order {
			for _, b := range t.textData {
				if err := write(i, b); err != nil {
					return err
				}
			}
		}
		return nil
	}
	switch kind {
	case "mp4", "m4a":
		for _, t := range order {
			if t.mkv != nil && !t.skip {
				return res, unsupported("WebM pieces can't go into MP4")
			}
		}
		mx := &mp4Muxer{tracks: order, text: placed}
		if err := mx.prepare(true); err != nil {
			return res, err
		}
		return res, writeMP4(job.out, mx, func(f *os.File) error {
			return copyAll(func(ti int, data []byte) error { return mx.write(f, ti, data) })
		})
	case "opus":
		return res, writeOggOpus(ctx, job.out, order, srcs, idx, progress)
	}
	mm := &mkvMuxer{tracks: order, webm: kind == "webm", text: placed}
	if err := mm.prepare(); err != nil {
		return res, err
	}
	return res, mm.writeFile(job.out, copyAll)
}

// foreignParts finds parts whose tracks are in another format than the rest of their file.
func foreignParts(scanned [][]*muxTrack) []int {
	set := map[int]bool{}
	for _, ts := range scanned {
		for _, kind := range []bool{true, false} {
			var same []*muxTrack
			fams := map[string]bool{}
			for _, t := range ts {
				if t.video == kind && !t.skip && len(t.samples) > 0 {
					same = append(same, t)
					fams[trackFamily(t)] = true
				}
			}
			if len(same) < 2 || len(fams) < 2 {
				continue
			}
			main := same[0]
			for _, t := range same {
				if len(t.samples) > len(main.samples) {
					main = t
				}
			}
			has := map[int]bool{}
			for _, s := range main.samples {
				has[int(s.part)] = true
			}
			for _, t := range same {
				if trackFamily(t) == trackFamily(main) {
					continue
				}
				for _, s := range t.samples {
					if !has[int(s.part)] {
						set[int(s.part)] = true
					}
				}
			}
		}
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// partRuns groups sorted part numbers into runs of consecutive ones.
func partRuns(ps []int) [][]int {
	var out [][]int
	for i, p := range ps {
		if i > 0 && p == ps[i-1]+1 {
			out[len(out)-1] = append(out[len(out)-1], p)
		} else {
			out = append(out, []int{p})
		}
	}
	return out
}

// ---------------------------------------------------------------- the download's side

// streamExt is the extension the stream's file will most likely get, from the codecs the
// manifest names (packaging decides for sure, from the tracks themselves).
func streamExt(info *hlsInfo, d *Download) string {
	var tr []*muxTrack
	add := func(codec string, webm, video bool) {
		f := codecFamily(codec)
		if f == "" && !video && !webm {
			f = "aac" // (what an HLS stream that doesn't say has)
		}
		tr = append(tr, &muxTrack{video: video, family: f, mkv: webmMark(webm)})
	}
	vc, ac := splitCodecs(info.Chosen.Codecs)
	if !d.AudioOnly {
		for _, c := range vc {
			add(c, info.VideoWebm, true)
		}
		if len(vc) == 0 && info.Format == "HLS" && !audioOnlyVariant(info.Chosen) { // (a playlist that doesn't say: video)
			add("avc1", false, true)
		}
	}
	switch t := info.AudioTrack; {
	case t != nil && t.Codec != "" && !t.Builtin:
		add(t.Codec, t.Webm, false)
	case len(ac) > 0:
		add(ac[0], false, false)
	case t != nil || info.Format == "HLS":
		add("", info.VideoWebm, false)
	}
	for _, x := range info.Extras {
		add(x.track.Codec, x.track.Webm, false)
	}
	hasVideo := false
	for _, t := range tr {
		hasVideo = hasVideo || t.video
	}
	return "." + pickContainer(tr, d.AudioOnly || !hasVideo)
}

// webmMark stands in for "this track comes as WebM" when guessing from the manifest.
func webmMark(webm bool) *mkvTrack {
	if webm {
		return &mkvTrack{}
	}
	return nil
}

// shownVariants leaves out the qualities whose video can't be saved (unless that's all of them).
func shownVariants(vs []hlsVariant) []hlsVariant {
	var out []hlsVariant
	for _, v := range vs {
		if holdable(v.Codecs) {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return vs
	}
	return out
}

// repackageStream puts the downloaded tracks into out (the final file's temporary name): the
// video file with its separate audio (inputs/skip, as before), the other audio languages and
// the subtitles. Parts in another format are saved as files of their own beside it.
func (m *Manager) repackageStream(ctx context.Context, d *Download, info *hlsInfo, out string, inputs []string, skip []bool, progress func(float64)) (remuxResult, error) {
	m.mu.Lock()
	marks := func(path string) []PartMark { return append([]PartMark(nil), d.Marks[partsKey(d, path)]...) }
	job := &muxJob{out: out, audioOnly: d.AudioOnly}
	for i, p := range inputs {
		in := muxIn{path: p, marks: marks(p), skipAudio: i < len(skip) && skip[i]}
		switch {
		case i == 0 && len(inputs) == 1: // the sound is inside the video (or it's sound alone)
			in.def = true
			if info.AudioTrack != nil {
				in.audio = []*streamTrack{info.AudioTrack}
			}
		case i == 0 && d.AudioAll: // the video's own sound stays as a further language
			for _, t := range info.MoreAudio {
				if t.Builtin {
					in.skipAudio, in.audio = false, []*streamTrack{t}
				}
			}
		case i == 1:
			in.audio, in.def = []*streamTrack{info.AudioTrack}, true
		}
		job.ins = append(job.ins, in)
	}
	for i, x := range info.Extras {
		p := d.extraPartPath(i)
		job.ins = append(job.ins, muxIn{path: p, marks: marks(p), audio: []*streamTrack{x.track}})
	}
	m.mu.Unlock()
	if len(info.SubMedia) > 0 {
		m.setStage(d, "Adding subtitles", 0)
		subs, err := m.fetchSubtitles(ctx, d, info)
		if ctx.Err() != nil {
			return remuxResult{}, ctx.Err()
		}
		job.subs = subs
		if err != nil {
			m.addNote(d, "Some subtitles couldn't be downloaded ("+friendlyRemuxErr(err)+")")
		}
	}
	res, err := muxStream(ctx, job, progress)
	if err != nil {
		return res.remuxResult, err
	}
	m.mu.Lock()
	final := d.FileName
	if !nameFits(final, res.ext) { // (what packageHLS renames it to)
		final = m.uniqueName(d.Dir, strings.TrimSuffix(final, filepath.Ext(final))+res.ext, d)
	}
	m.mu.Unlock()
	if len(job.subs) > 0 {
		if _, err := m.writeSidecars(d, final, job.subs, res.placed); err != nil {
			return res.remuxResult, err
		}
	}
	for _, run := range res.foreign {
		set := map[int]bool{}
		for _, p := range run {
			set[p] = true
		}
		tmp := out + ".part" + fmt.Sprint(run[0])
		sep := *job
		sep.out, sep.subs = tmp, nil
		sep.keep = func(p int) bool { return set[p] }
		r, err := muxStream(ctx, &sep, nil)
		if err != nil {
			os.Remove(tmp)
			if ctx.Err() != nil {
				return res.remuxResult, ctx.Err()
			}
			m.addNote(d, "A part in another format couldn't be saved ("+friendlyRemuxErr(err)+")")
			continue
		}
		// (named after the video, which has a name of its own: a file of that name is from an
		// earlier, interrupted try)
		name := strings.TrimSuffix(final, filepath.Ext(final)) + fmt.Sprintf(" (part %d)", run[0]+1) + r.ext
		if err := os.Rename(tmp, filepath.Join(d.Dir, name)); err != nil {
			return res.remuxResult, err
		}
		m.addNote(d, "A part in another format is saved separately as "+name)
	}
	return res.remuxResult, nil
}

// addNote adds a sentence to the download's note.
func (m *Manager) addNote(d *Download, s string) {
	m.mu.Lock()
	if d.Note != "" && !strings.Contains(d.Note, s) {
		d.Note += ". " + s
	} else if d.Note == "" {
		d.Note = s
	}
	m.dirty = true
	m.mu.Unlock()
}

// removeExtras deletes the other audio tracks' part files once they're merged in.
func removeExtras(d *Download) {
	for i := range d.Extra {
		os.Remove(d.extraPartPath(i))
	}
}
