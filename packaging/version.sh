#!/usr/bin/env bash
# Prints the version series in the source (appVersion in cmd/zipline/version.go): major.minor, like 1.0.
set -euo pipefail
cd "$(dirname "$0")/.."
v=$(sed -n 's/^var appVersion = "\([0-9][0-9]*\.[0-9][0-9]*\)"$/\1/p' cmd/zipline/version.go)
[ -n "$v" ] || { echo 'cmd/zipline/version.go: appVersion should hold the major and minor version only, like "1.0"' >&2; exit 1; }
echo "$v"
