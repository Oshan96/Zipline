//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// B6: the part file is sparse while it downloads and a normal file once it's finished
// (so an ISO or VHD can be mounted).
func TestFinishAllocationClearsSparse(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "disk.vhd"+partSuffix))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	attrs := func() uint32 {
		var fi syscall.ByHandleFileInformation
		if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &fi); err != nil {
			t.Fatal(err)
		}
		return fi.FileAttributes
	}
	if err := preallocate(f, 4<<20); err != nil {
		t.Fatal(err)
	}
	if attrs()&fileAttributeSparse == 0 {
		t.Skip("the file system doesn't make sparse files (Wine ignores FSCTL_SET_SPARSE)")
	}
	f.WriteAt(make([]byte, 4<<20), 0)
	if err := finishAllocation(f); err != nil {
		t.Fatal(err)
	}
	if attrs()&fileAttributeSparse != 0 {
		t.Fatal("still sparse")
	}
}
