//go:build !windows

package main

// unregisterCopy is for the Windows uninstaller (unregister_windows.go). macOS and Linux have no
// installer: Zipline there is just its files, so --unregister does nothing.
func unregisterCopy() {}
