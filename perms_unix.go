//go:build !windows

package main

import (
	"io/fs"
	"os"
	"path/filepath"
)

// makePrivateDir creates the data folder, and makes it (an older version made it 0755) private.
func makePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// tightenDataDir fixes what an older version left behind: folders 0700, files 0600. The browser
// profile of the app window is the browser's own business, and links are never followed.
func tightenDataDir(dir string) {
	filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if e.IsDir() {
			if p != dir && e.Name() == "window" && filepath.Dir(p) == dir {
				return filepath.SkipDir
			}
			tighten(p, 0o700)
		} else if e.Type().IsRegular() {
			tighten(p, privateFile)
		}
		return nil
	})
}

func tighten(p string, want os.FileMode) {
	if st, err := os.Lstat(p); err == nil && st.Mode().Perm() != want {
		os.Chmod(p, want)
	}
}
