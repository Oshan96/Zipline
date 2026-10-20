//go:build windows

package main

import (
	"net/url"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Windows system proxy: the same settings as Settings → Network → Proxy (Internet Options),
// including "Use setup script" (PAC) and "Automatically detect settings" (WPAD), resolved
// through WinHTTP so PAC scripts are evaluated by Windows itself.

var (
	winhttp                  = syscall.NewLazyDLL("winhttp.dll")
	pWinHttpGetIEProxyConfig = winhttp.NewProc("WinHttpGetIEProxyConfigForCurrentUser")
	pWinHttpOpen             = winhttp.NewProc("WinHttpOpen")
	pWinHttpGetProxyForUrl   = winhttp.NewProc("WinHttpGetProxyForUrl")
	pWinHttpDetectAutoProxy  = winhttp.NewProc("WinHttpDetectAutoProxyConfigUrl")
	pGlobalFree              = kernel32.NewProc("GlobalFree")
)

type ieProxyConfig struct {
	autoDetect    int32
	autoConfigURL uintptr
	proxy         uintptr
	bypass        uintptr
}

type autoProxyOptions struct {
	flags           uint32
	autoDetectFlags uint32
	autoConfigURL   *uint16
	reserved        uintptr
	reserved2       uint32
	autoLogon       int32
}

type winProxyInfo struct {
	accessType uint32
	proxy      uintptr
	bypass     uintptr
}

type cachedProxy struct {
	val string
	at  time.Time
}

var sp struct {
	mu         sync.Mutex
	loaded     time.Time
	static     string
	bypass     string
	pacURL     string
	autoDetect bool
	detected   string // PAC URL found by auto-detect
	detectAt   time.Time
	detecting  bool
	session    uintptr
	cache      map[string]cachedProxy
	lookupMu   sync.Mutex
}

func systemProxyReset() {
	sp.mu.Lock()
	sp.loaded = time.Time{}
	sp.cache = nil
	sp.mu.Unlock()
}

func takeWide(p uintptr) string {
	if p == 0 {
		return ""
	}
	s := wideToString(p)
	pGlobalFree.Call(p)
	return s
}

func loadIEConfig() {
	var c ieProxyConfig
	r, _, _ := pWinHttpGetIEProxyConfig.Call(uintptr(unsafe.Pointer(&c)))
	sp.loaded = time.Now()
	sp.cache = map[string]cachedProxy{}
	if r == 0 {
		sp.static, sp.bypass, sp.pacURL, sp.autoDetect = "", "", "", false
		return
	}
	sp.autoDetect = c.autoDetect != 0
	sp.pacURL = takeWide(c.autoConfigURL)
	sp.static = takeWide(c.proxy)
	sp.bypass = takeWide(c.bypass)
}

func detectWPAD() {
	var p uintptr
	r, _, _ := pWinHttpDetectAutoProxy.Call(0x1|0x2, uintptr(unsafe.Pointer(&p))) // DHCP | DNS_A
	found := ""
	if r != 0 && p != 0 {
		found = takeWide(p)
	}
	sp.mu.Lock()
	sp.detected, sp.detecting, sp.detectAt = found, false, time.Now()
	sp.cache = map[string]cachedProxy{}
	sp.mu.Unlock()
}

func systemProxy(u *url.URL) (*url.URL, error) {
	sp.mu.Lock()
	if time.Since(sp.loaded) > time.Minute {
		loadIEConfig()
	}
	// Auto-detect can take seconds when there is no WPAD server, so it runs in the
	// background and the static settings are used until it finishes.
	if sp.autoDetect && sp.pacURL == "" && !sp.detecting && time.Since(sp.detectAt) > 30*time.Minute {
		sp.detecting = true
		go detectWPAD()
	}
	pac := sp.pacURL
	if pac == "" {
		pac = sp.detected
	}
	static, bypass := sp.static, sp.bypass
	key := u.Scheme + "://" + u.Host
	if c, ok := sp.cache[key]; ok && time.Since(c.at) < 5*time.Minute {
		sp.mu.Unlock()
		return proxyFromList(c.val, u)
	}
	sp.mu.Unlock()

	val := ""
	if pac != "" {
		if v, ok := pacLookup(u, pac); ok {
			val = v
		} else if static != "" && !bypassed(u.Hostname(), bypass) {
			val = static
		}
	} else if static != "" && !bypassed(u.Hostname(), bypass) {
		val = static
	}
	sp.mu.Lock()
	if sp.cache != nil {
		sp.cache[key] = cachedProxy{val, time.Now()}
	}
	sp.mu.Unlock()
	return proxyFromList(val, u) // the first entry that hasn't failed lately
}

// pacLookup asks WinHTTP to run the PAC script for this URL. ok=false means it failed.
func pacLookup(u *url.URL, pac string) (string, bool) {
	sp.lookupMu.Lock()
	defer sp.lookupMu.Unlock()
	if sp.session == 0 {
		ua, _ := syscall.UTF16PtrFromString("Zipline")
		h, _, _ := pWinHttpOpen.Call(uintptr(unsafe.Pointer(ua)), 1, 0, 0, 0) // NO_PROXY session
		if h == 0 {
			return "", false
		}
		sp.session = h
	}
	target, err := syscall.UTF16PtrFromString(u.String())
	if err != nil {
		return "", false
	}
	pu, err := syscall.UTF16PtrFromString(pac)
	if err != nil {
		return "", false
	}
	opts := autoProxyOptions{flags: 0x2, autoConfigURL: pu, autoLogon: 1} // CONFIG_URL
	var info winProxyInfo
	r, _, _ := pWinHttpGetProxyForUrl.Call(sp.session, uintptr(unsafe.Pointer(target)), uintptr(unsafe.Pointer(&opts)), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return "", false
	}
	list := takeWide(info.proxy)
	takeWide(info.bypass)
	if info.accessType == 1 { // WINHTTP_ACCESS_TYPE_NO_PROXY → DIRECT
		return "", true
	}
	return list, true
}
