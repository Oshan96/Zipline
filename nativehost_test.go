package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hostPipe drives a native host the way the browser does: framed JSON on its stdin and stdout.
type hostPipe struct {
	t   *testing.T
	in  *io.PipeWriter
	out chan map[string]any
}

func (p *hostPipe) send(v any) {
	p.t.Helper()
	b, _ := json.Marshal(v)
	var n [4]byte
	binary.NativeEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := p.in.Write(append(n[:], b...)); err != nil {
		p.t.Fatal(err)
	}
}

// next waits for a message that match accepts (others are skipped).
func (p *hostPipe) next(d time.Duration, match func(map[string]any) bool) map[string]any {
	p.t.Helper()
	end := time.After(d)
	for {
		select {
		case m, ok := <-p.out:
			if !ok {
				p.t.Fatal("the host closed its output")
			}
			if match(m) {
				return m
			}
		case <-end:
			p.t.Fatal("no message from the host in time")
		}
	}
}

// drain drops what the host has sent so far (an early wake-up, say).
func (p *hostPipe) drain() {
	time.Sleep(100 * time.Millisecond)
	for {
		select {
		case <-p.out:
		default:
			return
		}
	}
}

func byID(id float64) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["id"] == id && m["type"] == nil }
}

func startHost(t *testing.T, h *nativeHost) (*hostPipe, chan error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h.in, h.out = inR, outW
	p := &hostPipe{t: t, in: inW, out: make(chan map[string]any, 64)}
	go func() {
		defer close(p.out)
		for {
			b, err := readNativeMsg(outR)
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(b, &m)
			p.out <- m
		}
	}()
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- h.run(ctx); outW.Close() }()
	t.Cleanup(func() { inW.Close(); cancel() })
	return p, done
}

// The host relays the extension's requests to the app with the secret, starts Zipline when it isn't
// running (not for a plain check-in), passes the app's wake-ups on, and refuses what isn't the extension's.
func TestNativeHostRelay(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: strings.Repeat("ab", 32)}
	dir := t.TempDir()
	var launches int32
	apps := make(chan *httptest.Server, 4)
	t.Cleanup(func() {
		close(apps)
		for a := range apps {
			a.Close()
		}
	})
	h := &nativeHost{dir: dir, origin: "chrome-extension://" + chromeExtIDs[0] + "/", startFor: 5 * time.Second, waitSecs: 2, gen: -1,
		launch: func() error { // "starting Zipline": it listens and writes run.json a moment later
			atomic.AddInt32(&launches, 1)
			go func() {
				time.Sleep(300 * time.Millisecond)
				app := httptest.NewServer(s.routes())
				apps <- app
				writeRunFile(dir, runInfo{Port: app.Listener.Addr().(*net.TCPAddr).Port, PID: os.Getpid(), Version: appVersion, Secret: s.token})
			}()
			return nil
		}}
	p, done := startHost(t, h)
	if hello := p.next(3*time.Second, func(m map[string]any) bool { return m["type"] == "hello" }); hello["host"] != appVersion {
		t.Fatalf("hello: %v", hello)
	}
	// a check-in doesn't start Zipline
	p.send(map[string]any{"id": 1, "path": "ping", "ver": "1.0.0", "minApp": "1.0"})
	if r := p.next(3*time.Second, byID(1)); r["status"] != 0.0 || r["error"] == nil || atomic.LoadInt32(&launches) != 0 {
		t.Fatalf("ping while not running: %v (launches %d)", r, launches)
	}
	// a link does: "starting", then the answer
	p.send(map[string]any{"id": 2, "path": "add", "body": map[string]any{"url": "https://x.example/a.zip", "title": "A"}, "timeout": 5000, "ver": "1.0.0", "minApp": "1.0"})
	if st := p.next(3*time.Second, func(m map[string]any) bool { return m["id"] == 2.0 && m["type"] == "starting" }); st == nil {
		t.Fatal("no starting message")
	}
	r := p.next(8*time.Second, byID(2))
	if r["status"] != 200.0 || r["body"].(map[string]any)["ok"] != true || r["app"] != appVersion || r["minExt"] != minExtVersion || atomic.LoadInt32(&launches) != 1 {
		t.Fatalf("add: %v (launches %d)", r, launches)
	}
	m.mu.Lock()
	n := len(m.prompts)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d prompts", n)
	}
	p.send(map[string]any{"id": 3, "path": "ping", "ver": "1.0.0", "minApp": "1.0"})
	if r := p.next(3*time.Second, byID(3)); r["status"] != 200.0 || r["body"].(map[string]any)["config"] == nil {
		t.Fatalf("ping: %v", r)
	}
	// the window sees the extension through its native link
	st := jsonOf(winReq(s.routes(), "GET", "/api/linkstatus", nil, nil, map[string]string{"X-Token": s.token}))
	if st["state"] != "native" || st["ext"].(map[string]any)["version"] != "1.0.0" {
		t.Fatalf("status: %v", st)
	}
	// not the extension's: the command line, the host's own wait, pairing, anything odd
	for i, path := range []string{"cli", "wait", "pair", "../settings", "ADD", ""} {
		id := 10 + i
		p.send(map[string]any{"id": id, "path": path, "body": map[string]any{}})
		if r := p.next(3*time.Second, byID(float64(id))); r["status"] != 400.0 {
			t.Errorf("path %q: %v", path, r)
		}
	}
	// wake-ups: asked for by the app, and a page waiting for the extension
	sec := s.security()
	for i := 0; ; i++ { // (until the host's long poll is under way)
		sec.mu.Lock()
		n := sec.link.waiting
		sec.mu.Unlock()
		if n > 0 {
			break
		}
		if i > 100 {
			t.Fatal("the host doesn't wait for wake-ups")
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.drain()
	wakeExtension()
	p.next(4*time.Second, func(m map[string]any) bool { return m["type"] == "wake" })
	p.drain()
	m.mu.Lock()
	m.refresh["w1"] = &refreshJob{ID: "w1", Page: "https://example.com/dl", added: time.Now()}
	m.mu.Unlock()
	p.next(4*time.Second, func(m map[string]any) bool { return m["type"] == "wake" })
	m.mu.Lock()
	jobs := m.takeRefreshJobs()
	m.mu.Unlock()
	if len(jobs) != 1 {
		t.Fatalf("the job was taken early: %v", jobs)
	}
	// Zipline quits and the next link starts it again
	first := <-apps
	first.Close()
	p.send(map[string]any{"id": 30, "path": "add", "body": map[string]any{"url": "https://x.example/b.zip"}, "timeout": 5000})
	if r := p.next(8*time.Second, byID(30)); r["status"] != 200.0 || atomic.LoadInt32(&launches) != 2 {
		t.Fatalf("add after Zipline quit: %v (launches %d)", r, launches)
	}
	// the browser closes the pipe: the host ends
	p.in.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the host didn't end")
	}
}

// The host's framing: native byte order, a size limit each way, and a cut-off message is an error.
func TestNativeHostFraming(t *testing.T) {
	var buf bytes.Buffer
	h := &nativeHost{out: &buf}
	if err := h.send(map[string]any{"type": "wake"}); err != nil {
		t.Fatal(err)
	}
	if n := binary.NativeEndian.Uint32(buf.Bytes()[:4]); int(n) != buf.Len()-4 || !bytes.Equal(buf.Bytes()[4:], []byte(`{"type":"wake"}`)) {
		t.Fatalf("frame %q", buf.Bytes())
	}
	buf.Reset()
	if err := h.send(map[string]any{"body": strings.Repeat("x", nmMaxOut)}); err != errTooLarge || buf.Len() != 0 {
		t.Fatalf("over 1 MB: %v, wrote %d bytes", err, buf.Len())
	}
	if _, err := readNativeMsg(bytes.NewReader(nil)); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
	var big [4]byte
	binary.NativeEndian.PutUint32(big[:], nmMaxIn+1)
	if _, err := readNativeMsg(bytes.NewReader(big[:])); err == nil {
		t.Fatal("a message over the limit was read")
	}
	var short [4]byte
	binary.NativeEndian.PutUint32(short[:], 10)
	if _, err := readNativeMsg(bytes.NewReader(append(short[:], "abc"...))); err != io.ErrUnexpectedEOF {
		t.Fatalf("cut off: %v", err)
	}
	for _, c := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"chrome-extension://" + chromeExtIDs[0] + "/"}, true},
		{[]string{"chrome-extension://" + chromeExtIDs[0] + "/", "--parent-window=1234"}, true},
		{[]string{"/home/me/.mozilla/native-messaging-hosts/app.zipline.host.json", firefoxExtID}, true},
		{[]string{"https://example.com/file.zip"}, false},
		{[]string{"--nowindow"}, false},
		{nil, false},
	} {
		if _, ok := nativeHostCaller(c.args); ok != c.ok {
			t.Errorf("%v: host mode %v", c.args, ok)
		}
	}
}

// When Zipline starts while the browser's host is running, the extension is told to check in at once.
func TestNativeHostWakesWhenZiplineStarts(t *testing.T) {
	s := &Server{m: newTestManager(t), token: strings.Repeat("cd", 32)}
	dir := t.TempDir()
	p, _ := startHost(t, &nativeHost{dir: dir, origin: firefoxExtID, startFor: time.Second, waitSecs: 2, gen: -1})
	p.next(3*time.Second, func(m map[string]any) bool { return m["type"] == "hello" })
	app := httptest.NewServer(s.routes())
	defer app.Close()
	writeRunFile(dir, runInfo{Port: app.Listener.Addr().(*net.TCPAddr).Port, PID: os.Getpid(), Secret: s.token})
	p.next(5*time.Second, func(m map[string]any) bool { return m["type"] == "wake" })
}
