#!/usr/bin/env bash
# Prints the version the next release gets: appVersion's series (cmd/zipline/version.go) with the next
# free patch number among the v<series>.<n> tags, like 1.0.4. Refuses a series lower than the newest
# one released, so versions only go up. Needs the tags (git fetch --tags).
set -euo pipefail
cd "$(dirname "$0")/.."
base=$(packaging/version.sh)
newest=$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | sed -n 's/^v\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\)$/\1/p' | sort -V | tail -1)
if [ -n "$newest" ] && [ "${newest%.*}" != "$base" ] && [ "$(printf '%s\n' "${newest%.*}" "$base" | sort -V | head -1)" = "$base" ]; then
  echo "appVersion $base is lower than the released $newest: raise it in cmd/zipline/version.go" >&2
  exit 1
fi
last=$(git tag -l "v$base.*" | sed -n "s/^v${base//./\\.}\.\([0-9][0-9]*\)$/\1/p" | sort -n | tail -1)
echo "$base.$((${last:--1} + 1))"
