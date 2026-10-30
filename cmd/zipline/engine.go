package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"zipline/internal/filename"
)

const (
	StQueued      = "queued"
	StConnecting  = "connecting"
	StDownloading = "downloading"
	StPaused      = "paused"
	StCompleted   = "completed"
	StError       = "error"

	minSplit   = 128 * 1024 // splitting never leaves a piece smaller than this
	minInitSeg = 1 << 20    // smallest initial segment
	bufSize    = 256 * 1024
	stallLimit = 30 * time.Second
	splitAhead = 2.0       // seconds: a new connection only takes over work that would last longer (as browsers do)
	drainMax   = 256 << 10 // what's left of a reply that's no longer needed is read out up to this, to reuse its connection
	drainSmall = 64 << 10  // small replies closed unread (error pages, one-byte checks) are read out up to this
	browserUA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"
	partSuffix = ".zdl"
)

var defaultUA = "Zipline/" + appVersion

// Timing, replaced in tests.
var (
	maxTries     = 10               // failed tries in a row (without progress) before a connection gives up
	retryBase    = time.Second      // a connection's first wait before trying again; it doubles each time
	retryCap     = time.Minute      // the longest wait between tries
	ctlEvery     = time.Second      // how often a download reconsiders its number of connections
	growPause    = 30 * time.Second // after adding connections stopped helping, try again this often
	learnedCreep = time.Minute      // a server's learned connection limit goes up by one this long after it last changed
	sampleWait   = 10 * time.Second // how long a new mirror waits for something to compare with
	// waits before trying a whole download again after every connection gave up (an outage)
	wholeRetry = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute}
)

// Segment is a byte range [Start, End] of the file; Pos is the next byte to write.
type Segment struct {
	Start  int64 `json:"s"`
	End    int64 `json:"e"`
	Pos    int64 `json:"p"`
	active bool
	state  string
	t0     time.Time // when the current worker started on it
	p0     int64     // Pos at t0
}

// eta estimates seconds until this segment finishes at its current rate.
func (s *Segment) eta(now time.Time) (float64, float64) {
	el := now.Sub(s.t0).Seconds()
	if s.t0.IsZero() || el < 0.5 {
		return float64(s.rem()) / (1 << 20), 0 // unknown: assume 1 MB/s
	}
	rate := float64(s.Pos-s.p0) / el
	if rate < 1 {
		rate = 1
	}
	return float64(s.rem()) / rate, rate
}

func (s *Segment) rem() int64 { return s.End - s.Pos + 1 }

type Download struct {
	ID        string            `json:"id"`
	URL       string            `json:"url"`
	FinalURL  string            `json:"finalUrl,omitempty"`
	FileName  string            `json:"name"`
	Dir       string            `json:"dir"`
	Category  string            `json:"category"`
	Size      int64             `json:"size"`
	Resumable bool              `json:"resumable"`
	Status    string            `json:"status"`
	Error     string            `json:"error,omitempty"`
	Added     time.Time         `json:"added"`
	Finished  time.Time         `json:"finished,omitempty"`
	LastTry   time.Time         `json:"lastTry,omitempty"`
	Segments  []*Segment        `json:"segments,omitempty"`
	Conns     int               `json:"conns"`
	Headers   map[string]string `json:"headers,omitempty"`
	Queue     string            `json:"queue,omitempty"`
	LegacyQ   bool              `json:"inQueue,omitempty"` // pre-1.2 lists: true meant the main queue
	Desc      string            `json:"desc,omitempty"`
	LimitKB   int               `json:"limitKB,omitempty"` // per-download speed limit, 0 = none
	Scan      string            `json:"scan,omitempty"`    // virus scan: "" = follow settings, "on", "off"
	ScanState string            `json:"scanState,omitempty"`
	ScanNote  string            `json:"scanNote,omitempty"`
	ETag      string            `json:"etag,omitempty"`
	LastMod   string            `json:"lastMod,omitempty"`
	Replace   bool              `json:"replace,omitempty"` // sync update: overwrite the old file
	Checked   time.Time         `json:"checked,omitempty"`
	SyncNote  string            `json:"syncNote,omitempty"`
	SyncSig   string            `json:"syncSig,omitempty"`   // a stream's playlist fingerprint (sync queues)
	Kind      string            `json:"kind,omitempty"`      // "" = plain file, "hls" = video stream
	Variant   int               `json:"variant,omitempty"`   // HLS: chosen bandwidth (0 = best)
	VarRes    string            `json:"varRes,omitempty"`    // HLS: the chosen quality's resolution, codecs and frame rate
	VarCodecs string            `json:"varCodecs,omitempty"` // (bandwidth alone can't tell H.264 and HEVC apart)
	VarFPS    float64           `json:"varFps,omitempty"`
	Parts     int               `json:"parts,omitempty"`     // HLS: segments in the stream
	PartsDone int               `json:"partsDone,omitempty"` // HLS: segments written in order
	PartBytes int64             `json:"partBytes,omitempty"` // HLS: bytes written for PartsDone
	AParts    int               `json:"aParts,omitempty"`    // HLS: pieces of the separate audio track
	ADone     int               `json:"aDone,omitempty"`
	ABytes    int64             `json:"aBytes,omitempty"`
	Packaged  bool              `json:"packaged,omitempty"` // stream pieces combined into the final file (saved before it replaces them)
	Note      string            `json:"note,omitempty"`
	Mirrors   []string          `json:"mirrors,omitempty"`    // other addresses for the same file
	Check     *Checksum         `json:"check,omitempty"`      // the checksum the file should match
	Pieces    *PieceHashes      `json:"pieces,omitempty"`     // a metalink's checksums per piece
	PieceOK   bitfield          `json:"pieceOk,omitempty"`    // pieces that matched theirs (guarded by mu; piececheck.go)
	SumLooked bool              `json:"sumLooked,omitempty"`  // looked for a checksum the site publishes
	Link      string            `json:"link,omitempty"`       // "expired", "refreshing" or "waiting": the link needs renewing
	Torrent   *TorrentState     `json:"torrent,omitempty"`    // a torrent (Kind "torrent")
	LMStrong  bool              `json:"lmStrong,omitempty"`   // LastMod tells versions apart (well before the server's clock), so If-Range may use it
	Waiting   bool              `json:"waiting,omitempty"`    // started by hand while "downloads at once" was reached: starts when one ends
	Challenge bool              `json:"challenge,omitempty"`  // stopped by the site's bot protection: open it in the browser
	RetryAt   time.Time         `json:"-"`                    // every connection was lost: the whole download tries again then
	Resume    bool              `json:"resume,omitempty"`     // was running: carries on when Zipline starts again (startup.go)
	Method    string            `json:"method,omitempty"`     // the browser's own request: "POST" for a form's download ("" = GET; extension.go)
	Body      []byte            `json:"body,omitempty"`       // the form it sent
	BodySeal  string            `json:"bodySealed,omitempty"` // Body as saved: sealed, as forms can carry passwords (vault.go)
	Private   bool              `json:"-"`                    // from a private window: never saved, off the list once finished

	// Streams: the rest of the quality a download started with, so carrying on never mixes two (hlsident.go);
	// the playlist's length of the pieces written and how much of it they were missing, for the
	// main track and the separate audio (hlscheck.go); a live recording and its settings (hlslive.go).
	VarInit    string  `json:"varInit,omitempty"` // the init piece's fingerprint (MP4 pieces) or the stream types (MPEG-TS)
	VarAvgBW   int     `json:"varAvgBw,omitempty"`
	WantSecs   float64 `json:"wantSecs,omitempty"`
	ShortSecs  float64 `json:"shortSecs,omitempty"`
	AWantSecs  float64 `json:"aWantSecs,omitempty"`
	AShortSecs float64 `json:"aShortSecs,omitempty"`
	Live       bool    `json:"live,omitempty"`      // a live stream, recorded as it plays
	LiveFrom   string  `json:"liveFrom,omitempty"`  // "start": an event stream from its beginning (else from now)
	StopAfter  int     `json:"stopAfter,omitempty"` // seconds: the recording stops at this length (0: when the stream ends)
	// A video stream's chosen tracks (tracks.go), its other audio tracks being downloaded and
	// where its parts start in each file (discont.go).
	StreamPick
	Extra []*TrackPart `json:"extra,omitempty"`
	Marks partMarks    `json:"marks,omitempty"`

	mu       sync.Mutex
	done     int64 // atomic
	workers  int32 // atomic
	speed    float64
	lastDone int64
	running  bool
	cancel   context.CancelFunc
	lastErr  error
	fatalErr error
	lim      Limiter
	stage    string // after downloading: "Converting to MP4" and the like (guarded by Manager.mu)
	stagePct int

	srcs      *sourceSet           // addresses in use while it runs
	file      *os.File             // the part file while it downloads (guarded by mu): synced before progress is saved
	jar       http.CookieJar       // its own cookies, as a browser keeps them (guarded by mu)
	auth      map[string]*hostAuth // servers (host:port) that asked for its login, and how (guarded by mu)
	sumDone   chan struct{}        // closed when the search for a published checksum is over
	repairs   int                  // damaged pieces downloaded again this run
	linkTries int                  // automatic attempts to get a fresh link
	linkWait  time.Time            // waiting for its link to be clicked again until then
	distrust  map[string]bool      // addresses that sent damaged pieces
	pc        *pieceChecker        // checks pieces as they complete (set under mu; piececheck.go)
	hs        *hlsRun              // a stream's pieces while they download (hlsfetch.go)

	// Provisional: started while the Add dialog is still open (Options → General). Hidden
	// from the list until confirmed; dropped if the dialog goes away.
	Provisional bool `json:"-"`
	keepAlive   time.Time
	announce    bool // finished while provisional: say so once it's confirmed
	meteredOK   bool // started by hand on a metered connection
	noSeed      bool // Stop sharing was pressed while its run was starting over: it doesn't share when it finishes (guarded by Manager.mu)

	shareNote *ShareError // why a finished torrent wasn't shared when asked to (treshare.go; guarded by Manager.mu)

	doneQueue string // the queue it finished in: told with that queue, not with a window and balloon of its own (guarded by Manager.mu)
}

func (d *Download) path() string     { return filepath.Join(d.Dir, d.FileName) }
func (d *Download) partPath() string { return d.path() + partSuffix }

// audioPartPath holds a stream's separate audio track until it's merged in.
func (d *Download) audioPartPath() string { return d.partPath() + ".audio" }

func (d *Download) removeParts() {
	os.Remove(d.partPath())
	os.Remove(d.audioPartPath())
	for i := range d.Extra {
		os.Remove(d.extraPartPath(i))
	}
}

// ---------------------------------------------------------------- HTTP client

var httpClient = newHTTPClient()

func newHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 proxyForRequest,
		DialContext:           dialNet, // several adapters at once when that's on
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 40 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    true,
		ReadBufferSize:        bufSize,
		WriteBufferSize:       64 * 1024,
		// new connections to a server resume its TLS session instead of a full handshake
		TLSClientConfig: &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(512)},
		// HTTP/1.1 only: separate TCP connections per segment is what makes
		// multi-connection downloading fast (HTTP/2 would multiplex them on one).
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &http.Client{Transport: tr}
}

// browserHosts are servers (host:port) that turned Zipline's own name away but took a browser's.
// Remembered while Zipline runs.
var browserHosts sync.Map

func needsBrowserUA(u *url.URL) bool {
	_, ok := browserHosts.Load(strings.ToLower(u.Host))
	return ok
}

func buildRequest(ctx context.Context, d *Download, u string, rng string) (*http.Request, error) {
	method, body := requestBody(d, u)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	ua := defaultUA // honest by default; a browser's name only for servers that insist
	if needsBrowserUA(req.URL) {
		ua = browserUA
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	own := true // sign-in details only go to the site they came from (not to mirrors or other sites)
	if pu, err := url.Parse(d.URL); err == nil && pu.Host != "" && !sameSite(pu.Hostname(), req.URL.Hostname()) {
		own = false
	}
	for k, v := range d.Headers {
		if v == "" || isSealed(v) || !own && (strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Authorization")) {
			continue
		}
		if strings.EqualFold(k, "Authorization") && strings.HasPrefix(v, "Basic ") {
			continue // a user name and password: sent by doReq only when the server asks
		}
		req.Header.Set(k, v)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	// A login in the link itself: kept aside (Go would send it to anyone up front, as Basic).
	if req.URL.User != nil {
		req = req.WithContext(context.WithValue(ctx, urlLoginKey{}, req.URL.User))
		req.URL.User = nil
	}
	return req, nil
}

type probeResult struct {
	body      io.ReadCloser
	length    int64 // of this reply (-1 = unknown)
	partial   bool  // the stream starts at the requested offset
	size      int64
	resumable bool
	name      string
	finalURL  string
	mime      string
	etag      string
	lastMod   string
	lmStrong  bool          // lastMod can tell versions apart (see strongLastMod)
	hdrs      []http.Header // every reply's headers, redirects included (mirrors, checksums)
}

// discard closes the probe's reply, reading it out first when it's small (a one-byte check) so
// the connection can be used again.
func (pr *probeResult) discard() {
	if pr.length >= 0 && pr.length <= drainSmall {
		drainClose(pr.body)
		return
	}
	pr.body.Close()
}

// drainClose reads out a small reply (an error page, a one-byte check) before closing it, so its
// connection goes back to the pool. A big or slow one is simply closed.
func drainClose(rc io.ReadCloser) {
	t := time.AfterFunc(2*time.Second, func() { rc.Close() })
	io.CopyN(io.Discard, rc, drainSmall)
	t.Stop()
	rc.Close()
}

var (
	errChallenge = errors.New("the site wants to check you're a person (bot protection) — open the link in your browser")
	errChanged   = errors.New("the file changed on the server since this download started — use Redownload")
	errWrongPart = errors.New("the server sent the wrong part of the file")
)

// challenged: a bot-protection check (Cloudflare's "are you human" page and the like) came back
// instead of the file. Retrying can't get past it; the browser can.
func challenged(resp *http.Response) bool {
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("Cf-Mitigated")), "challenge") {
		return true
	}
	switch resp.StatusCode {
	case 403, 429, 503:
		return strings.EqualFold(resp.Header.Get("Server"), "cloudflare") &&
			strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html")
	}
	return false
}

// strongLastMod: a Last-Modified at least a minute older than the reply's Date can tell versions
// apart (RFC 9110 8.8.2.2). Servers that stamp every reply with the current time can't be asked
// "only if unchanged" with it.
func strongLastMod(h http.Header) bool {
	lm, err := http.ParseTime(h.Get("Last-Modified"))
	if err != nil {
		return false
	}
	date, err := http.ParseTime(h.Get("Date"))
	return err == nil && date.Sub(lm) >= time.Minute
}

// ifRange is the validator that requests for the rest of d's file carry (If-Range), so a server
// whose file changed answers with the new version (200) instead of a piece of it: the strong
// ETag, else a Last-Modified that tells versions apart. "" = none. Caller holds m.mu.
func ifRange(d *Download) string {
	if d.ETag != "" && !strings.HasPrefix(d.ETag, "W/") {
		return d.ETag
	}
	if d.LastMod != "" && d.LMStrong {
		return d.LastMod
	}
	return ""
}

func rangeHeader(pos, end int64) string {
	if end < 0 {
		return fmt.Sprintf("bytes=%d-", pos)
	}
	return fmt.Sprintf("bytes=%d-%d", pos, end)
}

// rightPart checks a 206 reply's Content-Range against the range asked for (end -1 = to the end):
// it must start at pos and reach end unless the file ends first.
func rightPart(cr string, pos, end int64) bool {
	first, last, total, ok := parseContentRange(cr)
	return ok && first == pos && (end < 0 || last >= end || total > 0 && last == total-1)
}

// probe opens the file at pos (end -1 = to the end) and inspects the reply. The body
// stays open so the first segment keeps streaming on the same connection.
func probe(ctx context.Context, d *Download, u string, pos, end int64) (*probeResult, error) {
	return probeIf(ctx, d, u, pos, end, "")
}

// probeIf is probe for a resumed download: with a validator (If-Range) the server only sends the
// rest if the file is still the same version.
func probeIf(ctx context.Context, d *Download, u string, pos, end int64, validator string) (*probeResult, error) {
	if isFTP(u) {
		return probeFTP(ctx, d, u, pos)
	}
	if cloudService(u) != "" { // a Google Drive, Dropbox or OneDrive share link: ask for the file itself
		ru, err := resolveCloud(ctx, d, u)
		if err != nil {
			return nil, err
		}
		u = ru
	}
	req, err := buildRequest(ctx, d, u, rangeHeader(pos, end))
	if err != nil {
		return nil, err
	}
	// servers that can, say the file's checksum: RFC 3230 and its successor RFC 9530
	req.Header.Set("Want-Digest", "SHA-512;q=1, SHA-256;q=0.9, SHA;q=0.2, MD5;q=0.1")
	req.Header.Set("Want-Repr-Digest", "sha-512=3, sha-256=10")
	if validator != "" {
		req.Header.Set("If-Range", validator)
	}
	resp, err := doReq(d, req)
	if err != nil {
		return nil, err
	}
	// Some servers turn an app's name away but not a browser's: ask once more as a browser, and
	// remember the server for the rest of the session.
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotAcceptable) && req.Header.Get("User-Agent") == defaultUA {
		drainClose(resp.Body)
		again := resend(req, ctx)
		again.Header.Set("User-Agent", browserUA)
		if resp, err = doReq(d, again); err != nil {
			return nil, err
		}
		if resp.StatusCode < 400 {
			browserHosts.Store(strings.ToLower(req.URL.Host), true)
			browserHosts.Store(strings.ToLower(resp.Request.URL.Host), true)
		}
	}
	if challenged(resp) {
		drainClose(resp.Body)
		return nil, permanentErr{errChallenge}
	}
	pr := &probeResult{body: resp.Body, length: resp.ContentLength, size: -1, finalURL: resp.Request.URL.String(), mime: resp.Header.Get("Content-Type"),
		etag: resp.Header.Get("ETag"), lastMod: resp.Header.Get("Last-Modified"), lmStrong: strongLastMod(resp.Header)}
	for r := resp; r != nil; {
		pr.hdrs = append(pr.hdrs, r.Header)
		if r.Request == nil {
			break
		}
		r = r.Request.Response
	}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		cr := resp.Header.Get("Content-Range")
		if _, _, _, ok := parseContentRange(cr); ok && !rightPart(cr, pos, end) {
			drainClose(resp.Body)
			return nil, permanentErr{errWrongPart}
		}
		pr.partial = true
		if total := parseContentRangeTotal(cr); total > 0 {
			pr.size, pr.resumable = total, true
		}
	case http.StatusOK:
		if validator != "" { // asked for the rest "if unchanged" and got the whole file: it changed
			resp.Body.Close()
			return nil, permanentErr{errChanged}
		}
		pr.size = resp.ContentLength
		pr.resumable = false // server ignored our Range header
		pr.partial = pos == 0
	case http.StatusRequestedRangeNotSatisfiable:
		drainClose(resp.Body)
		return nil, permanentErr{errors.New("server rejected the byte range (file changed on server?)")}
	case http.StatusProxyAuthRequired:
		drainClose(resp.Body)
		return nil, permanentErr{errors.New("the proxy needs a user name and password (Options → Proxy)")}
	case http.StatusUnauthorized:
		drainClose(resp.Body)
		return nil, loginError(resp.Request)
	default:
		drainClose(resp.Body)
		return nil, statusErr{resp.StatusCode, resp.Status, retryAfterHeader(resp.Header)}
	}
	pr.name = fileNameFrom(resp)
	return pr, nil
}

// loginError explains a 401 reply. Retrying wouldn't help, so it's permanent.
func loginError(req *http.Request) error {
	if req != nil && req.Header.Get("Authorization") != "" {
		return permanentErr{errors.New("the site refused the user name and password (check them in Options → Sites)")}
	}
	return permanentErr{errors.New("the site needs a user name and password (save one in Options → Sites)")}
}

func parseContentRangeTotal(cr string) int64 {
	// "bytes 0-99/1234"
	i := strings.LastIndex(cr, "/")
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// parseContentRange reads "bytes first-last/total" (total -1 when the server writes "*").
func parseContentRange(cr string) (first, last, total int64, ok bool) {
	unit, rest, found := strings.Cut(strings.TrimSpace(cr), " ")
	if !found || !strings.EqualFold(unit, "bytes") {
		return 0, 0, 0, false
	}
	rng, tot, found := strings.Cut(strings.TrimSpace(rest), "/")
	a, b, found2 := strings.Cut(rng, "-")
	if !found || !found2 {
		return 0, 0, 0, false
	}
	first, err1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	last, err2 := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	if err1 != nil || err2 != nil || first < 0 || last < first {
		return 0, 0, 0, false
	}
	total = -1
	if t := strings.TrimSpace(tot); t != "*" {
		n, err := strconv.ParseInt(t, 10, 64)
		if err != nil || n <= last {
			return 0, 0, 0, false
		}
		total = n
	}
	return first, last, total, true
}

func fileNameFrom(resp *http.Response) string {
	// a lenient reading of Content-Disposition, as browsers do (RFC 6266/8187, broken real-world headers)
	if n, ok := filename.ParseContentDisposition(resp.Header.Get("Content-Disposition")); ok {
		return filename.Clean(n)
	}
	for _, u := range []*url.URL{resp.Request.URL, firstURL(resp)} {
		if u == nil {
			continue
		}
		if b := filename.FromURLPath(u.EscapedPath()); b != "" {
			n := filename.Clean(b)
			if !strings.Contains(n, ".") {
				ct := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
				if x, ok := preferredExt[ct]; ok {
					n += x
				} else if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 && ct != "application/octet-stream" {
					n += exts[0]
				}
			}
			return n
		}
	}
	return "download"
}

// preferredExt: the usual extension where the system's list starts with an odd one
// (text/plain → .asc on some systems).
var preferredExt = map[string]string{"text/plain": ".txt", "text/html": ".html", "application/json": ".json", "image/jpeg": ".jpg",
	"application/zip": ".zip", "application/pdf": ".pdf", "video/mp4": ".mp4", "audio/mpeg": ".mp3", "text/csv": ".csv", "application/xml": ".xml", "text/xml": ".xml"}

func firstURL(resp *http.Response) *url.URL {
	r := resp.Request
	for r != nil && r.Response != nil {
		r = r.Response.Request
	}
	if r == nil {
		return nil
	}
	return r.URL
}

// sanitizeName makes a name safe to save under on every system (see filename.Clean).
func sanitizeName(n string) string { return filename.Clean(n) }

// ---------------------------------------------------------------- limiter

type Limiter struct {
	mu    sync.Mutex
	rate  int64 // bytes per second, 0 = unlimited
	avail float64
	last  time.Time
}

func (l *Limiter) SetRate(r int64) {
	l.mu.Lock()
	l.rate, l.avail, l.last = r, 0, time.Now()
	l.mu.Unlock()
}

func (l *Limiter) Wait(ctx context.Context, n int) {
	l.mu.Lock()
	if l.rate <= 0 {
		l.mu.Unlock()
		return
	}
	now := time.Now()
	l.avail += now.Sub(l.last).Seconds() * float64(l.rate)
	l.last = now
	if l.avail > float64(l.rate)/4 {
		l.avail = float64(l.rate) / 4
	}
	l.avail -= float64(n)
	var wait time.Duration
	if l.avail < 0 {
		wait = time.Duration(-l.avail / float64(l.rate) * float64(time.Second))
	}
	l.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}
}

func (l *Limiter) Rate() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// Refund returns reserved bandwidth that a read didn't use.
func (l *Limiter) Refund(n int) {
	if n <= 0 {
		return
	}
	l.mu.Lock()
	if l.rate > 0 {
		l.avail += float64(n)
	}
	l.mu.Unlock()
}

// pace waits out the hourly data cap, reserves bandwidth before a read (so limits are
// exact even at the end of a download) and returns how many bytes to read. long=true
// means the read waited a long time for the data cap, so the caller should reconnect
// (servers drop idle connections) rather than read from a stale one.
func (m *Manager) pace(ctx context.Context, d *Download, max int) (n int, long bool) {
	long = m.capWait(ctx)
	g, p, pl := m.limiter.Rate(), d.lim.Rate(), m.politeLim.Rate()
	lim := g
	for _, r := range []int64{p, pl} {
		if r > 0 && (lim == 0 || r < lim) {
			lim = r
		}
	}
	if lim == 0 {
		return max, long
	}
	chunk := int(lim / 8)
	if chunk < 4096 {
		chunk = 4096
	}
	if chunk > max {
		chunk = max
	}
	m.limiter.Wait(ctx, chunk)
	d.lim.Wait(ctx, chunk)
	m.politeLim.Wait(ctx, chunk)
	return chunk, long
}

// paceBlock waits for the speed limits (overall, this download's, polite mode) and the data
// cap to allow n more bytes. A torrent calls it for each block it is about to read, rather than
// before every read, so its other messages are never made to wait for the limit.
func (m *Manager) paceBlock(ctx context.Context, d *Download, n int) {
	m.capWait(ctx)
	m.limiter.Wait(ctx, n)
	d.lim.Wait(ctx, n)
	m.politeLim.Wait(ctx, n)
}

func (m *Manager) unpace(d *Download, reserved, got int) {
	if got > 0 {
		m.countDown(int64(got))
	}
	if got < reserved {
		m.limiter.Refund(reserved - got)
		d.lim.Refund(reserved - got)
		m.politeLim.Refund(reserved - got)
	}
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, bufSize); return &b }}

// ---------------------------------------------------------------- engine

type permanentErr struct{ error }

func (e permanentErr) Unwrap() error { return e.error }

// run downloads d until it completes, fails or ctx is cancelled. When every connection was lost
// (an outage, the computer asleep) it waits and tries the whole download again a few times,
// sooner when the network comes back; time spent offline doesn't use up the tries.
func (m *Manager) run(ctx context.Context, cancel context.CancelFunc, d *Download) {
	err := m.download(ctx, cancel, d)
	for tries := 0; tries < len(wholeRetry) && m.retryWhole(ctx, d, err); {
		wait := wholeRetry[tries]
		if online() {
			tries++
		}
		m.mu.Lock()
		if d.Status != StPaused && d.Status != StQueued {
			d.Status = StConnecting
		}
		d.Error, d.RetryAt = downloadErrText(err, d.URL), time.Now().Add(wait)
		m.dirty = true
		m.mu.Unlock()
		logf("%s: every connection was lost (%v) — trying again in %s", d.FileName, err, wait)
		ok := backoffSleep(ctx, wait)
		m.mu.Lock()
		d.Error, d.RetryAt = "", time.Time{}
		m.mu.Unlock()
		if !ok {
			break
		}
		err = m.download(ctx, cancel, d)
	}
	m.mu.Lock()
	d.running = false
	d.cancel = nil
	atomic.StoreInt32(&d.workers, 0)
	d.speed = 0
	switch {
	case err == nil:
		d.Status = StCompleted
		d.Error = ""
		d.Finished = time.Now()
		logf("%s finished: %d bytes (%s)", d.FileName, d.Size, d.URL)
		d.doneQueue = d.Queue
		if q := m.queueByID(d.Queue); q != nil && q.Sync {
			d.Checked, d.SyncNote = time.Now(), "Up to date"
		} else {
			d.Queue = ""
		}
		if d.Provisional {
			d.announce = true // the Add dialog is still open
		} else {
			m.announceDone(d)
		}
	case ctx.Err() != nil && d.fatalErr == nil:
		if d.Status != StQueued {
			d.Status = StPaused
		}
	case errors.Is(err, errLinkExpired) && d.fatalErr == nil:
		m.linkExpired(d)
	default:
		if d.fatalErr != nil {
			err = d.fatalErr
		}
		d.Status = StError
		d.Error = downloadErrText(err, d.URL)
		logf("%s failed: %v (%s)", d.FileName, err, d.URL)
		d.Challenge = errors.Is(err, errChallenge)
		m.noteFailure(d, err)
		m.notify("error", d.ID)
		if !d.Provisional {
			m.playEvent("failed")
		}
	}
	m.dirty = true
	m.mu.Unlock()
	m.kick()
}

// retryWhole: download ended because its connections were lost, not because of something
// trying again can't fix (a disk error, a reply that won't change, a changed file).
func (m *Manager) retryWhole(ctx context.Context, d *Download, err error) bool {
	var pe permanentErr
	if err == nil || ctx.Err() != nil || d.fatalErr != nil || errors.As(err, &pe) || errors.Is(err, errLinkExpired) {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// (torrents and recordings handle their own trouble; a link that never worked isn't an outage)
	return (d.Kind == "" || d.Kind == "hls" && !d.Live) && (len(d.Segments) > 0 || atomic.LoadInt64(&d.done) > 0 || d.streamStarted())
}

// retryDelay is the wait before a connection's n-th try again (n ≥ 1): 1 s, 2 s, 4 s … up to a
// minute, give or take a fifth so connections that failed together don't all come back at once.
func retryDelay(n int) time.Duration {
	d := retryCap
	if n < 30 {
		d = min(retryBase<<max(n-1, 0), retryCap)
	}
	return time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))
}

// backoffSleep waits dur, or less when the network changes or the computer wakes up (that's
// when a lost connection is most likely to work again). False when ctx ended.
func backoffSleep(ctx context.Context, dur time.Duration) bool {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
	case <-netChanged():
	}
	return true
}

// retryStatus: replies that may well be different on the next try.
func retryStatus(code int) bool {
	switch code {
	case 408, 425, 429, 500, 502, 503, 504:
		return true
	}
	return false
}

// netFailure: the error came from the network itself (no route, a lookup that failed, a dropped
// connection) rather than from a server's answer.
func netFailure(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errClosedEarly)
}

var errClosedEarly = errors.New("connection closed early")

// certFailure: the server's certificate can't be trusted. Trying again won't change that.
func certFailure(err error) bool {
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	return errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci)
}

// announceDone tells the user a download finished (window, tray, sound) and starts the
// virus scan if it's on. Caller holds m.mu.
func (m *Manager) announceDone(d *Download) {
	d.announce = false
	m.notify("complete", d.ID)
	// a queue's downloads (a batch of fifty, say) are told together, when the queue finishes: the window
	// opens no "Download complete" for them, and the tray no balloon each
	m.notices[len(m.notices)-1].Queue = d.doneQueue != ""
	if c := d.Check; c != nil && c.State == "bad" {
		m.notify("checkBad", d.ID, iso(d.FileName)+" is damaged: its "+algoNames[c.Algo]+" checksum doesn't match. Use Redownload to fetch it again.")
		trayNotify("Downloaded file is damaged", d.FileName+": the checksum doesn't match", true)
		m.playEvent("failed")
	} else {
		if d.doneQueue != "" {
			m.qDone[d.doneQueue]++
		} else {
			trayDone(d.FileName)
		}
		m.playEvent("done")
	}
	if m.shouldScan(d) {
		d.ScanState, d.ScanNote = "waiting", "Waiting to scan"
		go m.scanFile(d)
	}
	if d.Private {
		m.forgetPrivate(d) // (the file stays)
	}
}

// doneBalloon is the tray's balloon for downloads that finished together.
func doneBalloon(names []string) (title, text string) {
	if len(names) == 1 {
		return "Download complete", names[0]
	}
	return fmt.Sprintf("%d downloads complete", len(names)), names[0] + " and " + plural(len(names)-1, "other")
}

// queueBalloon is the tray's balloon when a queue finishes: what it downloaded, and what it couldn't.
func queueBalloon(queue string, done, failed int) (title, text string, warning bool) {
	switch {
	case failed == 0:
		text = plural(done, "file") + " downloaded"
	case done == 0:
		text = plural(failed, "file") + " couldn't be downloaded"
	default:
		text = fmt.Sprintf("%s downloaded, %d couldn't be", plural(done, "file"), failed)
	}
	return queue + " finished", text, failed > 0
}

func (m *Manager) setStatus(d *Download, s string) {
	m.mu.Lock()
	if d.Status != StPaused && d.Status != StQueued {
		d.Status = s
	}
	m.dirty = true
	m.mu.Unlock()
}

func (m *Manager) download(ctx context.Context, cancel context.CancelFunc, d *Download) error {
	m.setStatus(d, StConnecting)
	d.fatalErr, d.lastErr = nil, nil
	if d.Kind == "torrent" {
		return m.downloadTorrent(ctx, d)
	}
	if d.Kind == "hls" || (len(d.Segments) == 0 && looksLikeHLS(d.URL)) {
		return m.downloadHLS(ctx, d)
	}

	d.mu.Lock()
	fresh := len(d.Segments) == 0 || !d.Resumable
	var first *Segment
	var pos0, end0 int64 = 0, -1
	if !fresh {
		for _, s := range d.Segments {
			if s.Pos <= s.End {
				first = s
				break
			}
		}
		if first == nil { // already complete on disk
			d.mu.Unlock()
			if err := m.finish(ctx, d); !errors.Is(err, errRepair) {
				return err
			}
			return m.download(ctx, cancel, d)
		}
		pos0, end0 = first.Pos, first.End
	}
	d.mu.Unlock()

	// Mirrors are checked alongside the main link, so they can take connections from the start.
	m.mu.Lock()
	mirrors := append([]string(nil), d.Mirrors...)
	validator := ""
	if !fresh {
		validator = ifRange(d)
	}
	m.hosts.setDefault(m.connsWanted())
	m.mu.Unlock()
	mc := newMirrorCheck(ctx, m, d)
	mc.start(mirrors)

	// Probe — reuses the connection for the first segment.
	pr, err := m.probeFirst(ctx, d, fresh, pos0, end0, validator, len(mirrors) > 0)
	ss := &sourceSet{distrust: d.distrust, hosts: m.hosts, d: d, validator: validator}
	primary := &source{url: d.URL, final: d.FinalURL}
	ss.add(primary)
	via := primary
	if err != nil && ctx.Err() == nil && len(mirrors) > 0 && !errors.Is(err, errChanged) {
		// The main link is down (or its signed address ran out): a mirror can stand in.
		if mpr, ms := mc.standIn(ctx, d, pos0, end0, fresh); mpr != nil {
			var se statusErr
			if errors.As(err, &se) && expiredStatus(se.code) && !fresh {
				primary.dead, primary.deadErr, primary.expired = "link expired", err, true
			} else {
				primary.rest = time.Now().Add(20 * time.Second)
			}
			pr, via, err = mpr, ms, nil
			ss.add(ms)
		}
	}
	if err != nil {
		var se statusErr
		if !fresh && errors.As(err, &se) && expiredStatus(se.code) {
			return permanentErr{errLinkExpired} // it worked before: the link has run out
		}
		return err
	}
	if !fresh {
		// Size and ETag only: some servers report a fresh Last-Modified on every request.
		changed := (pr.size > 0 && pr.size != d.Size) ||
			(via == primary && pr.etag != "" && d.ETag != "" && pr.etag != d.ETag)
		if changed || !pr.resumable || !pr.partial {
			pr.discard()
			if changed {
				return permanentErr{errChanged}
			}
			return permanentErr{errors.New("server no longer supports resuming — use Redownload")}
		}
	}

	if fresh && isHLSMime(pr.mime) { // a video stream behind a plain-looking link
		pr.discard()
		return m.downloadHLS(ctx, d)
	}

	var base *url.URL
	if via == primary {
		base, _ = url.Parse(pr.finalURL)
	}
	links := parseLinks(pr.hdrs, base)
	m.mu.Lock()
	if via == primary {
		d.FinalURL = pr.finalURL
		primary.final = pr.finalURL
	}
	if fresh {
		d.Size = pr.size
		d.Resumable = pr.resumable
		if via == primary {
			d.ETag, d.LastMod, d.LMStrong = pr.etag, pr.lastMod, pr.lmStrong
			ss.validator = ifRange(d) // from now on only this version of the file is accepted
		}
		named := d.FileName == ""
		if named {
			d.FileName = pr.name
		}
		if d.Category == "" {
			d.Category = m.categoryFor(d.FileName)
		}
		if d.Dir == "" {
			d.Dir = m.dirFor(d.Category)
		}
		if named { // the server named it: another download may already use that name
			d.FileName = m.uniqueName(d.Dir, d.FileName, d)
		}
	}
	// mirrors the server lists (Metalink/HTTP), and a checksum it sends
	var more []string
	if via == primary && d.Resumable {
		for _, u := range mirrorsFromLinks(links, d.URL, d.FinalURL) {
			if !slices.Contains(d.Mirrors, u) && len(d.Mirrors) < 10 {
				d.Mirrors = append(d.Mirrors, u)
				more = append(more, u)
			}
		}
	}
	if c := headerChecksum(pr.hdrs); c != nil && (d.Check == nil || d.Check.Want == "") && via == primary {
		d.Check = c
	}
	look := m.settings.Checksums && !d.SumLooked && d.Kind == "" && d.Size >= sumMinSize && (d.Check == nil || d.Check.Want == "" || d.Pieces == nil && hasMetalinkLink(links))
	if look {
		d.sumDone = make(chan struct{})
	}
	d.srcs = ss
	d.Status = StDownloading
	m.dirty = true
	m.mu.Unlock()
	mc.start(more)
	if look {
		go m.lookForChecksum(ctx, d, links, mc, d.sumDone)
	}
	if d.Resumable && d.Size > 0 {
		mc.reference(pr)
		mc.join(ss, d.Size, 2*time.Second) // the ones that answered in time take connections now, the rest as they do
	}

	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		pr.discard()
		return permanentErr{err}
	}
	if fresh { // room for it, a file system that can hold it, a path the system accepts
		if err := checkDiskFor(d.Dir, d.FileName, d.Size); err != nil {
			pr.discard()
			return permanentErr{err}
		}
	}
	d.lim.SetRate(int64(d.LimitKB) * 1024)

	if !d.Resumable || d.Size <= 0 {
		return m.singleStream(ctx, d, pr.body)
	}

	flags := os.O_RDWR | os.O_CREATE
	if fresh {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(d.partPath(), flags, 0o644)
	if err != nil {
		pr.discard()
		return permanentErr{err}
	}
	if fresh {
		if err := preallocate(f, d.Size); err != nil { // reserved up front: no "disk full" half-way
			f.Close()
			pr.discard()
			return permanentErr{fmt.Errorf("disk: %w", err)}
		}
	}

	conns := connsFor(d, m.dlConns(d)) // site rules can lower it (and the traffic profile set it)
	if ss.count() > 1 {
		conns = max(m.dlConns(d), 1) // with mirrors each address keeps to its own site's limit
	}
	if isFTP(d.URL) && conns > 8 {
		conns = 8 // FTP servers usually cap connections per client
	}
	d.mu.Lock()
	d.file = f
	if fresh {
		n := int64(conns)
		if n < 1 {
			n = 1
		}
		if d.Size/n < minInitSeg {
			n = d.Size / minInitSeg
			if n < 1 {
				n = 1
			}
		}
		step := d.Size / n
		d.Segments = nil
		for i := int64(0); i < n; i++ {
			s := &Segment{Start: i * step, End: (i+1)*step - 1}
			if i == n-1 {
				s.End = d.Size - 1
			}
			s.Pos = s.Start
			d.Segments = append(d.Segments, s)
		}
		first = d.Segments[0]
		atomic.StoreInt64(&d.done, 0)
	}
	var sum int64
	for _, s := range d.Segments {
		sum += s.Pos - s.Start
		s.active = false
		s.state = ""
	}
	atomic.StoreInt64(&d.done, sum)
	first.active = true
	first.t0, first.p0 = time.Now(), first.Pos
	d.mu.Unlock()
	pc := m.newPieceCheck(d, f, ss) // (nil without piece checksums)

	ss.mu.Lock()
	useProbe := !via.distrusted || !ss.hasTrusted() // (not from an address that sent damaged pieces)
	var body io.ReadCloser
	if useProbe {
		via.active++ // the first connection is the probe's
		host := connHost(via.addr())
		ss.hosts.add(host, d)
		body = &srcBody{ReadCloser: pr.body, ss: ss, s: via, host: host, t0: time.Now(), left: -1} // (an open-ended reply: never drained)
	}
	ss.mu.Unlock()
	if !useProbe {
		pr.discard()
	}
	m.runWorkers(ctx, cancel, d, f, ss, first, body, conns)
	for seg := pc.settle(ctx); seg != nil; seg = pc.settle(ctx) { // a piece checked after the connections ended was damaged
		m.runWorkers(ctx, cancel, d, f, ss, seg, nil, conns)
	}
	pc.stop()
	d.mu.Lock()
	d.file = nil
	whole := !d.unfinished()
	d.mu.Unlock()
	if whole { // (never on a part-done file: Windows would fill its gaps with zeros)
		if err := finishAllocation(f); err != nil {
			logf("%s: couldn't clear the sparse flag: %v", d.FileName, err)
		}
	}
	f.Close()

	if d.fatalErr != nil {
		return d.fatalErr
	}
	d.mu.Lock()
	complete := !d.unfinished()
	d.mu.Unlock()
	if complete {
		err := m.finish(ctx, d)
		if errors.Is(err, errRepair) && ctx.Err() == nil {
			return m.download(ctx, cancel, d) // fetch the damaged pieces again
		}
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if d.lastErr != nil {
		return d.lastErr
	}
	if !ss.anyUsable() {
		return ss.gone()
	}
	return errors.New("all connections failed")
}

// probeFirst makes a download's first request. Like a connection it waits out network trouble and
// busy servers (backing off, faster when the network comes back), but a fresh download fails at
// once on a reply that won't change: a 404 shouldn't take minutes.
func (m *Manager) probeFirst(ctx context.Context, d *Download, fresh bool, pos, end int64, validator string, mirrors bool) (*probeResult, error) {
	tries := 0
	for attempt := 0; ; attempt++ {
		u := d.URL
		if attempt > 0 && d.FinalURL != "" && attempt%2 == 1 {
			u = d.FinalURL
		}
		if !m.hosts.wait(ctx, connHost(u)) { // the server asked us to come back later (Retry-After)
			return nil, ctx.Err()
		}
		pr, err := probeIf(ctx, d, u, pos, end, validator)
		if err == nil || ctx.Err() != nil {
			return pr, err
		}
		var pe permanentErr
		if errors.As(err, &pe) {
			return nil, err
		}
		if certFailure(err) {
			return nil, permanentErr{err}
		}
		var se statusErr
		if errors.As(err, &se) {
			m.hosts.retryAfter(connHost(u), se.wait)
			switch {
			case !fresh && expiredStatus(se.code):
				if attempt >= 1 { // asked both addresses: the link has run out
					return nil, err
				}
			case !retryStatus(se.code):
				return nil, permanentErr{err}
			}
		}
		// A link that never worked yet: a server that doesn't exist is a typo, not an outage, and
		// one that doesn't answer gets a few tries (as before) rather than many minutes.
		untried := fresh && atomic.LoadInt64(&d.done) == 0
		limit := maxTries
		if untried && online() {
			var de *net.DNSError
			if errors.As(err, &de) && de.IsNotFound {
				return nil, permanentErr{err}
			}
			if se.code == 0 {
				limit = min(maxTries, 4)
			}
		}
		if !netFailure(err) || online() { // offline, waiting isn't trying
			tries++
		}
		if tries >= limit || mirrors && tries >= 3 { // (a mirror can stand in)
			return nil, err
		}
		if !backoffSleep(ctx, retryDelay(attempt+1)) {
			return nil, ctx.Err()
		}
	}
}

// exit says why a connection stopped.
type exit int

const (
	exitDone    exit = iota // no work left for it
	exitStopped             // cancelled, or failed in a way retrying can't fix
	exitGaveUp              // failed too many times in a row
	exitNoSlot              // the servers take no more connections
	exitShrink              // made way for another download from the same server
)

// runWorkers runs a segmented download's connections. Like a browser it starts with a few, adds
// more while that makes it faster (up to conns, and the download's fair share of each server),
// and replaces connections that drop, until the work is done or nothing more can be started.
func (m *Manager) runWorkers(ctx context.Context, cancel context.CancelFunc, d *Download, f *os.File, ss *sourceSet, first *Segment, body io.ReadCloser, conns int) {
	exits := make(chan exit)
	live := 0
	start := func(seg *Segment, body io.ReadCloser) {
		live++
		atomic.AddInt32(&d.workers, 1)
		go func() {
			r := m.worker(ctx, cancel, d, f, seg, body)
			atomic.AddInt32(&d.workers, -1)
			exits <- r
		}()
	}
	// add starts one more connection when there's work for it and a server can take it.
	add := func() bool {
		if ctx.Err() != nil || !ss.canTake(time.Now()) {
			return false
		}
		d.mu.Lock()
		seg := d.nextSegment()
		d.mu.Unlock()
		if seg == nil {
			return false
		}
		start(seg, nil)
		return true
	}

	g := newGrower(conns)
	start(first, body)
	for live < g.target && add() {
		sleepCtx(ctx, 15*time.Millisecond) // slight stagger so servers don't see a burst of handshakes
	}

	tick := time.NewTicker(ctlEvery)
	defer tick.Stop()
	stopped := ctx.Done()
	var (
		lastDone = atomic.LoadInt64(&d.done)
		lastAt   = time.Now()
		gaveUp   bool // a connection gave up and nothing moved since: an outage, so don't replace it
		failed   bool // a connection failed for good: start no more
	)
	for {
		if live == 0 {
			d.mu.Lock()
			more := d.unfinished()
			d.mu.Unlock()
			if failed || gaveUp || ctx.Err() != nil || !more || !ss.anyUsable() {
				return
			}
		}
		select {
		case r := <-exits:
			live--
			switch r {
			case exitGaveUp:
				gaveUp = true
			case exitStopped:
				failed = true
			}
		case <-stopped:
			stopped = nil // the connections notice by themselves
		case now := <-tick.C:
			done := atomic.LoadInt64(&d.done)
			speed := float64(done-lastDone) / max(now.Sub(lastAt).Seconds(), 0.001)
			if done > lastDone {
				gaveUp = false // others are getting through: a lost connection is worth replacing
			}
			lastDone, lastAt = done, now
			// (only measured with all the connections it wants running, and not while a speed
			// limit is what holds it back)
			if live >= g.target && !m.limited(d, speed) {
				g.step(now, speed)
			}
		}
		for !failed && !gaveUp && live < g.target && add() {
			sleepCtx(ctx, 15*time.Millisecond)
		}
	}
}

// grower decides how many connections a download wants: start with a few (up to 8), double them
// while that makes the download at least 10% faster, and stop at the first increase that didn't.
// Every growPause it tries a smaller increase again, as conditions change. Doubling gets a server
// that caps each connection's speed its full set of connections within a couple of seconds.
type grower struct {
	target, max int
	growing     bool
	before      float64 // speed measured before the last increase (-1: none yet)
	again       time.Time
}

func newGrower(conns int) *grower {
	return &grower{target: min(conns, 8), max: conns, growing: true, before: -1}
}

// step takes the speed of the last period and raises the target when that pays.
func (g *grower) step(now time.Time, speed float64) {
	if g.target >= g.max {
		return
	}
	raise := 0
	switch {
	case g.growing && (g.before < 0 || speed >= g.before*1.1):
		raise = max(2, g.target)
	case g.growing: // the last increase didn't pay: hold here for a while
		g.growing, g.again = false, now.Add(growPause)
	case now.After(g.again): // a smaller try now and then; if it doesn't pay, hold again
		g.growing, raise = true, max(2, g.target/4)
	}
	if raise > 0 {
		g.before, g.target = speed, min(g.max, g.target+raise)
	}
}

// limited: a speed limit (the overall one, this download's own or polite mode's) is what holds
// the speed down, so more connections wouldn't help.
func (m *Manager) limited(d *Download, speed float64) bool {
	if r := d.lim.Rate(); r > 0 && speed >= 0.9*float64(r) {
		return true
	}
	g, p := m.limiter.Rate(), m.politeLim.Rate()
	if g == 0 && p == 0 {
		return false
	}
	m.mu.Lock()
	var total float64
	for _, x := range m.list {
		if x.running {
			total += x.speed
		}
	}
	m.mu.Unlock()
	total = max(total, speed)
	return g > 0 && total >= 0.9*float64(g) || p > 0 && total >= 0.9*float64(p)
}

// reconnecting: the download's connections are all between tries (the server or the network
// went away), so the list can say so instead of showing a download that's merely slow.
func (d *Download) reconnecting() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lastErr == nil {
		return false
	}
	waiting := 0
	for _, s := range d.Segments {
		switch {
		case !s.active:
		case s.state == "retry" || s.state == "connecting":
			waiting++
		default:
			return false // one is receiving
		}
	}
	return waiting > 0
}

// unfinished: some byte range is still to be downloaded. Caller holds d.mu.
func (d *Download) unfinished() bool {
	for _, s := range d.Segments {
		if s.Pos <= s.End {
			return true
		}
	}
	return false
}

func (m *Manager) finish(ctx context.Context, d *Download) error {
	if d.sumDone != nil {
		select {
		case <-d.sumDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// (errors here are about the file on disk: trying the download again wouldn't help)
	if err := m.cloudCheck(ctx, d); err != nil { // a share link's own check (cloud_hooks.go)
		return err
	}
	if err := m.verify(ctx, d); err != nil {
		if errors.Is(err, errRepair) || ctx.Err() != nil {
			return err
		}
		return permanentErr{err}
	}
	final := uniquePath(d.path())
	if d.Replace { // sync update: the new version takes the old file's place
		final = d.path()
		if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
			return permanentErr{fmt.Errorf("couldn't replace the old file (is it open?): %v", err)}
		}
	}
	if err := os.Rename(d.partPath(), final); err != nil {
		return permanentErr{err}
	}
	m.mu.Lock()
	d.Replace = false
	d.FileName = filepath.Base(final)
	d.Segments, d.PieceOK = nil, nil
	if d.Size < 0 {
		d.Size = atomic.LoadInt64(&d.done)
	}
	atomic.StoreInt64(&d.done, d.Size)
	src, ref := d.URL, d.Headers["Referer"]
	m.mu.Unlock()
	// came from the internet: SmartScreen, Office's Protected View and Gatekeeper check it as
	// they would a browser download
	markFromInternet(final, src, ref)
	return nil
}

// nextSegment returns an orphaned segment, or splits the active one with the most time left at
// its midpoint (dynamic segmentation). Like a browser it only splits when the new half would
// take more than splitAhead seconds at that segment's speed; while the speed isn't known yet
// (a young segment) each half just has to be at least minSplit. Caller holds d.mu.
func (d *Download) nextSegment() *Segment {
	for _, s := range d.Segments {
		if !s.active && s.Pos <= s.End {
			s.active = true
			s.t0, s.p0 = time.Now(), s.Pos
			return s
		}
	}
	now := time.Now()
	var best *Segment
	var bestETA, bestRate, sumRate float64
	var nRate int
	for _, s := range d.Segments {
		if !s.active || s.rem() < 2*minSplit {
			continue
		}
		eta, rate := s.eta(now)
		if rate > 0 {
			sumRate += rate
			nRate++
		}
		if eta > bestETA {
			best, bestETA, bestRate = s, eta, rate
		}
	}
	if best == nil {
		return nil
	}
	rem := best.rem()
	cut := rem / 2
	// A crawling connection keeps only a small slice; the fresh one takes the rest.
	if bestRate > 0 && nRate > 1 && bestRate*4 < sumRate/float64(nRate) {
		if c := int64(bestRate * 2); c < cut {
			cut = c
		}
		if cut < minSplit {
			cut = minSplit
		}
	}
	if bestRate > 0 && float64(rem-cut) <= bestRate*splitAhead {
		return nil // it finishes soon anyway: a new request wouldn't pay for itself
	}
	mid := best.Pos + cut
	ns := &Segment{Start: mid, End: best.End, Pos: mid, active: true, t0: now, p0: mid}
	best.End = mid - 1
	d.Segments = append(d.Segments, ns)
	return ns
}

// worker downloads segments one after another until none is left, it fails for good, or it makes
// way for another download from the same server. It says why it stopped.
func (m *Manager) worker(ctx context.Context, cancel context.CancelFunc, d *Download, f *os.File, seg *Segment, body io.ReadCloser) exit {
	tries, fails := 0, 0 // failures in a row: those that count towards giving up, and all of them (for the wait)
	for {
		if ctx.Err() != nil {
			if body != nil {
				body.Close()
			}
			d.mu.Lock()
			seg.active = false
			d.mu.Unlock()
			return exitStopped
		}
		d.mu.Lock()
		before := seg.Pos
		d.mu.Unlock()
		err := m.fetch(ctx, d, f, seg, body)
		d.mu.Lock()
		gained := seg.Pos - before
		d.mu.Unlock()
		progressed := err != nil && gained >= 64<<10 // dropped after real progress: a flaky line, not a dead server
		body = nil
		if err == nil {
			tries, fails = 0, 0
			d.mu.Lock()
			seg.state = "done"
			seg.active = false
			d.mu.Unlock()
			if d.srcs != nil && d.srcs.overShare() { // another download from this server waits for its share
				return exitShrink
			}
			d.mu.Lock()
			seg = d.nextSegment()
			d.mu.Unlock()
			if seg == nil {
				return exitDone
			}
			continue
		}
		if ctx.Err() != nil {
			d.mu.Lock()
			seg.active = false
			d.mu.Unlock()
			return exitStopped
		}
		if errors.Is(err, errCapReconnect) {
			continue // the data cap paused us for a while: reconnect, not a failure
		}
		if errors.Is(err, errNoSlot) { // the server takes no more connections: the others carry on
			d.mu.Lock()
			seg.active = false
			seg.state = ""
			d.mu.Unlock()
			return exitNoSlot
		}
		var pe permanentErr
		if errors.As(err, &pe) {
			d.mu.Lock()
			if strings.HasPrefix(err.Error(), "disk:") {
				d.fatalErr = err // (keeps the system error, so its message can be explained)
				cancel()
			}
			seg.active = false
			seg.state = "failed"
			d.lastErr = err
			d.mu.Unlock()
			return exitStopped
		}
		if progressed { // reconnect right away
			tries, fails = 0, 0
			d.mu.Lock()
			d.lastErr = err
			d.mu.Unlock()
			continue
		}
		fails++
		if !netFailure(err) || online() { // while offline, waiting isn't trying
			tries++
		}
		d.mu.Lock()
		d.lastErr = err
		seg.state = "retry"
		if tries >= maxTries {
			seg.active = false
			seg.state = "failed"
			d.mu.Unlock()
			return exitGaveUp
		}
		d.mu.Unlock()
		if !backoffSleep(ctx, retryDelay(fails)) {
			d.mu.Lock()
			seg.active = false
			d.mu.Unlock()
			return exitStopped
		}
	}
}

func (m *Manager) fetch(ctx context.Context, d *Download, f *os.File, seg *Segment, body io.ReadCloser) error {
	if body == nil {
		d.mu.Lock()
		pos, end := seg.Pos, seg.End
		seg.state = "connecting"
		d.mu.Unlock()
		if pos > end {
			return nil
		}
		b, err := m.openRange(ctx, d, pos, end)
		if err != nil {
			return err
		}
		body = b
	}
	defer body.Close()

	d.mu.Lock()
	seg.state = "receiving"
	from := seg.Pos
	d.mu.Unlock()
	if sb, ok := body.(*srcBody); ok { // remember who sent these bytes
		defer func() {
			d.mu.Lock()
			to := seg.Pos
			d.mu.Unlock()
			sb.ss.wrote(sb.s, from, to)
		}()
	}

	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp

	stall := time.AfterFunc(stallLimit, func() { body.Close() })
	defer stall.Stop()

	for {
		stall.Stop()
		want, long := m.pace(ctx, d, len(buf))
		if ctx.Err() != nil {
			// stopped: don't drain what the network already buffered (that would run up
			// the speed limiter's debt for the next download)
			m.unpace(d, want, 0)
			return ctx.Err()
		}
		if long {
			return errCapReconnect
		}
		stall.Reset(stallLimit)
		n, rerr := body.Read(buf[:want])
		m.unpace(d, want, n)
		if n > 0 {
			stall.Reset(stallLimit)
			d.mu.Lock()
			pos, end := seg.Pos, seg.End
			d.mu.Unlock()
			if pos > end {
				m.drainRest(body) // the segment was split meanwhile
				return nil
			}
			w := int64(n)
			if w > end-pos+1 {
				w = end - pos + 1
			}
			if _, err := f.WriteAt(buf[:w], pos); err != nil {
				return permanentErr{fmt.Errorf("disk: %w", err)}
			}
			d.mu.Lock()
			np := pos + w
			if np > seg.End+1 {
				np = seg.End + 1
			}
			if np < pos {
				np = pos
			}
			seg.Pos = np
			fin := seg.Pos > seg.End
			d.mu.Unlock()
			atomic.AddInt64(&d.done, np-pos)
			if !d.pc.wrote(body, pos, np) && !fin { // (piececheck.go)
				return errDropped
			}
			if fin {
				if rerr == nil {
					m.drainRest(body)
				}
				return nil
			}
		}
		if rerr != nil {
			d.mu.Lock()
			fin := seg.Pos > seg.End
			d.mu.Unlock()
			if fin {
				return nil
			}
			if rerr == io.EOF {
				return errClosedEarly
			}
			return rerr
		}
	}
}

// drainRest reads out what's left of a reply that's no longer needed (its segment was split
// while it streamed) when that's little, so the connection goes back to the pool instead of a
// new one being opened. It counts towards the data cap like any other byte.
func (m *Manager) drainRest(body io.ReadCloser) {
	sb, ok := body.(*srcBody)
	if !ok || sb.left <= 0 || sb.left > drainMax {
		return
	}
	t := time.AfterFunc(time.Second, func() { sb.ReadCloser.Close() }) // a slow line isn't worth the wait
	n, _ := io.CopyN(io.Discard, sb.ReadCloser, sb.left)
	t.Stop()
	sb.left -= n
	m.countDown(n)
}

// singleStream handles servers without range support / unknown size.
func (m *Manager) singleStream(ctx context.Context, d *Download, body io.ReadCloser) error {
	defer body.Close()
	f, err := os.OpenFile(d.partPath(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return permanentErr{err}
	}
	atomic.StoreInt64(&d.done, 0)
	d.mu.Lock()
	d.Segments = nil
	d.mu.Unlock()
	atomic.StoreInt32(&d.workers, 1)
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	stall := time.AfterFunc(stallLimit, func() { body.Close() })
	defer stall.Stop()
	var off int64
	for {
		stall.Stop()
		want, _ := m.pace(ctx, d, len(buf)) // can't reconnect: no range support
		if ctx.Err() != nil {
			m.unpace(d, want, 0)
			return ctx.Err()
		}
		stall.Reset(stallLimit)
		n, rerr := body.Read(buf[:want])
		m.unpace(d, want, n)
		if n > 0 {
			stall.Reset(stallLimit)
			if _, err := f.WriteAt(buf[:n], off); err != nil {
				f.Close()
				return permanentErr{err}
			}
			off += int64(n)
			atomic.AddInt64(&d.done, int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%v (server doesn't support resume — download restarts from 0)", rerr)
		}
	}
	f.Close()
	if d.Size > 0 && off != d.Size {
		return fmt.Errorf("incomplete: got %d of %d bytes", off, d.Size)
	}
	m.mu.Lock()
	d.Size = off
	m.mu.Unlock()
	return m.finish(ctx, d)
}

func sleepCtx(ctx context.Context, dur time.Duration) bool {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(c); os.IsNotExist(err) {
			if _, err := os.Stat(c + partSuffix); os.IsNotExist(err) {
				return c
			}
		}
	}
}
