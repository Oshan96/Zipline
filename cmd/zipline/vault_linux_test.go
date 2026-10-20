//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// vtSecretTool puts a stand-in for libsecret's secret-tool on PATH. It keeps the secret in a file
// and logs how it was called. fail makes every call fail with a message.
func vtSecretTool(t *testing.T, fail string) (store, calls string) {
	t.Helper()
	bin := t.TempDir()
	store, calls = filepath.Join(bin, "store"), filepath.Join(bin, "calls")
	script := "#!/bin/sh\necho \"$@\" >> '" + calls + "'\n"
	if fail != "" {
		script += "echo '" + fail + "' >&2\nexit 1\n"
	} else {
		script += "case \"$1\" in\nstore) cat > '" + store + "'; exit 0;;\nlookup) [ -f '" + store + "' ] && cat '" + store + "' && exit 0; exit 1;;\nesac\nexit 2\n"
	}
	if err := os.WriteFile(filepath.Join(bin, "secret-tool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent")
	systemKeyStore = true
	t.Cleanup(func() { systemKeyStore = false })
	return store, calls
}

func TestKeyInTheSecretService(t *testing.T) {
	store, calls := vtSecretTool(t, "")
	dir := vtDir(t)
	v := &vault{c: platformSealer(dir)}
	sealed := v.protect("pw")
	if v.reveal(sealed) != "pw" || v.info().How != "secret-service" {
		t.Fatalf("reveal %q, how %q", v.reveal(sealed), v.info().How)
	}
	key := strings.TrimSpace(vtRead(t, store))
	if _, err := parseKey(key); err != nil {
		t.Fatalf("the key stored by secret-tool: %q", key)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.key")); err == nil {
		t.Error("a key file was made although the Secret Service holds the key")
	}
	log := vtRead(t, calls)
	if !strings.Contains(log, "lookup app zipline") || !strings.Contains(log, "store --label=Zipline key for saved passwords app zipline") {
		t.Errorf("secret-tool was called as:\n%s", log)
	}
	if strings.Contains(log, key) {
		t.Error("the key was on secret-tool's command line")
	}
	// a later start finds the same key, makes no new one
	v2 := &vault{c: platformSealer(dir)}
	if v2.reveal(sealed) != "pw" || strings.Count(vtRead(t, calls), "store") != 1 {
		t.Error("the second start didn't reuse the key")
	}

	// secret-tool gone on a later run: the sealed value is kept; new secrets get a key file, and
	// both open once secret-tool is back
	t.Setenv("PATH", "/nonexistent")
	v3 := &vault{c: platformSealer(dir)}
	if v3.reveal(sealed) != sealed || !v3.info().Locked {
		t.Fatalf("without secret-tool the sealed value must stay as it is")
	}
	other := v3.protect("new")
	if !isSealed(other) || v3.info().How != "file" {
		t.Fatalf("new secrets: %q (%s)", other, v3.info().How)
	}
	if st, err := os.Stat(filepath.Join(dir, "vault.key")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	t.Setenv("PATH", filepath.Dir(store)+string(os.PathListSeparator)+"/usr/bin:/bin")
	v4 := &vault{c: platformSealer(dir)}
	if v4.reveal(sealed) != "pw" || v4.reveal(other) != "new" || v4.info().Locked {
		t.Error("with secret-tool back, values sealed under either key should open")
	}
	if re := v4.protect("again"); v4.info().How != "secret-service" || !isSealed(re) {
		t.Errorf("new secrets should use the Secret Service again: %s", v4.info().How)
	}
}

func TestFailingSecretServiceNeverMakesAKey(t *testing.T) {
	_, calls := vtSecretTool(t, "Cannot autolaunch D-Bus without X11 $DISPLAY")
	dir := vtDir(t)
	v := &vault{c: platformSealer(dir)}
	if got := v.protect("pw"); got != "pw" || !v.info().Plain {
		t.Fatalf("with the Secret Service failing, the text must stay as it is: %q", got)
	}
	if strings.Contains(vtRead(t, calls), "store") {
		t.Error("a key was stored although the lookup failed")
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.key")); err == nil {
		t.Error("a key file was made although the Secret Service may hold the key")
	}
	if n := v.sealNote(); !strings.Contains(n, "Secret Service said") {
		t.Errorf("note %q", n)
	}
}

func TestNoSessionMeansKeyFile(t *testing.T) {
	vtSecretTool(t, "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir := vtDir(t)
	v := &vault{c: platformSealer(dir)}
	if s := v.protect("pw"); !isSealed(s) || v.info().How != "file" {
		t.Fatalf("%q %s", s, v.info().How)
	}
}
