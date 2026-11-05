package unpack

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type entry struct {
	name, body string
	mode       fs.FileMode
}

func writeZip(t *testing.T, path string, entries []entry, edit func(*zip.FileHeader)) {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)}
		if e.mode != 0 {
			h.SetMode(e.mode)
		}
		if edit != nil {
			edit(h)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(e.body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTarGz(t *testing.T, path string, entries []entry) {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Size: int64(len(e.body)), Mode: 0o644, ModTime: time.Unix(1700000000, 0)}
		switch {
		case e.mode&fs.ModeSymlink != 0:
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.body, 0
		case strings.HasSuffix(e.name, "/"):
			h.Typeflag, h.Size = tar.TypeDir, 0
		case e.mode&0o111 != 0:
			h.Mode = 0o755
		}
		tw.WriteHeader(h)
		if h.Size > 0 {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// files lists what's under dir as "relative/path=content".
func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel)+"="+string(b))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func noLeftovers(t *testing.T, archive string) {
	t.Helper()
	if _, err := os.Stat(TempDir(archive)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the unpacking folder was left behind")
	}
}

func TestKind(t *testing.T) {
	for name, want := range map[string]string{"a.zip": "zip", "A.ZIP": "zip", "b.tar": "tar", "c.tar.gz": "tar.gz", "d.TGZ": "tar.gz",
		"e.tar.bz2": "tar.bz2", "f.tbz2": "tar.bz2", "g.gz": "", "h.7z": "", "i.rar": "", "zip": "", "movie.mp4": ""} {
		if got := Kind(name); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
	if got := folderName("Photos 2024.tar.gz"); got != "Photos 2024" {
		t.Errorf("folderName: %q", got)
	}
}

// An archive holding one folder unpacks as that folder; one with loose files gets a folder named after
// it; a name already taken gets " (1)". Each file is marked as the archive was, and keeps its time.
func TestUnpackFolders(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "release.zip")
	writeZip(t, one, []entry{{"tool-1.2/", "", 0}, {"tool-1.2/bin/run.sh", "#!/bin/sh\n", 0o755}, {"tool-1.2/README", "read me", 0}}, nil)
	var marked []string
	var last int
	res, err := Unpack(context.Background(), one, Options{Mark: func(p string) { marked = append(marked, filepath.Base(p)) }, Progress: func(p int) { last = p }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dir != filepath.Join(dir, "tool-1.2") || res.Files != 2 || res.Skipped != 0 {
		t.Fatalf("one folder: %+v", res)
	}
	if got := files(t, res.Dir); strings.Join(got, "|") != "README=read me|bin/run.sh=#!/bin/sh\n" {
		t.Fatalf("files: %q", got)
	}
	if len(marked) != 2 || last != 100 {
		t.Errorf("marked %v, progress ended at %d", marked, last)
	}
	if fi, err := os.Stat(filepath.Join(res.Dir, "README")); err != nil || !fi.ModTime().Equal(time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)) {
		t.Errorf("the file's time: %v %v", fi.ModTime(), err)
	}
	noLeftovers(t, one)

	loose := filepath.Join(dir, "photos.tar.gz")
	writeTarGz(t, loose, []entry{{"a.jpg", "A", 0}, {"b.jpg", "B", 0}, {"more/", "", 0}, {"more/c.jpg", "C", 0}})
	os.Mkdir(filepath.Join(dir, "photos"), 0o755) // (taken)
	res, err = Unpack(context.Background(), loose, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dir != filepath.Join(dir, "photos (1)") || res.Files != 3 {
		t.Fatalf("loose files: %+v", res)
	}
	if got := files(t, res.Dir); strings.Join(got, "|") != "a.jpg=A|b.jpg=B|more/c.jpg=C" {
		t.Fatalf("files: %q", got)
	}
	noLeftovers(t, loose)
}

// Nothing lands outside the folder: "..", absolute paths and drive letters are left out, and so are
// links (a link could point anywhere, and the next file written "through" it would follow it).
func TestUnpackStaysInside(t *testing.T) {
	dir := t.TempDir()
	evil := filepath.Join(dir, "evil.zip")
	writeZip(t, evil, []entry{{"../escaped.txt", "x", 0}, {"/abs.txt", "x", 0}, {`C:\win.txt`, "x", 0}, {"ok/../../up.txt", "x", 0},
		{"ok/fine.txt", "fine", 0}, {"ok/link", "/etc/passwd", fs.ModeSymlink | 0o777}, {`back\slash.txt`, "bs", 0}}, nil)
	res, err := Unpack(context.Background(), evil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 5 {
		t.Errorf("skipped %d, want 5", res.Skipped)
	}
	got := files(t, dir)
	want := []string{"evil.zip=" + string(mustRead(t, evil)), "evil/back/slash.txt=bs", "evil/ok/fine.txt=fine"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("on disk: %q", got)
	}
	tgz := filepath.Join(dir, "links.tar.gz")
	writeTarGz(t, tgz, []entry{{"pkg/", "", 0}, {"pkg/real.txt", "real", 0}, {"pkg/to-etc", "/etc", fs.ModeSymlink}, {"../../out.txt", "x", 0}})
	res, err = Unpack(context.Background(), tgz, Options{})
	if err != nil || res.Skipped != 2 || res.Files != 1 {
		t.Fatalf("tar: %+v %v", res, err)
	}
	if fi, err := os.Lstat(filepath.Join(res.Dir, "to-etc")); err == nil {
		t.Errorf("a link was made: %v", fi.Mode())
	}
}

func mustRead(t *testing.T, p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Names are made safe for any system (an archive made elsewhere can hold names Windows can't).
func TestUnpackSafeNames(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "names.zip")
	writeZip(t, a, []entry{{"what?.txt", "1", 0}, {"a:b.txt", "2", 0}, {"CON.txt", "3", 0}}, nil)
	res, err := Unpack(context.Background(), a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files(t, res.Dir) {
		if strings.ContainsAny(strings.SplitN(f, "=", 2)[0], `?:`) || strings.HasPrefix(strings.ToLower(f), "con.txt=") {
			t.Errorf("an unsafe name was kept: %q", f)
		}
	}
	if res.Files != 3 {
		t.Errorf("%d files (%+v): %q", res.Files, res, files(t, res.Dir))
	}
}

// What it can't unpack fails cleanly, with nothing left behind.
func TestUnpackRefuses(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.zip")
	writeZip(t, locked, []entry{{"secret.txt", "s", 0}}, func(h *zip.FileHeader) { h.Flags |= 0x1 })
	if _, err := Unpack(context.Background(), locked, Options{}); !errors.Is(err, ErrEncrypted) {
		t.Errorf("password-protected: %v", err)
	}
	noLeftovers(t, locked)

	odd := filepath.Join(dir, "odd.zip")
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	w, _ := zw.CreateRaw(&zip.FileHeader{Name: "x.bin", Method: 98, CompressedSize64: 3, UncompressedSize64: 3})
	w.Write([]byte("abc"))
	zw.Close()
	os.WriteFile(odd, b.Bytes(), 0o644)
	if _, err := Unpack(context.Background(), odd, Options{}); !errors.Is(err, ErrMethod) {
		t.Errorf("unknown compression: %v", err)
	}
	noLeftovers(t, odd)

	big := filepath.Join(dir, "big.tar.gz")
	writeTarGz(t, big, []entry{{"a.bin", strings.Repeat("a", 4000), 0}, {"b.bin", strings.Repeat("b", 4000), 0}})
	if _, err := Unpack(context.Background(), big, Options{Room: 5000}); !errors.Is(err, ErrNoRoom) {
		t.Errorf("no room (tar): %v", err)
	}
	bigZip := filepath.Join(dir, "big.zip")
	writeZip(t, bigZip, []entry{{"a.bin", strings.Repeat("a", 4000), 0}, {"b.bin", strings.Repeat("b", 4000), 0}}, nil)
	if _, err := Unpack(context.Background(), bigZip, Options{Room: 5000}); !errors.Is(err, ErrNoRoom) {
		t.Errorf("no room (zip, before writing): %v", err)
	}
	if _, err := Unpack(context.Background(), bigZip, Options{MaxFile: 1000}); !errors.Is(err, ErrTooBig) {
		t.Errorf("a file too big for the drive: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Unpack(ctx, bigZip, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	notZip := filepath.Join(dir, "fake.zip")
	os.WriteFile(notZip, []byte("<html>not a zip</html>"), 0o644)
	if _, err := Unpack(context.Background(), notZip, Options{}); err == nil {
		t.Error("a web page saved as .zip unpacked")
	}
	for _, a := range []string{big, bigZip, notZip} {
		noLeftovers(t, a)
	}
	if list, _ := os.ReadDir(dir); len(list) != 5 {
		t.Errorf("something was left in the folder: %v", list)
	}
}

// tarBz2 holds notes/readme.txt, notes/sub/data.bin and a link (made with Python's tarfile and bz2:
// Go can read bzip2 but not write it).
const tarBz2 = "QlpoOTFBWSZTWaNOZpoAAZT/////////////////////////////////////////////wAKbiQoASYACYAAmAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACTAATAAEwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGklN6QU9RtTNCaaAxMTQ0GEPUZNGgNDTINBo0NMCek2oHqNBoaBo0aGjTIDQaA0MgMCGgMjI0NAADRoMmjTTINGgFVUIm1E/UDSZGDQA0GoDABHpMmAGjRDamhk00YDQBPJMJpkwTCYGpkZMIwQxGmIyMmJpiNDJgmAJgCYE2hGmP2uXwkQ5g5gZUYiBIJQACH+I13F2XleCYQTGmBnZABMRETMGBGkUgTwgwzwSFPSCnx4B4RW5+T0oBQShFDI4jaIUUoxRykFJKUUsphHlNKcU8qBIHiE7gX2/eOeQeSVEqRUyqFVKsVcrBWStDqAtiHcDLW/OrjBWLoRAWQlgQBEiYgWQyMEiQLAAIuwMlDuYgYHCyKUup0b4gBgggnYAYBhI4eSSA4BwcFYCSgHUotDIRZCylmLOWgki0lqJMtZbDzjzz0C2koeiW49I9M9QoNvKYQrgR56pcT1i5EY9ckD2C5l0LqXYu5eC8kqRF5vRWoUDA3qWkSRrEZImC8uuEOSlYhgJtAhzkiX8mcCQycy8sS0uRIkuRkpOkAEwsYGeW1Z1dhIlKgCygMpMCvQgKiSNBJwEXzRHQJFQSAEdBoinxxEIvCEVRiIULCkmYY9sw57hiDEnumKMWYwxpjj3jHmQMiZI98yZlDKmWMuZgmD4DMmaPhPiM2Zw+MzpnjPmgPkPlNCaI0ZpDSnzHzn0GmNOag+k+o1J9ZqjVmsPsPtNaa4+4+815sD8DYmyNmfibQ/I2ptjbn5n6G4P1P2P3Nybo/g3ZvDem+N+fycA4JwjhnEOKcY45yD+jknKOWcw5pzjnnQOif2dI6Z1DqmW6xjTrn+HYJpMkGYOwdk7R2z/T/iGgQIHcO4f+d07x3xdyRThQkKNOZpo="

func TestUnpackTarBz2(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "notes.tar.bz2")
	b, _ := base64.StdEncoding.DecodeString(tarBz2)
	os.WriteFile(a, b, 0o644)
	res, err := Unpack(context.Background(), a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dir != filepath.Join(dir, "notes") || res.Files != 2 || res.Skipped != 1 {
		t.Fatalf("%+v", res)
	}
	if got := mustRead(t, filepath.Join(res.Dir, "readme.txt")); string(got) != "hello from bz2\n" {
		t.Errorf("readme: %q", got)
	}
	if fi, err := os.Stat(filepath.Join(res.Dir, "sub", "data.bin")); err != nil || fi.Size() != 1024 {
		t.Errorf("data.bin: %v", err)
	}
}
