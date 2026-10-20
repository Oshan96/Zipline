//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

type instanceLock struct{ f *os.File }

// lockInstance takes the per-user lock; errLocked when another Zipline holds it.
func lockInstance(dir string) (*instanceLock, error) {
	f, err := os.OpenFile(lockPath(dir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return &instanceLock{f}, nil
}

func (l *instanceLock) Release() {
	if l != nil && l.f != nil {
		syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		l.f.Close()
		l.f = nil
	}
}
