//go:build !windows && !darwin && !linux

package main

import "path/filepath"

// Other systems have no keyring Zipline knows how to ask: the key is a private file.
func platformSealer(dir string) sealer {
	return newGCMSealer(fileKeys{filepath.Join(dir, "vault.key")})
}
