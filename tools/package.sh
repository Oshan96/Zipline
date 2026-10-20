#!/usr/bin/env bash
# Build every platform and zip them into pkg/, with a Windows installer when NSIS is there.
# Usage: tools/package.sh [version]
# The version (1.0, 1.0.3) goes into the app (-X main.appVersion) and the extensions' manifests. Without
# one, it's appVersion from phone.go. Releases get theirs from .github/workflows/release.yml.
set -euo pipefail
cd "$(dirname "$0")/.."
V=${1:-$(grep -oE 'appVersion = "[0-9.]+"' phone.go | grep -oE '[0-9.]+')}
[[ $V =~ ^[0-9]+(\.[0-9]+){1,3}$ ]] || { echo "version must look like 1.0 or 1.0.3, not \"$V\"" >&2; exit 2; }
rm -rf dist pkg && mkdir -p dist pkg
L="-s -w -X main.appVersion=$V"
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H windowsgui $L" -o dist/Zipline-windows-x64.exe .
GOOS=windows GOARCH=arm64 go build -trimpath -ldflags "-H windowsgui $L" -o dist/Zipline-windows-arm64.exe .
GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$L" -o dist/zipline-macos-apple-silicon .
GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$L" -o dist/zipline-macos-intel .
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "$L" -o dist/zipline-linux-x64 .
python3 -c "from PIL import Image; Image.open('extension/icons/icon256.png').save('pkg/zipline.icns')"
PLIST="<?xml version=\"1.0\" encoding=\"UTF-8\"?>
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">
<plist version=\"1.0\"><dict>
<key>CFBundleName</key><string>Zipline</string><key>CFBundleDisplayName</key><string>Zipline</string>
<key>CFBundleIdentifier</key><string>app.zipline.downloadmanager</string><key>CFBundleVersion</key><string>$V</string>
<key>CFBundleShortVersionString</key><string>$V</string><key>CFBundleExecutable</key><string>zipline</string>
<key>CFBundleIconFile</key><string>zipline.icns</string><key>CFBundlePackageType</key><string>APPL</string>
<key>LSUIElement</key><true/><key>LSMinimumSystemVersion</key><string>11.0</string>
<key>LSFileQuarantineEnabled</key><true/>
</dict></plist>"
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
# key's .pem file: it goes into the zip as key.pem (Chrome's older way of keeping an id on the first upload; see README).
S=$(mktemp -d) && cp -r $X/chrome/. $S/ && python3 -c "import json,sys; m=json.load(open(sys.argv[1])); m.pop('key',None); json.dump(m,open(sys.argv[1],'w'),indent=2)" $S/manifest.json && { [ -z "${ZL_EXT_KEY:-}" ] || cp "$ZL_EXT_KEY" $S/key.pem; } && (cd $S && zip -qr - .) > $X/zipline-chrome-store.zip && rm -rf $S
addext() { mkdir -p "$1/extension-firefox"; cp -r $X/chrome "$1/extension"; cp -r $X/firefox/. "$1/extension-firefox/"; cp $X/zipline-firefox.xpi "$1/"; }
for A in x64 arm64; do D=pkg/Zipline-Windows-$A; mkdir -p $D; cp dist/Zipline-windows-$A.exe $D/Zipline.exe; addext $D; cp README.md $D/; (cd pkg && zip -qr Zipline-Windows-$A.zip Zipline-Windows-$A); done
for A in apple-silicon intel; do D=pkg/Zipline-macOS-$A; APP=$D/Zipline.app/Contents; mkdir -p $APP/MacOS $APP/Resources; cp dist/zipline-macos-$A $APP/MacOS/zipline; cp pkg/zipline.icns $APP/Resources/; echo "$PLIST" > $APP/Info.plist; addext $D; cp README.md $D/; (cd pkg && zip -qry Zipline-macOS-$A.zip Zipline-macOS-$A); done
# the Windows installer, next to the portable zips (NSIS: apt install nsis)
if command -v makensis >/dev/null; then
  VI=$V; while [[ $VI =~ ^[0-9]+(\.[0-9]+){0,2}$ ]]; do VI=$VI.0; done # (Windows wants four parts)
  python3 tools/installer_art.py pkg/installer-side.bmp
  makensis -V2 -DVERSION="$V" -DVIVERSION="$VI" -DROOT="$PWD" -DOUT="$PWD/pkg/Zipline-Windows-Setup.exe" ${ZL_NSIS_TARGET:+-DTARGET=$ZL_NSIS_TARGET} tools/installer.nsi
else
  echo "makensis not found: no Windows installer this time (apt install nsis)" >&2
fi
D=pkg/Zipline-Linux-x64; mkdir -p $D; cp dist/zipline-linux-x64 $D/; addext $D; cp README.md $D/; (cd pkg && tar czf Zipline-Linux-x64.tar.gz Zipline-Linux-x64)
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then # the source as committed
  git archive --format=zip --prefix=zipline/ -o pkg/Zipline-source.zip HEAD
else
  N=$(basename "$PWD"); (cd .. && zip -qr "$N/pkg/Zipline-source.zip" "$N" -x "$N/dist/*" "$N/pkg/*" "$N/.git/*" "*.pem" "*/__pycache__/*")
fi
ls -la pkg/*.zip pkg/*.tar.gz; ls -la pkg/*.exe 2>/dev/null || true
