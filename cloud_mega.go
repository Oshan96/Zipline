package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// MEGA (mega.nz): every file is encrypted with a key of its own, which travels in the link after
// the # and never reaches MEGA. MEGA's API gives the file's address, its size and its encrypted
// name. The file is AES-128 in counter mode, so each connection decrypts its own range as the bytes
// arrive (the counter starts at the range's byte offset / 16) and the engine writes plain bytes as
// for any server. Ranges are asked for the way MEGA's own apps do, in the address (…/start-end).
// When the download finishes the file is checked against the MAC the key carries (megaCheck).

var megaAPI = "https://g.api.mega.co.nz" // replaced in tests

// megaKey is a file's key, unfolded: the AES key, the counter's nonce and the MAC the file must have.
type megaKey struct {
	aes   [16]byte
	nonce [8]byte
	mac   [8]byte
}

var megaHandle = regexp.MustCompile(`^[A-Za-z0-9_-]{8}$`)

func megaHost(h string) bool {
	switch strings.ToLower(h) {
	case "mega.nz", "www.mega.nz", "mega.co.nz", "www.mega.co.nz":
		return true
	}
	return false
}

// megaLink reads a MEGA link (mega.nz/file/<id>#<key>, the older mega.nz/#!<id>!<key>, the embed
// forms): the file's handle and key, or folder for a folder's link.
func megaLink(u *url.URL) (id, key string, folder bool) {
	p, frag := strings.TrimSuffix(u.Path, "/"), u.Fragment
	switch {
	case strings.HasPrefix(p, "/folder/") || strings.HasPrefix(frag, "F!"):
		return "", "", true
	case strings.HasPrefix(p, "/file/"):
		id, key = p[len("/file/"):], frag
	case strings.HasPrefix(p, "/embed/"):
		id, key = p[len("/embed/"):], frag
	case strings.HasPrefix(frag, "!") && (p == "" || p == "/embed"):
		id, key, _ = strings.Cut(frag[1:], "!")
	}
	if !megaHandle.MatchString(id) {
		return "", "", false
	}
	return id, strings.TrimSpace(key), false
}

// megaParseKey unfolds the key from a link: 32 bytes, the AES key XORed with the nonce and MAC
// that follow it.
func megaParseKey(s string) (*megaKey, error) {
	if s == "" {
		return nil, errors.New("this MEGA link has no key (the part after #): copy the whole link")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.NewReplacer("+", "-", "/", "_", "=", "", ",", "").Replace(s))
	if err != nil || len(b) != 32 {
		return nil, errors.New("the key in this MEGA link (the part after #) is damaged: copy the whole link again")
	}
	k := &megaKey{}
	for i := range k.aes {
		k.aes[i] = b[i] ^ b[16+i]
	}
	copy(k.nonce[:], b[16:24])
	copy(k.mac[:], b[24:32])
	return k, nil
}

var megaSeq atomic.Int64

// megaCall sends one command to MEGA's API (about a shared folder's contents when folder isn't "")
// and returns its answer. MEGA answers a number for an error; "try again" (-3) is retried a few times.
func megaCall(ctx context.Context, cmd map[string]any, folder string) (json.RawMessage, error) {
	body, _ := json.Marshal([]any{cmd})
	for try := 0; ; try++ {
		q := "?id=" + strconv.FormatInt(megaSeq.Add(1), 10)
		if folder != "" {
			q += "&n=" + url.QueryEscape(folder)
		}
		req, err := http.NewRequestWithContext(ctx, "POST", megaAPI+"/cs"+q, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		var out json.RawMessage
		if _, err := cloudJSON(req, nil, &out); err != nil {
			return nil, err
		}
		var list []json.RawMessage
		if json.Unmarshal(out, &list) == nil && len(list) > 0 {
			out = list[0]
		}
		code := 0
		if json.Unmarshal(out, &code) != nil {
			var e struct {
				E int `json:"e"`
			}
			json.Unmarshal(out, &e)
			code = e.E
		}
		if code == -3 && try < 3 { // busy: back off and ask again
			if !sleepCtx(ctx, time.Duration(1<<try)*time.Second) {
				return nil, ctx.Err()
			}
			continue
		}
		if code < 0 {
			return nil, megaErr(code)
		}
		return out, nil
	}
}

// megaErr says what MEGA's error codes mean, as MEGA puts it.
func megaErr(code int) error {
	var msg string
	switch code {
	case -2:
		msg = "MEGA: The link is invalid (check that it was copied whole)"
	case -9:
		msg = "MEGA: The file you are trying to download is no longer available"
	case -11:
		msg = "MEGA: Access denied"
	case -16:
		msg = "MEGA: The file you are trying to download has been taken down"
	case -17:
		msg = "MEGA: Transfer quota exceeded. Try again later"
	case -18:
		msg = "MEGA: The file is temporarily not available. Try again later"
	default:
		msg = fmt.Sprintf("MEGA refused the link (error %d)", code)
	}
	if code == -17 || code == -18 {
		return errors.New(msg) // (may work later)
	}
	return permanentErr{errors.New(msg)}
}

// resolveMega asks MEGA for the file's address, size and name.
func resolveMega(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", permanentErr{err}
	}
	if fid, fkey, node, ok := megaFolderLink(u); ok { // a shared folder, or a file in one (cloud_megafolder.go)
		return resolveMegaInFolder(ctx, fid, fkey, node)
	}
	id, key, _ := megaLink(u)
	k, err := megaParseKey(key)
	if err != nil {
		return "", permanentErr{err}
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, err := megaCall(ctx, map[string]any{"a": "g", "g": 1, "ssl": 2, "p": id}, "")
	if err != nil {
		return "", err
	}
	return megaAddress(out, k, "")
}

// megaAddress reads MEGA's answer about a file: its address, its size and (decrypted) its name,
// which are noted for the reply that brings the file.
func megaAddress(out json.RawMessage, k *megaKey, name string) (string, error) {
	var g struct {
		S  int64           `json:"s"`
		At string          `json:"at"`
		G  json.RawMessage `json:"g"`
	}
	json.Unmarshal(out, &g)
	var addr string
	if json.Unmarshal(g.G, &addr) != nil || !(strings.HasPrefix(addr, "https://") || strings.HasPrefix(addr, "http://")) {
		if len(g.G) > 0 && g.G[0] == '[' {
			return "", permanentErr{errors.New("MEGA keeps this file in several parts (CloudRAID), which Zipline can't download yet: download it in the browser")}
		}
		return "", errors.New("MEGA didn't hand out the file's address. Try again later")
	}
	if n, ok := megaName(g.At, k); ok {
		name = n
	} else if name == "" {
		return "", permanentErr{errors.New("the key in this MEGA link (the part after #) doesn't fit the file: copy the whole link again")}
	}
	noteCloudFact(addr, &cloudFact{name: name, size: g.S, mega: k})
	return addr, nil
}

// megaName decrypts the file's attributes (AES-CBC, zero IV: "MEGA" and JSON) for its name. A key
// that doesn't fit gives no "MEGA" in front.
func megaName(at string, k *megaKey) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(at, "="))
	if err != nil || len(b) == 0 || len(b)%16 != 0 {
		return "", false
	}
	block, _ := aes.NewCipher(k.aes[:])
	cipher.NewCBCDecrypter(block, make([]byte, 16)).CryptBlocks(b, b)
	b = bytes.TrimRight(b, "\x00")
	if !bytes.HasPrefix(b, []byte("MEGA{")) {
		return "", false
	}
	var a struct {
		N string `json:"n"`
	}
	json.Unmarshal(b[4:], &a)
	return a.N, true
}

// megaStream is the key stream for the file from byte off on.
func megaStream(k *megaKey, off int64) cipher.Stream {
	block, _ := aes.NewCipher(k.aes[:])
	var iv [16]byte
	copy(iv[:8], k.nonce[:])
	binary.BigEndian.PutUint64(iv[8:], uint64(off/16))
	s := cipher.NewCTR(block, iv[:])
	if skip := off % 16; skip > 0 { // (a range that starts inside a block)
		var x [16]byte
		s.XORKeyStream(x[:skip], x[:skip])
	}
	return s
}

// ctrBody decrypts a reply as it's read.
type ctrBody struct {
	io.ReadCloser
	s cipher.Stream
}

func (b *ctrBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.s.XORKeyStream(p[:n], p[:n])
	return n, err
}

// megaDo fetches a range of a MEGA file: asked for in the address, as MEGA's apps do, and answered
// to the engine as an ordinary 206 reply with the bytes decrypted.
func megaDo(d *Download, req *http.Request, f *cloudFact) (*http.Response, error) {
	if f.size == 0 { // nothing to fetch
		h := http.Header{}
		f.label(h)
		return &http.Response{StatusCode: 200, Status: "200 OK", Header: h, Body: http.NoBody, Request: req, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
	}
	start, end, ranged := int64(0), f.size-1, false
	if r := req.Header.Get("Range"); r != "" {
		var a, b int64 = 0, -1
		if from, to, ok := strings.Cut(strings.TrimPrefix(r, "bytes="), "-"); ok {
			a, _ = strconv.ParseInt(from, 10, 64)
			if to != "" {
				b, _ = strconv.ParseInt(to, 10, 64)
			}
		}
		if b < 0 || b >= f.size {
			b = f.size - 1
		}
		start, end, ranged = a, b, true
	}
	if start > end {
		return &http.Response{StatusCode: 416, Status: "416 Range Not Satisfiable", Header: http.Header{}, Body: http.NoBody, Request: req}, nil
	}
	out := req
	if ranged {
		out = req.Clone(req.Context())
		u := *req.URL
		u.Path = strings.TrimSuffix(u.Path, "/") + fmt.Sprintf("/%d-%d", start, end)
		u.RawPath = ""
		out.URL, out.Host = &u, ""
		out.Header.Del("Range")
		out.Header.Del("If-Range")
	}
	resp, err := doReq(d, out)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == 509: // the free transfer quota is used up
		wait := resp.Header.Get("X-MEGA-Time-Left")
		drainClose(resp.Body)
		msg := "MEGA: Bandwidth Limit Exceeded (the free transfer quota for your connection is used up)"
		if n, err := strconv.Atoi(strings.TrimSpace(wait)); err == nil && n > 0 {
			msg += ". It frees up in about " + waitWords(time.Duration(n)*time.Second)
		}
		return nil, permanentErr{errors.New(msg)}
	case resp.StatusCode != 200 && resp.StatusCode != 206:
		return resp, nil
	case resp.ContentLength >= 0 && resp.ContentLength != end-start+1 && resp.ContentLength != f.size:
		resp.Body.Close()
		return nil, fmt.Errorf("MEGA sent %d bytes for a range of %d", resp.ContentLength, end-start+1)
	}
	switch {
	case resp.StatusCode == 206: // (asked for in a header after all)
		if first, _, _, ok := parseContentRange(resp.Header.Get("Content-Range")); ok {
			start = first
		}
	case ranged && !(start > 0 && resp.ContentLength == f.size):
		resp.StatusCode, resp.Status = 206, "206 Partial Content"
		resp.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, f.size))
	default:
		start = 0 // the whole file
	}
	resp.Header.Set("Accept-Ranges", "bytes")
	f.label(resp.Header)
	resp.Body = &ctrBody{ReadCloser: resp.Body, s: megaStream(f.mega, start)}
	resp.Request = req // (the file's address without the range in it)
	return resp, nil
}

// waitWords is a wait in words: "2 h 10 min", "25 min".
func waitWords(w time.Duration) string {
	m := int(w.Round(time.Minute) / time.Minute)
	switch {
	case m < 1:
		return "a minute"
	case m < 60:
		return strconv.Itoa(m) + " min"
	case m%60 == 0:
		return strconv.Itoa(m/60) + " h"
	}
	return strconv.Itoa(m/60) + " h " + strconv.Itoa(m%60) + " min"
}

// megaChunks calls fn for each of MEGA's MAC chunks of a file: 128 KB, 256 KB … growing by 128 KB
// up to 1 MB, then 1 MB each.
func megaChunks(size int64, fn func(start, n int64) bool) {
	var p, s int64 = 0, 128 << 10
	for p+s < size {
		if !fn(p, s) {
			return
		}
		p += s
		if s < 1<<20 {
			s += 128 << 10
		}
	}
	fn(p, size-p)
}

// megaMAC works out a file's condensed MAC: a CBC-MAC of each chunk (starting from the nonce
// twice), those MACs CBC-MACed in turn, and that folded to 8 bytes.
func megaMAC(ctx context.Context, r io.Reader, k *megaKey, size int64, progress func(done int64)) ([8]byte, error) {
	var out [8]byte
	block, _ := aes.NewCipher(k.aes[:])
	meta := cipher.NewCBCEncrypter(block, make([]byte, 16))
	var iv, mac, sum [16]byte
	copy(iv[:8], k.nonce[:])
	copy(iv[8:], k.nonce[:])
	buf := make([]byte, 1<<20+16)
	var err error
	megaChunks(size, func(start, n int64) bool {
		if err = ctx.Err(); err != nil {
			return false
		}
		if _, err = io.ReadFull(r, buf[:n]); err != nil {
			return false
		}
		padded := (n + 15) / 16 * 16
		clear(buf[n:padded])
		cipher.NewCBCEncrypter(block, iv[:]).CryptBlocks(buf[:padded], buf[:padded]) // (its last block is the chunk's MAC)
		copy(mac[:], buf[padded-16:padded])
		meta.CryptBlocks(sum[:], mac[:])
		if progress != nil {
			progress(start + n)
		}
		return true
	})
	if err != nil {
		return out, err
	}
	for i := 0; i < 4; i++ {
		out[i] = sum[i] ^ sum[4+i]
		out[4+i] = sum[8+i] ^ sum[12+i]
	}
	return out, nil
}

var errMegaMAC = errors.New("the file doesn't match the MAC in its MEGA link: it was damaged on the way. Use Redownload to fetch it again")

// megaCheck checks a finished MEGA download against its key's MAC. Caller doesn't hold m.mu.
func (m *Manager) megaCheck(ctx context.Context, d *Download) error {
	m.mu.Lock()
	raw, size, p := d.URL, d.Size, d.partPath()
	m.mu.Unlock()
	u, err := url.Parse(raw)
	if err != nil || cloudService(raw) != "MEGA" || size <= 0 {
		return nil
	}
	_, key, _ := megaLink(u)
	k, err := megaParseKey(key)
	if err != nil {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return permanentErr{err}
	}
	defer f.Close()
	m.mu.Lock()
	d.stage, d.stagePct = "Checking the file", 0
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		d.stage, d.stagePct = "", 0
		m.mu.Unlock()
	}()
	last := time.Now()
	got, err := megaMAC(ctx, f, k, size, func(done int64) {
		if time.Since(last) > 150*time.Millisecond {
			last = time.Now()
			m.mu.Lock()
			d.stagePct = int(done * 100 / size)
			m.mu.Unlock()
		}
	})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return permanentErr{err}
	}
	if got != k.mac {
		logf("%s: MEGA MAC %x, want %x", d.FileName, got, k.mac)
		return permanentErr{errMegaMAC}
	}
	return nil
}
