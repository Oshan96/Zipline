#!/usr/bin/env bash
# Build Zipline's Windows programs (x64 and Arm64), with this version in their version info, into a folder.
# packaging/package.sh uses it; the release workflow runs it on its own first when it has the programs
# signed, then packages the signed ones (ZL_WIN_EXES; docs/releasing.md → Signing the Windows files).
# Usage: packaging/windows/build-exes.sh version outdir
set -euo pipefail
cd "$(dirname "$0")/../.."
V=$1 OUT=$2
mkdir -p "$OUT"
# the version info is in the icon's resource file: written for this version, then put back as it was
K=$(mktemp -d)
cp cmd/zipline/rsrc_windows_amd64.syso cmd/zipline/rsrc_windows_arm64.syso "$K/"
trap 'cp "$K"/rsrc_windows_*.syso cmd/zipline/; rm -rf "$K"' EXIT
python3 packaging/windows/mksyso.py assets/icon.ico cmd/zipline "$V" >/dev/null
L="-s -w -H windowsgui -X main.appVersion=$V"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$L" -o "$OUT/Zipline-windows-x64.exe" ./cmd/zipline
GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "$L" -o "$OUT/Zipline-windows-arm64.exe" ./cmd/zipline
