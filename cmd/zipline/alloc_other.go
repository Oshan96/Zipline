//go:build !linux && !windows

package main

import "os"

// preallocate sets the file's size. On macOS that's all it should do: APFS is copy-on-write,
// so blocks reserved now would be replaced by new ones when the data arrives anyway, and the
// unwritten part costs no space until then.
func preallocate(f *os.File, size int64) error { return f.Truncate(size) }

func finishAllocation(f *os.File) error { return nil }
