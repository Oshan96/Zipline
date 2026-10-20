//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// B6: on Linux the space is really reserved (blocks allocated), not just a sparse size.
func TestPreallocateReservesBlocks(t *testing.T) {
	dir := t.TempDir()
	probe, err := os.Create(filepath.Join(dir, "probe"))
	if err != nil {
		t.Fatal(err)
	}
	supported := syscall.Fallocate(int(probe.Fd()), 0, 0, 4096) == nil
	probe.Close()

	f, err := os.Create(filepath.Join(dir, "big"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const size = 32 << 20
	if err := preallocate(f, size); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatal(err)
	}
	if st.Size != size {
		t.Fatalf("size %d, want %d", st.Size, size)
	}
	if !supported {
		t.Skipf("the temp folder's file system has no fallocate; got a plain truncated file (%d blocks)", st.Blocks)
	}
	if st.Blocks*512 < size {
		t.Fatalf("only %d bytes reserved for a %d-byte file", st.Blocks*512, size)
	}
}
