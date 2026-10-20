package main

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metalink (RFC 5854, and the older 3.0 format): an XML file listing one or more files with
// their size, checksums (whole file and per piece) and the mirrors that carry them. Zipline
// downloads each file from several mirrors at once, checks each piece as it arrives
// (piececheck.go) and the whole file as it finishes. A torrent the metalink lists goes to the
// torrent engine, with the mirrors as web seeds (mltorrent.go).
//
// Metalink/HTTP (RFC 6249): a server can list mirrors in "Link: <…>; rel=duplicate" headers
// and point to a metalink with rel=describedby. Zipline uses those mirrors automatically.

type mlURL struct {
	URL string `json:"url"`
	Pri int    `json:"pri"`
	Loc string `json:"loc,omitempty"`
}

type mlFile struct {
	Name   string       `json:"name"`
	Dir    string       `json:"dir,omitempty"` // the folders it goes in ("dir/sub", made safe: mlpaths.go)
	Size   int64        `json:"size"`
	URLs   []mlURL      `json:"-"`
	Hash   *Checksum    `json:"hash,omitempty"`
	Pieces *PieceHashes `json:"-"`
	// a torrent for it (mltorrent.go), and its path inside a torrent of several files
	Torrent     string `json:"-"`
	TorrentName string `json:"-"`
}

type xmlHash struct {
	Type  string `xml:"type,attr"`
	Piece string `xml:"piece,attr"`
	Value string `xml:",chardata"`
}

type xmlPieces struct {
	Length int64     `xml:"length,attr"`
	Type   string    `xml:"type,attr"`
	Hashes []xmlHash `xml:"hash"`
}

type xmlURL struct {
	Location   string `xml:"location,attr"`
	Priority   string `xml:"priority,attr"`   // metalink 4: 1 is best
	Preference string `xml:"preference,attr"` // metalink 3: 100 is best
	Type       string `xml:"type,attr"`
	Value      string `xml:",chardata"`
}

type xmlMetaURL struct {
	MediaType string `xml:"mediatype,attr"`
	Name      string `xml:"name,attr"`
	Priority  string `xml:"priority,attr"`
	Value     string `xml:",chardata"`
}

type xmlFile struct {
	Name   string     `xml:"name,attr"`
	Size   int64      `xml:"size"`
	Hashes []xmlHash  `xml:"hash"`
	Pieces *xmlPieces `xml:"pieces"`
	URLs   []xmlURL   `xml:"url"`
	// metalink 3
	Verification struct {
		Hashes []xmlHash   `xml:"hash"`
		Pieces []xmlPieces `xml:"pieces"`
	} `xml:"verification"`
	Resources struct {
		URLs []xmlURL `xml:"url"`
	} `xml:"resources"`

	MetaURLs []xmlMetaURL `xml:"metaurl"` // metalink 4: torrents (mltorrent.go)
}

type xmlMetalink struct {
	XMLName xml.Name
	Files   []xmlFile `xml:"file"`
	Files3  struct {
		Files []xmlFile `xml:"file"`
	} `xml:"files"`
}

// parseMetalink reads a metalink 4 or 3 document.
func parseMetalink(r io.Reader) ([]mlFile, error) {
	var doc xmlMetalink
	dec := xml.NewDecoder(io.LimitReader(r, 8<<20))
	dec.Strict = false
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if doc.XMLName.Local != "metalink" {
		return nil, errNotMetalink
	}
	files := append(doc.Files, doc.Files3.Files...)
	var out []mlFile
	for _, f := range files {
		dir, name := metalinkPath(f.Name)
		if name == "" {
			continue
		}
		mf := mlFile{Name: name, Dir: dir, Size: f.Size}
		if mf.Size <= 0 {
			mf.Size = -1
		}
		for _, h := range append(f.Hashes, f.Verification.Hashes...) {
			a := normAlgo(h.Type)
			v := strings.ToLower(strings.TrimSpace(h.Value))
			if a != "" && len(v) == algoLen[a] && hexRe.MatchString(v) && (mf.Hash == nil || algoRank[a] > algoRank[mf.Hash.Algo]) {
				mf.Hash = &Checksum{Algo: a, Want: v, From: "metalink"}
			}
		}
		pcs := f.Verification.Pieces
		if f.Pieces != nil {
			pcs = append(pcs, *f.Pieces)
		}
		for _, p := range pcs {
			a := normAlgo(p.Type)
			if a == "" || p.Length <= 0 || len(p.Hashes) == 0 {
				continue
			}
			ph := &PieceHashes{Algo: a, Length: p.Length}
			okAll := true
			for _, h := range p.Hashes {
				v := strings.ToLower(strings.TrimSpace(h.Value))
				if len(v) != algoLen[a] || !hexRe.MatchString(v) {
					okAll = false
					break
				}
				ph.Hashes = append(ph.Hashes, v)
			}
			if okAll && (mf.Size <= 0 || int64(len(ph.Hashes)) == (mf.Size+p.Length-1)/p.Length) && (mf.Pieces == nil || algoRank[a] > algoRank[mf.Pieces.Algo]) {
				mf.Pieces = ph
			}
		}
		for _, u := range append(f.URLs, f.Resources.URLs...) {
			v := strings.TrimSpace(u.Value)
			pu, err := url.Parse(v)
			if err != nil || !(pu.Scheme == "http" || pu.Scheme == "https" || pu.Scheme == "ftp" || pu.Scheme == "ftps") || pu.Host == "" || strings.EqualFold(u.Type, "bittorrent") {
				continue // torrents (metalink 3 lists their .torrent files here) and the like
			}
			pri := 999999
			if n, err := strconv.Atoi(u.Priority); err == nil && n > 0 {
				pri = n
			} else if n, err := strconv.Atoi(u.Preference); err == nil && n > 0 {
				pri = 101 - min(n, 100)
			}
			mf.URLs = append(mf.URLs, mlURL{URL: v, Pri: pri, Loc: strings.ToLower(u.Location)})
		}
		orderMirrors(mf.URLs)
		mf.Torrent, mf.TorrentName = metalinkTorrent(f)
		if len(mf.URLs) > 0 || mf.Torrent != "" {
			out = append(out, mf)
		}
	}
	if len(out) == 0 {
		return nil, errNotMetalink
	}
	return out, nil
}

type metalinkErr string

func (e metalinkErr) Error() string { return string(e) }

const errNotMetalink = metalinkErr("not a metalink, or it lists no downloadable files")

func isMetalinkMime(mime, rawURL string) bool {
	m := strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0]))
	if m == "application/metalink4+xml" || m == "application/metalink+xml" {
		return true
	}
	p := strings.ToLower(rawURL)
	if u, err := url.Parse(rawURL); err == nil {
		p = strings.ToLower(u.Path)
	}
	return (strings.HasSuffix(p, ".meta4") || strings.HasSuffix(p, ".metalink")) && !strings.HasPrefix(m, "text/html")
}

// linkHeader is one entry of a Link header.
type linkHeader struct {
	URL, Rel, Type string
	Pri            int
	Geo            string // the mirror's country (RFC 6249), lower case
}

// parseLinks reads Link headers (RFC 8288), resolving addresses against base.
func parseLinks(hs []http.Header, base *url.URL) []linkHeader {
	var out []linkHeader
	for _, h := range hs {
		for _, v := range h.Values("Link") {
			for _, part := range splitLinkValues(v) {
				part = strings.TrimSpace(part)
				if !strings.HasPrefix(part, "<") {
					continue
				}
				end := strings.Index(part, ">")
				if end < 0 {
					continue
				}
				l := linkHeader{URL: part[1:end], Pri: 999999}
				if base != nil {
					if ru, err := base.Parse(l.URL); err == nil {
						l.URL = ru.String()
					}
				}
				for _, p := range strings.Split(part[end+1:], ";") {
					k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
					val = strings.Trim(strings.TrimSpace(val), `"`)
					switch strings.ToLower(k) {
					case "rel":
						l.Rel = strings.ToLower(val)
					case "type":
						l.Type = strings.ToLower(val)
					case "pri":
						if n, err := strconv.Atoi(val); err == nil {
							l.Pri = n
						}
					case "geo":
						l.Geo = strings.ToLower(val)
					}
				}
				out = append(out, l)
			}
		}
	}
	return out
}

// splitLinkValues splits "<a>; rel=x, <b>; rel=y" on the commas between links.
func splitLinkValues(v string) []string {
	var out []string
	depth, quote, start := 0, false, 0
	for i, c := range v {
		switch {
		case c == '"':
			quote = !quote
		case c == '<' && !quote:
			depth++
		case c == '>' && !quote:
			depth--
		case c == ',' && depth == 0 && !quote:
			out = append(out, v[start:i])
			start = i + 1
		}
	}
	return append(out, v[start:])
}

// mirrorsFromLinks returns the rel=duplicate mirrors, best first (at most 10).
func mirrorsFromLinks(ls []linkHeader, skip ...string) []string {
	var dup []linkHeader
	seen := map[string]bool{}
	for _, s := range skip {
		seen[s] = true
	}
	for _, l := range ls {
		if l.Rel == "duplicate" && !seen[l.URL] {
			u, err := url.Parse(l.URL)
			if err != nil || !(u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "ftp" || u.Scheme == "ftps") {
				continue
			}
			seen[l.URL] = true
			dup = append(dup, l)
		}
	}
	sort.SliceStable(dup, func(i, j int) bool { return mirrorBefore(dup[i].Pri, dup[i].Geo, dup[j].Pri, dup[j].Geo) })
	var out []string
	for _, l := range dup {
		if len(out) >= 10 {
			break
		}
		out = append(out, l.URL)
	}
	return out
}

// Metalinks the Add dialog has read, so adding doesn't fetch them again.
var metaCache = struct {
	sync.Mutex
	m map[string]metaEntry
}{m: map[string]metaEntry{}}

type metaEntry struct {
	files []mlFile
	at    time.Time
}

func cacheMetalink(u string, files []mlFile) {
	metaCache.Lock()
	defer metaCache.Unlock()
	for k, e := range metaCache.m {
		if time.Since(e.at) > 30*time.Minute {
			delete(metaCache.m, k)
		}
	}
	metaCache.m[u] = metaEntry{files, time.Now()}
}

func cachedMetalink(u string) []mlFile {
	metaCache.Lock()
	defer metaCache.Unlock()
	return metaCache.m[u].files
}
