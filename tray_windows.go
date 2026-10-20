//go:build windows

package main

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

// Notification-area (tray) icon: left-click opens Zipline, right-click shows a menu,
// the tooltip shows live progress, and balloons announce finished downloads while the
// window is closed. Pure Win32 calls on a dedicated, locked OS thread.

var (
	shell32tray         = syscall.NewLazyDLL("shell32.dll")
	pShellNotifyIconW   = shell32tray.NewProc("Shell_NotifyIconW")
	pRegisterClassExW   = user32.NewProc("RegisterClassExW")
	pCreateWindowExW    = user32.NewProc("CreateWindowExW")
	pDefWindowProcW     = user32.NewProc("DefWindowProcW")
	pGetMessageW        = user32.NewProc("GetMessageW")
	pTranslateMessage   = user32.NewProc("TranslateMessage")
	pDispatchMessageW   = user32.NewProc("DispatchMessageW")
	pPostMessageW       = user32.NewProc("PostMessageW")
	pRegisterWindowMsgW = user32.NewProc("RegisterWindowMessageW")
	pLoadImageW         = user32.NewProc("LoadImageW")
	pGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	pCreatePopupMenu    = user32.NewProc("CreatePopupMenu")
	pAppendMenuW        = user32.NewProc("AppendMenuW")
	pSetMenuDefaultItem = user32.NewProc("SetMenuDefaultItem")
	pTrackPopupMenu     = user32.NewProc("TrackPopupMenu")
	pDestroyMenu        = user32.NewProc("DestroyMenu")
	pGetCursorPos       = user32.NewProc("GetCursorPos")
	pSetTimer           = user32.NewProc("SetTimer")
	pGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmTimer        = 0x0113
	wmCommand      = 0x0111
	wmLButtonUp    = 0x0202
	wmLButtonDbl   = 0x0203
	wmRButtonUp    = 0x0205
	wmApp          = 0x8000
	wmTray         = wmApp + 1 // icon events
	wmTrayCmd      = wmApp + 2 // wake up: show/hide/balloon requests
	ninBalloonClik = 0x0405

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4
	nifInfo    = 0x10

	niifInfo    = 0x1
	niifWarning = 0x2

	mfString    = 0x0
	mfChecked   = 0x8
	mfGrayed    = 0x1
	mfPopup     = 0x10
	mfSeparator = 0x800

	idOpen = iota + 100
	idAdd
	idStartQueues
	idStopAll
	idLimiter
	idExit
	idCancelPower
	idProfile0 = 300 // + the profile's place in the list
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         guid
	HBalloonIcon     uintptr
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type winMsg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	PtX     int32
	PtY     int32
	Private uint32
}

type balloon struct {
	title, text string
	warning     bool
}

var tray struct {
	hwnd       uintptr
	icon       uintptr
	shown      bool
	want       int32 // 1 = icon wanted (setting)
	started    int32
	m          *Manager
	mu         sync.Mutex
	balloons   []balloon
	taskbarMsg uint32
	lastTip    string
}

func trayAvailable() bool { return true }

func copyUTF16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = append(u[:len(dst)-1], 0)
	}
	copy(dst, u)
}

func (n *notifyIconData) base() {
	n.CbSize = uint32(unsafe.Sizeof(*n))
	n.HWnd = tray.hwnd
	n.UID = 1
}

func trayAdd() {
	if tray.shown || tray.hwnd == 0 {
		return
	}
	var n notifyIconData
	n.base()
	n.UFlags = nifMessage | nifIcon | nifTip
	n.UCallbackMessage = wmTray
	n.HIcon = tray.icon
	tray.lastTip = trayTip()
	copyUTF16(n.SzTip[:], tray.lastTip)
	if r, _, _ := pShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&n))); r != 0 {
		tray.shown = true
	}
}

func trayRemove() {
	if !tray.shown {
		return
	}
	var n notifyIconData
	n.base()
	pShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&n)))
	tray.shown = false
}

func trayTip() string {
	m := tray.m
	if m == nil {
		return appTitle
	}
	m.mu.Lock()
	active := 0
	var speed float64
	for _, d := range m.list {
		if d.running {
			active++
			speed += d.speed
		}
	}
	m.mu.Unlock()
	if active == 0 {
		return "Zipline — no active downloads"
	}
	return fmt.Sprintf("Zipline — %d downloading · %s/s", active, humanSize(int64(speed)))
}

func trayRefreshTip() {
	if !tray.shown {
		return
	}
	tip := trayTip()
	if tip == tray.lastTip {
		return
	}
	tray.lastTip = tip
	var n notifyIconData
	n.base()
	n.UFlags = nifTip
	copyUTF16(n.SzTip[:], tip)
	pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

func trayShowBalloon(b balloon) {
	if !tray.shown {
		return
	}
	var n notifyIconData
	n.base()
	n.UFlags = nifInfo
	copyUTF16(n.SzInfoTitle[:], b.title)
	copyUTF16(n.SzInfo[:], b.text)
	n.DwInfoFlags = niifInfo
	if b.warning {
		n.DwInfoFlags = niifWarning
	}
	pShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

func trayMenu() {
	m := tray.m
	m.mu.Lock()
	limiterOn := m.settings.SpeedLimitOn
	anyRunning := false
	for _, d := range m.list {
		anyRunning = anyRunning || d.running
	}
	prof := m.profileView(time.Now())
	m.mu.Unlock()
	cancel := m.cancelLabel() // a "when finished" action counting down

	h, _, _ := pCreatePopupMenu.Call()
	if h == 0 {
		return
	}
	defer pDestroyMenu.Call(h)
	add := func(id int, label string, flags uintptr) {
		l, _ := syscall.UTF16PtrFromString(label)
		pAppendMenuW.Call(h, flags, uintptr(id), uintptr(unsafe.Pointer(l)))
	}
	sep := func() { pAppendMenuW.Call(h, mfSeparator, 0, 0) }
	add(idOpen, "Open Zipline", mfString)
	add(idAdd, "Add link…", mfString)
	if cancel != "" {
		add(idCancelPower, cancel, mfString)
	}
	sep()
	add(idStartQueues, "Start all queues", mfString)
	stop := uintptr(mfString)
	if !anyRunning {
		stop |= mfGrayed
	}
	add(idStopAll, "Pause all downloads", stop)
	lim := uintptr(mfString)
	if limiterOn {
		lim |= mfChecked
	}
	add(idLimiter, "Speed limiter", lim)
	if sub, _, _ := pCreatePopupMenu.Call(); sub != 0 { // Traffic profile ▸ (destroyed with the menu)
		for i, p := range prof.List {
			f := uintptr(mfString)
			if p.ID == prof.ID {
				f |= mfChecked
			}
			l, _ := syscall.UTF16PtrFromString(p.Name)
			pAppendMenuW.Call(sub, f, uintptr(idProfile0+i), uintptr(unsafe.Pointer(l)))
		}
		l, _ := syscall.UTF16PtrFromString("Traffic profile: " + prof.Name)
		pAppendMenuW.Call(h, mfPopup, sub, uintptr(unsafe.Pointer(l)))
	}
	sep()
	add(idExit, "Exit Zipline", mfString)
	pSetMenuDefaultItem.Call(h, idOpen, 0)

	var pt struct{ X, Y int32 }
	pGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	pSetForegroundWindow.Call(tray.hwnd)                                                                // so the menu closes when you click elsewhere
	cmd, _, _ := pTrackPopupMenu.Call(h, 0x100|0x2|0x20, uintptr(pt.X), uintptr(pt.Y), 0, tray.hwnd, 0) // RETURNCMD|RIGHTBUTTON|BOTTOMALIGN
	pPostMessageW.Call(tray.hwnd, 0, 0, 0)
	switch int(cmd) {
	case idOpen:
		go bringToFront()
	case idAdd:
		go func() { m.uiCommand("add"); bringToFront() }()
	case idStartQueues:
		go m.StartAllQueues() // (the Free data hours queue only in its free hours)
	case idStopAll:
		go func() {
			m.Pause(allIDs(m))
			m.mu.Lock()
			var ids []string
			for _, q := range m.settings.Queues {
				ids = append(ids, q.ID)
			}
			m.mu.Unlock()
			for _, id := range ids {
				m.StopQueue(id)
			}
		}()
	case idLimiter:
		go func() {
			m.mu.Lock()
			m.settings.SpeedLimitOn = !m.settings.SpeedLimitOn
			m.applyLimit()
			m.dirty = true
			m.mu.Unlock()
		}()
	case idExit:
		go m.Quit()
	case idCancelPower:
		go m.powerControl("cancel", "")
	default:
		if i := int(cmd) - idProfile0; i >= 0 && i < len(prof.List) {
			go m.SetProfile(prof.List[i].ID)
		}
	}
}

func trayWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch uint32(msg) {
	case wmTray:
		switch uint32(lparam) {
		case wmLButtonUp, wmLButtonDbl, ninBalloonClik:
			go bringToFront()
		case wmRButtonUp:
			trayMenu()
		}
		return 0
	case wmTrayCmd:
		if atomic.LoadInt32(&tray.want) == 1 {
			trayAdd()
		} else {
			trayRemove()
		}
		tray.mu.Lock()
		bs := tray.balloons
		tray.balloons = nil
		tray.mu.Unlock()
		for _, b := range bs {
			trayShowBalloon(b)
		}
		return 0
	case wmTimer:
		trayRefreshTip()
		return 0
	}
	if tray.taskbarMsg != 0 && uint32(msg) == tray.taskbarMsg { // Explorer restarted: add the icon again
		tray.shown = false
		if atomic.LoadInt32(&tray.want) == 1 {
			trayAdd()
		}
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func startTray(m *Manager) {
	if !atomic.CompareAndSwapInt32(&tray.started, 0, 1) {
		return
	}
	tray.m = m
	ready := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		hinst, _, _ := pGetModuleHandleW.Call(0)
		cls, _ := syscall.UTF16PtrFromString("ZiplineTray")
		wc := wndClassEx{LpfnWndProc: syscall.NewCallback(trayWndProc), HInstance: hinst, LpszClassName: cls}
		wc.CbSize = uint32(unsafe.Sizeof(wc))
		pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		title, _ := syscall.UTF16PtrFromString("Zipline tray")
		// A hidden top-level window (not message-only) so it also hears "TaskbarCreated".
		hwnd, _, _ := pCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), 0, 0, 0, 0, 0, 0, 0, hinst, 0)
		tray.hwnd = hwnd
		tb, _ := syscall.UTF16PtrFromString("TaskbarCreated")
		r, _, _ := pRegisterWindowMsgW.Call(uintptr(unsafe.Pointer(tb)))
		tray.taskbarMsg = uint32(r)
		cx, _, _ := pGetSystemMetrics.Call(49) // SM_CXSMICON
		cy, _, _ := pGetSystemMetrics.Call(50)
		tray.icon, _, _ = pLoadImageW.Call(hinst, 1, 1, cx, cy, 0) // our app icon (resource #1)
		if tray.icon == 0 {
			tray.icon, _, _ = pLoadImageW.Call(0, 32512, 1, cx, cy, 0x8000) // IDI_APPLICATION, shared
		}
		close(ready)
		if hwnd == 0 {
			return
		}
		pSetTimer.Call(hwnd, 1, 2000, 0)
		if atomic.LoadInt32(&tray.want) == 1 {
			trayAdd()
		}
		var msg winMsg
		for {
			r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
		}
		trayRemove()
	}()
	<-ready
	go func() { // remove the icon when Zipline exits
		<-m.quit
		atomic.StoreInt32(&tray.want, 0)
		trayWake()
	}()
}

func trayWake() {
	if tray.hwnd != 0 {
		pPostMessageW.Call(tray.hwnd, wmTrayCmd, 0, 0)
	}
}

func traySetEnabled(on bool) {
	v := int32(0)
	if on {
		v = 1
	}
	atomic.StoreInt32(&tray.want, v)
	trayWake()
}

// trayNotify shows a balloon — only when no Zipline window is open (the window shows its own).
func trayNotify(title, text string, warning bool) {
	if atomic.LoadInt32(&sseClients) > 0 && !warning {
		return
	}
	tray.mu.Lock()
	tray.balloons = append(tray.balloons, balloon{title, text, warning})
	tray.mu.Unlock()
	trayWake()
}
