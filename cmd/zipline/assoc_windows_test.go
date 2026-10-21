//go:build windows

package main

import (
	"os"
	"strings"
	"testing"
)

// Changes HKEY_CURRENT_USER, so it only runs when asked (ZL_ASSOC_TEST=1, e.g. under Wine).
func TestTorrentAssociationWindows(t *testing.T) {
	if os.Getenv("ZL_ASSOC_TEST") == "" {
		t.Skip("changes the registry; set ZL_ASSOC_TEST=1")
	}
	if err := setTorrentAssociation(true); err != nil {
		t.Fatal(err)
	}
	if sup, on := torrentAssociation(); !sup || !on {
		t.Fatalf("after switching on: supported=%v on=%v", sup, on)
	}
	cmd := regGetString(`Software\Classes\magnet\shell\open\command`, "")
	exe, _ := os.Executable()
	if !strings.HasPrefix(cmd, `"`+exe+`"`) || !strings.HasSuffix(cmd, `"%1"`) {
		t.Fatalf("command: %q", cmd)
	}
	if regGetString(`Software\Classes\magnet`, "URL Protocol") != "" || regGetString(`Software\Classes\Zipline.Torrent\shell\open\command`, "") != cmd {
		t.Fatal("protocol or file type not registered")
	}
	// another app's .torrent registration is left alone when switching off
	if err := setTorrentAssociation(false); err != nil {
		t.Fatal(err)
	}
	if _, on := torrentAssociation(); on {
		t.Fatal("still on")
	}
	if regGetString(`Software\Classes\magnet\shell\open\command`, "") != "" || regGetString(`Software\Classes\Zipline.Torrent\shell\open\command`, "") != "" {
		t.Fatal("keys left behind")
	}
	regSetString(`Software\Classes\magnet\shell\open\command`, "", `"C:\Other\app.exe" "%1"`)
	setTorrentAssociation(false)
	if regGetString(`Software\Classes\magnet\shell\open\command`, "") == "" {
		t.Fatal("removed another app's magnet handler")
	}
	regDeleteTree(`Software\Classes\magnet`)
}
