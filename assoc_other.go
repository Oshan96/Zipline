//go:build !windows && !linux

package main

import "errors"

// On macOS and Linux the system's own settings decide which app opens magnet links
// (see the README); Zipline doesn't change them.
func torrentAssociation() (supported, on bool) { return false, false }

func setTorrentAssociation(on bool) error {
	return errors.New("set this in your system's settings")
}
