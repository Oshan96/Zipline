//go:build windows

package main

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Native Windows helpers: the Explorer-style folder picker (IFileOpenDialog) and
// bringing windows to the front. Pure syscalls, no cgo.

var (
	ole32                     = syscall.NewLazyDLL("ole32.dll")
	shell32                   = syscall.NewLazyDLL("shell32.dll")
	pCoInitializeEx           = ole32.NewProc("CoInitializeEx")
	pCoUninitialize           = ole32.NewProc("CoUninitialize")
	pCoCreateInstance         = ole32.NewProc("CoCreateInstance")
	pCoTaskMemFree            = ole32.NewProc("CoTaskMemFree")
	pSHCreateItemFromParsing  = shell32.NewProc("SHCreateItemFromParsingName")
	pGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	pSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	pBringWindowToTop         = user32.NewProc("BringWindowToTop")
	pShowWindow               = user32.NewProc("ShowWindow")
	pIsIconic                 = user32.NewProc("IsIconic")
	pFindWindowW              = user32.NewProc("FindWindowW")
	pGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	pAttachThreadInput        = user32.NewProc("AttachThreadInput")
	pEnumWindows              = user32.NewProc("EnumWindows")
	pGetWindowTextW           = user32.NewProc("GetWindowTextW")
	pIsWindowVisible          = user32.NewProc("IsWindowVisible")
	pGetCurrentThreadId       = kernel32.NewProc("GetCurrentThreadId")
	pickMu                    sync.Mutex
)

type guid struct {
	d1     uint32
	d2, d3 uint16
	d4     [8]byte
}

var (
	clsidFileOpenDialog = guid{0xDC1C5A9C, 0xE88A, 0x4DDE, [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = guid{0xD57C7288, 0xD4AD, 0x4768, [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	iidIShellItem       = guid{0x43826D1E, 0xE718, 0x42EE, [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
)

// comObj mirrors a COM interface pointer: the first word is the vtable.
type comObj struct{ vtbl *[32]uintptr }

func (o *comObj) call(i int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(o.vtbl[i], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return r
}
func (o *comObj) release() { o.call(2) }

const (
	vtShow          = 3
	vtSetOptions    = 9
	vtGetOptions    = 10
	vtSetFolder     = 12
	vtSetTitle      = 17
	vtSetOkLabel    = 18
	vtGetResult     = 20
	vtGetDisplayNam = 5 // IShellItem::GetDisplayName

	fosPickFolders     = 0x20
	fosForceFileSystem = 0x40
	fosNoChangeDir     = 0x8
	fosPathMustExist   = 0x800
	sigdnFileSysPath   = 0x80058000
	errCancelled       = 0x800704C7
	pickerTitle        = "Choose download folder"
)

var errPickCancelled = errors.New("cancelled")

// pickFolderNative shows the modern Explorer folder picker, owned by whatever window
// is in front (the Zipline window the user just clicked in), so it opens on top of it.
func pickFolderNative(start string) (string, error) {
	type res struct {
		dir string
		err error
	}
	ch := make(chan res, 1)
	owner, _, _ := pGetForegroundWindow.Call()
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hr, _, _ := pCoInitializeEx.Call(0, 0x2|0x4) // apartment threaded, no OLE1DDE
		if int32(hr) >= 0 {
			defer pCoUninitialize.Call()
		}
		var dlg *comObj
		hr, _, _ = pCoCreateInstance.Call(uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, 1,
			uintptr(unsafe.Pointer(&iidIFileOpenDialog)), uintptr(unsafe.Pointer(&dlg)))
		if int32(hr) < 0 || dlg == nil {
			ch <- res{"", errors.New("folder dialog unavailable")}
			return
		}
		defer dlg.release()
		var opts uint32
		dlg.call(vtGetOptions, uintptr(unsafe.Pointer(&opts)))
		dlg.call(vtSetOptions, uintptr(opts|fosPickFolders|fosForceFileSystem|fosNoChangeDir|fosPathMustExist))
		title, _ := syscall.UTF16PtrFromString(pickerTitle)
		dlg.call(vtSetTitle, uintptr(unsafe.Pointer(title)))
		ok, _ := syscall.UTF16PtrFromString("Select folder")
		dlg.call(vtSetOkLabel, uintptr(unsafe.Pointer(ok)))
		if start != "" {
			if _, err := os.Stat(start); err == nil {
				if p, err := syscall.UTF16PtrFromString(start); err == nil {
					var item *comObj
					r, _, _ := pSHCreateItemFromParsing.Call(uintptr(unsafe.Pointer(p)), 0,
						uintptr(unsafe.Pointer(&iidIShellItem)), uintptr(unsafe.Pointer(&item)))
					if int32(r) >= 0 && item != nil {
						dlg.call(vtSetFolder, uintptr(unsafe.Pointer(item)))
						item.release()
					}
				}
			}
		}
		go raiseWhenShown(pickerTitle)
		hr = dlg.call(vtShow, owner)
		if uint32(hr) == errCancelled {
			ch <- res{"", errPickCancelled}
			return
		}
		if int32(hr) < 0 {
			ch <- res{"", errors.New("folder dialog failed")}
			return
		}
		var item *comObj
		if r := dlg.call(vtGetResult, uintptr(unsafe.Pointer(&item))); int32(r) < 0 || item == nil {
			ch <- res{"", errors.New("no folder selected")}
			return
		}
		defer item.release()
		var p *uint16
		if r := item.call(vtGetDisplayNam, sigdnFileSysPath, uintptr(unsafe.Pointer(&p))); int32(r) < 0 || p == nil {
			ch <- res{"", errors.New("not a file system folder")}
			return
		}
		dir := wideToString(uintptr(unsafe.Pointer(p)))
		pCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
		ch <- res{dir, nil}
	}()
	r := <-ch
	return r.dir, r.err
}

// raiseWhenShown waits for our dialog window to appear and pulls it to the front.
// Windows only lets the foreground app hand out focus, so we briefly share input
// state with the foreground thread (the standard, documented workaround).
func raiseWhenShown(title string) {
	t, _ := syscall.UTF16PtrFromString(title)
	pid := uint32(os.Getpid())
	for i := 0; i < 60; i++ {
		time.Sleep(50 * time.Millisecond)
		h, _, _ := pFindWindowW.Call(0, uintptr(unsafe.Pointer(t)))
		if h == 0 {
			continue
		}
		var wpid uint32
		pGetWindowThreadProcessId.Call(h, uintptr(unsafe.Pointer(&wpid)))
		if wpid != pid {
			continue
		}
		forceForeground(h)
		return
	}
}

func forceForeground(h uintptr) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if iconic, _, _ := pIsIconic.Call(h); iconic != 0 {
		pShowWindow.Call(h, 9) // SW_RESTORE
	}
	fg, _, _ := pGetForegroundWindow.Call()
	cur, _, _ := pGetCurrentThreadId.Call()
	fgThread, _, _ := pGetWindowThreadProcessId.Call(fg, 0)
	if fgThread != 0 && fgThread != cur {
		pAttachThreadInput.Call(cur, fgThread, 1)
		defer pAttachThreadInput.Call(cur, fgThread, 0)
	}
	pBringWindowToTop.Call(h)
	pSetForegroundWindow.Call(h)
}

// The window search uses one callback for the life of the app: Go allows only about 2,000
// callbacks and never frees them, so a new one per call would eventually crash Zipline.
var (
	enumMu    sync.Mutex
	enumFound uintptr
	enumBuf   = make([]uint16, 256)
	enumCB    = syscall.NewCallback(enumAppWindow)
)

func enumAppWindow(h, _ uintptr) uintptr {
	if v, _, _ := pIsWindowVisible.Call(h); v == 0 {
		return 1
	}
	n, _, _ := pGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&enumBuf[0])), uintptr(len(enumBuf)))
	if n == 0 {
		return 1
	}
	if s := syscall.UTF16ToString(enumBuf[:n]); strings.Contains(s, appTitle) {
		enumFound = h
		return 0
	}
	return 1
}

// focusAppWindow brings an already-open Zipline window to the front. Returns false if none found.
func focusAppWindow() bool {
	enumMu.Lock()
	enumFound = 0
	pEnumWindows.Call(enumCB, 0)
	found := enumFound
	enumMu.Unlock()
	if found == 0 {
		return false
	}
	forceForeground(found)
	return true
}

// wideToString copies a NUL-terminated UTF-16 string owned by Windows.
func wideToString(p uintptr) string {
	n, _, _ := pLstrlenW.Call(p)
	if n == 0 || n > 1<<16 {
		return ""
	}
	u := make([]uint16, n)
	pMoveMemory.Call(uintptr(unsafe.Pointer(&u[0])), p, n*2)
	return syscall.UTF16ToString(u)
}
