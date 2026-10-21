//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// crossDevice: a rename failed because the two folders are on different drives.
func crossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// errCrossDevice is what a rename between drives fails with (tests make it up).
var errCrossDevice error = syscall.EXDEV
