//go:build !windows && !darwin && !linux

package main

// markFromInternet: these systems have no common way to label downloaded files.
func markFromInternet(path, srcURL, referrer string) {}
