package main

import (
	"os"
	"regexp"
	"syscall"
	"testing"
	"unsafe"
)

// The program carries version info (rsrc_windows_*.syso, packaging/windows/mksyso.py), read here the way
// Explorer reads it: Properties → Details, and code signing services that check a file is what it says.
func TestVersionInfo(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ver := syscall.NewLazyDLL("version.dll")
	p, _ := syscall.UTF16PtrFromString(exe)
	n, _, err := ver.NewProc("GetFileVersionInfoSizeW").Call(uintptr(unsafe.Pointer(p)), 0)
	if n == 0 {
		t.Fatalf("no version info: %v", err)
	}
	buf := make([]byte, n)
	if ok, _, err := ver.NewProc("GetFileVersionInfoW").Call(uintptr(unsafe.Pointer(p)), 0, n, uintptr(unsafe.Pointer(&buf[0]))); ok == 0 {
		t.Fatal(err)
	}
	query := func(key string) (unsafe.Pointer, uint32) {
		k, _ := syscall.UTF16PtrFromString(key)
		var at unsafe.Pointer
		var size uint32
		if ok, _, _ := ver.NewProc("VerQueryValueW").Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(&at)), uintptr(unsafe.Pointer(&size))); ok == 0 || at == nil {
			return nil, 0
		}
		return at, size
	}
	text := func(name string) string {
		at, size := query(`\StringFileInfo\040904b0\` + name)
		if at == nil {
			return ""
		}
		return syscall.UTF16ToString(unsafe.Slice((*uint16)(at), size))
	}
	if got := text("ProductName"); got != "Zipline" {
		t.Errorf("ProductName %q", got)
	}
	if got := text("ProductVersion"); !regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}$`).MatchString(got) { // (a release build has its own: packaging/windows/build-exes.sh)
		t.Errorf("ProductVersion %q", got)
	}
	if got := text("OriginalFilename"); got != "Zipline.exe" {
		t.Errorf("OriginalFilename %q", got)
	}
	at, size := query(`\`)
	if at == nil || size < 52 || *(*uint32)(at) != 0xFEEF04BD {
		t.Fatal("no fixed version info")
	}
}
