//go:build linux

package main

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// linuxFS names the file systems statfs(2) reports by magic number (linux/magic.h).
var linuxFS = map[uint32]string{
	0x4d44:     "vfat", // FAT12/16/32 (msdos and vfat)
	0x2011bab0: "exfat",
	0x5346544e: "ntfs",
	0x7366746e: "ntfs3",
	0xef53:     "ext4", // ext2, ext3 and ext4 share it
	0x9123683e: "btrfs",
	0x58465342: "xfs",
	0xf2f52010: "f2fs",
	0x2fc12fc1: "zfs",
	0xca451a4e: "bcachefs",
	0x01021994: "tmpfs",
	0x858458f6: "ramfs",
	0x794c7630: "overlay",
	0x65735546: "fuse",
	0x6969:     "nfs",
	0xff534d42: "cifs",
	0xfe534d42: "smb2",
	0x517b:     "smb",
	0x01021997: "9p", // WSL's Windows drives, VM shared folders
	0x9660:     "iso9660",
	0x15013346: "udf",
	0x4244:     "hfs",
	0x482b:     "hfsplus",
	0x73717368: "squashfs",
	0x52654973: "reiserfs",
	0x3153464a: "jfs",
	0x3434:     "nilfs",
	0x00c36400: "ceph",
	0xf15f:     "ecryptfs",
}

func fsTypeName(st *syscall.Statfs_t) string {
	t := uint32(st.Type)
	if n, ok := linuxFS[t]; ok {
		return n
	}
	return fmt.Sprintf("0x%x", t)
}

// volumeRoot is the mount point dir is on (where the device changes going up), for messages.
func volumeRoot(dir string) string {
	d := existingDir(dir)
	var st syscall.Stat_t
	if syscall.Stat(d, &st) != nil {
		return d
	}
	for {
		up := filepath.Dir(d)
		var su syscall.Stat_t
		if up == d || syscall.Stat(up, &su) != nil || su.Dev != st.Dev {
			return d
		}
		d = up
	}
}
