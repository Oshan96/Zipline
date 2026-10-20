package main

import (
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Torrent files (BEP 3, with BEP 12 tracker tiers, BEP 19 web seeds, BEP 47 padding files)
// and magnet links (BEP 9).

type TFile struct {
	Path   []string `json:"path"` // folders and name, made safe for the disk
	Length int64    `json:"length"`
	Offset int64    `json:"offset"` // where it starts in the torrent's data
	Pad    bool     `json:"pad,omitempty"`
	Orig   []string `json:"-"` // as published (web seeds serve these names)
}

type Metainfo struct {
	InfoHash  [20]byte
	Name      string
	PieceLen  int64
	Pieces    []byte // 20 bytes per piece
	Files     []TFile
	Total     int64
	Multi     bool // files sit in a folder named Name
	Private   bool
	Trackers  [][]string // tiers
	WebSeeds  []string
	Comment   string
	CreatedBy string
	Info      []byte // the info dictionary exactly as published
	OrigName  string
}

func (mi *Metainfo) NumPieces() int { return len(mi.Pieces) / 20 }

func (mi *Metainfo) PieceSize(i int) int64 {
	if i == mi.NumPieces()-1 {
		if r := mi.Total - int64(i)*mi.PieceLen; r > 0 {
			return r
		}
	}
	return mi.PieceLen
}

func (mi *Metainfo) PieceHash(i int) []byte { return mi.Pieces[i*20 : i*20+20] }

func (mi *Metainfo) HashHex() string { return hex.EncodeToString(mi.InfoHash[:]) }

// AllTrackers lists the trackers in tier order without repeats.
func (mi *Metainfo) AllTrackers() []string {
	var out []string
	seen := map[string]bool{}
	for _, tier := range mi.Trackers {
		for _, t := range tier {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	return out
}

var errV2Only = errors.New("this torrent uses BitTorrent v2 only, which Zipline doesn't support yet (v1 and hybrid torrents work)")

// parseTorrent reads a .torrent file.
func parseTorrent(b []byte) (*Metainfo, error) {
	top, raw, err := bdecodeDictSpans(b)
	if err != nil {
		return nil, fmt.Errorf("not a torrent file: %w", err)
	}
	info := raw["info"]
	if info == nil {
		return nil, errors.New("not a torrent file: it has no info section")
	}
	mi, err := parseInfo(info)
	if err != nil {
		return nil, err
	}
	if a := bstr(top, "announce"); a != "" {
		mi.Trackers = append(mi.Trackers, []string{a})
	}
	if al := blist(top, "announce-list"); len(al) > 0 {
		mi.Trackers = nil // the list replaces the single announce (BEP 12)
		for _, tier := range al {
			var ts []string
			if l, ok := tier.([]any); ok {
				for _, t := range l {
					if s, ok := t.(string); ok && validTracker(s) {
						ts = append(ts, s)
					}
				}
			}
			if len(ts) > 0 {
				mi.Trackers = append(mi.Trackers, ts)
			}
		}
	}
	switch ws := top["url-list"].(type) {
	case string:
		if validWebSeed(ws) {
			mi.WebSeeds = []string{ws}
		}
	case []any:
		for _, w := range ws {
			if s, ok := w.(string); ok && validWebSeed(s) {
				mi.WebSeeds = append(mi.WebSeeds, s)
			}
		}
	}
	mi.Comment = bstr(top, "comment")
	mi.CreatedBy = bstr(top, "created by")
	return mi, nil
}

func validTracker(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "udp")
}

func validWebSeed(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "ftp")
}

// parseInfo reads an info dictionary (from a .torrent file or from peers).
func parseInfo(info []byte) (*Metainfo, error) {
	v, err := bdecode(info)
	if err != nil {
		return nil, fmt.Errorf("the torrent's info section is damaged: %w", err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("the torrent's info section is damaged")
	}
	mi := &Metainfo{Info: append([]byte(nil), info...), InfoHash: sha1.Sum(info)}
	pl, _ := bint(d, "piece length")
	pieces := bstr(d, "pieces")
	if pieces == "" {
		if mv, _ := bint(d, "meta version"); mv == 2 {
			return nil, errV2Only
		}
		return nil, errors.New("the torrent lists no pieces")
	}
	if pl <= 0 || pl > 256<<20 || len(pieces)%20 != 0 {
		return nil, errors.New("the torrent's piece information is damaged")
	}
	mi.PieceLen, mi.Pieces = pl, []byte(pieces)
	name := bstr(d, "name.utf-8")
	if name == "" {
		name = bstr(d, "name")
	}
	mi.OrigName = name
	mi.Name = safeComponent(name)
	if mi.Name == "" {
		mi.Name = mi.HashHex()
	}
	if p, _ := bint(d, "private"); p == 1 {
		mi.Private = true
	}
	if files := blist(d, "files"); files != nil {
		mi.Multi = true
		for _, fv := range files {
			f, ok := fv.(map[string]any)
			if !ok {
				return nil, errors.New("the torrent's file list is damaged")
			}
			n, ok := bint(f, "length")
			if !ok || n < 0 {
				return nil, errors.New("the torrent's file list is damaged")
			}
			parts := blist(f, "path.utf-8")
			if parts == nil {
				parts = blist(f, "path")
			}
			var p, orig []string
			for _, c := range parts {
				s, _ := c.(string)
				orig = append(orig, s)
				if s = safeComponent(s); s != "" {
					p = append(p, s)
				}
			}
			if len(p) == 0 {
				p = []string{"file"}
			}
			tf := TFile{Path: p, Length: n, Offset: mi.Total, Pad: strings.Contains(bstr(f, "attr"), "p"), Orig: orig}
			mi.Files = append(mi.Files, tf)
			mi.Total += n
		}
		if len(mi.Files) == 0 {
			return nil, errors.New("the torrent has no files")
		}
	} else {
		n, ok := bint(d, "length")
		if !ok || n < 0 {
			return nil, errors.New("the torrent's size is missing")
		}
		mi.Files = []TFile{{Path: []string{mi.Name}, Length: n, Orig: []string{name}}}
		mi.Total = n
	}
	if want := (mi.Total + mi.PieceLen - 1) / mi.PieceLen; int64(mi.NumPieces()) != want || mi.Total <= 0 {
		return nil, errors.New("the torrent's pieces don't add up to its size")
	}
	uniqueFilePaths(mi.Files)
	return mi, nil
}

// safeComponent makes one folder or file name safe: no separators, no ".." and no names
// Windows reserves.
func safeComponent(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("/", "_", `\`, "_").Replace(s))
	if s == "" || s == "." || s == ".." {
		return ""
	}
	s = sanitizeName(s)
	base := strings.ToUpper(strings.TrimSuffix(s, filepath.Ext(s)))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		s = "_" + s
	}
	return strings.TrimRight(s, ". ")
}

// uniqueFilePaths renames files whose paths collide once made safe (or differ only in case).
func uniqueFilePaths(fs []TFile) {
	seen := map[string]bool{}
	for i := range fs {
		key := strings.ToLower(strings.Join(fs[i].Path, "/"))
		for n := 1; seen[key]; n++ {
			last := fs[i].Path[len(fs[i].Path)-1]
			ext := filepath.Ext(last)
			fs[i].Path[len(fs[i].Path)-1] = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(last, ext), n, ext)
			key = strings.ToLower(strings.Join(fs[i].Path, "/"))
		}
		seen[key] = true
	}
}

// ---------------------------------------------------------------- magnet links

type Magnet struct {
	InfoHash [20]byte
	Name     string
	Trackers []string
	WebSeeds []string
	Peers    []string // x.pe: peers to try straight away
	Select   []int    // so: the files to download (BEP 53)
}

func isMagnet(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "magnet:?")
}

func parseMagnet(s string) (*Magnet, error) {
	s = strings.TrimSpace(s)
	if !isMagnet(s) {
		return nil, errors.New("not a magnet link")
	}
	q, err := url.ParseQuery(s[len("magnet:?"):])
	if err != nil {
		return nil, fmt.Errorf("the magnet link is damaged: %w", err)
	}
	mg := &Magnet{}
	found, v2 := false, false
	for k, vs := range q {
		if !strings.HasPrefix(k, "xt") {
			continue
		}
		for _, xt := range vs {
			low := strings.ToLower(xt)
			switch {
			case strings.HasPrefix(low, "urn:btih:"):
				h := xt[len("urn:btih:"):]
				var b []byte
				switch len(h) {
				case 40:
					b, err = hex.DecodeString(h)
				case 32:
					b, err = base32.StdEncoding.DecodeString(strings.ToUpper(h))
				default:
					err = errors.New("bad length")
				}
				if err == nil && len(b) == 20 {
					copy(mg.InfoHash[:], b)
					found = true
				}
			case strings.HasPrefix(low, "urn:btmh:"):
				v2 = true
			}
		}
	}
	if !found {
		if v2 {
			return nil, errV2Only
		}
		return nil, errors.New("the magnet link has no torrent hash (xt=urn:btih:…)")
	}
	mg.Name = safeComponent(q.Get("dn"))
	for _, t := range q["tr"] {
		if validTracker(t) {
			mg.Trackers = append(mg.Trackers, t)
		}
	}
	for _, w := range append(q["ws"], q["as"]...) {
		if validWebSeed(w) {
			mg.WebSeeds = append(mg.WebSeeds, w)
		}
	}
	mg.Peers = append(mg.Peers, q["x.pe"]...)
	for _, part := range strings.Split(q.Get("so"), ",") {
		a, b, isRange := strings.Cut(part, "-")
		var x, y int
		if _, err := fmt.Sscan(a, &x); err != nil {
			continue
		}
		y = x
		if isRange {
			if _, err := fmt.Sscan(b, &y); err != nil {
				continue
			}
		}
		for i := x; i <= y && i-x < 10000; i++ {
			mg.Select = append(mg.Select, i)
		}
	}
	return mg, nil
}

// magnetLink builds a magnet link for sharing.
func magnetLink(ih [20]byte, name string, trackers []string) string {
	v := url.Values{}
	if name != "" {
		v.Set("dn", name)
	}
	for _, t := range trackers {
		v.Add("tr", t)
	}
	s := "magnet:?xt=urn:btih:" + hex.EncodeToString(ih[:])
	if e := v.Encode(); e != "" {
		s += "&" + e
	}
	return s
}
