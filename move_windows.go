package main

import (
	"errors"
	"syscall"
)

// errNotSameDevice is Windows' ERROR_NOT_SAME_DEVICE: a rename between drives.
const errNotSameDevice = syscall.Errno(17)

// crossDevice: a rename failed because the two folders are on different drives.
func crossDevice(err error) bool { return errors.Is(err, errNotSameDevice) }

// errCrossDevice is what a rename between drives fails with (tests make it up).
var errCrossDevice error = errNotSameDevice
