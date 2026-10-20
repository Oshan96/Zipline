//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"
)

// preallocate makes f size bytes long and reserves the disk space for it up front, so a full
// disk shows up now (ENOSPC) instead of halfway through, and the file isn't fragmented by
// segments landing all over it. fallocate(2) mode 0 only marks the blocks as unwritten
// extents, which is instant on ext4, XFS and btrfs. File systems without it (FAT, exFAT,
// older NFS, many FUSE ones) get a plain truncate, as before.
func preallocate(f *os.File, size int64) error {
	if size > 0 {
		rc, err := f.SyscallConn()
		if err != nil {
			return err
		}
		var ferr error
		if err := rc.Control(func(fd uintptr) {
			for {
				ferr = syscall.Fallocate(int(fd), 0, 0, size)
				if ferr != syscall.EINTR { // a large reservation can be interrupted by Go's own signals
					break
				}
			}
		}); err != nil {
			return err
		}
		switch {
		case ferr == nil:
			if st, err := f.Stat(); err == nil && st.Size() == size {
				return nil
			}
		case !errors.Is(ferr, syscall.EOPNOTSUPP) && !errors.Is(ferr, syscall.ENOSYS):
			return ferr // ENOSPC, EFBIG and the like: the download can't fit
		}
	}
	return f.Truncate(size)
}

// finishAllocation has nothing to undo here: Linux files are never marked sparse.
func finishAllocation(f *os.File) error { return nil }
