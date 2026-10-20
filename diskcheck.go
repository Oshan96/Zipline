package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

// Checks made before a download starts, so a full drive, a FAT32 stick or a name the file
// system can't hold fails at once in plain words instead of after hours of downloading.

const (
	fatMaxFile = 1<<32 - 1 // FAT keeps file sizes in 32 bits
	maxNameLen = 255       // per file name: bytes on Linux and macOS, UTF-16 characters on Windows
	maxWinPath = 259       // MAX_PATH less the closing NUL: Explorer and most apps can't open longer paths
)

// checkDiskFor says, in words for the user, why name can't be saved in dir: the name or the
// full path is too long, the drive's file system can't hold a file of need bytes (FAT32), or
// there isn't room for it. need <= 0 means the size isn't known yet: only the names are
// checked. Nothing is reported when the drive can't be asked.
func checkDiskFor(dir, name string, need int64) error {
	if nameLen(name+partSuffix) > maxNameLen { // the part file must fit too
		return errors.New("the file name is too long for the file system — choose a shorter name")
	}
	if runtime.GOOS == "windows" {
		if p, err := filepath.Abs(filepath.Join(dir, name)); err == nil {
			if n := len(utf16.Encode([]rune(p))); n > maxWinPath {
				return fmt.Errorf("the full path is too long for Windows (%d characters) — choose a shorter folder or file name", n)
			}
		}
	}
	if need <= 0 {
		return nil
	}
	free, fs, maxFile, err := diskInfo(dir)
	if err != nil {
		return nil // can't tell: the download will find out
	}
	return diskVerdict(volumeRoot(dir), fs, free, maxFile, need)
}

// diskVerdict is checkDiskFor's decision once the drive's figures are known.
func diskVerdict(where, fs string, free, maxFile, need int64) error {
	if maxFile > 0 && need > maxFile {
		return fmt.Errorf("the drive at %s uses %s, which can't store files over 4 GB (this one is %s) — choose a folder on another drive", where, fsDisplayName(fs), humanSize(need))
	}
	if need > free {
		return fmt.Errorf("not enough space on %s (needs %s, %s free) — free up space or choose another folder", where, humanSize(need), humanSize(free))
	}
	return nil
}

// fileSizeLimit is the largest file the named file system can hold, 0 for no practical limit.
func fileSizeLimit(fs string) int64 {
	switch strings.ToLower(fs) {
	case "vfat", "msdos", "fat", "fat12", "fat16", "fat32":
		return fatMaxFile
	}
	return 0
}

// fsDisplayName names a file system the way users know it (Linux calls FAT32 "vfat").
func fsDisplayName(fs string) string {
	switch strings.ToLower(fs) {
	case "vfat", "msdos", "fat32":
		return "FAT32"
	}
	return fs
}

// nameLen measures a name the way the file system limits it.
func nameLen(s string) int {
	if runtime.GOOS == "windows" {
		return len(utf16.Encode([]rune(s)))
	}
	return len(s)
}

// existingDir is dir or its nearest folder that exists: the download's folder may only be
// created when it starts, but it will be on the same drive as its parent.
func existingDir(dir string) string {
	d, err := filepath.Abs(dir)
	if err != nil {
		d = dir
	}
	for {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
		up := filepath.Dir(d)
		if up == d {
			return dir
		}
		d = up
	}
}

// mulFree multiplies a block count by the block size without overflowing (network file
// systems sometimes report absurd numbers).
func mulFree(blocks, size uint64) int64 {
	if size == 0 {
		return 0
	}
	if blocks > math.MaxInt64/size {
		return math.MaxInt64
	}
	return int64(blocks * size)
}
