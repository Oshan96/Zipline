package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// Checksums: a download can carry the checksum it should match — pasted in the Add dialog,
// sent by the server (Digest / Repr-Digest / X-Goog-Hash headers), read from a metalink, or
// found in the checksum files sites publish next to their downloads (SHA256SUMS,
// file.iso.sha256 and the like). When the download finishes the file is checked before it
// takes its final name. A metalink can also give a checksum for every piece of the file, so
// a damaged piece is downloaded again instead of the whole file.

type Checksum struct {
	Algo  string `json:"algo"`            // md5, sha1, sha256, sha512
	Want  string `json:"want,omitempty"`  // expected, lower-case hex
	From  string `json:"from,omitempty"`  // where it came from: "you", "server", "metalink" or a file name
	State string `json:"state,omitempty"` // "", "ok", "bad"
	Got   string `json:"got,omitempty"`   // what the file has (when it doesn't match, or calculated on request)
}

// PieceHashes are a metalink's checksums for equal-sized pieces of the file.
type PieceHashes struct {
	Algo   string   `json:"algo"`
	Length int64    `json:"length"`
	Hashes []string `json:"hashes"`
}

var algoLen = map[string]int{"md5": 32, "sha1": 40, "sha256": 64, "sha512": 128}
var algoRank = map[string]int{"md5": 1, "sha1": 2, "sha256": 3, "sha512": 4}
var algoNames = map[string]string{"md5": "MD5", "sha1": "SHA-1", "sha256": "SHA-256", "sha512": "SHA-512"}

func newHash(algo string) hash.Hash {
	switch algo {
	case "md5":
		return md5.New()
	case "sha1":
		return sha1.New()
	case "sha256":
		return sha256.New()
	case "sha512":
		return sha512.New()
	case "quickxor": // OneDrive's own (quickxor.go)
		return newQuickXor()
	}
	return nil
}

// normAlgo turns "SHA-256", "sha256sum", "SHA256" and friends into sha256.
func normAlgo(a string) string {
	a = strings.ToLower(strings.TrimSpace(a))
	a = strings.TrimSuffix(strings.TrimSuffix(a, "sum"), "sums")
	a = strings.ReplaceAll(strings.ReplaceAll(a, "-", ""), "_", "")
	if a == "sha" {
		a = "sha1"
	}
	if _, ok := algoLen[a]; ok {
		return a
	}
	return ""
}

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

// algoForHex guesses the algorithm from a hex digest's length.
func algoForHex(h string) string {
	if !hexRe.MatchString(h) {
		return ""
	}
	for a, n := range algoLen {
		if len(h) == n {
			return a
		}
	}
	return ""
}

// digestFromBase64 reads a base64 digest (as servers send it) as hex.
func digestFromBase64(s string) (algo, hexv string) {
	s = strings.Trim(strings.TrimSpace(s), ":")
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(s); err != nil {
			return "", ""
		}
	}
	h := hex.EncodeToString(b)
	return algoForHex(h), h
}

var bsdSumRe = regexp.MustCompile(`(?i)^\s*(MD5|SHA-?1|SHA-?256|SHA-?512)\s*\((.+)\)\s*=\s*([0-9a-f]+)\s*$`)

// sumLine reads one line of a checksum list: "hash  name", "hash *name" or "SHA256 (name) = hash".
func sumLine(line string) (algo, hexv, name string) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	if m := bsdSumRe.FindStringSubmatch(line); m != nil {
		a := normAlgo(m[1])
		if a != "" && len(m[3]) == algoLen[a] {
			return a, strings.ToLower(m[3]), strings.TrimSpace(m[2])
		}
		return
	}
	f := strings.Fields(line)
	if len(f) == 0 {
		return
	}
	a := algoForHex(f[0])
	if a == "" {
		return
	}
	if len(f) > 1 {
		name = strings.TrimPrefix(strings.Join(f[1:], " "), "*")
	}
	return a, strings.ToLower(f[0]), name
}

// sameFileName compares file names the way checksum lists write them (with folders, or with
// the " (1)" Zipline adds when a name is taken).
func sameFileName(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(path.Base(strings.ReplaceAll(strings.TrimSpace(s), `\`, "/")))
		s = strings.TrimPrefix(s, "./")
		ext := path.Ext(s)
		base := strings.TrimSuffix(s, ext)
		if i := strings.LastIndex(base, " ("); i > 0 && strings.HasSuffix(base, ")") && strings.Trim(base[i+2:len(base)-1], "0123456789") == "" {
			base = base[:i]
		}
		return base + ext
	}
	return a != "" && b != "" && norm(a) == norm(b)
}

// parseChecksum reads what someone pasted: a bare hex digest, "sha256:…", a line from a
// checksum file, or a whole checksum file (the line for fileName is used).
func parseChecksum(s, fileName string) (*Checksum, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var cands []*Checksum
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if a, h, name := sumLine(line); a != "" {
			c := &Checksum{Algo: a, Want: h, From: "you"}
			if name != "" && fileName != "" && sameFileName(name, fileName) {
				return c, nil
			}
			cands = append(cands, c)
			continue
		}
		// "sha256:abc…", "SHA-256 abc…", "sha256=abc…"
		if i := strings.IndexAny(line, ":= "); i > 0 {
			if a := normAlgo(line[:i]); a != "" {
				v := strings.TrimSpace(line[i+1:])
				if hexRe.MatchString(v) && len(v) == algoLen[a] {
					cands = append(cands, &Checksum{Algo: a, Want: strings.ToLower(v), From: "you"})
					continue
				}
				if ba, bh := digestFromBase64(v); ba == a {
					cands = append(cands, &Checksum{Algo: a, Want: bh, From: "you"})
					continue
				}
			}
		}
		if a, h := digestFromBase64(line); a != "" && len(line) >= 22 {
			cands = append(cands, &Checksum{Algo: a, Want: h, From: "you"})
		}
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	if len(cands) > 1 {
		return nil, errors.New("that list has several checksums and none is for " + fileName)
	}
	return nil, errors.New("that doesn't look like an MD5, SHA-1, SHA-256 or SHA-512 checksum")
}

// headerChecksum reads a checksum the server sends with the file (the strongest one offered).
// h holds the headers of every reply on the way (redirects included).
func headerChecksum(hs []http.Header) *Checksum {
	var best *Checksum
	take := func(a, hexv string) {
		if a == "" || len(hexv) != algoLen[a] {
			return
		}
		if best == nil || algoRank[a] > algoRank[best.Algo] {
			best = &Checksum{Algo: a, Want: hexv, From: "server"}
		}
	}
	for _, h := range hs {
		// RFC 9530: Repr-Digest: sha-256=:base64:  (Content-Digest covers only this reply's bytes)
		for _, v := range h.Values("Repr-Digest") {
			for _, part := range strings.Split(v, ",") {
				k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
				if a := normAlgo(k); ok && a != "" {
					if ba, bh := digestFromBase64(val); ba == a {
						take(a, bh)
					}
				}
			}
		}
		// RFC 3230: Digest: SHA-256=base64, MD5=base64
		for _, v := range h.Values("Digest") {
			for _, part := range strings.Split(v, ",") {
				k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
				if a := normAlgo(k); ok && a != "" {
					if ba, bh := digestFromBase64(val); ba == a {
						take(a, bh)
					} else if hexRe.MatchString(val) {
						take(a, strings.ToLower(val))
					}
				}
			}
		}
		// Google Cloud Storage: x-goog-hash: crc32c=…, md5=…
		for _, v := range h.Values("X-Goog-Hash") {
			for _, part := range strings.Split(v, ",") {
				if k, val, ok := strings.Cut(strings.TrimSpace(part), "="); ok && strings.EqualFold(k, "md5") {
					if ba, bh := digestFromBase64(val); ba == "md5" {
						take("md5", bh)
					}
				}
			}
		}
	}
	if best == nil {
		best = cloudChecksum(hs) // what a cloud service said (cloud_hooks.go)
	}
	return best
}

// reprDigests reads the checksums of the whole file a server sends (RFC 9530 Repr-Digest), by
// algorithm, as hex. Two servers that send one for the same algorithm can be compared.
func reprDigests(hs []http.Header) map[string]string {
	out := map[string]string{}
	for _, h := range hs {
		for _, v := range h.Values("Repr-Digest") {
			for _, part := range strings.Split(v, ",") {
				k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
				if a := normAlgo(k); ok && a != "" {
					if ba, bh := digestFromBase64(val); ba == a {
						out[a] = bh
					}
				}
			}
		}
	}
	return out
}

// Checksum files sites commonly publish next to downloads, best first.
var sumSidecars = []string{".sha256", ".sha256sum", ".sha512", ".sha512sum", ".sha1", ".md5"}
var sumLists = []string{"SHA256SUMS", "SHA256SUMS.txt", "sha256sums.txt", "sha256sum.txt", "SHASUMS256.txt", "checksums.txt", "SHA512SUMS", "CHECKSUMS", "MD5SUMS"}

// publishedChecksum looks for a checksum file next to the download.
func publishedChecksum(ctx context.Context, d *Download, raw string) *Checksum {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Path == "" || strings.HasSuffix(u.Path, "/") {
		return nil
	}
	if cloudService(raw) != "" { // (a share link's service has no checksum files beside it)
		return nil
	}
	u.RawQuery, u.Fragment = "", ""
	name := path.Base(u.Path)
	type cand struct{ url, label string }
	var cands []cand
	for _, x := range sumSidecars {
		c := *u
		c.Path += x
		c.RawPath = ""
		cands = append(cands, cand{c.String(), name + x})
	}
	for _, x := range sumLists {
		c := *u
		c.Path = path.Join(path.Dir(u.Path), x)
		c.RawPath = ""
		cands = append(cands, cand{c.String(), x})
	}
	results := make([]*Checksum, len(cands))
	sem := make(chan struct{}, 3)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	done := make(chan int, len(cands))
	for i, c := range cands {
		go func() {
			sem <- struct{}{}
			defer func() { <-sem; done <- i }()
			if ctx.Err() != nil {
				return
			}
			results[i] = fetchSumFile(ctx, d, c.url, c.label, d.FileName, i < len(sumSidecars))
		}()
	}
	for range cands {
		<-done
	}
	for _, r := range results {
		if r != nil {
			return r
		}
	}
	return nil
}

func fetchSumFile(ctx context.Context, d *Download, u, label, fileName string, sidecar bool) *Checksum {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := buildRequest(ctx, d, u, "")
	if err != nil {
		return nil
	}
	resp, err := doReq(d, req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if bytes.IndexByte(b, 0) >= 0 || bytes.Contains(bytes.ToLower(b[:min(len(b), 512)]), []byte("<html")) {
		return nil
	}
	var only *Checksum
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		a, h, name := sumLine(sc.Text())
		if a == "" {
			continue
		}
		n++
		if name != "" && sameFileName(name, fileName) || name != "" && sameFileName(name, path.Base(d.URL)) {
			return &Checksum{Algo: a, Want: h, From: label}
		}
		if sidecar && (name == "" || n == 1) {
			only = &Checksum{Algo: a, Want: h, From: label}
		}
	}
	if sidecar && n == 1 && only != nil {
		return only // file.iso.sha256 with just the hash (or a name that was renamed)
	}
	return nil
}

// hashFile computes the file's digest, reporting progress (0–100).
func hashFile(ctx context.Context, p, algo string, size int64, progress func(int)) (string, error) {
	h := newHash(algo)
	if h == nil {
		return "", fmt.Errorf("unknown checksum type %q", algo)
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var done int64
	last := time.Now()
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			done += int64(n)
			if progress != nil && size > 0 && time.Since(last) > 150*time.Millisecond {
				last = time.Now()
				progress(int(done * 100 / size))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// badPieces lists the pieces whose checksum doesn't match.
func badPieces(ctx context.Context, p string, ph *PieceHashes, size int64) ([]int, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var bad []int
	buf := make([]byte, ph.Length)
	for i, want := range ph.Hashes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		off := int64(i) * ph.Length
		if off >= size {
			break
		}
		n, err := f.ReadAt(buf[:min(ph.Length, size-off)], off)
		if err != nil && err != io.EOF {
			return nil, err
		}
		h := newHash(ph.Algo)
		h.Write(buf[:n])
		if hex.EncodeToString(h.Sum(nil)) != strings.ToLower(want) {
			bad = append(bad, i)
		}
	}
	return bad, nil
}

// sumMinSize: smaller files aren't worth looking around the server for a checksum file.
var sumMinSize int64 = 20 << 20

var errRepair = errors.New("some pieces were damaged and are downloaded again")

// verify checks a finished part file against the download's checksum. When pieces don't
// match and the metalink has piece checksums, it marks just those for downloading again
// and returns errRepair. Caller doesn't hold m.mu.
func (m *Manager) verify(ctx context.Context, d *Download) error {
	if err := m.checkPieces(ctx, d); err != nil { // (the pieces not checked while it downloaded: piececheck.go)
		return err
	}
	m.mu.Lock()
	c := d.Check
	if c == nil || c.Want == "" || d.Kind == "hls" {
		m.mu.Unlock()
		return nil
	}
	algo, want := c.Algo, c.Want
	d.stage, d.stagePct = "Checking the file", 0
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		d.stage, d.stagePct = "", 0
		m.mu.Unlock()
	}()
	got, err := hashFile(ctx, d.partPath(), algo, d.Size, func(p int) {
		m.mu.Lock()
		d.stagePct = p
		m.mu.Unlock()
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if got == want {
		c.State, c.Got = "ok", ""
		return nil
	}
	if ph := d.Pieces; ph != nil && ph.Length > 0 && len(ph.Hashes) > 0 && d.repairs < 2 && d.Resumable && d.Size > 0 {
		d.stage = "Finding damaged pieces"
		m.mu.Unlock()
		bad, berr := badPieces(ctx, d.partPath(), ph, d.Size)
		m.mu.Lock()
		if berr == nil && len(bad) > 0 {
			d.repairs++
			// don't trust the addresses that sent the damaged pieces again (when there are others)
			if d.srcs != nil {
				for _, i := range bad {
					for _, u := range d.srcs.sentRange(int64(i)*ph.Length, min(int64(i+1)*ph.Length, d.Size)) {
						if d.distrust == nil {
							d.distrust = map[string]bool{}
						}
						d.distrust[u] = true
					}
				}
			}
			d.mu.Lock()
			d.Segments = nil
			var lost int64
			for _, i := range bad {
				s := int64(i) * ph.Length
				e := min(s+ph.Length, d.Size) - 1
				d.Segments = append(d.Segments, &Segment{Start: s, End: e, Pos: s})
				lost += e - s + 1
			}
			d.mu.Unlock()
			atomic.AddInt64(&d.done, -lost)
			d.Note = fmt.Sprintf("%d damaged piece%s downloaded again", len(bad), map[bool]string{true: "", false: "s"}[len(bad) == 1])
			m.dirty = true
			return errRepair
		}
		if berr != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	c.State, c.Got = "bad", got
	return nil
}

func hasMetalinkLink(ls []linkHeader) bool {
	for _, l := range ls {
		if l.Rel == "describedby" && strings.Contains(l.Type, "metalink") {
			return true
		}
	}
	return false
}

// lookForChecksum finds the checksum for a download that has none: in the metalink the
// server points to, or in a checksum file the site publishes next to it.
func (m *Manager) lookForChecksum(ctx context.Context, d *Download, links []linkHeader, mc *mirrorCheck, done chan struct{}) {
	defer close(done)
	for _, l := range links {
		if l.Rel != "describedby" || !strings.Contains(l.Type, "metalink") {
			continue
		}
		files, err := fetchMetalink(ctx, d, l.URL)
		if err != nil {
			continue
		}
		m.mu.Lock()
		name, size := d.FileName, d.Size
		m.mu.Unlock()
		for _, f := range files {
			if f.Size != size || !(sameFileName(f.Name, name) || len(files) == 1) {
				continue
			}
			var more []string
			m.mu.Lock()
			if f.Hash != nil && (d.Check == nil || d.Check.Want == "" || d.Check.From != "you" && algoRank[f.Hash.Algo] > algoRank[d.Check.Algo]) {
				d.Check = f.Hash
			}
			if f.Pieces != nil {
				d.Pieces = f.Pieces
			}
			for _, u := range f.URLs {
				if u.URL != d.URL && u.URL != d.FinalURL && !slices.Contains(d.Mirrors, u.URL) && len(d.Mirrors) < 10 {
					d.Mirrors = append(d.Mirrors, u.URL)
					more = append(more, u.URL)
				}
			}
			m.dirty = true
			m.mu.Unlock()
			mc.start(more)
			break
		}
	}
	m.mu.Lock()
	need := d.Check == nil || d.Check.Want == ""
	m.mu.Unlock()
	var found *Checksum
	if need {
		found = publishedChecksum(ctx, d, d.URL)
	}
	m.mu.Lock()
	if found != nil && (d.Check == nil || d.Check.Want == "") {
		d.Check = found
	}
	if ctx.Err() == nil {
		d.SumLooked = true
	}
	m.dirty = true
	m.mu.Unlock()
}

// fetchMetalink downloads and reads a metalink.
func fetchMetalink(ctx context.Context, d *Download, u string) ([]mlFile, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := buildRequest(ctx, d, u, "")
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/metalink4+xml, application/metalink+xml;q=0.9, */*;q=0.1")
	resp, err := doReq(d, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, statusErr{resp.StatusCode, resp.Status, 0}
	}
	return parseMetalink(resp.Body)
}

// checkFinished checks (or calculates) the checksum of a file that's already downloaded.
func (m *Manager) checkFinished(d *Download) {
	m.mu.Lock()
	c := d.Check
	if c == nil || d.Status != StCompleted || d.stage != "" {
		m.mu.Unlock()
		return
	}
	p, algo, size := d.path(), c.Algo, d.Size
	d.stage, d.stagePct = "Checking the file", 0
	m.mu.Unlock()
	got, err := hashFile(context.Background(), p, algo, size, func(pct int) {
		m.mu.Lock()
		d.stagePct = pct
		m.mu.Unlock()
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	d.stage, d.stagePct = "", 0
	m.dirty = true
	if d.Check != c { // changed meanwhile
		return
	}
	switch {
	case err != nil:
		c.State, c.Got = "", ""
		m.notify("checkErr", d.ID, "Couldn't check "+iso(d.FileName)+": "+err.Error())
	case c.Want == "":
		c.Got = got
	case got == c.Want:
		c.State, c.Got = "ok", ""
		m.notify("checkOk", d.ID, iso(d.FileName)+" matches its "+algoNames[algo]+" checksum")
	default:
		c.State, c.Got = "bad", got
		m.notify("checkBad", d.ID, iso(d.FileName)+" doesn't match the "+algoNames[algo]+" checksum — the file is damaged or a different version")
	}
}
