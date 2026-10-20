## What this changes

<!-- What and why, in a few sentences. Link the issue it fixes, if there is one. -->

## How it was tested

- [ ] `gofmt -l .` prints nothing, and `go vet ./...` passes for Windows, macOS and Linux
- [ ] `go test ./...` passes (and `go test -race ./...` for concurrency changes)
- [ ] A test covers the bug this fixes
- [ ] The window or phone page: checked in light and dark themes and at narrow widths
- [ ] README.md and the user guide describe the change

## Release

Merging releases the next patch version; this run's summary names it. To start a new series instead, raise `appVersion` in `cmd/zipline/version.go`. Label this `feature` or `bug` to sort it in the release notes.
