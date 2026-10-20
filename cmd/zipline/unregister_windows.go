//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// unregisterCopy undoes what this copy of Zipline registered with Windows: opening magnet links and
// .torrent files, and the browsers' native messaging host. The Windows uninstaller runs it
// (zipline --unregister) before it removes the files. What points at another copy, a portable one
// say, is left alone; that copy registers itself again when it next starts.
func unregisterCopy() {
	if cmd := assocCommand(); cmd != "" {
		if regGetString(`Software\Classes\magnet\shell\open\command`, "") == cmd {
			regDeleteTree(`Software\Classes\magnet`)
		}
		if regGetString(`Software\Classes\`+assocProgID+`\shell\open\command`, "") == cmd {
			setTorrentAssociation(false) // (the .torrent registration with it)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	unregisterNativeHostFor(exe, appDataDir())
}

// unregisterNativeHostFor removes the native messaging host's registrations and manifests when
// they lead to exe (registerNativeHostFor wrote them for the copy that ran last).
func unregisterNativeHostFor(exe, dataDir string) {
	chromium, firefox := filepath.Join(dataDir, "nmhost", nmHostName+".json"), filepath.Join(dataDir, "nmhost", "firefox", nmHostName+".json")
	b, err := os.ReadFile(chromium)
	if err != nil {
		return
	}
	var m nmManifest
	if json.Unmarshal(b, &m) != nil || !strings.EqualFold(filepath.Clean(m.Path), filepath.Clean(exe)) {
		return
	}
	for _, r := range nmRegKeys {
		key := r.vendor + `\NativeMessagingHosts\` + nmHostName
		if v := regGetString(key, ""); strings.EqualFold(v, chromium) || strings.EqualFold(v, firefox) {
			regDeleteTree(key)
		}
	}
	os.Remove(chromium)
	os.Remove(firefox)
}
