#!/usr/bin/env bash
# Have Mozilla sign the Firefox add-on.
#
#   packaging/sign-firefox.sh [--listed] [version]
#
# Without --listed it's signed for self-distribution: an "unlisted" add-on that isn't on addons.mozilla.org,
# which regular Firefox then installs for good. The signed .xpi lands in pkg/ext/signed/.
# With --listed it's submitted for listing on addons.mozilla.org, with the listing's details from
# packaging/stores/amo-metadata.json (docs/store-listing.md). Mozilla reviews it first; once the review
# passes, the add-on shows on the site and the signed .xpi can be downloaded there.
#
#   1. Sign in at https://addons.mozilla.org/developers/addon/api/key/ and generate API credentials.
#   2. export AMO_JWT_ISSUER='user:12345:678'  AMO_JWT_SECRET='…'   (keep them out of the repository)
#   3. packaging/sign-firefox.sh --listed 1.1.0
#
# The add-on id (zipline-catcher@zipline.app) comes from extension/manifest.firefox.json, and the version
# given goes into the manifest as packaging/package.sh does (without one, the manifest's own). Mozilla
# takes each version once, on either channel. Needs Node.js (npx).
set -euo pipefail
cd "$(dirname "$0")/.."
CHANNEL=unlisted
if [ "${1:-}" = --listed ]; then CHANNEL=listed; shift; fi
V=${1:-}
[ -z "$V" ] || [[ $V =~ ^[0-9]+(\.[0-9]+){1,3}$ ]] || { echo "version must look like 1.1 or 1.1.0, not \"$V\"" >&2; exit 2; }
: "${AMO_JWT_ISSUER:?set AMO_JWT_ISSUER (addons.mozilla.org → Developer Hub → Manage API Keys)}"
: "${AMO_JWT_SECRET:?set AMO_JWT_SECRET}"
SRC=$(mktemp -d)
trap 'rm -rf "$SRC"' EXIT
cp -r extension/. "$SRC/"
mv -f "$SRC/manifest.firefox.json" "$SRC/manifest.json"
if [ -n "$V" ]; then
  python3 -c "import re,sys; p=sys.argv[1]; s=open(p).read(); open(p,'w').write(re.sub(r'\"version\": \"[0-9.]+\"', '\"version\": \"'+sys.argv[2]+'\"', s, count=1))" "$SRC/manifest.json" "$V"
fi
MORE=()
if [ "$CHANNEL" = listed ]; then MORE=(--amo-metadata "$PWD/packaging/stores/amo-metadata.json"); fi
mkdir -p pkg/ext/signed
# (web-ext reads its options from WEB_EXT_* variables: the secret never shows in the process list)
WEB_EXT_API_KEY="$AMO_JWT_ISSUER" WEB_EXT_API_SECRET="$AMO_JWT_SECRET" \
  npx --yes web-ext@8.3.0 sign --channel="$CHANNEL" "${MORE[@]}" --source-dir "$SRC" --artifacts-dir pkg/ext/signed
ls -la pkg/ext/signed
