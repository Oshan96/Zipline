//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The uninstaller's --unregister removes this copy's registrations and nothing that leads to another
// copy. Changes HKEY_CURRENT_USER, so it only runs when asked (ZL_ASSOC_TEST=1, e.g. under Wine).
func TestUnregisterCopy(t *testing.T) {
	if os.Getenv("ZL_ASSOC_TEST") == "" {
		t.Skip("changes the registry; set ZL_ASSOC_TEST=1")
	}
	chromeKey := `Software\Google\Chrome\NativeMessagingHosts\` + nmHostName
	t.Cleanup(func() {
		regDeleteTree(chromeKey)
		regDeleteTree(`Software\Classes\magnet`)
		setTorrentAssociation(false)
	})

	// the native messaging host: left alone while it leads to another copy
	dir := t.TempDir()
	here, other := `C:\Users\me\AppData\Local\Programs\Zipline\Zipline.exe`, `D:\Portable\Zipline.exe`
	if errs := registerNativeHostFor(other, dir); len(errs) > 0 {
		t.Fatal(errs)
	}
	manifest := filepath.Join(dir, "nmhost", nmHostName+".json")
	unregisterNativeHostFor(here, dir)
	if regGetString(chromeKey, "") != manifest {
		t.Fatal("removed the host another copy registered")
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatal("removed another copy's manifest")
	}
	if errs := registerNativeHostFor(here, dir); len(errs) > 0 {
		t.Fatal(errs)
	}
	unregisterNativeHostFor(`c:\users\ME\AppData\Local\Programs\Zipline\Zipline.exe`, dir) // (paths compare without case)
	if regGetString(chromeKey, "") != "" {
		t.Fatal("the host is still registered")
	}
	for _, p := range []string{manifest, filepath.Join(dir, "nmhost", "firefox", nmHostName+".json")} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s is still there", p)
		}
	}

	// magnet links and .torrent files: this copy's go, another app's magnet handler stays
	if err := setTorrentAssociation(true); err != nil {
		t.Fatal(err)
	}
	unregisterCopy()
	if _, on := torrentAssociation(); on || regGetString(`Software\Classes\`+assocProgID+`\shell\open\command`, "") != "" {
		t.Fatal("the association is still there")
	}
	regSetString(`Software\Classes\magnet\shell\open\command`, "", `"C:\Other\app.exe" "%1"`)
	unregisterCopy()
	if regGetString(`Software\Classes\magnet\shell\open\command`, "") == "" {
		t.Fatal("removed another app's magnet handler")
	}
}
