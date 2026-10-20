# Contributing to Zipline

Thanks for helping make Zipline better. Bug reports, ideas and pull requests are all welcome.

## Reporting a bug

[Open a bug report](https://github.com/Oshan96/Zipline/issues/new/choose) with what you did, what happened and what you expected. **Help → Diagnostics** in Zipline gives your version, settings (without passwords) and the recent log, ready to paste in.

Found a security problem? Please don't open an issue: follow the [security policy](SECURITY.md) instead.

## Suggesting a feature

[Open a feature request](https://github.com/Oshan96/Zipline/issues/new/choose) describing what you're trying to do. For a bigger change, it's worth agreeing on the approach in an issue before writing the code.

## Sending a pull request

1. Fork the repository and branch from `main`.
2. Make your change. [docs/development.md](docs/development.md) explains the layout, how to build and how to test.
3. Run the checks: `gofmt -l .` prints nothing, `go vet ./...` passes for Windows, macOS and Linux, and `go test ./...` passes.
4. Open a pull request into `main`. Its checks build every package and installer, and its run's summary names the version your merge will release.

A few ground rules keep Zipline easy to build and safe to run:

- **Go's standard library only:** no third-party modules and no cgo.
- **Tests:** add one for every bug you fix. A test that depends on timing waits for its condition instead of sleeping for a fixed time.
- **Docs that stay true:** update [README.md](README.md) and the [user guide](docs/user-guide.md) in the same change as any feature, limitation or behaviour you change.
- **The window and the phone page:** colours come from `web/colors.css` (`python3 tools/contrast.py` must pass), words from `web/i18n.js`. Check light and dark themes and narrow windows.
- **Comments** say why, in plain words, in the style of the file you're in.

[CLAUDE.md](CLAUDE.md) has the full set of conventions, including the lock order the download manager relies on.

## Releases

Every merge to `main` is released as the next patch version (`v1.0.3` → `v1.0.4`). To start a new series, raise `appVersion` in `cmd/zipline/version.go` in your pull request. Label it `feature` or `bug` to sort it in the release notes. See [docs/releasing.md](docs/releasing.md).
