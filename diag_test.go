package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRedactURL(t *testing.T) {
	for _, c := range [][2]string{
		{"https://user:pa55@example.com/file.zip?token=abc123&name=x#frag", "https://example.com/file.zip?token=REDACTED&name=x#frag"},
		{"ftp://anon:secret@ftp.example.org/pub/x.iso", "ftp://ftp.example.org/pub/x.iso"},
		{"https://b.s3.amazonaws.com/a.iso?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIA%2F20261011&X-Amz-Date=20261011T000000Z&X-Amz-Expires=3600&X-Amz-Security-Token=tok&X-Amz-Signature=deadbeef",
			"https://b.s3.amazonaws.com/a.iso?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=REDACTED&X-Amz-Date=REDACTED&X-Amz-Expires=3600&X-Amz-Security-Token=REDACTED&X-Amz-Signature=REDACTED"},
		{"https://storage.googleapis.com/b/o?X-Goog-Credential=c&X-Goog-Signature=s&alt=media", "https://storage.googleapis.com/b/o?X-Goog-Credential=REDACTED&X-Goog-Signature=REDACTED&alt=media"},
		{"https://a.blob.core.windows.net/c/f.bin?sv=2022-11-02&se=2026-10-12&sp=r&sig=AbC%2Bd", "https://a.blob.core.windows.net/c/f.bin?sv=2022-11-02&se=2026-10-12&sp=r&sig=REDACTED"},
		{"http://[::1]:8080/a?api_key=1&Password=2&sessionid=3&auth=4&id=5", "http://[::1]:8080/a?api_key=REDACTED&Password=REDACTED&sessionid=REDACTED&auth=REDACTED&id=5"},
		{"https://example.com/page#access_token=xyz&state=1", "https://example.com/page#access_token=REDACTED&state=1"},
		{"https://example.com/@user/video.mp4", "https://example.com/@user/video.mp4"},
		{"magnet:?xt=urn:btih:abcdef&dn=Ubuntu", "magnet:?xt=urn:btih:abcdef&dn=Ubuntu"},
		{"https://example.com/a?flag&x=", "https://example.com/a?flag&x="},
	} {
		if got := redactURL(c[0]); got != c[1] {
			t.Errorf("redactURL(%s)\n got %s\nwant %s", c[0], got, c[1])
		}
	}
	got := redactText(`GET https://u:p@h.example/x?sig=1 failed, trying "ftp://a:b@c.example/f?key=k".`)
	if want := `GET https://h.example/x?sig=REDACTED failed, trying "ftp://c.example/f?key=REDACTED".`; got != want {
		t.Errorf("redactText\n got %s\nwant %s", got, want)
	}
}

func TestVersionText(t *testing.T) {
	want := "Zipline " + appVersion + " (" + runtime.GOOS + "/" + runtime.GOARCH + ", " + runtime.Version() + ")"
	if got := versionText(); got != want || !strings.Contains(got, ", go1.") {
		t.Fatalf("%q, want %q", got, want)
	}
}

// J8: logf writes nothing before openLog, timestamps and redacts lines, is safe from many
// goroutines, and keeps one old file when it rotates at 1 MB.
func TestLogRotates(t *testing.T) {
	closeLog()
	logf("before openLog: %s", "dropped") // must not panic or write anywhere
	dir := t.TempDir()
	if err := openLog(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeLog)
	pad := strings.Repeat("x", 180)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1500; i++ { // 12000 lines of ~220 bytes: rotates twice
				logf("goroutine %d line %d %s", g, i, pad)
			}
		}()
	}
	wg.Wait()
	closeLog()
	cur, err := os.ReadFile(filepath.Join(dir, "zipline.log"))
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(filepath.Join(dir, "zipline.log.1"))
	if err != nil {
		t.Fatal("no rotated file:", err)
	}
	if len(cur) > logMax || len(old) > logMax {
		t.Fatalf("sizes %d and %d, limit %d", len(cur), len(old), logMax)
	}
	line := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d\.\d{3} goroutine \d line \d+ x{180}$`)
	for _, l := range strings.Split(strings.TrimSpace(string(old)+string(cur)), "\n") {
		if !line.MatchString(l) {
			t.Fatalf("bad line %q", l)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "zipline.log.2")); err == nil {
		t.Fatal("kept more than one old file")
	}

	// a fresh log: the start line, and the redacted address
	dir = t.TempDir()
	openLog(dir)
	logf("download https://user:hunter2@example.com/f.iso?token=t0k3n started")
	closeLog()
	b, _ := os.ReadFile(filepath.Join(dir, "zipline.log"))
	if s := string(b); !strings.Contains(s, " "+versionText()+" started\n") ||
		!strings.Contains(s, " download https://example.com/f.iso?token=REDACTED started\n") || strings.Contains(s, "hunter2") || strings.Contains(s, "t0k3n") {
		t.Fatalf("log:\n%s", s)
	}
}

func TestTrimFileTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "crash.log")
	var sb strings.Builder
	for i := 0; sb.Len() < 1500<<10; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	os.WriteFile(p, []byte(sb.String()), 0o600)
	if err := trimFileTail(p, crashMax, crashKeep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	all := strings.Split(strings.TrimSuffix(sb.String(), "\n"), "\n")
	if len(b) > crashKeep+100 || lines[0] != "[older crash reports removed]" || !strings.HasPrefix(lines[1], "line ") ||
		lines[len(lines)-1] != all[len(all)-1] || !strings.HasSuffix(sb.String(), strings.Join(lines[1:], "\n")+"\n") {
		t.Fatalf("trimmed to %d bytes, starting %q %q", len(b), lines[0], lines[1])
	}
	before := len(b)
	trimFileTail(p, crashMax, crashKeep) // small enough now: left alone
	if b, _ := os.ReadFile(p); len(b) != before {
		t.Fatal("trimmed again")
	}
}

// TestCrashOutputChild is the process TestCrashOutput starts: it crashes on purpose.
func TestCrashOutputChild(t *testing.T) {
	dir := os.Getenv("ZL_CRASH_DIR")
	if dir == "" {
		t.Skip("run by TestCrashOutput")
	}
	setupCrashOutput(dir)
	go func() { panic("zipline crash test https://u:pw@example.com/x?token=t0k3n") }()
	time.Sleep(30 * time.Second)
}

// J8: an unrecovered panic in any goroutine lands in crash.log, after the line naming the
// run; a run that didn't crash leaves nothing; Copy diagnostics shows the latest crash.
func TestCrashOutput(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "crash.log")
	earlier := crashMark + "Zipline 0.9 (x/y, go1.24.0) started 2026-10-01 10:00:00 +0000\npanic: an old crash\n\ngoroutine 7 [running]:\nmain.old()\n" +
		crashMark + "Zipline 0.9 (x/y, go1.24.0) started 2026-10-02 10:00:00 +0000\n" // this run ended normally
	os.WriteFile(cp, []byte(earlier), 0o600)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashOutputChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ZL_CRASH_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil {
		t.Fatalf("the child didn't crash (%v):\n%s", err, out)
	}
	b, _ := os.ReadFile(cp)
	s := string(b)
	if !strings.HasPrefix(s, crashMark+"Zipline 0.9 (x/y, go1.24.0) started 2026-10-01") || strings.Contains(s, "2026-10-02") ||
		!strings.Contains(s, "\n"+crashMark+versionText()+" started ") ||
		!strings.Contains(s, "panic: zipline crash test") || !strings.Contains(s, "goroutine ") {
		t.Fatalf("crash.log:\n%s\nchild output:\n%s", s, out)
	}

	m := &Manager{dataDir: dir, byID: map[string]*Download{}, settings: defaultSettings()}
	txt := diagnosticsText(m)
	_, crash, _ := strings.Cut(txt, "\nLatest crash (crash.log)\n")
	if !strings.HasPrefix(crash, crashMark+versionText()) || !strings.Contains(crash, "panic: zipline crash test https://example.com/x?token=REDACTED") ||
		strings.Contains(crash, "an old crash") || strings.Contains(txt, "t0k3n") || strings.Contains(txt, "u:pw@") {
		t.Fatalf("diagnostics:\n%s", txt)
	}
}

func TestLatestCrash(t *testing.T) {
	h := func(day int) string {
		return fmt.Sprintf("%sZipline 1.0 (x/y, go1.24.7) started 2026-10-%02d\n", crashMark, day)
	}
	for _, c := range []struct{ log, want string }{
		{"", ""},
		{h(1), ""},
		{h(1) + "panic: a\n" + h(2), h(1) + "panic: a"},
		{h(1) + "panic: a\n" + h(2) + "fatal error: b\n\ngoroutine 1\n", h(2) + "fatal error: b\n\ngoroutine 1"},
		{"[older crash reports removed]\ngoroutine 9\n" + h(3), "[older crash reports removed]\ngoroutine 9"},
	} {
		if got := latestCrash(c.log); got != c.want {
			t.Errorf("latestCrash(%q) = %q, want %q", c.log, got, c.want)
		}
	}
	long := latestCrash(h(4) + "panic: big\n" + strings.Repeat("frame\n", 10000))
	if len(long) > diagCrashMax+10 || !strings.HasSuffix(long, "\n[…]") || !strings.Contains(long, "panic: big") {
		t.Fatalf("long report: %d bytes", len(long))
	}
}

// J8: Copy diagnostics never shows a password, login, pairing code, signed link or cookie.
func TestDiagnosticsHidesSecrets(t *testing.T) {
	dir := t.TempDir()
	s := defaultSettings()
	s.DownloadDir = dir
	s.Proxy = ProxySettings{Mode: "manual", URL: "http://proxyuser:proxyPW-777@proxy.example:3128", User: "proxy-login-name", Pass: "proxy-pass-123", Bypass: "intranet.local"}
	s.Sites = []SiteRule{{Site: "files.example.com", User: "site-user-42", Pass: "site-pass-99"}, {Site: "cdn.example.com", MaxConns: 4}}
	s.Phone = PhoneSettings{On: true, Port: 17891, Devices: []PhoneDevice{{ID: "d1", Name: "Pixel", Key: "PAIRCODE12345678"}, {ID: "d2", Key: "PENDINGCODE00000", Pending: true}}}
	s.Scan = ScanSettings{On: true, Program: "/opt/av/scan", Args: "--key=av-licence-555 %f"}
	m := &Manager{dataDir: dir, byID: map[string]*Download{}, settings: s}
	for i, d := range []*Download{
		{Status: StCompleted, URL: "https://user:dlpass-1@example.com/a.iso?token=dltoken-1", Headers: map[string]string{"Cookie": "cookie-secret-1", "Authorization": "Bearer bearer-secret-1"}},
		{Status: StCompleted, Kind: "hls"},
		{Status: StError, Kind: "torrent"},
		{Status: StPaused},
		{Status: StDownloading, Provisional: true}, // still in the Add dialog: not counted
	} {
		d.ID = fmt.Sprint(i)
		m.list = append(m.list, d)
	}
	if err := openLog(dir); err != nil {
		t.Fatal(err)
	}
	logf("fetching %s", "https://cdn.example/f.iso?X-Amz-Signature=sigsecret999&part=2")
	closeLog()

	txt := diagnosticsText(m)
	for _, secret := range []string{"proxyuser", "proxyPW-777", "proxy-login-name", "proxy-pass-123", "site-user-42", "site-pass-99",
		"PAIRCODE12345678", "PENDINGCODE00000", "av-licence-555", "dlpass-1", "dltoken-1", "cookie-secret-1", "bearer-secret-1", "sigsecret999"} {
		if strings.Contains(txt, secret) {
			t.Errorf("diagnostics show %q", secret)
		}
	}
	for _, want := range []string{
		versionText() + "\n",
		"Data folder: " + dir + "\n",
		"Proxy: manual http://proxy.example:3128, login saved, bypass list set\n",
		"Site rules: 2 (1 with a saved login)",
		"Phone control: on, port 17891, 1 paired\n",
		"Virus scan: on (scan)\n",
		"\nDownloads: 4\n  by status: paused 1, completed 2, error 1\n  by kind: file 2, hls 1, torrent 1\n",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("diagnostics lack %q", want)
		}
	}
	if !regexp.MustCompile(`\n  \S+ \S+ fetching https://cdn\.example/f\.iso\?X-Amz-Signature=REDACTED&part=2\n`).MatchString(txt) {
		t.Error("log line missing")
	}
	if !strings.HasSuffix(txt, "\nNo crash reports.\n") {
		t.Error("crash section")
	}
	if t.Failed() {
		t.Log(txt)
	}
}
