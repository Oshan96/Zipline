package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// extReq is a browser extension's request over HTTP: its origin, and when pair is set, signed with its key.
func extReq(h http.Handler, method, path string, body any, pair *extPair, nonce string, at time.Time) *httptest.ResponseRecorder {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:17890"+path, bytes.NewReader(b))
	req.Header.Set("Origin", "chrome-extension://"+chromeExtIDs[0])
	req.Header.Set("X-Zipline-Ext-Version", "1.0.0")
	req.Header.Set("X-Zipline-Min-App", "1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if pair != nil {
		ts := strconv.FormatInt(at.UnixMilli(), 10)
		req.Header.Set("X-Zipline-Pair", pair.ID)
		req.Header.Set("X-Zipline-Time", ts)
		req.Header.Set("X-Zipline-Nonce", nonce)
		req.Header.Set("X-Zipline-Sig", extSig(pair.Key, method, path, ts, nonce, b))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func jsonOf(rec *httptest.ResponseRecorder) map[string]any {
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return out
}

// Without a key or the secret the extension's endpoints are refused; the ping says only the version and
// that pairing is needed.
func TestExtRefusesUnpairedCallers(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: "t"}
	h := s.routes()
	for _, p := range []string{"add", "check", "links", "scan", "grab", "refresh", "show", "wait", "cli"} {
		rec := extReq(h, "POST", "/api/ext/"+p, map[string]any{"url": "https://x.example/a.zip"}, nil, "", time.Now())
		if rec.Code != 401 || jsonOf(rec)["pair"] != true {
			t.Errorf("ext/%s unpaired: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	m.mu.Lock()
	n := len(m.prompts)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d prompts from unpaired calls", n)
	}
	rec := extReq(h, "GET", "/api/ext/ping", nil, nil, "", time.Now())
	out := jsonOf(rec)
	var keys []string
	for k := range out {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if rec.Code != 200 || strings.Join(keys, ",") != "app,minExt,pair,version" || out["version"] != appVersion || out["minExt"] != minExtVersion || out["pair"] != true {
		t.Fatalf("read-only ping: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Zipline-Version") != appVersion || rec.Header().Get("X-Zipline-Min-Ext") != minExtVersion || rec.Header().Get("Access-Control-Expose-Headers") == "" {
		t.Fatalf("handshake headers: %v", rec.Header())
	}
	m.mu.Lock()
	seen := m.extSeen
	m.mu.Unlock()
	if !seen.IsZero() {
		t.Fatal("an unpaired ping counted as the extension checking in")
	}
	// a web page is turned away before anything else
	req := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/ext/ping", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("ping from a web page: %d", rec.Code)
	}
	// an older extension (no version) shows up as needing an update
	req = httptest.NewRequest("GET", "http://127.0.0.1:17890/api/ext/ping", nil)
	req.Header.Set("Origin", "chrome-extension://abc")
	h.ServeHTTP(httptest.NewRecorder(), req)
	st := jsonOf(winReq(h, "GET", "/api/linkstatus", nil, nil, map[string]string{"X-Token": "t"}))
	if st["state"] != "old" || st["extOld"] != true {
		t.Fatalf("status after an old extension: %v", st)
	}
}

// Pairing: the window asks, the extension gets its key once, and then every request is signed, fresh and
// answered with the app's proof. Forgetting the browser closes the door again.
func TestExtPairing(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: "t"}
	h := s.routes()
	ask := jsonOf(extReq(h, "POST", "/api/ext/pair", map[string]any{"version": "1.0.0", "browser": "Google Chrome"}, nil, "", time.Now()))
	id, _ := ask["id"].(string)
	if ask["state"] != "pending" || len(id) < 32 {
		t.Fatalf("pair: %v", ask)
	}
	if again := jsonOf(extReq(h, "POST", "/api/ext/pair", map[string]any{"version": "1.0.0"}, nil, "", time.Now())); again["state"] != "busy" || again["id"] != nil {
		t.Fatalf("a second request while one waits: %v", again)
	}
	m.mu.Lock()
	var p Prompt
	if len(m.prompts) == 1 {
		p = m.prompts[0]
	}
	m.mu.Unlock()
	if p.Kind != "pair" || p.JobID != id || p.URL != "chrome-extension://"+chromeExtIDs[0] || p.Title != "Google Chrome" || p.Name != "1.0.0" {
		t.Fatalf("prompt %+v", p)
	}
	if st := jsonOf(extReq(h, "POST", "/api/ext/pair", map[string]any{"id": id}, nil, "", time.Now())); st["state"] != "pending" || st["key"] != nil {
		t.Fatalf("poll before the answer: %v", st)
	}
	// only the window answers
	if rec := winReq(h, "POST", "/api/pairing", map[string]any{"id": id, "allow": true}, nil, map[string]string{"X-Token": "t"}); rec.Code != 403 {
		t.Fatalf("answered with the secret: %d", rec.Code)
	}
	c := signIn(t, s, h)
	if rec := winReq(h, "POST", "/api/pairing", map[string]any{"id": id, "allow": true}, c, nil); rec.Code != 200 {
		t.Fatalf("allow: %d %s", rec.Code, rec.Body.String())
	}
	// another origin can't collect it
	req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/ext/pair", strings.NewReader(`{"id":"`+id+`"}`))
	req.Header.Set("Origin", "chrome-extension://someoneelse")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if jsonOf(rec)["key"] != nil {
		t.Fatal("another extension got the key")
	}
	got := jsonOf(extReq(h, "POST", "/api/ext/pair", map[string]any{"id": id}, nil, "", time.Now()))
	key, _ := got["key"].(string)
	pid, _ := got["pair"].(string)
	if got["state"] != "allowed" || len(key) != 64 || pid == "" {
		t.Fatalf("poll after allow: %v", got)
	}
	if again := jsonOf(extReq(h, "POST", "/api/ext/pair", map[string]any{"id": id}, nil, "", time.Now())); again["key"] != nil {
		t.Fatal("the key was handed out twice")
	}
	pair := &extPair{ID: pid, Key: key}

	// signed requests
	rec = extReq(h, "POST", "/api/ext/add", map[string]any{"url": "https://x.example/a.zip"}, pair, "nonce-0000000001", time.Now())
	if rec.Code != 200 || jsonOf(rec)["ok"] != true || rec.Header().Get("X-Zipline-Proof") != linkProof(key, "nonce-0000000001") {
		t.Fatalf("signed add: %d %s proof %q", rec.Code, rec.Body.String(), rec.Header().Get("X-Zipline-Proof"))
	}
	if rec := extReq(h, "POST", "/api/ext/add", map[string]any{"url": "https://x.example/a.zip"}, pair, "nonce-0000000001", time.Now()); rec.Code != 401 {
		t.Fatalf("replayed nonce: %d", rec.Code)
	}
	if rec := extReq(h, "POST", "/api/ext/add", map[string]any{"url": "https://x.example/a.zip"}, pair, "nonce-0000000002", time.Now().Add(-2*time.Minute)); rec.Code != 401 {
		t.Fatalf("stale time: %d", rec.Code)
	}
	if rec := extReq(h, "POST", "/api/ext/add", map[string]any{"url": "https://x.example/a.zip"}, &extPair{ID: pid, Key: strings.Repeat("0", 64)}, "nonce-0000000003", time.Now()); rec.Code != 401 || rec.Header().Get("X-Zipline-Proof") != "" {
		t.Fatalf("wrong key: %d", rec.Code)
	}
	// the body is signed: a changed body doesn't pass
	b, _ := json.Marshal(map[string]any{"url": "https://x.example/a.zip"})
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	req = httptest.NewRequest("POST", "http://127.0.0.1:17890/api/ext/add", strings.NewReader(`{"url":"https://evil.example/b.exe"}`))
	req.Header.Set("Origin", "chrome-extension://"+chromeExtIDs[0])
	for k, v := range map[string]string{"X-Zipline-Pair": pid, "X-Zipline-Time": ts, "X-Zipline-Nonce": "nonce-0000000004", "X-Zipline-Sig": extSig(key, "POST", "/api/ext/add", ts, "nonce-0000000004", b)} {
		req.Header.Set(k, v)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("changed body: %d", rec.Code)
	}
	ping := jsonOf(extReq(h, "GET", "/api/ext/ping", nil, pair, "nonce-0000000005", time.Now()))
	if ping["config"] == nil || ping["ok"] != true {
		t.Fatalf("paired ping: %v", ping)
	}
	// the command line's endpoint stays closed to an extension, paired or not
	if rec := extReq(h, "POST", "/api/ext/cli", map[string]any{"quit": true}, pair, "nonce-0000000006", time.Now()); rec.Code != 403 {
		t.Fatalf("cli from a paired extension: %d", rec.Code)
	}
	st := jsonOf(winReq(h, "GET", "/api/linkstatus", nil, c, nil))
	if st["state"] != "http" || st["ext"].(map[string]any)["version"] != "1.0.0" || len(st["pairs"].([]any)) != 1 {
		t.Fatalf("status: %v", st)
	}

	// it's kept for next time, for this user only, with the key protected like saved passwords
	fi, err := os.Stat(s.pairFile())
	if err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("extpair.json: %v %v", fi, err)
	}
	h2 := (&Server{m: m, token: "t2"}).routes()
	if rec := extReq(h2, "GET", "/api/ext/ping", nil, pair, "nonce-0000000007", time.Now()); rec.Code != 200 {
		t.Fatalf("after a restart: %d", rec.Code)
	}

	// forgotten in Options → Browser
	if rec := winReq(h, "POST", "/api/pairing", map[string]any{"remove": pid}, c, nil); rec.Code != 200 {
		t.Fatalf("remove: %d", rec.Code)
	}
	if rec := extReq(h, "GET", "/api/ext/ping", nil, pair, "nonce-0000000008", time.Now()); rec.Code != 401 {
		t.Fatalf("a forgotten browser: %d", rec.Code)
	}

	// Deny: the extension hears so, and new requests are turned down for a while
	ask = jsonOf(extReq(h, "POST", "/api/ext/pair", map[string]any{"version": "1.0.0"}, nil, "", time.Now()))
	id, _ = ask["id"].(string)
	winReq(h, "POST", "/api/pairing", map[string]any{"id": id, "allow": false}, c, nil)
	if st := jsonOf(extReq(h, "POST", "/api/ext/pair", map[string]any{"id": id}, nil, "", time.Now())); st["state"] != "denied" || st["key"] != nil {
		t.Fatalf("after deny: %v", st)
	}
	if st := jsonOf(extReq(h, "POST", "/api/ext/pair", map[string]any{"version": "1.0.0"}, nil, "", time.Now())); st["state"] != "denied" {
		t.Fatalf("asking again at once: %v", st)
	}
	if rec := winReq(h, "POST", "/api/pairing", map[string]any{"id": id, "allow": true}, c, nil); jsonOf(rec)["error"] == nil {
		t.Fatal("an answered request was allowed afterwards")
	}
}

// The native host's way in (the secret, X-Zipline-Via: native) and the version handshake in Options → Browser.
func TestExtNativeAndHandshake(t *testing.T) {
	m := newTestManager(t)
	s := &Server{m: m, token: "t"}
	h := s.routes()
	native := map[string]string{"X-Token": "t", "X-Zipline-Via": "native", "X-Zipline-Ext-Version": "0.9.9", "X-Zipline-Min-App": "9.0", "X-Zipline-Ext-Origin": "chrome-extension://" + chromeExtIDs[0] + "/", "X-Zipline-Nonce": "n1"}
	rec := winReq(h, "GET", "/api/ext/ping", nil, nil, native)
	if rec.Code != 200 || jsonOf(rec)["config"] == nil || rec.Header().Get("X-Zipline-Proof") != linkProof("t", "n1") {
		t.Fatalf("native ping: %d %s", rec.Code, rec.Body.String())
	}
	st := jsonOf(winReq(h, "GET", "/api/linkstatus", nil, nil, map[string]string{"X-Token": "t"}))
	ext := st["ext"].(map[string]any)
	if st["state"] != "native" || ext["version"] != "0.9.9" || st["extOld"] != true || st["appOld"] != true || ext["origin"] != "chrome-extension://"+chromeExtIDs[0]+"/" {
		t.Fatalf("status: %v", st)
	}
	// the command line (the secret without the host's mark) isn't the extension
	s2 := &Server{m: m, token: "t"}
	winReq(s2.routes(), "GET", "/api/ext/ping", nil, nil, map[string]string{"X-Token": "t"})
	if st := jsonOf(winReq(s2.routes(), "GET", "/api/linkstatus", nil, nil, map[string]string{"X-Token": "t"})); st["state"] != "none" {
		t.Fatalf("the command line counted as the extension: %v", st)
	}
	if rec := winReq(h, "POST", "/api/ext/cli", map[string]any{}, nil, native); rec.Code != 403 {
		t.Fatalf("cli through the native host: %d", rec.Code)
	}
	for _, c := range []struct {
		a, b string
		less bool
	}{{"1.6.0", "1.7.0", true}, {"1.7", "1.7.0", false}, {"1.7.0", "1.7", false}, {"1.10", "1.9.9", false}, {"1.6.2", "1.10", true}, {"", "1.0", true}} {
		if versionLess(c.a, c.b) != c.less {
			t.Errorf("versionLess(%q, %q) = %v", c.a, c.b, !c.less)
		}
	}
}

// The pinned extension id is the one the manifests' key gives, and both manifests ask for native messaging.
func TestExtensionIDAndManifests(t *testing.T) {
	var cm struct {
		Version     string   `json:"version"`
		Key         string   `json:"key"`
		Permissions []string `json:"permissions"`
	}
	b, err := os.ReadFile(repoPath("extension/manifest.json"))
	if err != nil || json.Unmarshal(b, &cm) != nil {
		t.Fatalf("manifest.json: %v", err)
	}
	der, err := base64.StdEncoding.DecodeString(cm.Key)
	if err != nil || len(der) < 270 {
		t.Fatalf("key: %v (%d bytes)", err, len(der))
	}
	sum := sha256.Sum256(der)
	id := []byte{}
	for _, c := range sum[:16] {
		id = append(id, 'a'+c>>4, 'a'+c&15)
	}
	if string(id) != chromeExtIDs[0] || !strings.Contains(extLinkJS, `"`+chromeExtIDs[0]+`"`) {
		t.Fatalf("the key gives the id %s, the app knows %v", id, chromeExtIDs)
	}
	var fm struct {
		Version     string   `json:"version"`
		Permissions []string `json:"permissions"`
		Background  struct {
			Scripts []string `json:"scripts"`
		} `json:"background"`
		BSS struct {
			Gecko struct {
				ID string `json:"id"`
			} `json:"gecko"`
		} `json:"browser_specific_settings"`
	}
	b, _ = os.ReadFile(repoPath("extension/manifest.firefox.json"))
	if err := json.Unmarshal(b, &fm); err != nil {
		t.Fatal(err)
	}
	has := func(l []string, s string) bool {
		for _, x := range l {
			if x == s {
				return true
			}
		}
		return false
	}
	if !has(cm.Permissions, "nativeMessaging") || !has(fm.Permissions, "nativeMessaging") {
		t.Error("a manifest doesn't ask for nativeMessaging")
	}
	if fm.BSS.Gecko.ID != firefoxExtID || len(fm.Background.Scripts) == 0 || fm.Background.Scripts[0] != "link.js" {
		t.Errorf("Firefox manifest: id %q, scripts %v", fm.BSS.Gecko.ID, fm.Background.Scripts)
	}
	if cm.Version != fm.Version || versionLess(cm.Version, minExtVersion) {
		t.Errorf("extension version %s / %s, the app needs %s", cm.Version, fm.Version, minExtVersion)
	}
	bg, _ := os.ReadFile(repoPath("extension/background.js"))
	if !strings.Contains(string(bg), `importScripts("link.js")`) || strings.Contains(string(bg), "fetch(APP") {
		t.Error("background.js should reach the app only through link.js")
	}
}

// The window's new words are in the catalogue.
func TestLinkMessages(t *testing.T) {
	for name, src := range map[string]string{"extlink.js": extLinkJS, "locked.html": lockedHTML} {
		for _, m := range messageUse.FindAllStringSubmatch(src, -1) {
			if id := m[1]; !strings.HasSuffix(id, ".") && !strings.Contains(i18nJS, `"`+id+`":`) {
				t.Errorf("%s asks for %q, which the catalogue doesn't have", name, id)
			}
		}
	}
	for _, st := range []string{"native", "http", "none", "unpaired", "old", "extOld", "appOld"} { // (extlink.js builds these ids)
		if !strings.Contains(i18nJS, `"lnk.`+st+`":`) {
			t.Errorf("lnk.%s is missing", st)
		}
	}
}
