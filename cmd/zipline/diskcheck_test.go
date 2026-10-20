package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// C6: the temp folder's drive answers, and a folder that doesn't exist yet is looked up
// through its parent.
func TestDiskInfo(t *testing.T) {
	dir := t.TempDir()
	free, fs, maxFile, err := diskInfo(filepath.Join(dir, "not", "made", "yet"))
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("no disk information on " + runtime.GOOS)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("free %s, file system %q, max file %d, volume %s", humanSize(free), fs, maxFile, volumeRoot(dir))
	if free <= 0 || fs == "" {
		t.Fatalf("free %d, file system %q", free, fs)
	}
	if root := volumeRoot(dir); root == "" || !strings.HasPrefix(strings.ToLower(dir), strings.ToLower(strings.TrimRight(root, `\/`))) {
		t.Fatalf("volume %q for %s", root, dir)
	}
}

func TestCheckDiskFor(t *testing.T) {
	dir := t.TempDir()
	if err := checkDiskFor(dir, "small.zip", 1<<20); err != nil {
		t.Fatalf("1 MB: %v", err)
	}
	if err := checkDiskFor(dir, "unknown.bin", 0); err != nil {
		t.Fatalf("unknown size: %v", err)
	}
	if free, _, _, err := diskInfo(dir); err == nil {
		err := checkDiskFor(filepath.Join(dir, "new folder"), "huge.iso", 1<<52)
		if err == nil || !strings.HasPrefix(err.Error(), "not enough space on ") || !strings.Contains(err.Error(), "needs 4096.0 TB, "+humanSize(free)) ||
			!strings.HasSuffix(err.Error(), " free) — free up space or choose another folder") {
			t.Fatalf("huge file: %v", err)
		}
	}
	long := strings.Repeat("a", 248) + ".iso" // fits Linux's 255 bytes, but its part file doesn't
	if err := checkDiskFor(dir, long, 0); err == nil || !strings.Contains(err.Error(), "file name is too long") {
		t.Fatalf("long name: %v", err)
	}
	// 130 accented letters: 264 bytes with the extension, but only 134 UTF-16 characters
	if err := checkDiskFor(dir, strings.Repeat("é", 130)+".iso", 0); (err == nil) != (runtime.GOOS == "windows") {
		t.Fatalf("130 accented letters: %v", err)
	}
	if runtime.GOOS == "windows" {
		deep := filepath.Join(dir, strings.Repeat("d", 120), strings.Repeat("e", 120))
		err := checkDiskFor(deep, "file.zip", 0)
		if err == nil || !strings.HasPrefix(err.Error(), "the full path is too long for Windows (") {
			t.Fatalf("long path: %v", err)
		}
	}
}

func TestDiskVerdict(t *testing.T) {
	const gb = 1 << 30
	err := diskVerdict("/media/stick", "vfat", 20*gb, fileSizeLimit("vfat"), 5*gb)
	if err == nil || err.Error() != "the drive at /media/stick uses FAT32, which can't store files over 4 GB (this one is 5.0 GB) — choose a folder on another drive" {
		t.Fatalf("FAT32: %v", err)
	}
	if err := diskVerdict(`E:\`, "FAT32", 20*gb, fileSizeLimit("FAT32"), fatMaxFile); err != nil {
		t.Fatalf("exactly 4 GiB-1 on FAT32: %v", err)
	}
	err = diskVerdict(`D:\`, "NTFS", gb+gb/10, fileSizeLimit("NTFS"), 4*gb+gb/5)
	if err == nil || err.Error() != `not enough space on D:\ (needs 4.2 GB, 1.1 GB free) — free up space or choose another folder` {
		t.Fatalf("NTFS: %v", err)
	}
	for _, fs := range []string{"exfat", "exFAT", "NTFS", "ext4", "apfs"} {
		if fileSizeLimit(fs) != 0 {
			t.Errorf("%s has no 4 GB limit", fs)
		}
	}
	for _, fs := range []string{"vfat", "msdos", "FAT", "FAT32"} {
		if fileSizeLimit(fs) != fatMaxFile {
			t.Errorf("%s has a 4 GB limit", fs)
		}
	}
}
