package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"zipline/internal/filename"
)

// Shared folders (Google Drive, OneDrive, Yandex Disk, pCloud, MEGA): the Add dialog lists the files in
// one like a metalink's, to download all of them (keeping the folders, inside a folder named after
// the shared one) or just one. Each file becomes a download of its own, with a link that resolves to
// that file, so it resumes like any share link.

type cloudFile struct {
	Path string // inside the shared folder, "/" between folders
	Size int64  // -1 when the service doesn't say
	URL  string // a link to just this file
}

type cloudFolder struct {
	Service string
	Name    string
	Files   []cloudFile
	More    bool // the list was cut short (too many files, or folders too deep)
}

const (
	maxFolderFiles = 2000
	maxFolderDepth = 8
)

var errNotFolder = errors.New("not a shared folder")

// listCloudFolder lists a shared folder's files; errNotFolder for a link to a single file.
func listCloudFolder(ctx context.Context, raw string) (*cloudFolder, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errNotFolder
	}
	switch cloudService(raw) {
	case "Google Drive":
		if gdriveFolderID(u) != "" {
			return gdriveFolder(ctx, u)
		}
	case "OneDrive":
		return onedriveFolder(ctx, raw)
	case "Yandex Disk":
		return yandexFolder(ctx, raw)
	case "pCloud":
		return pcloudFolder(ctx, raw)
	case "MEGA":
		return megaFolder(ctx, raw)
	}
	return nil, errNotFolder
}

// Folders the Add dialog has listed, so adding doesn't list them again.
var folderCache = struct {
	sync.Mutex
	m map[string]folderEntry
}{m: map[string]folderEntry{}}

type folderEntry struct {
	f  *cloudFolder
	at time.Time
}

func cacheCloudFolder(u string, f *cloudFolder) {
	folderCache.Lock()
	defer folderCache.Unlock()
	for k, e := range folderCache.m {
		if time.Since(e.at) > 30*time.Minute {
			delete(folderCache.m, k)
		}
	}
	folderCache.m[u] = folderEntry{f, time.Now()}
}

func cachedCloudFolder(u string) *cloudFolder {
	folderCache.Lock()
	defer folderCache.Unlock()
	if e, ok := folderCache.m[u]; ok && time.Since(e.at) < 30*time.Minute {
		return e.f
	}
	return nil
}

// probeCloudFolder describes a shared folder's files for the Add dialog.
func (s *Server) probeCloudFolder(w http.ResponseWriter, ctx context.Context, d *Download) {
	cf, err := listCloudFolder(ctx, d.URL)
	if err == nil && len(cf.Files) == 0 {
		err = errors.New("the shared folder is empty")
	}
	if err != nil {
		writeJSON(w, map[string]any{"error": downloadErrText(err, d.URL), "service": cloudService(d.URL)})
		return
	}
	cacheCloudFolder(d.URL, cf)
	type fv struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	list := make([]fv, 0, len(cf.Files))
	var total int64
	known := true
	for _, f := range cf.Files {
		list = append(list, fv{f.Path, f.Size})
		if f.Size >= 0 {
			total += f.Size
		} else {
			known = false
		}
	}
	if !known {
		total = -1
	}
	name, one := folderName(cf), filename.Clean(path.Base(cf.Files[0].Path))
	s.m.mu.Lock()
	cat := "General"
	if len(cf.Files) == 1 { // just one file: the dialog shows it as that file
		cat = s.m.categoryFor(one)
	}
	dir := s.m.dirFor(cat)
	s.m.mu.Unlock()
	res := map[string]any{"name": name, "size": total, "resumable": true, "category": cat, "dir": dir, "service": cf.Service,
		"folder": map[string]any{"name": name, "files": list, "total": total, "more": cf.More}}
	if len(cf.Files) == 1 {
		res["name"] = one
	}
	writeJSON(w, res)
}

// addCloudFolder adds a shared folder's files (all, or the one the dialog picked). False when the
// link isn't a shared folder.
func (s *Server) addCloudFolder(w http.ResponseWriter, r *http.Request, req AddReq) bool {
	cf := cachedCloudFolder(req.URL)
	if cf == nil {
		if cloudService(req.URL) == "" {
			return false
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		var err error
		if cf, err = listCloudFolder(ctx, req.URL); errors.Is(err, errNotFolder) {
			return false
		} else if err == nil && len(cf.Files) == 0 {
			err = errors.New("the shared folder is empty")
		}
		if err != nil {
			writeJSON(w, map[string]any{"error": "couldn't list the shared folder: " + downloadErrText(err, req.URL)})
			return true
		}
	}
	var ids []string
	for _, d := range s.m.AddCloudFolder(req, cf) {
		ids = append(ids, d.ID)
	}
	writeJSON(w, map[string]any{"id": ids[0], "ids": ids})
	return true
}

// AddCloudFolder adds a shared folder's files (one, or all with MetaFile -1). All of them go into a
// folder named after the shared one, with its folders inside.
func (m *Manager) AddCloudFolder(r AddReq, cf *cloudFolder) []*Download {
	pick := cf.Files
	if r.MetaFile >= 0 && r.MetaFile < len(cf.Files) {
		pick = cf.Files[r.MetaFile : r.MetaFile+1]
	}
	base := r.Dir
	if base == "" {
		m.mu.Lock()
		base = m.dirFor("General")
		m.mu.Unlock()
	}
	var out []*Download
	for _, f := range pick {
		rr := r
		rr.Meta, rr.URL, rr.Size, rr.Mirrors = false, f.URL, f.Size, nil
		if len(pick) > 1 {
			rr.Name, rr.Checksum = path.Base(f.Path), ""
			rr.Dir = filepath.Join(append([]string{base}, folderParts(folderName(cf)+"/"+path.Dir(f.Path))...)...)
		} else {
			if rr.Name == "" {
				rr.Name = path.Base(f.Path)
			}
			rr.Dir = base
		}
		out = append(out, m.Add(rr))
	}
	return out
}

// folderName is the shared folder's name as a folder on this computer.
func folderName(cf *cloudFolder) string {
	if n := filename.CleanOrEmpty(cf.Name); n != "" {
		return n
	}
	return cf.Service
}

// folderParts turns a path inside a shared folder into safe folder names (no "..", nothing a
// system forbids).
func folderParts(p string) []string {
	var out []string
	for _, s := range strings.Split(p, "/") {
		if s = filename.CleanOrEmpty(s); s != "" && s != "." && s != ".." {
			out = append(out, s)
		}
	}
	return out
}
