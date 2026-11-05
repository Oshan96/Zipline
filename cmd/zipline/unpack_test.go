package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return b.Bytes()
}

func waitUnpacked(t *testing.T, m *Manager, d *Download, want string) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		m.mu.Lock()
		st, note := d.UnpackState, d.UnpackNote
		m.mu.Unlock()
		if st == want {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("unpack state %q (%s), want %q", st, note, want)
		}
	}
}

// With Options → File checks → Unpack on, a finished archive is unpacked next to it (the one folder it
// holds, here), the archive goes when asked to, and Open and Show in folder then lead to the folder.
func TestUnpackWhenDone(t *testing.T) {
	srv := testServer(zipOf(t, map[string]string{"pkg/readme.txt": "hello", "pkg/bin/tool": "x"}), 0, true)
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Unpack = UnpackSettings{On: true, Delete: true}
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.URL + "/pkg.zip", Start: true})
	waitFor(t, m, d, StCompleted, 10*time.Second)
	waitUnpacked(t, m, d, "done")
	m.mu.Lock()
	dir, note, archive := d.UnpackDir, d.UnpackNote, d.path()
	open := unpackedOrArchive(d, archive)
	var told bool
	for _, n := range m.notices {
		told = told || n.Kind == "unpacked" && n.ID == d.ID
	}
	m.mu.Unlock()
	if dir != filepath.Join(d.Dir, "pkg") || !strings.Contains(note, "2 files") || !strings.Contains(note, "archive was deleted") || !told {
		t.Fatalf("unpacked into %q: %q (told %v)", dir, note, told)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "readme.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("readme: %q %v", b, err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) || open != dir {
		t.Fatalf("the archive should be gone and Open lead to the folder: %v, %q", err, open)
	}

	// a download's own choice wins over the setting, and anything else isn't touched
	off := m.Add(AddReq{URL: srv.URL + "/other.zip"})
	m.mu.Lock()
	off.Unpack = "off"
	m.mu.Unlock()
	m.Start([]string{off.ID})
	movie := m.Add(AddReq{URL: srv.URL + "/movie.mp4", Start: true})
	waitFor(t, m, off, StCompleted, 10*time.Second)
	waitFor(t, m, movie, StCompleted, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	if off.UnpackState != "" || movie.UnpackState != "" {
		t.Errorf("unpacked against its choice (%q) or not an archive (%q)", off.UnpackState, movie.UnpackState)
	}
	m.mu.Unlock()
}

// A file that doesn't match its checksum is never unpacked, even when asked to by hand.
func TestUnpackNotWhenDamaged(t *testing.T) {
	srv := testServer(zipOf(t, map[string]string{"a.txt": "a"}), 0, true)
	defer srv.Close()
	m := newTestManager(t)
	m.mu.Lock()
	m.settings.Unpack.On = true
	m.mu.Unlock()
	d := m.Add(AddReq{URL: srv.URL + "/bad.zip", Start: true, Checksum: "sha256:" + strings.Repeat("0", 64)})
	waitFor(t, m, d, StCompleted, 10*time.Second)
	time.Sleep(200 * time.Millisecond)
	m.mu.Lock()
	st := d.UnpackState
	m.mu.Unlock()
	if st != "" {
		t.Fatalf("a damaged archive was unpacked: %q", st)
	}
	m.UnpackNow([]string{d.ID})
	waitUnpacked(t, m, d, "failed")
	m.mu.Lock()
	defer m.mu.Unlock()
	if !strings.Contains(d.UnpackNote, "checksum") {
		t.Errorf("note: %q", d.UnpackNote)
	}
	if _, err := os.Stat(filepath.Join(d.Dir, "bad")); !os.IsNotExist(err) {
		t.Error("it was unpacked")
	}
}
