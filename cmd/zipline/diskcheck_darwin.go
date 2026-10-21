//go:build darwin

package main

import "syscall"

// fsTypeName: "apfs", "hfs", "msdos" (FAT), "exfat", "smbfs", "nfs" and so on.
func fsTypeName(st *syscall.Statfs_t) string { return cString(st.Fstypename[:]) }

// volumeRoot is the mount point dir is on, for messages.
func volumeRoot(dir string) string {
	d := existingDir(dir)
	var st syscall.Statfs_t
	if syscall.Statfs(d, &st) != nil {
		return d
	}
	return cString(st.Mntonname[:])
}

func cString(b []int8) string {
	s := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		s = append(s, byte(c))
	}
	return string(s)
}
