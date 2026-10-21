#!/usr/bin/env bash
# Sign the Firefox add-on for self-distribution: Mozilla signs it as an "unlisted" add-on (it isn't
# listed on addons.mozilla.org), and regular Firefox then installs the signed .xpi for good.
#
#   1. Sign in at https://addons.mozilla.org/developers/addon/api/key/ and generate API credentials.
#   2. export AMO_JWT_ISSUER='user:12345:678'  AMO_JWT_SECRET='…'   (keep them out of the repository)
#   3. packaging/sign-firefox.sh    → the signed .xpi lands in pkg/ext/signed/
#
# The add-on id (zipline-catcher@zipline.app) and version come from extension/manifest.firefox.json;
# Mozilla signs each version once, so raise the version before signing again. Needs Node.js (npx).
set -euo pipefail
cd "$(dirname "$0")/.."
: "${AMO_JWT_ISSUER:?set AMO_JWT_ISSUER (addons.mozilla.org → Developer Hub → Manage API Keys)}"
: "${AMO_JWT_SECRET:?set AMO_JWT_SECRET}"
SRC=$(mktemp -d)
trap 'rm -rf "$SRC"' EXIT
cp -r extension/. "$SRC/"
mv -f "$SRC/manifest.firefox.json" "$SRC/manifest.json"
mkdir -p pkg/ext/signed
# (web-ext reads its options from WEB_EXT_* variables: the secret never shows in the process list)
WEB_EXT_API_KEY="$AMO_JWT_ISSUER" WEB_EXT_API_SECRET="$AMO_JWT_SECRET" \
  npx --yes web-ext@8.3.0 sign --channel=unlisted --source-dir "$SRC" --artifacts-dir pkg/ext/signed
ls -la pkg/ext/signed
