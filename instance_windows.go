//go:build windows

package main

import "syscall"

type instanceLock struct{ h syscall.Handle }

const errSharingViolation = syscall.Errno(32) // ERROR_SHARING_VIOLATION

// lockInstance takes the per-user lock; errLocked when another Zipline holds it.
func lockInstance(dir string) (*instanceLock, error) {
	p, err := syscall.UTF16PtrFromString(lockPath(dir))
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if err == errSharingViolation {
			return nil, errLocked
		}
		return nil, err
	}
	return &instanceLock{h}, nil
}

func (l *instanceLock) Release() {
	if l != nil && l.h != 0 {
		syscall.CloseHandle(l.h)
		l.h = 0
	}
}
