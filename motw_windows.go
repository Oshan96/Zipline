//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// markFromInternet writes the Zone.Identifier alternate data stream browsers write, so
// SmartScreen checks a downloaded program and Office opens a document in Protected View.
// Best effort, never failing a download. Skipped when policy says not to keep zone
// information, and on drives without alternate data streams.
func markFromInternet(path, srcURL, referrer string) {
	if zoneInfoOff() || !namedStreams(path) {
		return
	}
	if err := writeZoneID(path, srcURL, referrer); err != nil {
		logf("mark of the web: %s: %v", filepath.Base(path), err)
	}
}

func writeZoneID(path, srcURL, referrer string) error {
	return os.WriteFile(path+":Zone.Identifier", []byte(zoneIdentifier(srcURL, referrer)), 0o644)
}

// namedStreams says whether the drive holding path keeps alternate data streams. NTFS and
// ReFS do; FAT32 and exFAT don't, and Wine's drives would turn the stream into a stray file
// named "file:Zone.Identifier".
func namedStreams(path string) bool {
	root, err := syscall.UTF16PtrFromString(volumeRoot(filepath.Dir(path)))
	if err != nil {
		return false
	}
	var flags uint32
	r, _, _ := pGetVolumeInformationW.Call(uintptr(unsafe.Pointer(root)), 0, 0, 0, 0, uintptr(unsafe.Pointer(&flags)), 0, 0)
	return r != 0 && flags&fileNamedStreams != 0
}

const fileNamedStreams = 0x00040000

// zoneInfoOff: the "Do not preserve zone information in file attachments" policy
// (SaveZoneInformation = 1, for the user or the whole machine).
func zoneInfoOff() bool {
	const key = `Software\Microsoft\Windows\CurrentVersion\Policies\Attachments`
	for _, root := range []syscall.Handle{syscall.HKEY_CURRENT_USER, syscall.HKEY_LOCAL_MACHINE} {
		if v, ok := regDWORD(root, key, "SaveZoneInformation"); ok && v == 1 {
			return true
		}
	}
	return false
}

func regDWORD(root syscall.Handle, path, name string) (uint32, bool) {
	p, _ := syscall.UTF16PtrFromString(path)
	var k syscall.Handle
	if syscall.RegOpenKeyEx(root, p, 0, syscall.KEY_READ, &k) != nil {
		return 0, false
	}
	defer syscall.RegCloseKey(k)
	np, _ := syscall.UTF16PtrFromString(name)
	var v, typ uint32
	n := uint32(4)
	if syscall.RegQueryValueEx(k, np, nil, &typ, (*byte)(unsafe.Pointer(&v)), &n) != nil || typ != syscall.REG_DWORD {
		return 0, false
	}
	return v, true
}
