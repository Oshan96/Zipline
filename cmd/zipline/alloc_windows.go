//go:build windows

package main

import (
	"os"
	"syscall"
)

const (
	fsctlSetSparse      = 0x000900c4
	fileAttributeSparse = 0x200
)

// preallocate keeps the sparse approach on Windows. Reserving the space for real (setting
// the end of a normal file) leaves NTFS's valid data length at 0, so the first write far
// into the file makes NTFS zero-fill everything before it, synchronously, inside that one
// write (Raymond Chen, "Why does my single-byte write take forever?"). With segments
// writing all over a large file that stalls the download. A sparse file has no gap to fill:
// unwritten ranges are simply unallocated.
func preallocate(f *os.File, size int64) error {
	makeSparse(f)
	return f.Truncate(size)
}

// finishAllocation turns the finished file back into a normal one. Windows refuses to
// mount a sparse ISO or VHD ("virtual hard disk files must be uncompressed and unencrypted
// and must not be sparse"), and some backup and sync tools treat sparse files differently.
// The file is complete, so there are no holes left to fill and clearing the flag is quick.
func finishAllocation(f *os.File) error {
	h := syscall.Handle(f.Fd())
	var fi syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &fi); err != nil {
		return err
	}
	if fi.FileAttributes&fileAttributeSparse == 0 {
		return nil // never made sparse (FAT32, exFAT) or already cleared
	}
	in := [1]byte{0} // FILE_SET_SPARSE_BUFFER{SetSparse: FALSE}
	var ret uint32
	return syscall.DeviceIoControl(h, fsctlSetSparse, &in[0], 1, nil, 0, &ret, nil)
}
