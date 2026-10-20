//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// J2: the origin and referrer go into the freedesktop.org attributes, without credentials.
func TestMarkFromInternetLinux(t *testing.T) {
	p := filepath.Join(t.TempDir(), "setup.run")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := setOriginAttrs(p, "https://user:pw@dl.example.com/setup.run?v=2", "https://example.com/get")
	if errors.Is(err, syscall.ENOTSUP) {
		t.Skip("the temp folder's file system doesn't keep user attributes")
	}
	if err != nil {
		t.Fatal(err)
	}
	get := func(name string) string {
		buf := make([]byte, 1024)
		n, err := syscall.Getxattr(p, name, buf)
		if err != nil {
			return "error: " + err.Error()
		}
		return string(buf[:n])
	}
	if got := get("user.xdg.origin.url"); got != "https://dl.example.com/setup.run?v=2" {
		t.Errorf("origin %q", got)
	}
	if got := get("user.xdg.referrer.url"); got != "https://example.com/get" {
		t.Errorf("referrer %q", got)
	}

	// a torrent: no web address to record, and nothing fails
	q := filepath.Join(t.TempDir(), "file.mkv")
	os.WriteFile(q, nil, 0o644)
	markFromInternet(q, "magnet:?xt=urn:btih:abc", "")
	if _, err := syscall.Getxattr(q, "user.xdg.origin.url", make([]byte, 64)); err == nil {
		t.Error("magnet link recorded")
	}
	markFromInternet(filepath.Join(t.TempDir(), "missing"), "https://example.com/x", "") // must not panic
}
