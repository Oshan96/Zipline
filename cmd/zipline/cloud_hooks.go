package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// How a share link's file reaches the engine: resolveCloud turns the link into the address the
// file comes from, and notes what the service said about the file (cloudFact). Every request a
// download makes goes through doReq, which hands requests for a share link's file to cloudDo: it
// adds what the service wants with the request and puts what it said about the file into the reply
// as ordinary headers (the name as Content-Disposition, a checksum as Repr-Digest), so the engine
// reads them like any server's. MEGA's encrypted files are decrypted there too (cloud_mega.go), and
// a service's refusal page becomes an error in its own words instead of being saved as the file.

// cloudFact is what a service said about a file when its link was resolved.
type cloudFact struct {
	name     string
	size     int64  // -1 unknown
	sha256   string // hex
	quickXor string // base64, as OneDrive gives it
	mega     *megaKey
	used     time.Time
}

// The facts by the address the file comes from (they're looked up for each reply).
var cloudFacts = struct {
	sync.Mutex
	m map[string]*cloudFact
}{m: map[string]*cloudFact{}}

func noteCloudFact(direct string, f *cloudFact) {
	cloudFacts.Lock()
	defer cloudFacts.Unlock()
	for k, v := range cloudFacts.m {
		if time.Since(v.used) > 24*time.Hour || len(cloudFacts.m) > 2000 {
			delete(cloudFacts.m, k)
		}
	}
	f.used = time.Now()
	cloudFacts.m[factKey(direct)] = f
}

func cloudFactFor(u string) *cloudFact {
	cloudFacts.Lock()
	defer cloudFacts.Unlock()
	f := cloudFacts.m[factKey(u)]
	if f != nil {
		f.used = time.Now()
	}
	return f
}

// factKey writes an address the way a request does (so the two always compare equal).
func factKey(u string) string {
	if pu, err := url.Parse(u); err == nil {
		return pu.String()
	}
	return u
}

type cloudDoKey struct{}

// cloudHandles: req is one of a share link's download's own requests that cloudDo hasn't seen yet.
func cloudHandles(d *Download, req *http.Request) bool {
	return d != nil && req.Context().Value(cloudDoKey{}) == nil && cloudService(d.URL) != ""
}

// cloudDo sends a share link download's request the way its service wants it.
func cloudDo(d *Download, req *http.Request) (*http.Response, error) {
	svc := cloudService(d.URL)
	f := cloudFactFor(req.URL.String())
	req = req.WithContext(context.WithValue(req.Context(), cloudDoKey{}, true))
	if svc == "Google Drive" {
		gdriveKeyHeader(d.URL, req)
	}
	if f != nil && f.mega != nil {
		return megaDo(d, req, f)
	}
	if svc == "MEGA" && !megaHost(req.URL.Hostname()) { // an address from before (Zipline restarted): it can't be decrypted
		return nil, errMegaAgain
	}
	resp, err := doReq(d, req)
	if err != nil {
		return nil, err
	}
	if svc == "Dropbox" {
		if err := dropboxRefusal(resp); err != nil {
			return nil, err
		}
	}
	if f != nil && resp.StatusCode < 300 {
		f.label(resp.Header)
	}
	return resp, nil
}

// errMegaAgain: the engine asks the share link for the file's address again (and learns its key).
var errMegaAgain = errors.New("MEGA's address for the file has to be asked for again")

// quickXorHeader carries OneDrive's checksum from cloudDo to headerChecksum (it's never sent).
const quickXorHeader = "Zipline-Quickxorhash"

// label writes the facts into a reply's headers, where the engine looks for them.
func (f *cloudFact) label(h http.Header) {
	if f.name != "" && (f.mega != nil || h.Get("Content-Disposition") == "") {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.name}))
	}
	if b, err := hex.DecodeString(f.sha256); err == nil && len(b) == 32 {
		h.Add("Repr-Digest", "sha-256=:"+base64.StdEncoding.EncodeToString(b)+":")
	}
	if f.quickXor != "" {
		h.Set(quickXorHeader, f.quickXor)
	}
}

// cloudChecksum is the checksum a service gave that headerChecksum doesn't know: OneDrive's
// quickXorHash.
func cloudChecksum(hs []http.Header) *Checksum {
	for _, h := range hs {
		if b, err := base64.StdEncoding.DecodeString(h.Get(quickXorHeader)); err == nil && len(b) == 20 {
			return &Checksum{Algo: "quickxor", Want: hex.EncodeToString(b), From: "OneDrive"}
		}
	}
	return nil
}

func init() { algoNames["quickxor"] = "QuickXorHash" }

// cloudCheck checks a finished share link download the way its service allows: a MEGA file
// against its key's MAC. Caller doesn't hold m.mu.
func (m *Manager) cloudCheck(ctx context.Context, d *Download) error {
	return m.megaCheck(ctx, d)
}

// pageText reads the start of a web page a service answered with, and puts it back so the reply
// can still be read as it was.
func pageText(resp *http.Response, n int64) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, n))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), resp.Body), resp.Body}
	return string(b)
}

func isHTML(h http.Header) bool {
	return strings.Contains(strings.ToLower(h.Get("Content-Type")), "text/html")
}

// errCloudFolder: the link is a shared folder. The Add dialog lists its files (cloudfolder.go);
// added as one download, it fails with this.
var errCloudFolder = errors.New("this link is a shared folder: paste it into Add link to choose the files in it")

// cloudGet asks a service's API for JSON. It answers the status too, so a refusal's words can be
// read from out.
func cloudGet(ctx context.Context, u string, hdr map[string]string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return 0, err
	}
	return cloudJSON(req, hdr, out)
}

func cloudJSON(req *http.Request, hdr map[string]string, out any) (int, error) {
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if err := json.Unmarshal(b, out); err != nil {
		if resp.StatusCode >= 400 {
			return resp.StatusCode, statusErr{resp.StatusCode, resp.Status, 0}
		}
		return resp.StatusCode, err
	}
	return resp.StatusCode, nil
}
