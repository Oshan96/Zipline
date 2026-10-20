package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// Registering the native-messaging host (nativehost.go) with the browsers: a small manifest per
// browser naming this program and the one extension allowed to start it. Chromium browsers find it
// in their NativeMessagingHosts folder (macOS, Linux) or through a registry key (Windows), Firefox
// in its own. It's done at every start and only rewrites what changed, so the manifests always name
// this copy of Zipline wherever it was moved. Browsers that aren't installed are left alone.

type nmManifest struct {
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Path              string   `json:"path"`
	Type              string   `json:"type"`
	AllowedOrigins    []string `json:"allowed_origins,omitempty"`    // Chromium browsers
	AllowedExtensions []string `json:"allowed_extensions,omitempty"` // Firefox
}

func nmManifestJSON(exe string, firefox bool) []byte {
	m := nmManifest{Name: nmHostName, Description: "Zipline Download Manager (browser extension link)", Path: exe, Type: "stdio"}
	if firefox {
		m.AllowedExtensions = []string{firefoxExtID}
	} else {
		for _, id := range chromeExtIDs {
			m.AllowedOrigins = append(m.AllowedOrigins, "chrome-extension://"+id+"/")
		}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	return append(b, '\n')
}

// writeIfChanged writes b to p unless p already holds exactly that.
func writeIfChanged(p string, b []byte) error {
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, b) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// registerNativeHost registers this executable as the host for every installed browser.
func registerNativeHost(dataDir string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	for _, err := range registerNativeHostFor(exe, dataDir) {
		logf("native messaging: %v", err)
	}
}
