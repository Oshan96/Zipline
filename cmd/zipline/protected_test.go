package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestProtectedHosts(t *testing.T) {
	yes := []string{
		"https://www.youtube.com/watch?v=abc", "https://youtu.be/abc", "https://m.youtube.com/", "HTTPS://WWW.YOUTUBE.COM/x",
		"https://www.youtube-nocookie.com/embed/abc", "https://rr3---sn-x.googlevideo.com/videoplayback?id=1",
		"https://www.netflix.com/watch/1", "https://ipv4-c001.nflxvideo.net/range/0-1", "https://www.disneyplus.com/video/1",
		"https://www.primevideo.com/detail/1", "https://play.max.com/x", "https://www.hulu.com/watch/1", "https://open.spotify.com/track/1",
		"https://tv.apple.com/us/show/x", "https://www.crunchyroll.com/watch/1", "https://www.youtube.com./watch",
		"https://user:pw@www.youtube.com/watch", "youtube.com/watch?v=abc", "www.youtu.be/abc", "ftp://youtube.com/x.mp4",
	}
	for _, u := range yes {
		if !protectedURL(u) {
			t.Errorf("%s should be protected", u)
		}
	}
	no := []string{
		"https://example.com/youtube.com/video.mp4", "https://notyoutube.com/a.mp4", "https://youtube.com.example.org/a.mp4",
		"https://www.example.com/?next=https://www.youtube.com/watch", "https://youtube.com@example.com/a.mp4", "https://apple.com/a.zip",
		"https://maxx.com/a.zip", "https://cdn.example.com/netflix-guide.pdf", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=youtube.com",
		"", "not a link",
	}
	for _, u := range no {
		if protectedURL(u) {
			t.Errorf("%s should not be protected", u)
		}
	}
}

// Streams from the services' CDN hosts (not on the list) are caught through the page they came from;
// a plain file linked from such a page is not.
func TestProtectedLinkByReferer(t *testing.T) {
	ref := map[string]string{"Referer": "https://www.netflix.com/watch/1"}
	if !protectedLink("https://cdn.example.net/vid/master.m3u8?x=1", ref) || !protectedLink("https://cdn.example.net/vid/manifest.mpd", map[string]string{"referer": "https://www.hulu.com/"}) {
		t.Error("a stream manifest requested from a protected page should be refused")
	}
	if protectedLink("https://github.com/x/y/releases/download/v1/tool.zip", ref) {
		t.Error("a plain file linked from a protected page should still download")
	}
	if protectedLink("https://cdn.example.net/vid/master.m3u8", map[string]string{"Referer": "https://example.com/"}) || protectedLink("https://cdn.example.net/vid/master.m3u8", nil) {
		t.Error("a stream from an ordinary page should be allowed")
	}
}

func extPost(h http.Handler, path string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "http://127.0.0.1:17890/api/"+path, bytes.NewReader(b))
	req.Header.Set("Origin", "chrome-extension://abc")
	req.Header.Set("X-Token", "t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestProtectedProbeAndAdd(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	for _, u := range []string{"https://www.youtube.com/watch?v=abc", "https://www.netflix.com/title/1", "https://youtu.be/abc"} {
		code, out := extPost(h, "probe", map[string]any{"url": u, "early": true})
		if code != 200 || out["error"] != protectedMsg || out["protected"] != true || out["early"] != nil {
			t.Fatalf("probe %s: %d %v", u, code, out)
		}
		code, out = extPost(h, "add", map[string]any{"url": u, "start": true})
		if code != 200 || out["error"] != protectedMsg || out["id"] != nil {
			t.Fatalf("add %s: %d %v", u, code, out)
		}
	}
	// a stream from an unlisted CDN is refused through its Referer
	code, out := extPost(h, "add", map[string]any{"url": "https://cdn.example.net/v/master.m3u8", "headers": map[string]string{"Referer": "https://www.disneyplus.com/"}})
	if code != 200 || out["error"] != protectedMsg {
		t.Fatalf("add by referer: %d %v", code, out)
	}
	m.mu.Lock()
	n := len(m.list)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d downloads were added", n)
	}
	// and an ordinary link still gets in
	if code, out = extPost(h, "add", map[string]any{"url": "https://x.example/a.zip", "queueId": "main"}); code != 200 || out["id"] == nil {
		t.Fatalf("add normal: %d %v", code, out)
	}
}

func TestProtectedExtension(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	for _, path := range []string{"add", "check"} {
		code, out := extPost(h, "ext/"+path, map[string]any{"url": "https://www.youtube.com/watch?v=abc", "referer": "https://www.youtube.com/"})
		if code != 200 || out["ok"] != false || out["error"] != protectedMsg || out["protected"] != true {
			t.Fatalf("ext/%s: %d %v", path, code, out)
		}
	}
	// a manifest the player fetched on a protected page, from a CDN that isn't listed
	code, out := extPost(h, "ext/add", map[string]any{"url": "https://cdn.example.net/v/master.m3u8", "referer": "https://www.netflix.com/watch/1"})
	if code != 200 || out["ok"] != false {
		t.Fatalf("ext/add by referer: %d %v", code, out)
	}
	m.mu.Lock()
	n := len(m.prompts)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d prompts were opened", n)
	}
	// the ping hands the same list and message to the extension
	req := httptest.NewRequest("GET", "http://127.0.0.1:17890/api/ext/ping", nil)
	req.Header.Set("Origin", "chrome-extension://abc")
	req.Header.Set("X-Token", "t") // (the native host's way in)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var ping struct {
		Config struct {
			Protected    []string `json:"protected"`
			ProtectedMsg string   `json:"protectedMsg"`
		} `json:"config"`
	}
	json.Unmarshal(rec.Body.Bytes(), &ping)
	if strings.Join(ping.Config.Protected, ",") != strings.Join(protectedSites, ",") || ping.Config.ProtectedMsg != protectedMsg {
		t.Fatalf("ping config: %s", rec.Body.String())
	}
}

// What the extension adds from a player: the headers of the player's own request and the page's title.
func TestExtAddKeepsStreamHeaders(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	code, out := extPost(h, "ext/add", map[string]any{"url": "https://cdn.example.net/v/master.m3u8", "referer": "https://example.com/watch/1",
		"origin": "https://example.com", "cookie": "a=b", "userAgent": "UA/1", "title": "  My Movie  "})
	if code != 200 || out["ok"] != true {
		t.Fatalf("ext/add: %d %v", code, out)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.prompts) != 1 {
		t.Fatalf("%d prompts", len(m.prompts))
	}
	p := m.prompts[0]
	want := map[string]string{"Referer": "https://example.com/watch/1", "Origin": "https://example.com", "Cookie": "a=b", "User-Agent": "UA/1"}
	for k, v := range want {
		if p.Headers[k] != v {
			t.Errorf("header %s = %q, want %q", k, p.Headers[k], v)
		}
	}
	if p.Title != "My Movie" || p.URL != "https://cdn.example.net/v/master.m3u8" || p.Name != "" { // (no name: the Add dialog names it from the title)
		t.Errorf("prompt %+v", p)
	}
}

// A long title in a script that takes several bytes a letter is cut between letters.
func TestExtAddCutsTitleAtLetters(t *testing.T) {
	m := newTestManager(t)
	h := (&Server{m: m, token: "t"}).routes()
	title := strings.Repeat("ශ්‍රී", 60) // 300 letters (runes)
	if code, out := extPost(h, "ext/add", map[string]any{"url": "https://cdn.example.net/v/master.m3u8", "title": title}); code != 200 || out["ok"] != true {
		t.Fatalf("ext/add: %d %v", code, out)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	got := m.prompts[0].Title
	if !utf8.ValidString(got) || len([]rune(got)) != 120 || !strings.HasPrefix(title, got) {
		t.Fatalf("title %q (%d letters)", got, len([]rune(got)))
	}
}

func TestProtectedClipboard(t *testing.T) {
	m := newTestManager(t)
	// a direct video link on a protected host would otherwise look like a file
	for _, u := range []string{"https://rr1.googlevideo.com/videoplayback/clip.mp4", "https://www.netflix.com/clip.mp4", "https://youtu.be/x.mp4"} {
		if m.offerClipboard(u) {
			t.Errorf("%s: a prompt was opened", u)
		}
	}
	if !m.offerClipboard("https://example.com/clip.mp4") {
		t.Error("an ordinary file link should still be offered")
	}
	m.mu.Lock()
	n := len(m.prompts)
	m.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d prompts, want 1", n)
	}
}

func TestProtectedCommandLine(t *testing.T) {
	m := newTestManager(t)
	_, err := m.ApplyCLI(&CLIRequest{URLs: []string{"https://www.youtube.com/watch?v=abc", "youtu.be/abc"}})
	if err == nil || err.Error() != protectedMsg {
		t.Fatalf("only protected links: %v", err)
	}
	m.mu.Lock()
	n := len(m.prompts)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d prompts", n)
	}
	// a mix: the ordinary link goes in, the other is named in the note
	note, err := m.ApplyCLI(&CLIRequest{URLs: []string{"https://www.youtube.com/watch?v=abc", "https://x.example/a.zip"}, Queue: "Weekend", Silent: true, Dir: t.TempDir()})
	if err != nil || !strings.Contains(note, "added 1 link") || !strings.Contains(note, protectedMsg) {
		t.Fatalf("mixed: %q %v", note, err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.list) != 1 || m.list[0].URL != "https://x.example/a.zip" {
		t.Fatalf("list: %d", len(m.list))
	}
	if got := cliPrefix(protectedMsg); got != protectedMsg || cliPrefix("boom") != "Zipline: boom" {
		t.Errorf("cliPrefix: %q", got)
	}
}

func TestProtectedGrabberFiles(t *testing.T) {
	m := newTestManager(t)
	files := []GrabFile{{URL: "https://example.com/a.zip", Name: "a.zip"}, {URL: "https://rr1.googlevideo.com/clip.mp4", Name: "clip.mp4"},
		{URL: "https://www.netflix.com/b.mp4", Name: "b.mp4"}}
	n := m.grab.addDownloads(files, GrabOptions{URL: "https://example.com/"}, m.settings.DownloadDir, false, "main", false)
	m.mu.Lock()
	defer m.mu.Unlock()
	if n != 1 || len(m.list) != 1 || m.list[0].URL != "https://example.com/a.zip" {
		t.Fatalf("added %d, list %d", n, len(m.list))
	}
}

func TestProtectedPhone(t *testing.T) {
	m, c, _ := phoneManager(t)
	c.do("POST", "hello", map[string]string{"name": "Pixel"}, nil)
	st, body := c.do("POST", "add", map[string]any{"url": "https://www.youtube.com/watch?v=abc", "start": false}, nil)
	if st != 200 || !strings.Contains(body, "YouTube") || strings.Contains(body, `"ok":true`) {
		t.Fatalf("phone add: %d %s", st, body)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.list) != 0 {
		t.Fatalf("%d downloads", len(m.list))
	}
}

// The extension keeps a copy of the list for when the app is older or not running; it must stay
// the same as the app's.
func TestExtensionListMatchesApp(t *testing.T) {
	b, err := os.ReadFile(repoPath("extension/background.js"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)const BLOCKED = \[(.*?)\];`).FindSubmatch(b)
	if m == nil {
		t.Fatal("BLOCKED list not found in extension/background.js")
	}
	var got []string
	for _, s := range regexp.MustCompile(`"([^"]+)"`).FindAllSubmatch(m[1], -1) {
		got = append(got, string(s[1]))
	}
	if strings.Join(got, ",") != strings.Join(protectedSites, ",") {
		t.Fatalf("extension list %v\napp list       %v", got, protectedSites)
	}
	msg := regexp.MustCompile(`const BLOCKED_MSG = "([^"]+)"`).FindSubmatch(b)
	if msg == nil || string(msg[1]) != protectedMsg {
		t.Fatalf("extension message %q, want %q", msg, protectedMsg)
	}
}
