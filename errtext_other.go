//go:build !windows

package main

import "syscall"

// errnoKinds: the macOS and Linux error codes Zipline explains in plain words.
var errnoKinds = map[syscall.Errno]errKind{
	syscall.ECONNREFUSED: ekRefused,
	syscall.ECONNRESET:   ekReset,
	syscall.EPIPE:        ekReset, // writing to a connection the server already closed
	syscall.ECONNABORTED: ekAborted,
	syscall.ETIMEDOUT:    ekTimedOut,
	syscall.ENETUNREACH:  ekNetUnreach,
	syscall.ENETDOWN:     ekNetDown,
	syscall.EHOSTUNREACH: ekHostUnreach,
	syscall.EHOSTDOWN:    ekHostUnreach,

	syscall.ENOSPC:       ekDiskFull,
	syscall.EDQUOT:       ekQuota,
	syscall.EACCES:       ekDenied,
	syscall.EPERM:        ekDenied, // also what a local firewall rule gives a connection
	syscall.EROFS:        ekReadOnly,
	syscall.ETXTBSY:      ekInUse,
	syscall.EBUSY:        ekInUse,
	syscall.ENAMETOOLONG: ekPathLong,
	syscall.ENOENT:       ekNoPath,
	syscall.EMFILE:       ekTooManyFiles,
	syscall.ENFILE:       ekTooManyFiles,
	syscall.EIO:          ekDiskError,
}
