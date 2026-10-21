//go:build windows

package main

import (
	"math"
	"syscall"
	"unsafe"
)

var (
	pGetDiskFreeSpaceExW   = kernel32.NewProc("GetDiskFreeSpaceExW")
	pGetVolumePathNameW    = kernel32.NewProc("GetVolumePathNameW")
	pGetVolumeInformationW = kernel32.NewProc("GetVolumeInformationW")
)

// diskInfo reports the space dir's drive has for this user (disk quotas count), the drive's
// file system ("NTFS", "FAT32", "exFAT", "ReFS"), and the largest file it can hold (0 = no
// limit).
func diskInfo(dir string) (free int64, fsName string, maxFile int64, err error) {
	d, err := syscall.UTF16PtrFromString(existingDir(dir))
	if err != nil {
		return 0, "", 0, err
	}
	var avail, total, totalFree uint64
	if r, _, e := pGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(d)), uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&totalFree))); r == 0 {
		return 0, "", 0, e
	}
	if root, err := syscall.UTF16PtrFromString(volumeRoot(dir)); err == nil {
		buf := make([]uint16, 64)
		if r, _, _ := pGetVolumeInformationW.Call(uintptr(unsafe.Pointer(root)), 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r != 0 {
			fsName = syscall.UTF16ToString(buf)
		}
	}
	return int64(min(avail, math.MaxInt64)), fsName, fileSizeLimit(fsName), nil
}

// volumeRoot is the drive or mounted folder dir is on, with its closing backslash ("D:\",
// `\\server\share\`).
func volumeRoot(dir string) string {
	d := existingDir(dir)
	p, err := syscall.UTF16PtrFromString(d)
	if err != nil {
		return d
	}
	buf := make([]uint16, 1024)
	if r, _, _ := pGetVolumePathNameW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); r == 0 {
		return d
	}
	return syscall.UTF16ToString(buf)
}
