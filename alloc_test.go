package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// B6: the part file gets its full size up front, keeps what segments write at both ends,
// and finishing the allocation changes nothing in it.
func TestPreallocateAndFinish(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.iso"+partSuffix)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const size = 8<<20 + 123
	if err := preallocate(f, size); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Stat(); st.Size() != size {
		t.Fatalf("size %d, want %d", st.Size(), size)
	}
	if _, err := f.WriteAt([]byte("tail"), size-4); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("head"), 0); err != nil {
		t.Fatal(err)
	}
	if err := finishAllocation(f); err != nil {
		t.Fatal(err)
	}
	if err := finishAllocation(f); err != nil { // twice is harmless
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != size || string(b[:4]) != "head" || string(b[size-4:]) != "tail" || !bytes.Equal(b[4:size-4], make([]byte, size-8)) {
		t.Fatalf("contents changed (len %d)", len(b))
	}

	// a file that already held more is cut to the new size
	if err := preallocate(f, 1000); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.Stat(); st.Size() != 1000 {
		t.Fatalf("size %d after shrinking, want 1000", st.Size())
	}
}
