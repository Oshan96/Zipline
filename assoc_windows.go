//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// Opening magnet links and .torrent files with Zipline: per-user registrations under
// HKEY_CURRENT_USER\Software\Classes (no administrator rights needed).

var (
	advapi32assoc      = syscall.NewLazyDLL("advapi32.dll")
	pRegCreateKeyExW   = advapi32assoc.NewProc("RegCreateKeyExW")
	pRegSetValueExW    = advapi32assoc.NewProc("RegSetValueExW")
	pRegDeleteTreeW    = advapi32assoc.NewProc("RegDeleteTreeW")
	pRegDeleteKeyValue = advapi32assoc.NewProc("RegDeleteKeyValueW")
	pRegDeleteKeyW     = advapi32assoc.NewProc("RegDeleteKeyW")
	pSHChangeNotify    = shell32.NewProc("SHChangeNotify")
)

const (
	assocProgID = "Zipline.Torrent"
	keyWrite    = 0x20006
	regSZ       = 1
)

func regSetString(path, name, value string) error {
	var k syscall.Handle
	p, _ := syscall.UTF16PtrFromString(path)
	if r, _, _ := pRegCreateKeyExW.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p)), 0, 0, 0, keyWrite, 0, uintptr(unsafe.Pointer(&k)), 0); r != 0 {
		return syscall.Errno(r)
	}
	defer syscall.RegCloseKey(k)
	var np *uint16
	if name != "" {
		np, _ = syscall.UTF16PtrFromString(name)
	}
	v, _ := syscall.UTF16FromString(value)
	if r, _, _ := pRegSetValueExW.Call(uintptr(k), uintptr(unsafe.Pointer(np)), 0, regSZ, uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)*2)); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func regGetString(path, name string) string {
	var k syscall.Handle
	p, _ := syscall.UTF16PtrFromString(path)
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, p, 0, syscall.KEY_READ, &k) != nil {
		return ""
	}
	defer syscall.RegCloseKey(k)
	var np *uint16
	if name != "" {
		np, _ = syscall.UTF16PtrFromString(name)
	}
	buf := make([]uint16, 1024)
	n := uint32(len(buf) * 2)
	var typ uint32
	if syscall.RegQueryValueEx(k, np, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil || typ != regSZ {
		return ""
	}
	return syscall.UTF16ToString(buf[:n/2])
}

// regDeleteTree removes a key with everything under it.
func regDeleteTree(path string) {
	p, _ := syscall.UTF16PtrFromString(path)
	pRegDeleteTreeW.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p)))
	pRegDeleteKeyW.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p))) // (if the key itself is still there)
}

func assocCommand() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return `"` + exe + `" "%1"`
}

// torrentAssociation says whether this copy of Zipline opens magnet links and .torrent files.
func torrentAssociation() (supported, on bool) {
	cmd := assocCommand()
	return true, cmd != "" && regGetString(`Software\Classes\magnet\shell\open\command`, "") == cmd &&
		regGetString(`Software\Classes\.torrent`, "") == assocProgID
}

func setTorrentAssociation(on bool) error {
	cmd := assocCommand()
	exe, _ := os.Executable()
	icon := `"` + exe + `",0`
	if on {
		for _, kv := range [][3]string{
			{`Software\Classes\magnet`, "", "URL:Magnet link"},
			{`Software\Classes\magnet`, "URL Protocol", ""},
			{`Software\Classes\magnet\DefaultIcon`, "", icon},
			{`Software\Classes\magnet\shell\open\command`, "", cmd},
			{`Software\Classes\` + assocProgID, "", "Torrent file"},
			{`Software\Classes\` + assocProgID + `\DefaultIcon`, "", icon},
			{`Software\Classes\` + assocProgID + `\shell\open\command`, "", cmd},
			{`Software\Classes\.torrent`, "", assocProgID},
			{`Software\Classes\.torrent`, "Content Type", "application/x-bittorrent"},
			{`Software\Classes\.torrent\OpenWithProgids`, assocProgID, ""},
		} {
			if err := regSetString(kv[0], kv[1], kv[2]); err != nil {
				return err
			}
		}
	} else {
		// only undo what points at Zipline
		if regGetString(`Software\Classes\magnet\shell\open\command`, "") == cmd {
			regDeleteTree(`Software\Classes\magnet`)
		}
		if regGetString(`Software\Classes\.torrent`, "") == assocProgID {
			regSetString(`Software\Classes\.torrent`, "", "")
		}
		p, _ := syscall.UTF16PtrFromString(`Software\Classes\.torrent\OpenWithProgids`)
		n, _ := syscall.UTF16PtrFromString(assocProgID)
		pRegDeleteKeyValue.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(n)))
		regDeleteTree(`Software\Classes\` + assocProgID)
	}
	pSHChangeNotify.Call(0x08000000, 0, 0, 0) // SHCNE_ASSOCCHANGED
	return nil
}
