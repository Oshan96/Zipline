package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// tstorage maps the torrent's byte range onto its files. Bytes that belong to files you didn't
// choose (but share a piece with ones you did) go into a hidden ".parts" file instead, so the
// piece can still be checked and shared. Padding files (BEP 47) are all zeros and never written.

type tstorage struct {
	mi    *Metainfo
	root  string // folder the files go in (the torrent's own folder for multi-file torrents)
	skip  []bool
	parts string
	mu    sync.Mutex
	open  map[int]*os.File
	pf    *os.File
}

func newStorage(mi *Metainfo, dir string, skip []bool, ihHex string) *tstorage {
	root := dir
	if mi.Multi {
		root = filepath.Join(dir, mi.Name)
	}
	return &tstorage{mi: mi, root: root, skip: skip, open: map[int]*os.File{},
		parts: filepath.Join(dir, "."+mi.Name+"."+ihHex[:8]+".parts")}
}

func (s *tstorage) filePath(i int) string {
	f := s.mi.Files[i]
	if !s.mi.Multi {
		return filepath.Join(s.root, f.Path[len(f.Path)-1])
	}
	return filepath.Join(append([]string{s.root}, f.Path...)...)
}

// file opens a file for reading and writing (creating it at full size, sparse, when it's new).
// Caller holds s.mu.
func (s *tstorage) file(i int) (*os.File, error) {
	if s.skip[i] {
		if s.pf == nil {
			f, err := os.OpenFile(s.parts, os.O_RDWR|os.O_CREATE, 0o644)
			if err != nil {
				return nil, err
			}
			makeSparse(f)
			s.pf = f
		}
		return s.pf, nil
	}
	if f := s.open[i]; f != nil {
		return f, nil
	}
	p := s.filePath(i)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err == nil && st.Size() < s.mi.Files[i].Length {
		preallocate(f, s.mi.Files[i].Length) // (pieces arrive in any order)
	} else if err == nil && st.Size() > s.mi.Files[i].Length {
		f.Truncate(s.mi.Files[i].Length) // a longer file was there: what's past the end isn't the torrent's
	}
	if len(s.open) >= 64 { // don't keep too many files open
		for k, of := range s.open {
			of.Close()
			delete(s.open, k)
			break
		}
	}
	s.open[i] = f
	return f, nil
}

// span calls fn for each file piece of [off, off+n).
func (s *tstorage) span(off int64, n int, fn func(i int, fileOff int64, a, b int) error) error {
	fs := s.mi.Files
	i := sort.Search(len(fs), func(k int) bool { return fs[k].Offset+fs[k].Length > off })
	pos := 0
	for pos < n && i < len(fs) {
		f := fs[i]
		if f.Length == 0 {
			i++
			continue
		}
		fo := off + int64(pos) - f.Offset
		take := int(min(int64(n-pos), f.Length-fo))
		if err := fn(i, fo, pos, pos+take); err != nil {
			return err
		}
		pos += take
		i++
	}
	if pos < n {
		return errors.New("beyond the end of the torrent")
	}
	return nil
}

func (s *tstorage) WriteAt(p []byte, off int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.span(off, len(p), func(i int, fo int64, a, b int) error {
		if s.mi.Files[i].Pad {
			return nil
		}
		f, err := s.file(i)
		if err != nil {
			return err
		}
		at := fo
		if s.skip[i] {
			at = s.mi.Files[i].Offset + fo
		}
		_, err = f.WriteAt(p[a:b], at)
		return err
	})
}

func (s *tstorage) ReadAt(p []byte, off int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.span(off, len(p), func(i int, fo int64, a, b int) error {
		if s.mi.Files[i].Pad {
			clear(p[a:b])
			return nil
		}
		f, err := s.file(i)
		if err != nil {
			return err
		}
		at := fo
		if s.skip[i] {
			at = s.mi.Files[i].Offset + fo
		}
		n, err := f.ReadAt(p[a:b], at)
		if err == io.EOF && n == b-a {
			err = nil
		}
		if err == io.EOF { // never written: zeros (the piece check finds out)
			clear(p[a+n : b])
			err = nil
		}
		return err
	})
}

// setSkip changes which files are wanted (new files are created when first written).
// finished runs when every chosen file is complete: Windows' sparse flag comes off (so an ISO or
// VHD mounts) and each file is marked as coming from the internet, like a browser download.
func (s *tstorage) finished(src, ref string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, tf := range s.mi.Files {
		if s.skip[i] || tf.Pad {
			continue
		}
		if f, err := s.file(i); err == nil {
			finishAllocation(f)
		}
		markFromInternet(s.filePath(i), src, ref)
	}
}

func (s *tstorage) setSkip(skip []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skip = skip
}

func (s *tstorage) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, f := range s.open {
		f.Close()
		delete(s.open, k)
	}
	if s.pf != nil {
		s.pf.Close()
		s.pf = nil
	}
}

// removeParts deletes the hidden file for bytes of files that weren't chosen.
func (s *tstorage) removeParts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pf != nil {
		s.pf.Close()
		s.pf = nil
	}
	os.Remove(s.parts)
}

// pieceFiles lists the files a piece touches.
func (mi *Metainfo) pieceFiles(piece int) []int {
	off := int64(piece) * mi.PieceLen
	end := off + mi.PieceSize(piece)
	var out []int
	for i, f := range mi.Files {
		if f.Length > 0 && f.Offset < end && off < f.Offset+f.Length {
			out = append(out, i)
		}
	}
	return out
}
