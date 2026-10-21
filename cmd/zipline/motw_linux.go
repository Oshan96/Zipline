//go:build linux

package main

import (
	"errors"
	"path/filepath"
	"syscall"
)

// markFromInternet records where a download came from in the extended attributes of the
// freedesktop.org shared file metadata spec, as Chrome and Firefox do; file managers show
// them. Best effort: tmpfs, FAT and some network drives don't keep user attributes.
func markFromInternet(path, srcURL, referrer string) {
	if err := setOriginAttrs(path, srcURL, referrer); err != nil && !errors.Is(err, syscall.ENOTSUP) {
		logf("mark of the web: %s: %v", filepath.Base(path), err)
	}
}

func setOriginAttrs(path, srcURL, referrer string) error {
	if u, ok := originURL(srcURL); ok {
		if err := syscall.Setxattr(path, "user.xdg.origin.url", []byte(u), 0); err != nil {
			return err
		}
	}
	if r, ok := originURL(referrer); ok {
		return syscall.Setxattr(path, "user.xdg.referrer.url", []byte(r), 0)
	}
	return nil
}
