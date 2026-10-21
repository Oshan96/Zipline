//go:build windows

package main

import "testing"

// Go allows about 2,000 callbacks per process; looking for the window must not use one per call.
func TestFocusAppWindowManyTimes(t *testing.T) {
	for i := 0; i < 3000; i++ {
		focusAppWindow()
	}
}
