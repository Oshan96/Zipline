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
| Every package and installer, built with the version the merge will get | ✓ (the run's **Artifacts**, kept 3 days) | ✓ (into a draft release) |
| A summary naming that version and listing the files | ✓ | ✓ |
| Check that every installer is there, tag `v<version>` and publish the release | | ✓ |

So each pull request shows the release its merge will make, and its packages can be tried before merging. If another pull request merges first, the next one takes the following number.

A release is put together as a draft, which only people who can push see: the packaging jobs add their files to it as they finish, and it's published once the tests pass and every installer is there. Releasing doesn't use the account's artifact storage, which is small for private repositories (500 MB on GitHub Free, shared by every repository); only the pull requests' previews do, and when it's full they say so in a warning instead of failing.

A commit that's already released is skipped. To release `main` again (after a failed run, say), run the workflow from the Actions tab: **Build and release → Run workflow**. On another branch, that builds the packages without releasing; tick **Rehearse the release** to also go through the release itself, into a draft that's deleted at the end.

The release notes start with a download table and list the pull requests merged since the last release, grouped by label (`.github/release.yml`: `feature` or `enhancement`, `bug` or `fix`, everything else; `skip-changelog` leaves one out).

## What a release holds

| File | What |
|---|---|
| `Zipline-Windows-Setup.exe` | The Windows installer: per user, no administrator prompt, picks the x64 or Arm64 build |
| `Zipline-Windows-x64.zip`, `Zipline-Windows-arm64.zip` | Portable Windows packages |
| `Zipline-macOS.dmg` | The macOS disk image: one app for Apple silicon and Intel |
| `Zipline-macOS-apple-silicon.zip`, `Zipline-macOS-intel.zip` | `Zipline.app` for each kind of Mac |
| `Zipline-Linux-x64.deb` | The Debian and Ubuntu package |
| `Zipline-Linux-x64.rpm` | The Fedora and openSUSE package |
| `Zipline-Linux-x64.tar.gz` | The Linux program on its own |
| `Zipline-source.zip` | The source |

Every package carries the browser extensions (`extension`, `extension-firefox`, `zipline-firefox.xpi`), the [user guide](user-guide.md) as `USER-GUIDE.md` and the `LICENSE`. The names have no version in them, so `https://github.com/Oshan96/Zipline/releases/latest/download/<file>` always points at the newest. Zipline's own update check (`cmd/zipline/update.go`) finds its file by these names too and takes it only from this repository's releases, checked against the SHA-256 GitHub publishes for each file, so keep the names as they are. **Update now** runs the installer with `/S /RUN`: silently, starting Zipline again when it's done.

## Building the packages yourself

```sh
packaging/package.sh [version]        # everything into pkg/
packaging/macos/make-dmg.sh           # on a Mac, afterwards: pkg/Zipline-macOS.dmg
```

`package.sh` builds every platform, the portable packages, the source, the Windows installer (`packaging/windows/installer.nsi`), the `.deb` and the `.rpm` (`packaging/linux/`). It needs Python with Pillow, `zip`, NSIS (`makensis`) for the Windows installer, `dpkg-deb` for the `.deb` and `rpmbuild` for the `.rpm` (`apt install nsis rpm`); without one of those it skips that installer. Without a version it uses `appVersion`.

- The Windows installer is a 32-bit NSIS program, which 64-bit-only Wine can't run; to try it under Wine, build it with `ZL_NSIS_TARGET=amd64-unicode packaging/package.sh`. Its uninstaller runs `Zipline.exe --unregister` (`cmd/zipline/unregister_windows.go`) to undo this copy's registry entries.
- `ZL_PLATFORMS` builds only some platforms: `ZL_PLATFORMS="macos" packaging/package.sh` makes just the macOS packages.
- The disk image needs macOS (`lipo`, `codesign`, `hdiutil`), so the workflow builds the macOS packages on a macOS runner (`ZL_PLATFORMS=macos`) and makes the image from them. The app gets an ad-hoc signature; it isn't signed with a Developer ID or notarized yet.

## Signing the Windows files

Windows warns before running a program that isn't signed ("Windows protected your PC"). The release workflow is ready to sign Zipline's Windows programs and its installer with [SignPath](https://about.signpath.io), which signs open-source projects for free through the [SignPath Foundation](https://signpath.org). Until SignPath is set up, it skips those steps and nothing is signed.

**What SignPath Foundation asks of a project:** an OSI-approved licence (MIT), the source in a public repository, releases built by GitHub Actions on GitHub's own runners, files whose product name and version are set (`packaging/windows/mksyso.py` writes them into `Zipline.exe`; the installer has its own), and a code signing policy on the project's page (below). Each signing request for the release certificate is approved by hand on SignPath's site.

**Setting it up:**

1. Apply at [signpath.org](https://signpath.org) with this repository.
2. In SignPath, once the project is accepted:
   - Name the project `zipline` (or set the variable below), and link the GitHub trusted build system to this repository.
   - Add two artifact configurations, pasted from `packaging/windows/signpath/`: `windows-programs` (the two `Zipline.exe` builds) and `windows-installer` (`Zipline-Windows-Setup.exe`).
   - Use the signing policy `release-signing` (SignPath Foundation's certificate; `test-signing` signs with a test certificate, for trying it out).
   - Add a CI user who may submit requests to that policy, and copy its API token.
3. In this repository, under Settings → Secrets and variables → Actions:
   - the variable `SIGNPATH_ORGANIZATION_ID` (SignPath's organization ID);
   - the secret `SIGNPATH_API_TOKEN` (the CI user's token);
   - if they differ from the defaults, the variables `SIGNPATH_PROJECT_SLUG` (`zipline`) and `SIGNPATH_POLICY_SLUG` (`release-signing`).
4. Add the code signing policy below to the README.

**What a release of `main` does then:**

1. It builds the two `Zipline.exe` with `packaging/windows/build-exes.sh` and hands them to SignPath as a short-lived artifact. It waits up to an hour for them to come back signed, which leaves time for someone to approve the request.
2. It packages the signed programs into the zips and the installer (`ZL_WIN_EXES=… packaging/package.sh`).
3. It has the installer signed the same way.
4. It checks every signature (`packaging/windows/signed.py`) and deletes the artifacts it handed over.

The release notes then say the Windows files are signed. Pull requests and other branches aren't signed, so their previews stay unsigned. The installer's uninstaller isn't signed: NSIS writes it while it builds the installer. The files handed to SignPath use the account's artifact storage for a few minutes (about 25 MB). If the storage is full, signing fails and so does the release: delete old artifacts, then run the workflow again. The Mac app isn't signed with a Developer ID or notarized; SignPath only signs Windows files.

**The code signing policy** for the README (SignPath Foundation's wording for the first line):

```markdown
## Code signing policy

Free code signing provided by [SignPath.io](https://about.signpath.io), certificate by [SignPath Foundation](https://signpath.org).

- Committers and reviewers: [the repository's contributors](https://github.com/Oshan96/Zipline/graphs/contributors)
- Approvers: [Oshan96](https://github.com/Oshan96)

Privacy: Zipline connects to other computers only for what its user asks it to do: the downloads themselves (and the
services and torrent peers they involve), the proxy test in Options, and, if the user turns update checks on, a daily
look at this repository's latest release on GitHub. It sends nothing else anywhere.
```

## The browser extension in the stores

Zipline's extension isn't listed in any store yet; users load it from the folder in their download. What to fill in for each store (the texts, the permissions' justifications, the privacy answers and the images in `assets/store/`) is in [store-listing.md](store-listing.md). For a store release:

- **Chrome Web Store, Edge Add-ons:** `packaging/package.sh` also builds `pkg/ext/zipline-chrome-store.zip`, the Chrome extension without the `key` field (stores don't accept it). The extension's ID comes from the public key in `manifest.json`'s `key` field, so the source needs no secret: building, packaging and loading the folder never use the private key. It matters only if you want a store copy to keep that ID: run `ZL_EXT_KEY=/path/to/zipline-extension-key.pem packaging/package.sh 1.0` before the first Chrome Web Store upload and the key goes into the zip as `key.pem` (the older way Chrome documented for keeping an ID; it may no longer be honoured). Chrome's current way runs the other way round: upload, take the store's public key (Package → View public key) into `manifest.json`'s `key`, so unpacked copies get the store's ID. Whichever ID the store copy ends up with, Edge Add-ons included, add it to `chromeExtIDs` in `cmd/zipline/nativehost.go` and `ZL_EXT_IDS` in `web/extlink.js`, so the native messaging host lets it in. Keep the `.pem` file private and out of the repository: whoever has it can publish an extension with Zipline's ID.
- **Firefox:** `packaging/sign-firefox.sh` has Mozilla sign the add-on for self-distribution (unlisted, `web-ext sign --channel=unlisted`), so regular Firefox installs it permanently; `packaging/sign-firefox.sh --listed` submits it for listing on addons.mozilla.org instead, with the listing from `packaging/stores/amo-metadata.json`. Either needs Node.js and an addons.mozilla.org API key in `AMO_JWT_ISSUER` and `AMO_JWT_SECRET` (Developer Hub → Manage API Keys); a signed `.xpi` lands in `pkg/ext/signed/`. Mozilla takes each version once, so give the release's version: `packaging/sign-firefox.sh --listed 1.1.0`.
