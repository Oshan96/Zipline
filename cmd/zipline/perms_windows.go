package main

import "os"

func makePrivateDir(dir string) error { return os.MkdirAll(dir, 0o755) }

func tightenDataDir(dir string) {}
