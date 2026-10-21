//go:build windows

package main

import (
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Windows: a power request ("Zipline is downloading", listed by powercfg /requests) keeps the
// computer from sleeping by itself; a waitable timer with resume set wakes it for a scheduled
// start (if Power Options allow wake timers); SetSuspendState puts it to sleep or hibernates it.

var (
	powrprof            = syscall.NewLazyDLL("powrprof.dll")
	pSetSuspendState    = powrprof.NewProc("SetSuspendState")
	pIsPwrHibernate     = powrprof.NewProc("IsPwrHibernateAllowed")
	pIsPwrSuspend       = powrprof.NewProc("IsPwrSuspendAllowed")
	pPowerCreateRequest = kernel32.NewProc("PowerCreateRequest")
	pPowerSetRequest    = kernel32.NewProc("PowerSetRequest")
	pPowerClearRequest  = kernel32.NewProc("PowerClearRequest")
	pSetThreadExecState = kernel32.NewProc("SetThreadExecutionState")
	pCreateWaitableTmr  = kernel32.NewProc("CreateWaitableTimerW")
	pSetWaitableTimer   = kernel32.NewProc("SetWaitableTimer")
	pCancelWaitableTmr  = kernel32.NewProc("CancelWaitableTimer")
)

const powerRequestSystemRequired = 1

// reasonContext is REASON_CONTEXT with a simple reason string.
type reasonContext struct {
	version uint32
	flags   uint32 // POWER_REQUEST_CONTEXT_SIMPLE_STRING
	reason  *uint16
	_       [16]byte // (the rest of the union: the detailed form)
}

var awakeReq struct {
	sync.Mutex
	h     uintptr
	set   bool
	noReq bool // power requests aren't available: SetThreadExecutionState instead
}

func setKeepAwake(on bool) error {
	awakeReq.Lock()
	defer awakeReq.Unlock()
	if on == awakeReq.set {
		return nil
	}
	if awakeReq.h == 0 && !awakeReq.noReq {
		reason, _ := syscall.UTF16PtrFromString("Zipline is downloading")
		rc := reasonContext{flags: 1, reason: reason}
		if pPowerCreateRequest.Find() == nil {
			if h, _, _ := pPowerCreateRequest.Call(uintptr(unsafe.Pointer(&rc))); h != 0 && h != uintptr(syscall.InvalidHandle) {
				awakeReq.h = h
			}
		}
		awakeReq.noReq = awakeReq.h == 0
	}
	var err error
	switch {
	case awakeReq.noReq:
		err = setExecutionState(on)
	case on:
		if r, _, e := pPowerSetRequest.Call(awakeReq.h, powerRequestSystemRequired); r == 0 {
			err = e
		}
	default:
		pPowerClearRequest.Call(awakeReq.h, powerRequestSystemRequired)
	}
	if err == nil {
		awakeReq.set = on
	}
	return err
}

// setExecutionState is the older way to hold sleep off. The state belongs to the thread that
// set it, so it's set from a thread of its own that never ends.
var esThread struct {
	once sync.Once
	ch   chan bool
	res  chan error
}

func setExecutionState(on bool) error {
	const esContinuous, esSystemRequired = 0x80000000, 0x1
	esThread.once.Do(func() {
		esThread.ch, esThread.res = make(chan bool), make(chan error)
		go func() {
			runtime.LockOSThread() // (never unlocked: the thread keeps the state)
			for on := range esThread.ch {
				flags := uintptr(esContinuous)
				if on {
					flags |= esSystemRequired
				}
				var err error
				if r, _, e := pSetThreadExecState.Call(flags); r == 0 {
					err = e
				}
				esThread.res <- err
			}
		}()
	})
	esThread.ch <- on
	return <-esThread.res
}

var wakeTimer struct {
	sync.Mutex
	h uintptr
}

// setWakeTimer wakes the computer at t (zero: no wake). The timer lives as long as Zipline runs.
func setWakeTimer(t time.Time) error {
	wakeTimer.Lock()
	defer wakeTimer.Unlock()
	if t.IsZero() {
		if wakeTimer.h != 0 {
			pCancelWaitableTmr.Call(wakeTimer.h)
		}
		return nil
	}
	if wakeTimer.h == 0 {
		h, _, err := pCreateWaitableTmr.Call(0, 1, 0)
		if h == 0 {
			return err
		}
		wakeTimer.h = h
	}
	ft := syscall.NsecToFiletime(t.UnixNano())
	due := int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime) // positive: an absolute time (UTC)
	if r, _, err := pSetWaitableTimer.Call(wakeTimer.h, uintptr(unsafe.Pointer(&due)), 0, 0, 0, 1); r == 0 {
		return err
	}
	return nil
}

func sleepComputer()     { suspend(0) }
func hibernateComputer() { suspend(1) }

func suspend(hibernate uintptr) {
	if pSetSuspendState.Find() == nil {
		pSetSuspendState.Call(hibernate, 0, 0) // (wake timers stay on)
	}
}

func systemPowerActions() []string {
	allowed := func(p *syscall.LazyProc) bool {
		if p.Find() != nil {
			return false
		}
		r, _, _ := p.Call()
		return r&0xff != 0 // BOOLEAN
	}
	can := []string{"quit"}
	if allowed(pIsPwrSuspend) {
		can = append(can, "sleep")
	}
	if allowed(pIsPwrHibernate) {
		can = append(can, "hibernate")
	}
	return append(can, "shutdown")
}
