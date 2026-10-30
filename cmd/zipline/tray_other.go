//go:build !windows

package main

// The tray icon is Windows-only; on other systems these do nothing.
func startTray(m *Manager)                        {}
func traySetEnabled(on bool)                      {}
func trayNotify(title, text string, warning bool) {}
func trayDone(name string)                        {}
func trayAvailable() bool                         { return false }
