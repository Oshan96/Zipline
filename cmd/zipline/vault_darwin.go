//go:build darwin

package main

import (
	"os"
	"os/user"
	"path/filepath"
)

// On a Mac the key lives in the login Keychain (service "Zipline", the user's account name),
// read and written with the security command. If the Keychain can't be asked, nothing is
// sealed with a different key: see the notes in vault.go.
func platformSealer(dir string) sealer {
	if !systemKeyStore {
		return newGCMSealer(fileKeys{filepath.Join(dir, "vault.key")})
	}
	return newGCMSealer(keychainKeys{run: runCommand, user: keychainAccount()})
}

func keychainAccount() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if n := os.Getenv("USER"); n != "" {
		return n
	}
	return "zipline"
}
