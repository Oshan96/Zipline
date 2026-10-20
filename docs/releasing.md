# Releasing Zipline

Every merge to `main` is tested, built and published as a [GitHub release](https://github.com/Oshan96/Zipline/releases) by `.github/workflows/release.yml`. There's nothing to do by hand.

## Versions

`appVersion` in `cmd/zipline/version.go` holds the major and minor version (`1.0`). Each release takes the next patch number for them: `v1.0.0`, `v1.0.1`, `v1.0.2`, …

- **A normal change** needs nothing: its merge releases the next patch version.
- **A new series:** raise `appVersion` (to `1.1`, or `2.0`) in the pull request, and its merge releases `v1.1.0`.
- Versions only go up: a series lower than one already released is refused (`packaging/next-version.sh`).

The full version goes into the app (`-ldflags "-X main.appVersion=…"`), both extensions' manifests and the installers.

## What the workflow does

| | Pull request into `main` | Merge to `main` |
|---|---|---|
| Format, vet (Windows, macOS, Linux), contrast and tests | ✓ | ✓ |
| Every package and installer, built with the version the merge will get | ✓ (the run's **Artifacts**) | ✓ |
| A summary naming that version and listing the files | ✓ | ✓ |
| Tag `v<version>` and publish the release | | ✓ |

So each pull request shows the release its merge will make, and its packages can be tried before merging. If another pull request merges first, the next one takes the following number.

A commit that's already released is skipped. To release `main` again (after a failed publish, say), run the workflow from the Actions tab: **Build and release → Run workflow**. On another branch, that builds the packages without releasing.

The release notes start with a download table and list the pull requests merged since the last release, grouped by label (`.github/release.yml`: `feature` or `enhancement`, `bug` or `fix`, everything else; `skip-changelog` leaves one out).

## What a release holds

| File | What |
|---|---|
| `Zipline-Windows-Setup.exe` | The Windows installer: per user, no administrator prompt, picks the x64 or Arm64 build |
| `Zipline-Windows-x64.zip`, `Zipline-Windows-arm64.zip` | Portable Windows packages |
| `Zipline-macOS.dmg` | The macOS disk image: one app for Apple silicon and Intel |
| `Zipline-macOS-apple-silicon.zip`, `Zipline-macOS-intel.zip` | `Zipline.app` for each kind of Mac |
| `Zipline-Linux-x64.deb` | The Debian and Ubuntu package |
| `Zipline-Linux-x64.tar.gz` | The Linux program on its own |
| `Zipline-source.zip` | The source |

Every package carries the browser extensions (`extension`, `extension-firefox`, `zipline-firefox.xpi`) and the [user guide](user-guide.md) as `USER-GUIDE.md`. The names have no version in them, so `https://github.com/Oshan96/Zipline/releases/latest/download/<file>` always points at the newest.

## Building the packages yourself

```sh
packaging/package.sh [version]        # everything into pkg/
packaging/macos/make-dmg.sh           # on a Mac, afterwards: pkg/Zipline-macOS.dmg
```

`package.sh` builds every platform, the portable packages, the source, the Windows installer (`packaging/windows/installer.nsi`) and the `.deb`. It needs Python with Pillow, `zip`, NSIS (`makensis`) for the Windows installer and `dpkg-deb` for the `.deb`; without NSIS or `dpkg-deb` it skips that installer. Without a version it uses `appVersion`.

- The Windows installer is a 32-bit NSIS program, which 64-bit-only Wine can't run; to try it under Wine, build it with `ZL_NSIS_TARGET=amd64-unicode packaging/package.sh`. Its uninstaller runs `Zipline.exe --unregister` (`cmd/zipline/unregister_windows.go`) to undo this copy's registry entries.
- The disk image needs macOS (`lipo`, `codesign`, `hdiutil`), so the workflow makes it on a macOS runner from the two macOS zips. The app gets an ad-hoc signature; it isn't signed with a Developer ID or notarized yet.

## The browser extension in the stores

Zipline's extension isn't listed in any store yet; users load it from the folder in their download. For a store release:

- **Chrome Web Store, Edge Add-ons:** `packaging/package.sh` also builds `pkg/ext/zipline-chrome-store.zip`, the Chrome extension without the `key` field (stores don't accept it). The extension's ID comes from the public key in `manifest.json`'s `key` field, so the source needs no secret: building, packaging and loading the folder never use the private key. It matters only if you want a store copy to keep that ID: run `ZL_EXT_KEY=/path/to/zipline-extension-key.pem packaging/package.sh 1.0` before the first Chrome Web Store upload and the key goes into the zip as `key.pem` (the older way Chrome documented for keeping an ID; it may no longer be honoured). Chrome's current way runs the other way round: upload, take the store's public key (Package → View public key) into `manifest.json`'s `key`, so unpacked copies get the store's ID. Whichever ID the store copy ends up with, Edge Add-ons included, add it to `chromeExtIDs` in `cmd/zipline/nativehost.go` and `ZL_EXT_IDS` in `web/extlink.js`, so the native messaging host lets it in. Keep the `.pem` file private and out of the repository: whoever has it can publish an extension with Zipline's ID.
- **Firefox:** `packaging/sign-firefox.sh` has Mozilla sign the add-on for self-distribution (unlisted, `web-ext sign --channel=unlisted`), so regular Firefox installs it permanently. It needs Node.js and an addons.mozilla.org API key in `AMO_JWT_ISSUER` and `AMO_JWT_SECRET` (Developer Hub → Manage API Keys); the signed `.xpi` lands in `pkg/ext/signed/`. A listed add-on is submitted on addons.mozilla.org itself. Raise the version in both manifests for each new signing.
