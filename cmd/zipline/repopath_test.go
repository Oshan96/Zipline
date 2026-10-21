package main

import (
	"os"
	"path/filepath"
)

// repoPath turns a path from the repository's root (web/…, extension/…) into one the tests can
// open: they run in cmd/zipline, or wherever the test binary was started (under Wine, say).
func repoPath(name string) string {
	d, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return filepath.Join(d, filepath.FromSlash(name))
		}
		up := filepath.Dir(d)
		if up == d {
			return filepath.FromSlash(name)
		}
		d = up
	}
}
