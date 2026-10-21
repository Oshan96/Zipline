package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The runtime file: how programs on this computer find the running Zipline and know it's really
// Zipline. run.json in the data folder (which only this user can read) holds the port, the process,
// the version and a secret made at each start. The second launch from the command line, the
// browser's native-messaging host and `zipline --login-url` send the secret as X-Token. They also
// send a nonce, and the app answers with an HMAC of it under the secret: a program that took the
// port after Zipline stopped can't, so nothing (cookies, links) is ever handed to it.

type runInfo struct {
	Port    int    `json:"port"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
	Secret  string `json:"secret"`
}

var (
	errNotRunning = errors.New("Zipline isn't running")
	errNotZipline = errors.New("another program is using Zipline's port")
)

// appDataDir is the per-user folder with the settings, the list, the lock and run.json.
func appDataDir() string {
	cfg, _ := os.UserConfigDir()
	if cfg == "" {
		cfg, _ = os.UserHomeDir()
	}
	return filepath.Join(cfg, "Zipline")
}

func runFilePath(dir string) string { return filepath.Join(dir, "run.json") }

func writeRunFile(dir string, ri runInfo) error {
	b, _ := json.MarshalIndent(ri, "", " ")
	p := runFilePath(dir)
	tmp := p + ".tmp"
	os.Remove(tmp) // (a leftover keeps its old permissions through WriteFile)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func readRunFile(dir string) (runInfo, error) {
	var ri runInfo
	b, err := os.ReadFile(runFilePath(dir))
	if err != nil {
		return ri, err
	}
	err = json.Unmarshal(b, &ri)
	return ri, err
}

// removeRunFile deletes run.json when it's still this process's (a newer Zipline may have started).
func removeRunFile(dir string, pid int) {
	if ri, err := readRunFile(dir); err == nil && ri.PID == pid {
		os.Remove(runFilePath(dir))
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// linkProof is the app's answer to a nonce: proof that it holds key (the run secret or a pairing key).
func linkProof(key, nonce string) string {
	mac := hmac.New(sha256.New, []byte(key))
	io.WriteString(mac, "zipline-proof\n"+nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

var errOlderRunning = errors.New("an older Zipline is still running. Quit it (Tasks → Exit Zipline) and start Zipline again")

// listenApp listens where the window and the extension expect Zipline. When another program has
// that port, any free one does: run.json says which (the native host and the command line read it).
// An older Zipline there (it doesn't take the lock) would share the data folder, so that's refused.
func listenApp() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", appPort))
	if err != nil {
		if olderZiplineOn(appPort) {
			return nil, 0, errOlderRunning
		}
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return nil, 0, err
		}
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// olderZiplineOn: a Zipline from before the lock and the handshake answers the ping on port.
func olderZiplineOn(port int) bool {
	c := &http.Client{Transport: linkTransport, Timeout: 2 * time.Second}
	res, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ext/ping", port))
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var out struct {
		App string `json:"app"`
	}
	json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out)
	return out.App == "zipline" && res.Header.Get("X-Zipline-Version") == ""
}

// appLink talks to the running Zipline as a program of this user (the secret).
type appLink struct {
	base   string // http://127.0.0.1:port/
	secret string
	hc     *http.Client
}

// loopback requests never go through a proxy
var linkTransport = &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute}

// findApp reads run.json and checks that the program on its port is this user's Zipline.
func findApp(dir string) (*appLink, error) {
	ri, err := readRunFile(dir)
	if err != nil || ri.Port <= 0 || ri.Secret == "" {
		return nil, errNotRunning
	}
	a := &appLink{base: fmt.Sprintf("http://127.0.0.1:%d/", ri.Port), secret: ri.Secret, hc: &http.Client{Transport: linkTransport}}
	if err := a.verify(); err != nil {
		return nil, err
	}
	return a, nil
}

// waitApp waits up to d for Zipline to answer (it may be starting).
func waitApp(dir string, d time.Duration) (*appLink, error) {
	end := time.Now().Add(d)
	for {
		a, err := findApp(dir)
		if err == nil || time.Now().After(end) {
			return a, err
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// verify asks the read-only ping with a nonce (the secret isn't sent) and checks the proof.
func (a *appLink) verify() error {
	res, err := a.do("GET", "api/ext/ping", nil, nil, 3*time.Second, false)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// do sends a request; withSecret adds X-Token. The reply only counts with the right proof.
func (a *appLink) do(method, path string, body []byte, hdr http.Header, timeout time.Duration, withSecret bool) (*http.Response, error) {
	return a.doCtx(context.Background(), method, path, body, hdr, timeout, withSecret)
}

func (a *appLink) doCtx(ctx context.Context, method, path string, body []byte, hdr http.Header, timeout time.Duration, withSecret bool) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	nonce := randHex(16)
	req.Header.Set("X-Zipline-Nonce", nonce)
	if withSecret {
		req.Header.Set("X-Token", a.secret)
	}
	c := *a.hc
	c.Timeout = timeout
	res, err := c.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" { // nothing listens there: it isn't running (any more)
			return nil, errNotRunning
		}
		return nil, err
	}
	if !hmac.Equal([]byte(res.Header.Get("X-Zipline-Proof")), []byte(linkProof(a.secret, nonce))) {
		res.Body.Close()
		return nil, errNotZipline
	}
	return res, nil
}

// post sends JSON with the secret and reads a JSON reply into out (when not nil).
func (a *appLink) post(path string, in, out any) error {
	b, _ := json.Marshal(in)
	res, err := a.do("POST", path, b, nil, 60*time.Second, true)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s: %s", res.Status, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out)
	}
	return nil
}

// specialMode runs the modes that never become the app: the browser's native-messaging host,
// --unregister and --login-url. handled is false for everything else.
func specialMode(args []string) (code int, handled bool) {
	if origin, ok := nativeHostCaller(args); ok {
		return runNativeHostMain(origin), true
	}
	if len(args) == 1 && args[0] == "--unregister" { // the Windows uninstaller: undo what this copy registered
		unregisterCopy()
		return 0, true
	}
	if len(args) == 1 && args[0] == "--login-url" {
		a, err := findApp(appDataDir())
		if err != nil {
			cliMessage(cliPrefix(err.Error()), true)
			return 1, true
		}
		var res struct {
			URL string `json:"url"`
		}
		if err := a.post("api/login/new", map[string]any{}, &res); err != nil || res.URL == "" {
			cliMessage(fmt.Sprintf("Zipline: couldn't get an address (%v)", err), true)
			return 1, true
		}
		cliPrint(res.URL)
		return 0, true
	}
	return 0, false
}
