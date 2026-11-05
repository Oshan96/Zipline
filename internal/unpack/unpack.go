// Package unpack unpacks downloaded archives (zip, tar, tar.gz, tar.bz2) into a folder next to them,
// safely: nothing is written outside that folder, links and devices are left out, names are made safe
// for any system, and it stops before the disk fills up. The folder appears only once everything is in
// it.
package unpack

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"zipline/internal/filename"
)

var (
	ErrEncrypted = errors.New("it's password-protected")
	ErrMethod    = errors.New("it uses a kind of compression Zipline can't unpack")
	ErrNoRoom    = errors.New("there isn't enough room on the disk")
	ErrTooBig    = errors.New("it holds a file bigger than this drive can store")
)

// kinds: an archive's format by the end of its name, longest first.
var kinds = []struct{ ext, kind string }{
	{".tar.gz", "tar.gz"}, {".tgz", "tar.gz"}, {".tar.bz2", "tar.bz2"}, {".tbz2", "tar.bz2"}, {".tbz", "tar.bz2"},
	{".tar", "tar"}, {".zip", "zip"},
}

// Kind is an archive's format from its file name: "zip", "tar", "tar.gz" or "tar.bz2", or "" for
// anything else.
func Kind(name string) string {
	n := strings.ToLower(name)
	for _, k := range kinds {
		if strings.HasSuffix(n, k.ext) {
			return k.kind
		}
	}
	return ""
}

// folderName is the archive's name without its archive ending: "photos" for photos.tar.gz.
func folderName(name string) string {
	n := strings.ToLower(name)
	for _, k := range kinds {
		if strings.HasSuffix(n, k.ext) {
			name = name[:len(name)-len(k.ext)]
			break
		}
	}
	if name = filename.CleanOrEmpty(name); name == "" {
		name = "unpacked"
	}
	return name
}

// TempDir is where Unpack puts the files until it's done: a folder next to the archive.
func TempDir(archive string) string {
	return filepath.Join(filepath.Dir(archive), ".unpacking-"+filepath.Base(archive))
}

type Options struct {
	Room     int64             // the most it may write (0: no limit)
	MaxFile  int64             // the biggest file the drive holds (0: no limit; FAT32 holds 4 GB)
	Mark     func(path string) // for each file written: the mark the archive had (downloaded from the internet)
	Progress func(pct int)
}

type Result struct {
	Dir     string // the folder it unpacked into
	Files   int
	Bytes   int64
	Skipped int // links, devices, and names that would have left the folder
}

// Unpack unpacks the archive into a new folder next to it: the one folder the archive holds, when it
// holds just one, or else a folder named after the archive. A name already taken gets " (1)" and so on.
func Unpack(ctx context.Context, archive string, o Options) (Result, error) {
	kind := Kind(filepath.Base(archive))
	if kind == "" {
		return Result{}, fmt.Errorf("%s isn't a zip or tar archive", filepath.Base(archive))
	}
	parent, tmp := filepath.Dir(archive), TempDir(archive)
	os.RemoveAll(tmp) // (one left by a run that was cut off)
	if err := os.Mkdir(tmp, 0o755); err != nil {
		return Result{}, err
	}
	w := &writer{ctx: ctx, root: tmp, o: o, lastPct: -1}
	var err error
	if kind == "zip" {
		err = w.zip(archive)
	} else {
		err = w.tar(archive, kind)
	}
	if err != nil {
		os.RemoveAll(tmp)
		return Result{}, err
	}
	src, final := tmp, ""
	if list, _ := os.ReadDir(tmp); len(list) == 1 && list[0].IsDir() {
		src, final = filepath.Join(tmp, list[0].Name()), unique(filepath.Join(parent, list[0].Name()), true)
	} else {
		final = unique(filepath.Join(parent, folderName(filepath.Base(archive))), true)
	}
	if err := os.Rename(src, final); err != nil {
		os.RemoveAll(tmp)
		return Result{}, err
	}
	os.RemoveAll(tmp) // (empty, or already moved)
	return Result{Dir: final, Files: w.files, Bytes: w.bytes, Skipped: w.skipped}, nil
}

type writer struct {
	ctx     context.Context
	root    string
	o       Options
	files   int
	bytes   int64
	skipped int
	total   int64        // what progress is measured against: the files' sizes (zip), the archive's (tar)
	read    func() int64 // how far a tar archive has been read (nil for zip: progress is what's written)
	lastPct int
}

// target is where an entry goes: its path made safe piece by piece, under root. "" when it would leave
// root (an absolute path, "..") or names nothing.
func (w *writer) target(name string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	if strings.HasPrefix(name, "/") || len(name) >= 3 && name[1] == ':' && name[2] == '/' { // (C:/…; a colon elsewhere is made safe)
		return ""
	}
	parts := []string{w.root}
	for _, p := range strings.Split(name, "/") {
		switch p {
		case "", ".":
			continue
		case "..":
			return ""
		}
		c := filename.CleanOrEmpty(p)
		if c == "" {
			return ""
		}
		parts = append(parts, c)
	}
	if len(parts) == 1 {
		return ""
	}
	return filepath.Join(parts...)
}

func (w *writer) dir(name string) error {
	p := w.target(name)
	if p == "" {
		w.skipped++
		return nil
	}
	return os.MkdirAll(p, 0o755)
}

func (w *writer) file(name string, mode fs.FileMode, mod time.Time, size int64, r io.Reader) error {
	p := w.target(name)
	if p == "" {
		w.skipped++
		return nil
	}
	if w.o.MaxFile > 0 && size > w.o.MaxFile {
		return ErrTooBig
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if errors.Is(err, fs.ErrExist) { // two names that came out the same once made safe (or differ only in case)
		p = unique(p, false)
		f, err = os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	}
	if err != nil {
		return err
	}
	err = w.copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if !mod.IsZero() {
		os.Chtimes(p, mod, mod)
	}
	if w.o.Mark != nil {
		w.o.Mark(p)
	}
	w.files++
	return nil
}

func (w *writer) copy(dst io.Writer, src io.Reader) error {
	buf := make([]byte, 256<<10)
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(buf)
		if n > 0 {
			if w.o.Room > 0 && w.bytes+int64(n) > w.o.Room {
				return ErrNoRoom
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			w.bytes += int64(n)
			w.progress()
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (w *writer) progress() {
	if w.o.Progress == nil || w.total <= 0 {
		return
	}
	done := w.bytes
	if w.read != nil {
		done = w.read()
	}
	if pct := int(min(done*100/w.total, 100)); pct != w.lastPct {
		w.lastPct = pct
		w.o.Progress(pct)
	}
}

func (w *writer) zip(archive string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil && !(errors.Is(err, zip.ErrInsecurePath) && zr != nil) { // (unsafe names are made safe here)
		return fmt.Errorf("it isn't a zip file Zipline can read (%v)", err)
	}
	defer zr.Close()
	zr.RegisterDecompressor(12, func(r io.Reader) io.ReadCloser { return io.NopCloser(bzip2.NewReader(r)) })
	for _, f := range zr.File {
		if f.Flags&0x1 != 0 {
			return ErrEncrypted
		}
		if f.Mode().IsRegular() {
			w.total += int64(f.UncompressedSize64)
		}
	}
	if w.o.Room > 0 && w.total > w.o.Room {
		return ErrNoRoom
	}
	for _, f := range zr.File {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			err = w.dir(f.Name)
		case !mode.IsRegular(): // links and the like
			w.skipped++
		default:
			var rc io.ReadCloser
			if rc, err = f.Open(); err != nil {
				if errors.Is(err, zip.ErrAlgorithm) {
					return ErrMethod
				}
				return err
			}
			err = w.file(f.Name, mode, f.Modified, int64(f.UncompressedSize64), rc) // (a damaged file fails its CRC here)
			rc.Close()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (w *writer) tar(archive, kind string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		w.total = st.Size()
	}
	c := &counter{r: bufio.NewReaderSize(f, 1<<20)}
	w.read = func() int64 { return c.n }
	var r io.Reader = c
	switch kind {
	case "tar.gz":
		gz, err := gzip.NewReader(c)
		if err != nil {
			return fmt.Errorf("it isn't a gzip file (%v)", err)
		}
		defer gz.Close()
		r = gz
	case "tar.bz2":
		r = bzip2.NewReader(c)
	}
	tr := tar.NewReader(r)
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("it isn't a tar archive Zipline can read (%v)", err)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = w.dir(h.Name)
		case tar.TypeReg, '\x00': // ('\x00': the oldest tars' regular file)
			err = w.file(h.Name, h.FileInfo().Mode(), h.ModTime, h.Size, tr)
		default: // links, devices, FIFOs
			w.skipped++
		}
		if err != nil {
			return err
		}
	}
}

// unique is p, or p with " (1)", " (2)"… (before a file's extension) when p is taken.
func unique(p string, dir bool) string {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return p
	}
	ext := ""
	if !dir {
		ext = filepath.Ext(p)
	}
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Lstat(c); errors.Is(err, fs.ErrNotExist) {
			return c
		}
	}
}
