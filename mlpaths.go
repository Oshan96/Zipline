package main

import (
	"path"
	"path/filepath"
	"strings"
)

// A metalink's file names can carry folders ("dir/sub/file.iso"): the file is saved in those
// folders under the one chosen in the Add dialog. Every part is cleaned like any other name, and
// nothing can climb out of the chosen folder: "..", ".", empty parts and a leading drive ("C:")
// are dropped, and an absolute path becomes a relative one.

// metalinkPath splits a metalink file name into its folders ("a/b", "" = none) and its name
// ("" when nothing usable is left).
func metalinkPath(raw string) (dir, name string) {
	parts := safeParts(raw)
	if len(parts) == 0 {
		return "", ""
	}
	return strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1]
}

// safeParts cleans each folder and name of a path written with either slash.
func safeParts(raw string) []string {
	var out []string
	for i, p := range strings.FieldsFunc(strings.TrimSpace(raw), func(r rune) bool { return r == '/' || r == '\\' }) {
		if i == 0 && len(p) == 2 && p[1] == ':' { // C:\… names a drive, not a folder
			continue
		}
		if p = safeComponent(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// path is the file's place inside the chosen folder, with forward slashes.
func (f mlFile) path() string { return path.Join(f.Dir, f.Name) }

// metalinkName is the path the Add dialog suggests for a metalink file saved under dir: its
// folders and a name no other file there has. Caller holds m.mu.
func (m *Manager) metalinkName(dir string, f mlFile) string {
	return path.Join(f.Dir, m.uniqueName(filepath.Join(dir, filepath.FromSlash(f.Dir)), f.Name, nil))
}

// metalinkPlace turns the folder chosen in the Add dialog and a metalink file's path (as the
// dialog sends it back: it may have been edited) into the download's folder and name. No folder
// chosen means the folder for the file's type.
func (m *Manager) metalinkPlace(dir, rel string) (string, string) {
	parts := safeParts(rel)
	if len(parts) < 2 {
		return dir, rel // no folders: the name is cleaned as usual
	}
	name := parts[len(parts)-1]
	if dir == "" {
		m.mu.Lock()
		dir = m.dirFor(m.categoryFor(name))
		m.mu.Unlock()
	}
	return filepath.Join(append([]string{dir}, parts[:len(parts)-1]...)...), name
}
