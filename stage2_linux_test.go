//go:build linux

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Finished downloads say where they came from (Linux: the freedesktop xattrs browsers write).
func TestFinishedFileIsMarkedFromTheInternet(t *testing.T) {
	data := randData(300 << 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "f.bin", time.Now(), bytes.NewReader(data))
	}))
	defer srv.Close()
	m := newTestManager(t)
	d := m.Add(AddReq{URL: srv.URL + "/f.bin?token=abc", Start: true, Headers: map[string]string{"Referer": "https://example.com/page"}})
	waitFor(t, m, d, StCompleted, 20*time.Second)
	buf := make([]byte, 512)
	n, err := syscall.Getxattr(filepath.Join(d.Dir, d.FileName), "user.xdg.origin.url", buf)
	if err == syscall.ENOTSUP || err == syscall.EOPNOTSUPP {
		t.Skip("this file system has no user xattrs")
	}
	if err != nil || string(buf[:n]) != srv.URL+"/f.bin?token=abc" {
		t.Fatalf("origin: %q %v", buf[:n], err)
	}
}
