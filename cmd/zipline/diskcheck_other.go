//go:build !linux && !darwin && !windows

package main

import "errors"

func diskInfo(dir string) (free int64, fsName string, maxFile int64, err error) {
	return 0, "", 0, errors.New("not supported on this system")
}

func volumeRoot(dir string) string { return existingDir(dir) }
