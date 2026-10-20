//go:build windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// J2: the Zone.Identifier stream is written next to the file's data, not as another file.
func TestMarkFromInternetWindows(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "setup.exe")
	if err := os.WriteFile(p, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !namedStreams(p) {
		markFromInternet(p, "https://example.com/setup.exe", "")
		if names, _ := os.ReadDir(dir); len(names) != 1 {
			t.Fatalf("a drive without streams got %d files", len(names))
		}
		t.Skip("this drive has no alternate data streams (FAT, or Wine): nothing written, as it should be")
	}
	if err := writeZoneID(p, "https://user:pw@dl.example.com/setup.exe", "https://example.com/download"); err != nil {
		t.Fatal(err)
	}
	if names, _ := os.ReadDir(dir); len(names) != 1 {
		t.Fatalf("the stream became a separate file (%d entries)", len(names))
	}
	b, err := os.ReadFile(p + ":Zone.Identifier")
	if err != nil {
		t.Fatal(err)
	}
	if want := "[ZoneTransfer]\r\nZoneId=3\r\nReferrerUrl=https://example.com/download\r\nHostUrl=https://dl.example.com/setup.exe\r\n"; string(b) != want {
		t.Fatalf("stream %q", b)
	}
	if data, _ := os.ReadFile(p); string(data) != "MZ" {
		t.Fatal("file contents changed")
	}
	markFromInternet(filepath.Join(dir, "missing.exe"), "https://example.com/x", "") // must not panic
}

// Changes a policy in HKEY_CURRENT_USER, so it only runs when asked (ZL_REG_TEST=1, e.g. under Wine).
func TestZonePolicy(t *testing.T) {
	if os.Getenv("ZL_REG_TEST") == "" {
		t.Skip("changes the registry; set ZL_REG_TEST=1")
	}
	const key = `Software\Microsoft\Windows\CurrentVersion\Policies\Attachments`
	set := func(v uint32) {
		var k syscall.Handle
		p, _ := syscall.UTF16PtrFromString(key)
		if r, _, _ := pRegCreateKeyExW.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p)), 0, 0, 0, keyWrite, 0, uintptr(unsafe.Pointer(&k)), 0); r != 0 {
			t.Fatal(syscall.Errno(r))
		}
		defer syscall.RegCloseKey(k)
		n, _ := syscall.UTF16PtrFromString("SaveZoneInformation")
		if r, _, _ := pRegSetValueExW.Call(uintptr(k), uintptr(unsafe.Pointer(n)), 0, syscall.REG_DWORD, uintptr(unsafe.Pointer(&v)), 4); r != 0 {
			t.Fatal(syscall.Errno(r))
		}
	}
	t.Cleanup(func() {
		p, _ := syscall.UTF16PtrFromString(key)
		n, _ := syscall.UTF16PtrFromString("SaveZoneInformation")
		pRegDeleteKeyValue.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(n)))
	})
	set(1)
	if v, ok := regDWORD(syscall.HKEY_CURRENT_USER, key, "SaveZoneInformation"); !ok || v != 1 {
		t.Fatalf("read back %d %v", v, ok)
	}
	if !zoneInfoOff() {
		t.Fatal("policy 1 (don't preserve zone information) not honoured")
	}
	set(2)
	if zoneInfoOff() {
		t.Fatal("policy 2 (preserve) read as off")
	}
}
