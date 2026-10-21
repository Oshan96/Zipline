//go:build linux || darwin

package main

import "syscall"

// diskInfo reports the space dir's drive has for this user (not counting the part kept for
// root), the drive's file system name, and the largest file it can hold (0 = no limit).
func diskInfo(dir string) (free int64, fsName string, maxFile int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(existingDir(dir), &st); err != nil {
		return 0, "", 0, err
	}
	fsName = fsTypeName(&st)
	return mulFree(uint64(st.Bavail), uint64(st.Bsize)), fsName, fileSizeLimit(fsName), nil
}
