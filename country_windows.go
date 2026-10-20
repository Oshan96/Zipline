//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	pGetUserDefaultGeoName    = kernel32.NewProc("GetUserDefaultGeoName")    // Windows 10 1709 and later
	pGetUserDefaultLocaleName = kernel32.NewProc("GetUserDefaultLocaleName") // Vista and later
)

// systemCountry is Windows' "Country or region" setting (Settings → Time & language → Region),
// else the territory of the user's locale ("en-LK").
func systemCountry() string {
	buf := make([]uint16, 85) // LOCALE_NAME_MAX_LENGTH
	if pGetUserDefaultGeoName.Find() == nil {
		if n, _, _ := pGetUserDefaultGeoName.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); n > 0 {
			if c := countryCode(syscall.UTF16ToString(buf)); c != "" { // ("001", the world, when unset)
				return c
			}
		}
	}
	if pGetUserDefaultLocaleName.Find() == nil {
		if n, _, _ := pGetUserDefaultLocaleName.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); n > 0 {
			return localeCountry(syscall.UTF16ToString(buf))
		}
	}
	return ""
}
