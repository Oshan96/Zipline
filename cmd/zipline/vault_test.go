package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() { systemKeyStore = false } // no test touches the real Keychain or Secret Service

// ---------------------------------------------------------------- helpers

// vtKeys is a key source for tests: fixed keys, or a failure.
type vtKeys struct {
	mu     sync.Mutex
	keys   []vaultKey
	err    error
	loads  int
	makes  bool // make a key when asked to create and there is none
	where  string
	create int // times a key was made
}

func (k *vtKeys) kind() string { return "file" }

func (k *vtKeys) load(create bool) ([]vaultKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.loads++
	if k.err != nil {
		return nil, k.err
	}
	if len(k.keys) == 0 && create && k.makes {
		k.keys = []vaultKey{{newKey(), "file"}}
		k.create++
	}
	return k.keys, nil
}

func vtKey(b byte) []vaultKey { return []vaultKey{{bytes.Repeat([]byte{b}, keyBytes), "file"}} }

// vtUse makes every vault made from now on seal with src.
func vtUse(t *testing.T, src keySource) {
	t.Helper()
	old := vaultSealer
	vaultSealer = func(dir string) sealer {
		g := newGCMSealer(src)
		g.retry = 0
		return g
	}
	t.Cleanup(func() { vaultSealer = old })
}

// vtDir is a data folder that is removed at the end (the manager saves in the background, which can
// race a strict cleanup).
func vtDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zlvault")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 40; i++ {
			os.RemoveAll(dir)
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
}

func vtRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func vtNoPlain(t *testing.T, what, raw string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(raw, s) {
			t.Errorf("%s holds %q in plain text", what, s)
		}
	}
}

// ---------------------------------------------------------------- sealing

func TestVaultRoundTrip(t *testing.T) {
	v := &vault{c: newGCMSealer(&vtKeys{keys: vtKey(7)})}
	for _, s := range []string{"hunter2", "pässwörd ✓ 密码", strings.Repeat("long ", 400), "enc:v2:x", "a b\nc", "dpap"} {
		sealed := v.protect(s)
		if !strings.HasPrefix(sealed, sealPrefix) || strings.Contains(sealed, s) {
			t.Fatalf("protect(%q) = %q", s, sealed)
		}
		if got := v.reveal(sealed); got != s {
			t.Fatalf("reveal(protect(%q)) = %q", s, got)
		}
		if again := v.protect(sealed); again != sealed {
			t.Fatalf("a sealed value was sealed again")
		}
		if other := v.protect(s); other == sealed {
			t.Fatalf("two seals of the same text are identical (the nonce must differ)")
		}
	}
	if v.protect("") != "" || v.reveal("") != "" || v.reveal("plain text") != "plain text" {
		t.Fatal("empty and plain text must pass through")
	}
	if info := v.info(); info.Locked || info.Plain || info.How != "file" {
		t.Fatalf("info %+v", info)
	}
	var nilVault *vault
	if nilVault.protect("x") != "x" || nilVault.reveal("x") != "x" || nilVault.info().How != "none" {
		t.Fatal("a missing vault must pass text through")
	}
}

func TestVaultFormatIsBase64(t *testing.T) {
	v := &vault{c: newGCMSealer(&vtKeys{keys: vtKey(3)})}
	sealed := v.protect("secret")
	blob, err := decodeSealed(strings.TrimPrefix(sealed, sealPrefix))
	if err != nil || len(blob) != 1+12+len("secret")+16 || blob[0] != tagGCM {
		t.Fatalf("blob %d bytes, tag %v, err %v", len(blob), blob[:1], err)
	}
	if u := urlSafe(sealed); strings.ContainsAny(u[len(sealPrefix):], "+/=") || v.reveal(u) != "secret" {
		t.Fatalf("link-safe form %q", u)
	}
}

func TestVaultTamperedOrWrongKeyIsKeptNotBlanked(t *testing.T) {
	good := &vault{c: newGCMSealer(&vtKeys{keys: vtKey(1)})}
	sealed := good.protect("keep me")
	blob, _ := decodeSealed(sealed[len(sealPrefix):])
	blob[len(blob)-1] ^= 1
	damaged := sealPrefix + base64.StdEncoding.EncodeToString(blob)
	for name, v := range map[string]*vault{
		"tampered":   good,
		"wrong key":  {c: newGCMSealer(&vtKeys{keys: vtKey(2)})},
		"no key":     {c: newGCMSealer(&vtKeys{})},
		"key failed": {c: newGCMSealer(&vtKeys{err: errors.New("the Keychain is locked")})},
	} {
		in := sealed
		if name == "tampered" {
			in = damaged
		}
		if got := v.reveal(in); got != in {
			t.Errorf("%s: reveal gave %q, want the sealed text back", name, got)
		}
		if !v.info().Locked || v.openNote() == "" {
			t.Errorf("%s: no note about the locked secret", name)
		}
	}
	if (&vault{c: newGCMSealer(&vtKeys{keys: vtKey(1)})}).reveal("enc:v1:@@@") != "enc:v1:@@@" {
		t.Error("garbage after the prefix must be kept")
	}
}

func TestVaultSealFailureKeepsTheTextAndSaysSo(t *testing.T) {
	v := &vault{c: newGCMSealer(&vtKeys{err: errors.New("the Keychain is locked")})}
	if got := v.protect("typed"); got != "typed" {
		t.Fatalf("a secret that couldn't be sealed was changed to %q", got)
	}
	if !v.info().Plain {
		t.Error("info doesn't say it was saved plain")
	}
	if n := v.sealNote(); !strings.Contains(n, "without protection") || !strings.Contains(n, "locked") {
		t.Errorf("note %q", n)
	}
	if v.sealNote() != "" {
		t.Error("the note should come once")
	}
}

func TestGCMSealerAsksOnceAndRetriesLater(t *testing.T) {
	src := &vtKeys{keys: vtKey(5)}
	g := newGCMSealer(src)
	for i := 0; i < 5; i++ {
		if b, err := g.seal([]byte("x")); err != nil {
			t.Fatal(err)
		} else if _, err := g.open(b); err != nil {
			t.Fatal(err)
		}
	}
	if src.loads != 1 {
		t.Fatalf("the key source was asked %d times", src.loads)
	}
	bad := &vtKeys{err: errors.New("locked")}
	g = newGCMSealer(bad)
	g.retry = time.Hour
	g.seal([]byte("x"))
	g.seal([]byte("x"))
	g.open([]byte{tagGCM, 1, 2, 3})
	if bad.loads != 1 {
		t.Fatalf("a failing store was asked %d times within the retry time", bad.loads)
	}
	g.retry = 0
	bad.mu.Lock()
	bad.err, bad.keys = nil, vtKey(5)
	bad.mu.Unlock()
	if _, err := g.seal([]byte("x")); err != nil {
		t.Fatalf("after the store came back: %v", err)
	}
}

func TestGCMSealerCreatesAKeyOnlyToSeal(t *testing.T) {
	src := &vtKeys{makes: true}
	v := &vault{c: newGCMSealer(src)}
	if v.reveal("plain") != "plain" || v.reveal(sealPrefix+"AAAA") == "" || src.create != 0 {
		t.Fatal("opening something must not make a key")
	}
	s := v.protect("pw")
	if src.create != 1 || v.reveal(s) != "pw" {
		t.Fatalf("created %d keys; reveal %q", src.create, v.reveal(s))
	}
}

// ---------------------------------------------------------------- links with a password

func TestURLPasswords(t *testing.T) {
	for _, c := range []struct{ in, strip string }{
		{"https://bob:pw@files.example/f.zip", "https://bob@files.example/f.zip"},
		{"ftp://bob:p%40ss:word@host:2121/a/b?x=1#y", "ftp://bob@host:2121/a/b?x=1#y"},
		{"https://bob@files.example/f.zip", "https://bob@files.example/f.zip"},
		{"https://files.example/f.zip?u=http://a:b@c/", "https://files.example/f.zip?u=http://a:b@c/"},
		{"https://files.example/a@b:c", "https://files.example/a@b:c"},
		{"magnet:?xt=urn:btih:abc&tr=http://u:p@tracker.example/announce", "magnet:?xt=urn:btih:abc&tr=http://u:p@tracker.example/announce"},
		{"torrent:abcdef", "torrent:abcdef"},
		{"", ""},
		{"https://:pw@host/", "https://@host/"},
	} {
		if got := stripURLPass(c.in); got != c.strip {
			t.Errorf("stripURLPass(%q) = %q, want %q", c.in, got, c.strip)
		}
	}
	v := &vault{c: newGCMSealer(&vtKeys{keys: vtKey(4)})}
	for _, in := range []string{"https://bob:pw@files.example/f.zip?a=b#c", "ftp://bob:p%40ss:word@host:2121/", "http://u:@h/", "https://x/y"} {
		sealed := v.protectURL(in)
		if sealed != in && (strings.Contains(sealed, "pw@") || strings.Contains(sealed, "p%40ss")) {
			t.Errorf("%q kept its password: %q", in, sealed)
		}
		if got := v.revealURL(sealed); got != in {
			t.Errorf("round trip of %q gave %q", in, got)
		}
		if strings.HasPrefix(in, "https://bob") {
			if !strings.HasPrefix(sealed, "https://bob:enc:v1:") || strings.Contains(sealed[len("https://bob:"):strings.Index(sealed, "@")], "/") {
				t.Errorf("sealed link %q", sealed)
			}
		}
	}
	if got := restoreURLPass("https://bob@files.example/other.zip", "https://bob:pw@files.example/f.zip"); got != "https://bob:pw@files.example/other.zip" {
		t.Errorf("restore: %q", got)
	}
	for _, other := range []string{"https://alice:pw@files.example/f.zip", "https://bob:pw@evil.example/f.zip"} {
		if got := restoreURLPass("https://bob@files.example/f.zip", other); strings.Contains(got, "pw") {
			t.Errorf("restored a password for a different user or server: %q", got)
		}
	}
	if got := restoreURLPass("https://bob:new@files.example/f.zip", "https://bob:pw@files.example/f.zip"); got != "https://bob:new@files.example/f.zip" {
		t.Errorf("a typed password was replaced: %q", got)
	}
}

// ---------------------------------------------------------------- the key file

func TestKeyFile(t *testing.T) {
	dir := vtDir(t)
	f := fileKeys{filepath.Join(dir, "vault.key")}
	if keys, err := f.load(false); keys != nil || err != nil {
		t.Fatalf("no file, no create: %v %v", keys, err)
	}
	var wg sync.WaitGroup
	got := make([][]byte, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if keys, err := f.load(true); err != nil || len(keys) != 1 {
				t.Errorf("load: %v %v", keys, err)
			} else {
				got[i] = keys[0].key
			}
		}()
	}
	wg.Wait()
	for _, k := range got[1:] {
		if !bytes.Equal(k, got[0]) {
			t.Fatal("racing creators ended up with different keys")
		}
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("leftover files: %v", ents)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(f.path); st.Mode().Perm() != 0o600 {
			t.Errorf("key file mode %v", st.Mode().Perm())
		}
	}
	// a damaged key file is an error, and is never replaced
	os.WriteFile(f.path, []byte("not a key"), 0o600)
	if _, err := f.load(true); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("damaged key file: %v", err)
	}
	if vtRead(t, f.path) != "not a key" {
		t.Fatal("a damaged key file was overwritten")
	}
}

// ---------------------------------------------------------------- the Keychain

// vtSecurity plays the macOS security command.
type vtSecurity struct {
	mu            sync.Mutex
	items         map[string]string // account → password
	calls         [][]string
	stdins        []string
	noInteractive bool // `security -i` is not understood
	locked        bool
}

func (f *vtSecurity) run(stdin, name string, args ...string) (cmdResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.stdins = append(f.stdins, stdin)
	if name != "security" {
		return cmdResult{}, errors.New("not found")
	}
	if f.items == nil {
		f.items = map[string]string{}
	}
	if len(args) > 0 && args[0] == "-i" {
		if f.noInteractive {
			return cmdResult{errOut: "security: unknown option -i", code: 2}, nil
		}
		for _, line := range strings.Split(stdin, "\n") {
			if w := vtTokens(line); len(w) > 0 {
				f.command(w)
			}
		}
		return cmdResult{}, nil
	}
	return f.command(args), nil
}

func (f *vtSecurity) command(a []string) cmdResult {
	flag := func(name string) string {
		for i := range a {
			if a[i] == name && i+1 < len(a) {
				return a[i+1]
			}
		}
		return ""
	}
	if f.locked {
		return cmdResult{errOut: "security: User interaction is not allowed.", code: 36}
	}
	if flag("-s") != "Zipline" {
		return cmdResult{errOut: "wrong service", code: 1}
	}
	acct := flag("-a")
	switch a[0] {
	case "find-generic-password":
		if pw, ok := f.items[acct]; ok {
			return cmdResult{out: pw + "\n"}
		}
		return cmdResult{errOut: "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.", code: 44}
	case "add-generic-password":
		if _, ok := f.items[acct]; ok {
			return cmdResult{errOut: "already exists", code: 45}
		}
		f.items[acct] = flag("-w")
		return cmdResult{}
	}
	return cmdResult{code: 1}
}

// vtTokens splits a line like the tool does: words, with "double quotes" for spaces.
func vtTokens(line string) []string {
	var out []string
	var cur strings.Builder
	quoted, in := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && quoted && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case c == '"':
			quoted, in = !quoted, true
		case c == ' ' && !quoted:
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

func TestKeychainCommands(t *testing.T) {
	f := &vtSecurity{}
	k := keychainKeys{run: f.run, user: "Ann Lee"}
	if keys, err := k.load(false); keys != nil || err != nil {
		t.Fatalf("no key yet: %v %v", keys, err)
	}
	if len(f.calls) != 1 || strings.Join(f.calls[0], " ") != "security find-generic-password -s Zipline -a Ann Lee -w" {
		t.Fatalf("lookup command: %v", f.calls)
	}
	keys, err := k.load(true)
	if err != nil || len(keys) != 1 || keys[0].from != "keychain" || len(keys[0].key) != keyBytes {
		t.Fatalf("create: %v %v", keys, err)
	}
	if hex.EncodeToString(keys[0].key) != f.items["Ann Lee"] {
		t.Fatal("the Keychain doesn't hold the key that is used")
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), f.items["Ann Lee"]) {
			t.Errorf("the key is on a command line: %v", c)
		}
	}
	var added bool
	for _, s := range f.stdins {
		added = added || s == `add-generic-password -s Zipline -a "Ann Lee" -w `+f.items["Ann Lee"]+"\n"
	}
	if !added {
		t.Errorf("the key wasn't passed to security -i on standard input: %q", f.stdins)
	}
	// later runs find it and make nothing
	n := len(f.calls)
	if again, err := k.load(true); err != nil || !bytes.Equal(again[0].key, keys[0].key) || len(f.calls) != n+1 {
		t.Fatalf("second run: %v %v (%d calls)", again, err, len(f.calls)-n)
	}
}

func TestKeychainFallsBackToTheCommandLine(t *testing.T) {
	f := &vtSecurity{noInteractive: true}
	keys, err := (keychainKeys{run: f.run, user: "u"}).load(true)
	if err != nil || len(keys) != 1 || hex.EncodeToString(keys[0].key) != f.items["u"] {
		t.Fatalf("%v %v %v", keys, err, f.items)
	}
}

func TestLockedKeychainMakesNothing(t *testing.T) {
	f := &vtSecurity{locked: true}
	for _, create := range []bool{false, true} {
		if keys, err := (keychainKeys{run: f.run, user: "u"}).load(create); err == nil || keys != nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("create=%v: %v %v", create, keys, err)
		}
	}
	for _, c := range f.calls {
		if c[1] != "find-generic-password" {
			t.Errorf("a locked Keychain was written to: %v", c)
		}
	}
	if len(f.items) != 0 {
		t.Fatal("a key was stored")
	}
}

// ---------------------------------------------------------------- a whole data folder

type vtSecrets struct{ proxy, site, cookie, auth, link, mirror, url string }

func vtSetSecrets(m *Manager) vtSecrets {
	sec := vtSecrets{proxy: "proxy-pw", site: "s3cret!", cookie: "sid=abc123", auth: "Basic Ym9iOmh1bnRlcjI=", link: "dl-pw", mirror: "mir-pw"}
	m.mu.Lock()
	m.settings.Proxy = ProxySettings{Mode: "manual", URL: "http://pu:purl-pw@proxy.example:8080", User: "pu2", Pass: sec.proxy}
	m.settings.Sites = []SiteRule{{ID: "r1", Site: "files.example", User: "alice", Pass: sec.site}}
	d := &Download{ID: "d1", URL: "https://bob:" + sec.link + "@files.example/f.zip", FinalURL: "https://bob:" + sec.link + "@cdn.example/f.zip",
		Mirrors: []string{"ftp://bob:" + sec.mirror + "@mirror.example/f.zip"}, FileName: "f.zip", Dir: m.settings.DownloadDir, Status: StPaused, Added: time.Now(),
		Headers: map[string]string{"Cookie": sec.cookie, "Authorization": sec.auth, "Proxy-Authorization": "Basic cHJveHk6cHc=", "Referer": "https://files.example/page", "User-Agent": "ZL"}}
	m.list = append(m.list, d)
	m.byID[d.ID] = d
	m.mu.Unlock()
	return sec
}

func (s vtSecrets) all() []string {
	return []string{s.proxy, s.site, s.cookie, s.auth, s.link, s.mirror, "purl-pw", "cHJveHk6cHc="}
}

func TestSecretsAreSealedInEveryFile(t *testing.T) {
	vtUse(t, &vtKeys{keys: vtKey(1)})
	dir := vtDir(t)
	m := NewManager(dir)
	sec := vtSetSecrets(m)
	m.save()
	st, dl := vtRead(t, filepath.Join(dir, "settings.json")), vtRead(t, filepath.Join(dir, "downloads.json"))
	vtNoPlain(t, "settings.json", st, sec.all()...)
	vtNoPlain(t, "downloads.json", dl, sec.all()...)
	if strings.Count(st, sealPrefix) < 3 || strings.Count(dl, sealPrefix) < 6 {
		t.Errorf("too few sealed values: settings %d, downloads %d", strings.Count(st, sealPrefix), strings.Count(dl, sealPrefix))
	}
	for _, keep := range []string{`"Referer": "https://files.example/page"`, `"User-Agent": "ZL"`, `"url": "https://bob:enc:v1:`} {
		if !strings.Contains(dl, keep) {
			t.Errorf("downloads.json lost %s", keep)
		}
	}
	// what the next start reads is what was set
	m2 := NewManager(dir)
	m2.mu.Lock()
	defer m2.mu.Unlock()
	if m2.settings.Proxy.Pass != sec.proxy || m2.settings.Proxy.URL != "http://pu:purl-pw@proxy.example:8080" || m2.settings.Sites[0].Pass != sec.site {
		t.Fatalf("settings: %+v %+v", m2.settings.Proxy, m2.settings.Sites)
	}
	d := m2.byID["d1"]
	if d == nil || d.URL != "https://bob:dl-pw@files.example/f.zip" || d.FinalURL != "https://bob:dl-pw@cdn.example/f.zip" || d.Mirrors[0] != "ftp://bob:mir-pw@mirror.example/f.zip" ||
		d.Headers["Cookie"] != sec.cookie || d.Headers["Authorization"] != sec.auth || d.Headers["Referer"] != "https://files.example/page" || d.Headers["Proxy-Authorization"] != "Basic cHJveHk6cHc=" {
		t.Fatalf("download: %+v", d)
	}
	if m2.alert != "" {
		t.Errorf("alert %q", m2.alert)
	}
}

func TestOldPlainFilesAreReadAndRewrittenSealed(t *testing.T) {
	vtUse(t, &vtKeys{keys: vtKey(1)})
	dir := vtDir(t)
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"downloadDir":"`+filepath.ToSlash(dir)+`","proxy":{"mode":"manual","url":"p.example:3128","user":"u","pass":"old-proxy-pw"},
		"sites":[{"site":"files.example","user":"alice","pass":"old-site-pw"}],"phone":{"on":false,"port":17891,"devices":[{"id":"x","name":"Phone","key":"OLDPAIRCODE12345","added":"2026-01-01T00:00:00Z"}]}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "downloads.json"), []byte(`[{"id":"a1","url":"https://bob:old-link-pw@files.example/a.zip","name":"a.zip","dir":"`+filepath.ToSlash(dir)+`","status":"paused",
		"headers":{"Cookie":"old=cookie","Authorization":"Basic b2xkOnB3","Referer":"https://files.example/"}}]`), 0o644)
	m := NewManager(dir)
	m.mu.Lock()
	ok := m.settings.Proxy.Pass == "old-proxy-pw" && m.settings.Sites[0].Pass == "old-site-pw" && m.settings.Phone.Devices[0].Key == "OLDPAIRCODE12345" &&
		m.byID["a1"].URL == "https://bob:old-link-pw@files.example/a.zip" && m.byID["a1"].Headers["Cookie"] == "old=cookie"
	m.mu.Unlock()
	if !ok {
		t.Fatal("old plain values weren't read as before")
	}
	m.save()
	st, dl := vtRead(t, filepath.Join(dir, "settings.json")), vtRead(t, filepath.Join(dir, "downloads.json"))
	vtNoPlain(t, "settings.json", st, "old-proxy-pw", "old-site-pw", "OLDPAIRCODE12345")
	vtNoPlain(t, "downloads.json", dl, "old-link-pw", "old=cookie", "b2xkOnB3")
	if !strings.Contains(dl, `"format": 2`) || !strings.Contains(dl, "https://files.example/") {
		t.Errorf("downloads.json: %.300s", dl)
	}
	m2 := NewManager(dir)
	if m2.settings.Proxy.Pass != "old-proxy-pw" || m2.byID["a1"].Headers["Authorization"] != "Basic b2xkOnB3" {
		t.Fatal("what was rewritten sealed doesn't read back")
	}
}

// A key that can't be had never costs a password: the sealed text is kept and written back as it
// was, the window is told, and everything opens again once the key is back.
func TestMissingKeyNeverBlanksData(t *testing.T) {
	dir := vtDir(t)
	good := &vtKeys{keys: vtKey(1)}
	vtUse(t, good)
	m := NewManager(dir)
	sec := vtSetSecrets(m)
	m.save()
	before := vtRead(t, filepath.Join(dir, "settings.json")) + vtRead(t, filepath.Join(dir, "downloads.json"))
	sealedRe := regexp.MustCompile(`enc:v1:[A-Za-z0-9+/=_-]+`)
	pick := sealedRe.FindAllString // every sealed value in a file
	was := pick(before, -1)
	if len(was) < 8 {
		t.Fatalf("only %d sealed values: %s", len(was), before)
	}

	for name, src := range map[string]keySource{
		"store failed": &vtKeys{err: errors.New("the Keychain said: User interaction is not allowed.")},
		"other key":    &vtKeys{keys: vtKey(2)},
		"key lost":     &vtKeys{},
	} {
		vtUse(t, src)
		m2 := NewManager(dir)
		m2.mu.Lock()
		held := m2.settings.Proxy.Pass != "" && m2.settings.Sites[0].Pass != "" && isSealed(m2.settings.Sites[0].Pass) &&
			isSealed(m2.byID["d1"].Headers["Cookie"]) && isSealed(m2.byID["d1"].Headers["Authorization"])
		m2.mu.Unlock()
		if !held {
			t.Fatalf("%s: a sealed secret was blanked or changed in memory", name)
		}
		if !strings.Contains(m2.alert, "couldn't open") {
			t.Errorf("%s: no note for the window: %q", name, m2.alert)
		}
		if v := m2.settingsView(); !v.Vault.Locked || !v.Proxy.PassSet || !v.Sites[0].PassSet {
			t.Errorf("%s: settings view %+v", name, v.Vault)
		}
		m2.save()
		after := vtRead(t, filepath.Join(dir, "settings.json")) + vtRead(t, filepath.Join(dir, "downloads.json"))
		now := pick(after, -1)
		if strings.Join(now, "\n") != strings.Join(was, "\n") {
			t.Fatalf("%s: the saved secrets changed:\nbefore %v\nafter  %v", name, was, now)
		}
	}

	// a password typed while the key is out of reach is kept (plain, with a note) beside the sealed ones
	vtUse(t, &vtKeys{err: errors.New("the Keychain said: User interaction is not allowed.")})
	m3 := NewManager(dir)
	m3.mu.Lock()
	m3.settings.Sites = append(m3.settings.Sites, SiteRule{ID: "r2", Site: "new.example", User: "bo", Pass: "typed-while-locked"})
	m3.mu.Unlock()
	m3.save()
	if !strings.Contains(vtRead(t, filepath.Join(dir, "settings.json")), "typed-while-locked") {
		t.Fatal("a new password was lost while the key was out of reach")
	}
	m3.mu.Lock()
	var told bool
	for _, n := range m3.notices {
		told = told || strings.Contains(n.Text, "without protection")
	}
	m3.mu.Unlock()
	if !told {
		t.Error("the window wasn't told a password was saved unprotected")
	}

	// the key is back: everything opens, and the plain one is sealed at the next save
	vtUse(t, good)
	m4 := NewManager(dir)
	m4.mu.Lock()
	ok := m4.settings.Proxy.Pass == sec.proxy && m4.settings.Sites[0].Pass == sec.site && m4.settings.Sites[1].Pass == "typed-while-locked" &&
		m4.byID["d1"].Headers["Cookie"] == sec.cookie && m4.byID["d1"].Headers["Authorization"] == sec.auth
	m4.mu.Unlock()
	if !ok || m4.alert != "" {
		t.Fatalf("after the key came back: ok=%v alert=%q", ok, m4.alert)
	}
	m4.save()
	vtNoPlain(t, "settings.json", vtRead(t, filepath.Join(dir, "settings.json")), "typed-while-locked", sec.site)
}

func TestLockedSecretsAreNotSent(t *testing.T) {
	d := &Download{URL: "https://files.example/f", Headers: map[string]string{"Cookie": sealPrefix + "AAAA", "Referer": "https://files.example/"}}
	req, err := buildRequest(context.Background(), d, d.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Cookie") != "" || req.Header.Get("Referer") == "" {
		t.Errorf("headers: %v", req.Header)
	}
	if usableSecret(sealPrefix+"AAAA") != "" || usableSecret("pw") != "pw" {
		t.Error("usableSecret")
	}

	// a password still sealed in a link, a proxy address or a proxy login is left out too
	sealed := url.UserPassword("me", sealPrefix+"AAAA")
	if _, pw, _ := loginFor(context.WithValue(context.Background(), urlLoginKey{}, sealed), d, &url.URL{Host: "files.example"}); pw != "" {
		t.Errorf("link login sent %q", pw)
	}
	if _, pw := ftpCreds(&url.URL{Scheme: "ftp", Host: "files.example", User: sealed}, d); pw != "" {
		t.Errorf("ftp login sent %q", pw)
	}
	target := &url.URL{Scheme: "https", Host: "files.example"}
	for _, p := range []ProxySettings{
		{Mode: "manual", URL: "http://me:" + sealPrefix + "AAAA@proxy.example:8080"},
		{Mode: "manual", URL: "http://proxy.example:8080", User: "me", Pass: sealPrefix + "AAAA"},
	} {
		pu, err := proxyForWith(p, target)
		if err != nil {
			t.Fatal(err)
		}
		if pw, _ := pu.User.Password(); pw != "" {
			t.Errorf("proxy %v sent %q", p, pw)
		}
	}
}

// ---------------------------------------------------------------- the downloads file keeps its shape

func TestDownloadsFileKeepsItsShape(t *testing.T) {
	v := &vault{c: newGCMSealer(&vtKeys{keys: vtKey(1)})}
	d := &Download{ID: "x", URL: "https://example.com/a.zip", FinalURL: "https://cdn.example.com/a.zip", FileName: "a.zip", Dir: "/tmp", Size: 99, Status: StPaused,
		Added: time.Unix(1700000000, 0).UTC(), Conns: 4, Headers: map[string]string{"Referer": "https://example.com/", "User-Agent": "ZL"}, Mirrors: []string{"https://m.example.com/a.zip"},
		Segments: []*Segment{{Start: 0, End: 98, Pos: 10}}, Desc: "d", Kind: "hls", Live: true}
	var plain, wired map[string]any
	json.Unmarshal(mustJSON(d), &plain)
	json.Unmarshal(mustJSON(v.wire(d)), &wired)
	if !reflect.DeepEqual(plain, wired) {
		t.Fatalf("a download with no secrets must be written as it always was:\n%v\n%v", plain, wired)
	}
	// and every field of a Download is still in it
	var all, fromWire map[string]json.RawMessage
	d.Headers["Cookie"] = "c=1"
	d.URL = "https://u:p@example.com/a.zip"
	json.Unmarshal(mustJSON(d), &all)
	json.Unmarshal(mustJSON(v.wire(d)), &fromWire)
	if len(all) != len(fromWire) {
		t.Errorf("fields: %d in the download, %d written", len(all), len(fromWire))
	}
	if d.URL != "https://u:p@example.com/a.zip" || d.Headers["Cookie"] != "c=1" {
		t.Error("writing a download changed it in memory")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
