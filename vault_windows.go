//go:build windows

package main

import (
	"encoding/base64"
	"errors"
	"strings"
	"syscall"
	"unsafe"
)

// On Windows secrets are encrypted with the Windows account (DPAPI), so the files are useless
// if copied to another PC or read by another user.

var (
	crypt32         = syscall.NewLazyDLL("crypt32.dll")
	pProtectData    = crypt32.NewProc("CryptProtectData")
	pUnprotectData  = crypt32.NewProc("CryptUnprotectData")
	pLocalFree      = kernel32.NewProc("LocalFree")
	dpapiDescriptor = []uint16{'Z', 'i', 'p', 'l', 'i', 'n', 'e', 0}
)

const (
	tagDPAPI                = 2 // the first byte of what this sealer makes
	cryptprotectUIForbidden = 0x1
)

type dataBlob struct {
	n uint32
	p *byte
}

func blobBytes(b dataBlob) []byte {
	if b.p == nil || b.n == 0 {
		return nil
	}
	out := append([]byte(nil), unsafe.Slice(b.p, b.n)...)
	pLocalFree.Call(uintptr(unsafe.Pointer(b.p)))
	return out
}

type dpapiSealer struct{}

func platformSealer(dir string) sealer { return dpapiSealer{} }

func (dpapiSealer) how() string { return "dpapi" }

func (dpapiSealer) seal(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return []byte{tagDPAPI}, nil
	}
	src := dataBlob{uint32(len(plain)), &plain[0]}
	var dst dataBlob
	r, _, err := pProtectData.Call(uintptr(unsafe.Pointer(&src)), uintptr(unsafe.Pointer(&dpapiDescriptor[0])), 0, 0, 0, cryptprotectUIForbidden, uintptr(unsafe.Pointer(&dst)))
	if r == 0 {
		return nil, errors.New("Windows couldn't encrypt it: " + err.Error())
	}
	return append([]byte{tagDPAPI}, blobBytes(dst)...), nil
}

func (dpapiSealer) open(sealed []byte) ([]byte, error) {
	if len(sealed) == 0 || sealed[0] != tagDPAPI {
		return nil, errForeign
	}
	return dpapiOpen(sealed[1:])
}

// openLegacy reads what versions before the vault wrote: "dpapi:" and base64.
func (dpapiSealer) openLegacy(s string) ([]byte, error) {
	in, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, "dpapi:"))
	if err != nil {
		return nil, errDamaged
	}
	return dpapiOpen(in)
}

func dpapiOpen(in []byte) ([]byte, error) {
	if len(in) == 0 {
		return []byte{}, nil
	}
	src := dataBlob{uint32(len(in)), &in[0]}
	var dst dataBlob
	r, _, _ := pUnprotectData.Call(uintptr(unsafe.Pointer(&src)), 0, 0, 0, 0, cryptprotectUIForbidden, uintptr(unsafe.Pointer(&dst)))
	if r == 0 {
		return nil, errWrongKey // made by another user or PC
	}
	return blobBytes(dst), nil
}
