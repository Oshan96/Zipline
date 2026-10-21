//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	winmm      = syscall.NewLazyDLL("winmm.dll")
	pPlaySound = winmm.NewProc("PlaySoundW")
)

const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndMemory    = 0x0004
)

// playSound plays a chime from memory. The WAV stays cached, so the memory outlives the
// asynchronous playback.
func playSound(name string) bool {
	b := soundWAV(name)
	if b == nil || pPlaySound.Find() != nil {
		return false
	}
	r, _, _ := pPlaySound.Call(uintptr(unsafe.Pointer(&b[0])), 0, sndMemory|sndAsync|sndNoDefault)
	return r != 0
}
