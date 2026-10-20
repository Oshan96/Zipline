# Developing Zipline

Zipline is one Go program that serves its own window: a web UI built into the binary and opened in an Edge, Chrome or Brave app window. It uses only Go's standard library (no third-party modules, no cgo), so a Go toolchain is all you need to build it.

## Repository layout

```
cmd/zipline/          the program (package main): downloads, torrents, streams, queues, the API, the window's server
  testdata/           seed files for the fuzz tests, and the libtorrent peer the torrent tests talk to
internal/             libraries the program is built from, each with its own tests
  bencode/            BitTorrent's encoding
  filename/           picking and cleaning file names the way browsers do
  httpauth/           HTTP Basic and Digest logins
  pac/                proxy auto-config scripts, with a small JavaScript interpreter
  psl/                the Public Suffix List
  qr/                 QR codes for pairing a phone
  quickxor/           OneDrive's QuickXorHash
  sitemap/            reading sitemaps
  utp/                uTP, and the mux that shares the torrent port's UDP socket
web/                  the window (index.html, app.js), the phone page (m.html), their shared colours and words
extension/            the browser extension (one source; manifest.firefox.json for Firefox)
assets/               the icon (icon.svg, icon.ico) and the screenshots in the docs
packaging/            building the release packages and installers
  windows/            the NSIS installer, its artwork and the Windows icon resource
  macos/              the app bundle's Info.plist and the disk image
  linux/              the .deb's control file and menu entry
tools/                checks and helpers: colour contrast, browser tests, screenshots
docs/                 the user guide, this file and the release process
.github/              the build-and-release workflow, issue and pull request templates
```

`cmd/zipline` is one package because its parts share the download manager's state (and the lock rules in [CLAUDE.md](../CLAUDE.md)). Code that stands on its own, with no need for the manager, lives in `internal/`.

## Build and run

You need Go 1.24 or newer.

```sh
go run ./cmd/zipline                                     # build and start it
go build -ldflags "-s -w" -o zipline ./cmd/zipline       # for this computer
GOOS=windows GOARCH=amd64 go build -ldflags "-H windowsgui -s -w" -o Zipline.exe ./cmd/zipline
```

The window signs in with a one-time code: `zipline --login-url` prints a fresh address. Scripts call the API with the `X-Token` header from `run.json` in the [data folder](user-guide.md#where-data-is-kept).

`go build -tags zlfake ./cmd/zipline` builds a copy for trying the cloud services without the internet: it sends their requests to stand-ins at the address in `ZL_CLOUD_FAKE` (each service under its own path; see `cmd/zipline/cloud_fake.go`). Release builds leave that file out.

## Tests

Run these before you send a change:

```sh
gofmt -l .                       # prints nothing
GOOS=windows go vet ./... && GOOS=darwin go vet ./... && go vet ./... && go vet -tags zlfake ./...
go test ./...                    # about 6 minutes
go test -race ./...              # about 8 minutes
python3 tools/contrast.py        # the colours meet WCAG contrast
```

- Tests that need Python with libtorrent (torrent interoperability) or ffmpeg skip themselves when those are missing.
- `python3 tools/ui_list_test.py` drives the window with Playwright and Chromium: keyboard use, the list's live updates and its layout at different widths.
- With Wine, run the Windows tests too. Wine has caught races, file locking and DPAPI differences that Linux doesn't show:
  ```sh
  GOOS=windows GOARCH=amd64 go test -c -o zl.exe ./cmd/zipline
  cd cmd/zipline && wine ../../zl.exe -test.v
  ```

## The window and the phone page

- Colours come from the tokens in `web/colors.css`; `python3 tools/contrast.py` checks them (4.5:1 for text, 3:1 for controls).
- User-facing words live in `web/i18n.js` and are used through `te("key")`.
- Check a change in the light and dark themes, with the window at 1280, 720 and 420 pixels wide and the phone page at 390 and 360.
- `python3 tools/screenshots.py` retakes the screenshots in `assets/screenshots/`, with real downloads from a local stand-in server.

## Icons

`assets/icon.svg` is the icon; `assets/icon.ico` and the PNGs in `extension/icons/` are drawn from it. After changing `icon.ico`, rebuild the Windows resource that gives `Zipline.exe` its icon:

```sh
python3 packaging/windows/mksyso.py assets/icon.ico cmd/zipline
```

## Releases

Every merge to `main` is released. See [releasing.md](releasing.md) for versions, the packages and the installers.
