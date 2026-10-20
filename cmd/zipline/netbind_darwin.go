//go:build darwin

package main

import "syscall"

// IP_BOUND_IF pins the connection to the adapter.
func bindToInterface(fd uintptr, i netIf) error {
	if i.Index > 0 {
		syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, 25, i.Index)
	}
	return nil
}
