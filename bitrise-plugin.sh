#!/bin/bash
# Runs `bitrise :herdr`. Until release binaries are published, the plugin is
# built from source on first use and rebuilt when the source changes.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$DIR/.bin/bitrise-herdr-plugin"
if [ ! -x "$BIN" ] || [ -n "$(find "$DIR" -name '*.go' -newer "$BIN" -print -quit)" ]; then
  if ! command -v go >/dev/null 2>&1; then
    echo "bitrise :herdr: Go is needed to build the plugin from source (https://go.dev/dl)" >&2
    exit 1
  fi
  (cd "$DIR" && go build -o "$BIN" .) >&2
fi
exec "$BIN" "$@"
