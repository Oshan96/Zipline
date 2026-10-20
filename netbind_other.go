//go:build !windows && !linux && !darwin

package main

func bindToInterface(fd uintptr, i netIf) error { return nil }
