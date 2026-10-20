package main

import (
	"fmt"
	"os"
	"strings"
)

// Sharing a finished torrent again. A finished torrent shares until its ratio or time is reached,
// until you press Stop sharing, or until Zipline closes. "Share again" (and Resume, which does the
// same for a finished torrent) starts it once more, and start-up does it for the torrents that were
// sharing when Zipline closed (startup.go).
//
// The ratio counts from now, not from the first byte: a torrent that already uploaded twice its size
// would otherwise be over its ratio of 1 the moment it started, and sharing again would end at once.
// That matches the time limit, which also starts again. The ratio the window shows stays the whole
// one (everything uploaded over the size); only the point where sharing stops moves up with it.
//
// The files are looked at first. They may have been deleted, replaced or edited since, and peers
// would get pieces that don't match: then nothing is shared and the window says why.

// ShareError says why a finished torrent isn't shared. Why is "off" (its ratio or time is 0), "info"
// (the .torrent file is gone), or about the files: "missing", "changed", or "parts" (the hidden file
// that holds the bytes of files you didn't choose, which goes away when sharing ends).
type ShareError struct {
	Why  string `json:"why"`
	File string `json:"file,omitempty"` // the file that is missing or changed
}

func (e *ShareError) Error() string {
	switch e.Why {
	case "off":
		return "its sharing ratio or time is set to 0 (change it in Options → Torrents, or in its Properties)"
	case "info":
		return "Zipline no longer has the torrent's details — add the torrent again"
	case "missing":
		return fmt.Sprintf("\"%s\" is missing from its folder — Check files will download it again", e.File)
	case "changed":
		return fmt.Sprintf("\"%s\" has changed since it was downloaded — Check files will see if it is still good", e.File)
	}
	return "the part kept for the files you didn't choose is gone — Check files will download it again"
}

// ShareAgain starts sharing a finished torrent that isn't sharing, until its ratio or time (its own,
// or Options → Torrents') is reached from now. Anything else is left alone. When it can't share, it
// says why: in the returned error, in a notice, and in the torrent's row until it is tried again.
func (m *Manager) ShareAgain(id string) error {
	m.mu.Lock()
	d := m.byID[id]
	if d == nil || d.Torrent == nil || !d.Torrent.HasMeta || d.Status != StCompleted || d.running || m.torrents[id] != nil || moveActive(id) {
		m.mu.Unlock()
		return nil // (not a finished torrent, or it shares already)
	}
	d.noSeed = false
	ts := d.Torrent
	ratio, mins := seedTarget(m.settings.Torrent, ts)
	var mi *Metainfo
	if ih, ok := ts.hash(); ok {
		mi = m.loadTorrent(ih)
	}
	hash, dir := ts.InfoHash, d.Dir
	skip, saved := append([]bool(nil), ts.Skip...), append([]fileStamp(nil), ts.Stamps...)
	m.mu.Unlock()

	var why *ShareError
	switch {
	case ratio <= 0 || mins <= 0:
		why = &ShareError{Why: "off"}
	case mi == nil:
		why = &ShareError{Why: "info"}
	default:
		why = shareProblem(mi, dir, skip, saved, hash)
	}
	if why == nil && !m.reseedTorrent(d, true) {
		m.mu.Lock()
		sharing, name := m.torrents[id] != nil, d.FileName // (it is, when another call got there first)
		m.mu.Unlock()
		if !sharing {
			why = &ShareError{Why: "changed", File: name} // (a piece went missing since: the same trouble, found late)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d.shareNote = why
	if why == nil {
		return nil
	}
	m.notify("noShare", id, "Not sharing "+iso(d.FileName)+": "+why.Error())
	return why
}

// shareProblem looks at a finished torrent's files before they are shared again: each has to be
// there and as it was when the torrent was last saved, the same size and modified time (as checking
// files at start-up does: trecheck.go). Nil when they are.
func shareProblem(mi *Metainfo, dir string, skip []bool, saved []fileStamp, hash string) *ShareError {
	st := newStorage(mi, dir, skip, hash)
	chosen := func(i int) bool { return !mi.Files[i].Pad && !(i < len(skip) && skip[i]) }
	hidden := false // a piece shared with a file you didn't choose has that file's bytes in the parts file
	for i, f := range mi.Files {
		if f.Pad || f.Length == 0 {
			continue
		}
		if !chosen(i) {
			for _, p := range []int{int(f.Offset / mi.PieceLen), int((f.Offset + f.Length - 1) / mi.PieceLen)} {
				for _, j := range mi.pieceFiles(p) {
					hidden = hidden || chosen(j)
				}
			}
			continue
		}
		name := strings.Join(f.Path, "/")
		fi, err := os.Stat(st.filePath(i))
		switch {
		case err != nil:
			return &ShareError{Why: "missing", File: name}
		case fi.Size() != f.Length, len(saved) == len(mi.Files) && saved[i] != fileStamp{Size: fi.Size(), Mod: fi.ModTime().UnixNano()}:
			return &ShareError{Why: "changed", File: name}
		}
	}
	if _, err := os.Stat(st.parts); hidden && err != nil {
		return &ShareError{Why: "parts"}
	}
	return nil
}
