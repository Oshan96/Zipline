package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The browser's native-messaging host, the way IDM and FDM link their extensions: Zipline itself,
// started by the browser (Chrome, Edge, Brave, Vivaldi, Opera or Firefox) for the extension the
// host's manifest names, and for no one else (nmregister.go writes the manifests). It speaks the
// native-messaging framing on stdin/stdout (a 4-byte length in this machine's byte order, then JSON;
// at most 1 MB to the browser) and relays the extension's requests to the running app with
// run.json's secret, checking the app's proof on every reply. When Zipline isn't running it starts
// it (in the background, no window) for anything but a plain check-in, and waits up to 10 seconds.
// It also long-polls the app (/api/ext/wait) and passes wake-ups on as {"type":"wake"}, so the
// extension checks in at once instead of at its next 30-second ping.
//
// From the extension: {"id", "path", "body"?, "timeout"?, "start"?, "ver", "minApp"}.
// To it: {"id", "status", "body", "app", "minExt"} (status 0 and "error" when the app can't be
// reached), {"id", "type": "starting"} while Zipline starts, {"type": "hello"} and {"type": "wake"}.

const (
	nmHostName   = "app.zipline.host"
	firefoxExtID = "zipline-catcher@zipline.app"
	nmMaxOut     = 1 << 20  // the most a host may send the browser in one message
	nmMaxIn      = 64 << 20 // the most the browser sends a host
)

// chromeExtIDs: the extension's id in the Chromium browsers, fixed by the "key" in manifest.json
// (Edge gives an unpacked copy with that key the same id). A copy from a store has the store's id:
// add it here.
var chromeExtIDs = []string{"oogcbnoijbeekgjdkkjcbdbnhhjjbalf"}

// nativeHostCaller tells the browser starting Zipline as the host from a person starting it:
// Chrome passes the caller's origin ("chrome-extension://<id>/", and on Windows --parent-window=…),
// Firefox the host manifest's path and the add-on's id.
func nativeHostCaller(args []string) (string, bool) {
	if len(args) >= 1 && strings.HasPrefix(args[0], "chrome-extension://") {
		return args[0], true
	}
	if len(args) >= 2 && args[1] == firefoxExtID {
		return args[1], true
	}
	return "", false
}

type hostReq struct {
	ID      int64           `json:"id"`
	Path    string          `json:"path"`
	Body    json.RawMessage `json:"body,omitempty"`
	Timeout int             `json:"timeout,omitempty"` // ms
	Start   bool            `json:"start,omitempty"`   // a check-in that may start Zipline
	Ver     string          `json:"ver,omitempty"`
	MinApp  string          `json:"minApp,omitempty"`
}

type nativeHost struct {
	in       io.Reader
	out      io.Writer
	dir      string       // the data folder, with run.json
	origin   string       // the extension that started the host
	launch   func() error // starts Zipline in the background (tests: their own app)
	startFor time.Duration
	waitSecs int // one long poll for wake-ups

	outMu    sync.Mutex
	mu       sync.Mutex
	app      *appLink
	launched time.Time
	ver      string // what the extension said last
	minApp   string
	gen      int64 // the last wake-up seen (-1: none yet)
}

var errTooLarge = errors.New("the reply is too large for the browser")

// only the extension's own endpoints, and never the command line's or the host's own
var hostPathRe = regexp.MustCompile(`^[a-z]{1,24}$`)

func runNativeHostMain(origin string) int {
	exe, err := os.Executable()
	if err != nil {
		return 1
	}
	h := &nativeHost{in: os.Stdin, out: os.Stdout, dir: appDataDir(), origin: origin, startFor: 10 * time.Second, waitSecs: 25, gen: -1,
		launch: func() error { return startFromHost(exec.Command(exe, "--nowindow")) }}
	if h.run(context.Background()) != nil {
		return 1
	}
	return 0
}

// run reads the extension's messages until the browser closes the pipe.
func (h *nativeHost) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	h.send(map[string]any{"type": "hello", "host": appVersion})
	go h.wakeLoop(ctx)
	for {
		b, err := readNativeMsg(h.in)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		var req hostReq
		if json.Unmarshal(b, &req) != nil || req.ID == 0 {
			continue
		}
		go h.handle(ctx, req)
	}
}

func readNativeMsg(r io.Reader) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.NativeEndian.Uint32(n[:])
	if size > nmMaxIn {
		return nil, fmt.Errorf("a message of %d bytes is too large", size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, io.ErrUnexpectedEOF
	}
	return b, nil
}

// send writes one message to the browser (whole, never interleaved with another).
func (h *nativeHost) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > nmMaxOut {
		return errTooLarge
	}
	msg := make([]byte, 4+len(b))
	binary.NativeEndian.PutUint32(msg, uint32(len(b)))
	copy(msg[4:], b)
	h.outMu.Lock()
	defer h.outMu.Unlock()
	_, err = h.out.Write(msg)
	return err
}

// handle relays one request to the app and its reply back.
func (h *nativeHost) handle(ctx context.Context, req hostReq) {
	reply := map[string]any{"id": req.ID}
	if !hostPathRe.MatchString(req.Path) || req.Path == "cli" || req.Path == "wait" || req.Path == "pair" {
		reply["status"], reply["error"] = 400, "not an extension request"
		h.send(reply)
		return
	}
	h.mu.Lock()
	h.ver, h.minApp = clip(req.Ver, 20), clip(req.MinApp, 20)
	h.mu.Unlock()
	start := req.Start || req.Path != "ping"
	a, err := h.appFor(req.ID, start)
	if err != nil {
		reply["status"], reply["error"] = 0, err.Error()
		h.send(reply)
		return
	}
	method, body := "GET", []byte(nil)
	if len(req.Body) > 0 && string(req.Body) != "null" {
		method, body = "POST", req.Body
	}
	timeout := time.Duration(req.Timeout) * time.Millisecond
	if timeout < time.Second || timeout > 2*time.Minute {
		timeout = time.Minute
	}
	res, err := a.doCtx(ctx, method, "api/ext/"+req.Path, body, h.headers(), timeout, true)
	if err != nil && err != errNotZipline && ctx.Err() == nil {
		// Zipline quit since the last request (its connection is gone, run.json too): find it, or start
		// it, and try once more. One still there that failed to answer isn't asked twice.
		h.dropApp(a)
		if _, ferr := findApp(h.dir); ferr != nil {
			if a, err = h.appFor(req.ID, start); err == nil {
				res, err = a.doCtx(ctx, method, "api/ext/"+req.Path, body, h.headers(), timeout, true)
			}
		}
	}
	if err != nil {
		h.dropApp(a)
		reply["status"], reply["error"] = 0, err.Error()
		h.send(reply)
		return
	}
	data, _ := io.ReadAll(io.LimitReader(res.Body, nmMaxOut+1))
	res.Body.Close()
	reply["status"], reply["app"], reply["minExt"] = res.StatusCode, res.Header.Get("X-Zipline-Version"), res.Header.Get("X-Zipline-Min-Ext")
	if json.Valid(data) {
		reply["body"] = json.RawMessage(data)
	}
	if h.send(reply) == errTooLarge {
		h.send(map[string]any{"id": req.ID, "status": 0, "error": errTooLarge.Error()})
	}
}

// headers say the request comes through the host, for which extension and version.
func (h *nativeHost) headers() http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	hdr := http.Header{}
	hdr.Set("X-Zipline-Via", "native")
	hdr.Set("X-Zipline-Ext-Origin", h.origin)
	if h.ver != "" {
		hdr.Set("X-Zipline-Ext-Version", h.ver)
	}
	if h.minApp != "" {
		hdr.Set("X-Zipline-Min-App", h.minApp)
	}
	return hdr
}

// appFor finds the running app, or starts it when start is set (requests wait for the same start).
func (h *nativeHost) appFor(id int64, start bool) (*appLink, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.app != nil {
		return h.app, nil
	}
	if a, err := findApp(h.dir); err == nil {
		h.app = a
		return a, nil
	} else if !start || h.launch == nil {
		return nil, err
	}
	h.send(map[string]any{"id": id, "type": "starting"})
	if time.Since(h.launched) > h.startFor {
		if err := h.launch(); err != nil {
			return nil, fmt.Errorf("couldn't start Zipline: %v", err)
		}
		h.launched = time.Now()
	}
	a, err := waitApp(h.dir, h.startFor)
	if err != nil { // (still starting, perhaps: the next request waits for it rather than starting another)
		return nil, errors.New("Zipline didn't start in time")
	}
	h.app, h.launched = a, time.Time{}
	return a, nil
}

func (h *nativeHost) dropApp(a *appLink) {
	h.mu.Lock()
	if h.app == a {
		h.app = nil
	}
	h.mu.Unlock()
}

// wakeLoop waits on the app for something the extension should come for, and tells it.
func (h *nativeHost) wakeLoop(ctx context.Context) {
	pause := func(d time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
	for ctx.Err() == nil {
		h.mu.Lock()
		a := h.app
		h.mu.Unlock()
		if a == nil {
			found, err := findApp(h.dir)
			if err != nil {
				pause(2 * time.Second)
				continue
			}
			h.mu.Lock()
			if h.app == nil {
				h.app = found
			}
			a = h.app
			h.mu.Unlock()
			h.send(map[string]any{"type": "wake"}) // Zipline is (back) up: the extension checks in now
		}
		res, err := a.doCtx(ctx, "GET", fmt.Sprintf("api/ext/wait?t=%d&gen=%d", h.waitSecs, h.gen), nil, h.headers(), time.Duration(h.waitSecs+10)*time.Second, true)
		if err != nil {
			h.dropApp(a)
			pause(time.Second)
			continue
		}
		var out struct {
			Wake bool  `json:"wake"`
			Gen  int64 `json:"gen"`
		}
		json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&out)
		res.Body.Close()
		if res.StatusCode != 200 {
			pause(2 * time.Second)
			continue
		}
		h.gen = out.Gen // (a wake-up between two polls isn't lost)
		if out.Wake {
			h.send(map[string]any{"type": "wake"})
		}
	}
}
