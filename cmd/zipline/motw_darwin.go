//go:build darwin

package main

import (
	"errors"
	"path/filepath"
	"syscall"
	"unsafe"
)

// markFromInternet: the quarantine flag Gatekeeper checks needs nothing here. The app's
// Info.plist sets LSFileQuarantineEnabled, so macOS quarantines every file Zipline creates
// (the flag stays through the rename from the part file). What's added is Finder's "Where
// from": the download's address and the page it came from, as Safari and Chrome store them.
// Best effort: FAT and exFAT drives keep no attributes. (Only when started as Zipline.app is
// the quarantine flag set; the bare command-line binary gets the "Where from" alone.)
func markFromInternet(path, srcURL, referrer string) {
	var froms []string
	for _, u := range []string{srcURL, referrer} {
		if o, ok := originURL(u); ok {
			froms = append(froms, o)
		}
	}
	if len(froms) == 0 {
		return
	}
	if err := setxattr(path, "com.apple.metadata:kMDItemWhereFroms", whereFroms(froms)); err != nil && !errors.Is(err, syscall.ENOTSUP) {
		logf("mark of the web: %s: %v", filepath.Base(path), err)
	}
}

// setxattr calls setxattr(2) directly: the syscall package has no wrapper for it on macOS.
func setxattr(path, name string, value []byte) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	n, err := syscall.BytePtrFromString(name)
	if err != nil {
		return err
	}
	_, _, e := syscall.Syscall6(syscall.SYS_SETXATTR, uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(n)),
		uintptr(unsafe.Pointer(&value[0])), uintptr(len(value)), 0, 0) // position 0, no options
	if e != 0 {
		return e
	}
	return nil
}
