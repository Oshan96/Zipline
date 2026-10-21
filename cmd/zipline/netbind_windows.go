//go:build windows

package main

import "syscall"

// IP_UNICAST_IF makes Windows send through that adapter whatever the routing table says.
func bindToInterface(fd uintptr, i netIf) error {
	idx := uint32(i.Index)
	be := idx>>24 | (idx>>8)&0xFF00 | (idx<<8)&0xFF0000 | idx<<24 // network byte order
	syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 31, int(be))
	return nil // the local address alone usually suffices; don't fail the connection
}
