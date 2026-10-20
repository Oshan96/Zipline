package main

import "os"

// What Zipline keeps in its data folder is for its user alone: on macOS and Linux the folder is
// 0700 and every file in it 0600 (perms_unix.go). Windows folders under the user's profile are
// already private to the account.

const privateFile os.FileMode = 0o600
