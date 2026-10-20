package main

import (
	"errors"
	"path/filepath"
)

// One Zipline per user: a lock file in the user's data folder, held while the app runs. It used to be
// the port, but then any program that took 17890 first stopped Zipline from starting, and two users'
// Zipline would collide. The system drops the lock when the process ends, however it ends.
//   Unix: flock(LOCK_EX|LOCK_NB) on zipline.lock.
//   Windows: zipline.lock opened with no sharing (CreateFile, share mode 0): a second open fails
//   with a sharing violation. (A file rather than a named mutex, so the data folder, not the user
//   name, decides: two copies with different folders, as in the tests, don't block each other.)

var errLocked = errors.New("Zipline is already running")

func lockPath(dir string) string { return filepath.Join(dir, "zipline.lock") }
