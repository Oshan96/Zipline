//go:build windows

package main

import (
	"path/filepath"
	"syscall"
)

// On Windows each browser reads HKCU\Software\<vendor>\NativeMessagingHosts\<name> (its default value
// is the manifest's path). The manifests live in the data folder. Chrome's key is always written:
// several other Chromium browsers read it too.
var nmRegKeys = []struct {
	vendor  string
	firefox bool
}{
	{`Software\Google\Chrome`, false},
	{`Software\Chromium`, false},
	{`Software\Microsoft\Edge`, false},
	{`Software\BraveSoftware\Brave-Browser`, false},
	{`Software\Vivaldi`, false},
	{`Software\Opera Software`, false},
	{`Software\Mozilla`, true},
}

func regKeyExists(path string) bool {
	var k syscall.Handle
	p, _ := syscall.UTF16PtrFromString(path)
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, p, 0, syscall.KEY_READ, &k) != nil {
		return false
	}
	syscall.RegCloseKey(k)
	return true
}

func registerNativeHostFor(exe, dataDir string) []error {
	var errs []error
	chromium, firefox := filepath.Join(dataDir, "nmhost", nmHostName+".json"), filepath.Join(dataDir, "nmhost", "firefox", nmHostName+".json")
	if err := writeIfChanged(chromium, nmManifestJSON(exe, false)); err != nil {
		return []error{err}
	}
	if err := writeIfChanged(firefox, nmManifestJSON(exe, true)); err != nil {
		return []error{err}
	}
	for i, r := range nmRegKeys {
		if i > 0 && !regKeyExists(r.vendor) {
			continue
		}
		key, manifest := r.vendor+`\NativeMessagingHosts\`+nmHostName, chromium
		if r.firefox {
			manifest = firefox
		}
		if regGetString(key, "") == manifest {
			continue
		}
		if err := regSetString(key, "", manifest); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
