#!/usr/bin/env bash
# Make pkg/Zipline-macOS.dmg: one disk image for Apple silicon and Intel Macs, from the two macOS packages
# that packaging/package.sh built. It needs macOS (lipo, codesign and hdiutil); the release workflow
# runs it on a macOS runner.
# Usage: packaging/macos/make-dmg.sh [pkg folder]
#
# The image holds Zipline.app (both builds in one program), a link to Applications to drag it to, the
# browser extensions, the user guide and the licence. The app gets an ad-hoc signature, so macOS sees
# a whole, unchanged bundle; it still isn't signed with a Developer ID or notarized.
set -euo pipefail
cd "$(dirname "$0")/../.."
P=${1:-pkg}
W=$(mktemp -d)
trap 'rm -rf "$W"' EXIT
unzip -q "$P/Zipline-macOS-apple-silicon.zip" -d "$W/arm"
unzip -q "$P/Zipline-macOS-intel.zip" -d "$W/intel"
A="$W/arm/Zipline-macOS-apple-silicon" I="$W/intel/Zipline-macOS-intel" S="$W/Zipline"
mkdir -p "$S/Browser extension"
cp -R "$A/Zipline.app" "$S/"
lipo -create -output "$S/Zipline.app/Contents/MacOS/zipline" "$A/Zipline.app/Contents/MacOS/zipline" "$I/Zipline.app/Contents/MacOS/zipline"
codesign --force --deep --sign - "$S/Zipline.app"
codesign --verify --deep --strict "$S/Zipline.app"
ln -s /Applications "$S/Applications"
cp -R "$A/extension" "$A/extension-firefox" "$A/zipline-firefox.xpi" "$S/Browser extension/"
cp "$A/USER-GUIDE.md" "$A/LICENSE" "$S/"
hdiutil create -volname Zipline -srcfolder "$S" -fs HFS+ -format UDZO -ov "$P/Zipline-macOS.dmg"
lipo -info "$S/Zipline.app/Contents/MacOS/zipline"
ls -la "$P/Zipline-macOS.dmg"
