#!/usr/bin/env bash
# Build every platform into pkg/: the portable packages, the installers that can be made on Linux (the
# Windows Setup with NSIS, the .deb with dpkg-deb) and the source. The macOS disk image needs macOS:
# packaging/macos/make-dmg.sh turns the two macOS packages into one.
# Usage: packaging/package.sh [version]
# The version (1.0, 1.0.3) goes into the app (-X main.appVersion), the extensions' manifests and the
# installers. Without one, it's appVersion from cmd/zipline/version.go. Releases get theirs from
# .github/workflows/release.yml.
set -euo pipefail
cd "$(dirname "$0")/.."
V=${1:-$(packaging/version.sh)}
[[ $V =~ ^[0-9]+(\.[0-9]+){1,3}$ ]] || { echo "version must look like 1.0 or 1.0.3, not \"$V\"" >&2; exit 2; }
rm -rf dist pkg && mkdir -p dist pkg
L="-s -w -X main.appVersion=$V"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H windowsgui $L" -o dist/Zipline-windows-x64.exe ./cmd/zipline
GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "-H windowsgui $L" -o dist/Zipline-windows-arm64.exe ./cmd/zipline
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$L" -o dist/zipline-macos-apple-silicon ./cmd/zipline
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$L" -o dist/zipline-macos-intel ./cmd/zipline
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$L" -o dist/zipline-linux-x64 ./cmd/zipline
python3 -c "from PIL import Image; Image.open('extension/icons/icon256.png').save('pkg/zipline.icns')"
# browser extensions: one source, two manifests
X=pkg/ext && mkdir -p $X/chrome $X/firefox
cp -r extension/. $X/chrome/ && rm -f $X/chrome/manifest.firefox.json
cp -r extension/. $X/firefox/ && mv -f $X/firefox/manifest.firefox.json $X/firefox/manifest.json
for M in $X/chrome/manifest.json $X/firefox/manifest.json; do # (the extension carries the app's version)
  python3 -c "import re,sys; p=sys.argv[1]; s=open(p).read(); open(p,'w').write(re.sub(r'\"version\": \"[0-9.]+\"', '\"version\": \"'+sys.argv[2]+'\"', s, count=1))" $M "$V"
done
(cd $X/firefox && zip -qr ../zipline-firefox.xpi .)
# for the Chrome Web Store and Edge Add-ons: no "key" in the manifest. Without more, a store gives the extension an id of
# its own (add it to chromeExtIDs in nativehost.go). To keep Zipline's pinned id instead, set ZL_EXT_KEY to the private
# key's .pem file: it goes into the zip as key.pem (Chrome's older way of keeping an id on the first upload; see
# docs/releasing.md).
S=$(mktemp -d) && cp -r $X/chrome/. $S/ && python3 -c "import json,sys; m=json.load(open(sys.argv[1])); m.pop('key',None); json.dump(m,open(sys.argv[1],'w'),indent=2)" $S/manifest.json && { [ -z "${ZL_EXT_KEY:-}" ] || cp "$ZL_EXT_KEY" $S/key.pem; } && (cd $S && zip -qr - .) > $X/zipline-chrome-store.zip && rm -rf $S
addext() { mkdir -p "$1/extension-firefox"; cp -r $X/chrome "$1/extension"; cp -r $X/firefox/. "$1/extension-firefox/"; cp $X/zipline-firefox.xpi "$1/"; cp docs/user-guide.md "$1/USER-GUIDE.md"; }
for A in x64 arm64; do D=pkg/Zipline-Windows-$A; mkdir -p $D; cp dist/Zipline-windows-$A.exe $D/Zipline.exe; addext $D; (cd pkg && zip -qr Zipline-Windows-$A.zip Zipline-Windows-$A); done
for A in apple-silicon intel; do D=pkg/Zipline-macOS-$A; APP=$D/Zipline.app/Contents; mkdir -p $APP/MacOS $APP/Resources; cp dist/zipline-macos-$A $APP/MacOS/zipline; cp pkg/zipline.icns $APP/Resources/; sed "s/@VERSION@/$V/g" packaging/macos/Info.plist > $APP/Info.plist; addext $D; (cd pkg && zip -qry Zipline-macOS-$A.zip Zipline-macOS-$A); done
# the Windows installer, next to the portable zips (NSIS: apt install nsis)
if command -v makensis >/dev/null; then
  VI=$V; while [[ $VI =~ ^[0-9]+(\.[0-9]+){0,2}$ ]]; do VI=$VI.0; done # (Windows wants four parts)
  python3 packaging/windows/installer_art.py pkg/installer-side.bmp
  makensis -V2 -DVERSION="$V" -DVIVERSION="$VI" -DROOT="$PWD" -DOUT="$PWD/pkg/Zipline-Windows-Setup.exe" ${ZL_NSIS_TARGET:+-DTARGET=$ZL_NSIS_TARGET} packaging/windows/installer.nsi
else
  echo "makensis not found: no Windows installer this time (apt install nsis)" >&2
fi
D=pkg/Zipline-Linux-x64; mkdir -p $D; cp dist/zipline-linux-x64 $D/; addext $D; (cd pkg && tar czf Zipline-Linux-x64.tar.gz Zipline-Linux-x64)
# the Linux installer (dpkg-deb): the program in /usr/bin, its menu entry and icons, and the extensions and the guide
# in /usr/share, where the extension's folder stays put for "Load unpacked"
if command -v dpkg-deb >/dev/null; then
  R=$(mktemp -d)/zipline
  install -Dm755 dist/zipline-linux-x64 $R/usr/bin/zipline
  install -Dm644 packaging/linux/zipline.desktop $R/usr/share/applications/zipline.desktop
  for P in 16 32 48 128 256; do install -Dm644 extension/icons/icon$P.png $R/usr/share/icons/hicolor/${P}x$P/apps/zipline.png; done
  install -Dm644 assets/icon.svg $R/usr/share/icons/hicolor/scalable/apps/zipline.svg
  mkdir -p $R/usr/share/zipline && cp -r $D/extension $D/extension-firefox $D/zipline-firefox.xpi $R/usr/share/zipline/
  install -Dm644 docs/user-guide.md $R/usr/share/doc/zipline/USER-GUIDE.md
  chmod -R u=rwX,go=rX $R/usr/share && mkdir -p $R/DEBIAN
  sed -e "s/@VERSION@/$V/" -e "s/@SIZE@/$(du -sk $R/usr | cut -f1)/" packaging/linux/control > $R/DEBIAN/control
  dpkg-deb --root-owner-group -Zxz --build $R pkg/Zipline-Linux-x64.deb >/dev/null
  rm -rf "$(dirname $R)"
else
  echo "dpkg-deb not found: no .deb this time" >&2
fi
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then # the source as committed
  git archive --format=zip --prefix=zipline/ -o pkg/Zipline-source.zip HEAD
else
  N=$(basename "$PWD"); (cd .. && zip -qr "$N/pkg/Zipline-source.zip" "$N" -x "$N/dist/*" "$N/pkg/*" "$N/.git/*" "*.pem" "*/__pycache__/*")
fi
ls -la pkg/*.zip pkg/*.tar.gz; ls -la pkg/*.exe pkg/*.deb 2>/dev/null || true
