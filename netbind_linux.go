//go:build linux

package main

import "syscall"

// SO_BINDTODEVICE pins the connection to the adapter (allowed for normal users on recent
// kernels); the local address is set either way.
func bindToInterface(fd uintptr, i netIf) error {
	if i.Name != "" {
		syscall.BindToDevice(int(fd), i.Name)
	}
	return nil
}
