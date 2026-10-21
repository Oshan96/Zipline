//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// Windows 10 (2004) and later report the cost of the current connection: phone hotspots and
// Wi-Fi networks set as metered come back as Fixed or Variable.

var (
	iphlpapi          = syscall.NewLazyDLL("iphlpapi.dll")
	pConnectivityHint = iphlpapi.NewProc("GetNetworkConnectivityHint")
)

type nlConnectivityHint struct {
	level       int32
	cost        int32
	approaching byte
	over        byte
	roaming     byte
	_           byte
}

func systemMetered() (metered, known bool) {
	if pConnectivityHint.Find() != nil {
		return false, false // older Windows
	}
	var h nlConnectivityHint
	if r, _, _ := pConnectivityHint.Call(uintptr(unsafe.Pointer(&h))); r != 0 {
		return false, false
	}
	switch h.cost {
	case 2, 3: // fixed, variable
		return true, true
	case 1: // unrestricted
		return false, true
	}
	return false, false
}
