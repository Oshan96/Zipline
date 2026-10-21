//go:build windows

package main

import "syscall"

// errnoKinds: the Windows error codes Zipline explains in plain words. Connections report
// Winsock (WSA…) codes, files the ERROR_… ones.
var errnoKinds = map[syscall.Errno]errKind{
	10061: ekRefused,     // WSAECONNREFUSED
	1225:  ekRefused,     // ERROR_CONNECTION_REFUSED
	10054: ekReset,       // WSAECONNRESET
	64:    ekReset,       // ERROR_NETNAME_DELETED: the connection dropped during a read
	10053: ekAborted,     // WSAECONNABORTED: often a firewall, antivirus or VPN
	1236:  ekAborted,     // ERROR_CONNECTION_ABORTED
	10060: ekTimedOut,    // WSAETIMEDOUT
	121:   ekTimedOut,    // ERROR_SEM_TIMEOUT: what a connection attempt often ends with
	1460:  ekTimedOut,    // ERROR_TIMEOUT
	10051: ekNetUnreach,  // WSAENETUNREACH
	1231:  ekNetUnreach,  // ERROR_NETWORK_UNREACHABLE
	10050: ekNetDown,     // WSAENETDOWN
	10065: ekHostUnreach, // WSAEHOSTUNREACH
	10064: ekHostUnreach, // WSAEHOSTDOWN
	1232:  ekHostUnreach, // ERROR_HOST_UNREACHABLE
	11001: ekNoHost,      // WSAHOST_NOT_FOUND
	11004: ekNoHost,      // WSANO_DATA: the name has no address
	11002: ekDNSAgain,    // WSATRY_AGAIN
	10013: ekDenied,      // WSAEACCES: a firewall blocked the connection

	112:  ekDiskFull,     // ERROR_DISK_FULL
	39:   ekDiskFull,     // ERROR_HANDLE_DISK_FULL
	1295: ekQuota,        // ERROR_DISK_QUOTA_EXCEEDED
	5:    ekDenied,       // ERROR_ACCESS_DENIED
	19:   ekReadOnly,     // ERROR_WRITE_PROTECT
	32:   ekInUse,        // ERROR_SHARING_VIOLATION
	33:   ekInUse,        // ERROR_LOCK_VIOLATION
	206:  ekPathLong,     // ERROR_FILENAME_EXCED_RANGE
	2:    ekNoPath,       // ERROR_FILE_NOT_FOUND
	3:    ekNoPath,       // ERROR_PATH_NOT_FOUND
	21:   ekNoPath,       // ERROR_NOT_READY: the drive was removed
	4:    ekTooManyFiles, // ERROR_TOO_MANY_OPEN_FILES
	1224: ekMapped,       // ERROR_USER_MAPPED_FILE
	225:  ekVirus,        // ERROR_VIRUS_INFECTED
	226:  ekVirus,        // ERROR_VIRUS_DELETED
	23:   ekDiskError,    // ERROR_CRC
	1117: ekDiskError,    // ERROR_IO_DEVICE
}
