# Zipline: notes for working on this repository

Zipline is a standalone download manager in the style of IDM. It's one Go program that serves its own window: a web UI embedded from `web/` and opened in an Edge, Chrome or Brave app window. There's also a Chrome/Firefox extension in `extension/` and a phone page (`web/m.html`). It builds for Windows (x64, arm64), macOS (Apple silicon, Intel) and Linux (x64). `README.md` introduces it and `docs/user-guide.md` describes everything it does, from the user's side.

## Layout

- `cmd/zipline/`: the program (`package main`). File names below without a folder are here.
- `internal/<name>/`: code that stands on its own, without the download manager (`bencode`, `filename`, `httpauth`, `pac`, `psl`, `qr`, `quickxor`, `sitemap`, `utp`). Each package has its own tests. New code that needs nothing from `Manager`, `Download` or the settings goes in a package here.
- `web/`: the window and the phone page; `web/web.go` embeds them for the program.
- `extension/`: the browser extension. `assets/`: the icon and the docs' screenshots. `docs/`: the user guide, development and release docs.
- `packaging/`: `package.sh`, the Windows installer (`windows/`), the macOS disk image (`macos/`) and the `.deb` (`linux/`). `tools/`: contrast check, browser tests, screenshots.

## Ground rules

- **Go standard library only.** No third-party modules, no cgo. Don't edit `go.mod` (Go 1.24.7).
- **Version:** `appVersion` in `version.go` holds major.minor (`1.0`). Releases add the patch number themselves (see Releases). Raise it only for a new series. Keep `MIN_APP` (`extension/link.js`) and `minExtVersion` (`extlink.go`) as they are unless the app and extension stop working together.
- **No secrets in the repository.** The extension's private key (`.pem`) and the AMO and store credentials stay out of it (`.gitignore`). The extension ID comes from the public `key` in `extension/manifest.json`.
- **Keep the docs true.** Update `README.md` and `docs/user-guide.md` in the same change as any feature, limitation or behaviour you change.
- **Deferred ("Later"):** leave these alone unless asked:
  - J3: Windows signing and version info.
  - J4: a macOS menu-bar presence, signing and notarizing.
  - J5: notifications on every OS.
  - J6: start at login and a clean shutdown.
  - J9: updates and package-manager listings (the Windows installer, the macOS disk image and the `.deb` are done).
  - J10: a complete command line.
  - J11: honest file associations, and selecting the file in "Show in folder" on Linux.

## UI changes

- Keep styling and spacing consistent with what's there, and make every layout responsive. Check the window at 1280, 720 and 420 px wide, and the phone page at 390 and 360 px. Check both light and dark themes. Look for overflow, clipped text and uneven gaps, and take screenshots to confirm.
- Colours come from the tokens in `web/colors.css`. `python3 tools/contrast.py` must pass: 4.5:1 for text, 3:1 for controls.
- User-facing text goes in `web/i18n.js` and is used through `te("key")`. Keep it short and plain.
- Browser tests: `python3 tools/ui_list_test.py` (Playwright + Chromium).
- The window signs in with a one-time code: `zipline --login-url` prints a fresh address. Scripts call the API with the `X-Token` header from `run.json` in the data folder. Point Python's urllib at no proxy for 127.0.0.1.
- `python3 tools/screenshots.py` retakes `assets/screenshots/` (used by the README and the docs) after a visible change.

## Testing

Run these before you finish a change:

```
gofmt -l .                       # prints nothing
GOOS=windows go vet ./... && GOOS=darwin go vet ./... && go vet ./... && go vet -tags zlfake ./...
go test ./...                    # about 6 minutes
go test -race ./...              # about 8 minutes
```

- Bash calls time out at 10 minutes, so run the long suites in the background and check back.
- Tests that need python3 with libtorrent (torrent interop) or ffmpeg skip themselves when those are missing.
- If Wine is available, also run the Windows tests:
  ```
  GOOS=windows GOARCH=amd64 go test -c -o zl.exe ./cmd/zipline
  cd cmd/zipline && wine ../../zl.exe -test.v
  ```
  Run them from `cmd/zipline`, since some tests read `testdata/`; files elsewhere in the repository (`web/`, `extension/`) are found through `repoPath`. Wine has caught races, file-locking and DPAPI differences that Linux doesn't show.
- Add a test for every bug fixed. When a test is timing-sensitive, wait on the condition instead of sleeping a fixed time.
- `go build -tags zlfake ./cmd/zipline` points the cloud services (MEGA, OneDrive, Drive, Dropbox, …) at local stand-ins: `ZL_CLOUD_FAKE`, see `cloud_fake.go`.

## Code conventions

- Comments say why, in plain words. Follow the style of the file you're in.
- Locks:
  - A torrent's `t.mu` may be held while taking the manager's `m.mu`, never the reverse.
  - Never take `m.mu` while holding a download's `d.mu`.
  - Never take `t.mu` while holding a peer's `wmu`.
  - "Caller holds m.mu" comments mean exactly that.
- Security model (user guide → Security):
  - The window uses a session cookie (`session.go`).
  - Machine access uses `run.json` plus `X-Token`.
  - The extension uses the native messaging host or a paired, HMAC-signed HTTP link (`extlink.go`, `nativehost.go`).
  - The phone only works on a private LAN, after desktop approval (`phonenet.go`, `phonepair.go`).
  - Secrets are sealed by the vault (`vault*.go`).
  - Files are 0600 and folders 0700.
  - Changing the virus scanner is allowed only from the window.

## Packaging and releases

- `packaging/package.sh [version]` builds every platform into `pkg/`: the portable packages, the source, the Windows installer (`packaging/windows/installer.nsi`, NSIS: per-user, no administrator prompt, picks the x64 or Arm64 build) and the `.deb` (`packaging/linux/`). It needs Python with Pillow, `zip`, `makensis` and `dpkg-deb`. Every package carries the extensions and `docs/user-guide.md` as `USER-GUIDE.md`.
- `packaging/macos/make-dmg.sh` turns the two macOS zips into one universal `Zipline-macOS.dmg`. It needs macOS, so the workflow runs it on a macOS runner.
- The installer is a 32-bit NSIS program. 64-bit-only Wine can't run it; to try it under Wine, build it with `ZL_NSIS_TARGET=amd64-unicode`. Its uninstaller runs `Zipline.exe --unregister` (`unregister_windows.go`) to undo this copy's registry entries.
- `python3 packaging/windows/mksyso.py assets/icon.ico cmd/zipline` regenerates the Windows icon resource (`rsrc_windows_*.syso`).
- `.github/workflows/release.yml` runs on every pull request into `main` and every merge to `main`:
  - Both: tests, then every package and installer built with the version the merge will release (`packaging/next-version.sh`), shown in the run's summary and kept as its artifacts.
  - Merges: then the release, tagged `v<appVersion>.<n>` with the next free `n`, carrying every package and installer. A series lower than one already released is refused.
  - Re-run it from the Actions tab to release `main` again.
- `docs/releasing.md` describes all this for people; keep it in step.
