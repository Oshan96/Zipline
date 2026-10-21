//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The host's manifests go where each installed browser looks, name this program and only Zipline's
// extension, and are rewritten only when something changed.
func TestNativeHostManifests(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".config")
	for _, d := range []string{filepath.Join(config, "chromium"), filepath.Join(config, "BraveSoftware", "Brave-Browser"), filepath.Join(home, ".mozilla")} {
		os.MkdirAll(d, 0o755)
	}
	exe := "/opt/Zipline/zipline"
	if errs := writeHostManifests(exe, nmDirs(home, config, false)); len(errs) > 0 {
		t.Fatal(errs)
	}
	read := func(p string) nmManifest {
		t.Helper()
		var m nmManifest
		b, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(b, &m) != nil {
			t.Fatalf("%s: %v", p, err)
		}
		return m
	}
	for _, p := range []string{filepath.Join(config, "chromium", "NativeMessagingHosts", nmHostName+".json"), filepath.Join(config, "BraveSoftware", "Brave-Browser", "NativeMessagingHosts", nmHostName+".json")} {
		m := read(p)
		if m.Name != nmHostName || m.Path != exe || m.Type != "stdio" || len(m.AllowedOrigins) != 1 || m.AllowedOrigins[0] != "chrome-extension://"+chromeExtIDs[0]+"/" || m.AllowedExtensions != nil {
			t.Errorf("%s: %+v", p, m)
		}
	}
	ff := read(filepath.Join(home, ".mozilla", "native-messaging-hosts", nmHostName+".json"))
	if ff.Path != exe || len(ff.AllowedExtensions) != 1 || ff.AllowedExtensions[0] != firefoxExtID || ff.AllowedOrigins != nil {
		t.Errorf("Firefox: %+v", ff)
	}
	if _, err := os.Stat(filepath.Join(config, "google-chrome")); !os.IsNotExist(err) {
		t.Error("a folder was made for a browser that isn't installed")
	}
	// again: nothing rewritten; moved: rewritten
	p := filepath.Join(config, "chromium", "NativeMessagingHosts", nmHostName+".json")
	old := time.Now().Add(-time.Hour)
	os.Chtimes(p, old, old)
	writeHostManifests(exe, nmDirs(home, config, false))
	if st, _ := os.Stat(p); !st.ModTime().Equal(old) {
		t.Error("an unchanged manifest was written again")
	}
	writeHostManifests("/elsewhere/zipline", nmDirs(home, config, false))
	if read(p).Path != "/elsewhere/zipline" {
		t.Error("the manifest still names the old place")
	}
	mac := nmDirs("/Users/me", "", true)
	if mac[0].hosts != "/Users/me/Library/Application Support/Google/Chrome/NativeMessagingHosts" || !mac[len(mac)-1].firefox || mac[len(mac)-1].hosts != "/Users/me/Library/Application Support/Mozilla/NativeMessagingHosts" {
		t.Errorf("macOS folders: %+v", mac)
	}
}
