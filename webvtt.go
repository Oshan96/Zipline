package main

import (
	"bytes"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// WebVTT, the subtitle format of HLS and most DASH streams: reading cues (and the
// X-TIMESTAMP-MAP that ties an HLS segment's cue times to the video's clock), reading WebVTT
// carried in MP4 pieces (ISO 14496-30), and writing the cues out again as SubRip (.srt). The MP4
// text track they also become is in tx3g.go.

type vttCue struct {
	start, end int64 // ms, in the file's own time
	text       string
}

type vttFile struct {
	cues     []vttCue
	mapTS    int64 // X-TIMESTAMP-MAP: the MPEG-TS time (90 kHz) of local time mapLocal (ms); -1 = none
	mapLocal int64
}

var vttTimeRE = regexp.MustCompile(`^(?:(\d+):)?(\d{1,2}):(\d{2})[.,](\d{1,3})$`)

// vttTime reads "01:02:03.456" or "02:03.456" (ms; ok false when it isn't a time).
func vttTime(s string) (int64, bool) {
	m := vttTimeRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	h, _ := strconv.ParseInt(m[1], 10, 64)
	mi, _ := strconv.ParseInt(m[2], 10, 64)
	se, _ := strconv.ParseInt(m[3], 10, 64)
	frac := m[4]
	for len(frac) < 3 {
		frac += "0"
	}
	ms, _ := strconv.ParseInt(frac, 10, 64)
	return ((h*60+mi)*60+se)*1000 + ms, true
}

// parseVTT reads one WebVTT file or segment. Blocks it doesn't understand (STYLE, REGION,
// NOTE) are skipped.
func parseVTT(b []byte) *vttFile {
	f := &vttFile{mapTS: -1}
	s := strings.ReplaceAll(strings.ReplaceAll(string(bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))), "\r\n", "\n"), "\r", "\n")
	blocks := strings.Split(s, "\n\n")
	for bi, blk := range blocks {
		lines := strings.Split(strings.Trim(blk, "\n"), "\n")
		if len(lines) == 0 || lines[0] == "" {
			continue
		}
		if bi == 0 && strings.HasPrefix(lines[0], "WEBVTT") { // the header, maybe with the map
			for _, l := range lines[1:] {
				if v, ok := strings.CutPrefix(strings.TrimSpace(l), "X-TIMESTAMP-MAP="); ok {
					for _, kv := range strings.Split(v, ",") {
						k, val, _ := strings.Cut(strings.TrimSpace(kv), ":")
						switch strings.ToUpper(k) {
						case "MPEGTS":
							f.mapTS, _ = strconv.ParseInt(val, 10, 64)
						case "LOCAL":
							f.mapLocal, _ = vttTime(val)
						}
					}
				}
			}
			continue
		}
		at := 0
		if !strings.Contains(lines[0], "-->") { // a cue identifier (or NOTE, STYLE…)
			at = 1
		}
		if at >= len(lines) || !strings.Contains(lines[at], "-->") {
			continue
		}
		a, rest, _ := strings.Cut(lines[at], "-->")
		rest = strings.TrimSpace(rest)
		endStr, _, _ := strings.Cut(rest, " ") // cue settings follow the end time
		endStr, _, _ = strings.Cut(endStr, "\t")
		st, ok1 := vttTime(a)
		en, ok2 := vttTime(endStr)
		if !ok1 || !ok2 || en < st {
			continue
		}
		f.cues = append(f.cues, vttCue{start: st, end: en, text: strings.Join(lines[at+1:], "\n")})
	}
	return f
}

var vttTagRE = regexp.MustCompile(`</?([a-zA-Z]+)[^>]*>|<\d[\d:.]*>`)

// vttPlain drops WebVTT's markup (voice, class, ruby, karaoke timestamps); keep lists tags
// to leave in (SubRip players understand <b>, <i> and <u>).
func vttPlain(s string, keep ...string) string {
	s = vttTagRE.ReplaceAllStringFunc(s, func(tag string) string {
		m := vttTagRE.FindStringSubmatch(tag)
		for _, k := range keep {
			if strings.EqualFold(m[1], k) {
				if strings.HasPrefix(tag, "</") {
					return "</" + k + ">"
				}
				return "<" + k + ">"
			}
		}
		return ""
	})
	return strings.TrimSpace(html.UnescapeString(s))
}

// ---------------------------------------------------------------- placed cues

// placedCue is a cue on the output file's timeline (ms from its start).
type placedCue struct {
	start, end int64
	text       string // WebVTT cue text
}

// writeSRT renders cues as a SubRip file.
func writeSRT(cues []placedCue) []byte {
	var b strings.Builder
	ts := func(ms int64) string {
		return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
	}
	n := 0
	for _, c := range cues {
		text := vttPlain(c.text, "b", "i", "u")
		if text == "" {
			continue
		}
		n++
		fmt.Fprintf(&b, "%d\r\n%s --> %s\r\n%s\r\n\r\n", n, ts(c.start), ts(c.end), strings.ReplaceAll(text, "\n", "\r\n"))
	}
	return []byte(b.String())
}

// ---------------------------------------------------------------- WebVTT in MP4 (wvtt)

// wvttCues reads the cues of one wvtt sample (vttc boxes; vtte means no cue).
func wvttCues(b []byte) []string {
	var out []string
	eachBox(b, func(typ string, body, _ []byte) error {
		if typ != "vttc" {
			return nil
		}
		eachBox(body, func(typ string, body, _ []byte) error {
			if typ == "payl" {
				out = append(out, string(body))
			}
			return nil
		})
		return nil
	})
	return out
}
