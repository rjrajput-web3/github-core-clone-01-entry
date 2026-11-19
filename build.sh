#!/bin/sh
# Builds the statically linked forge binary next to this script.
set -e
cd "$(dirname "$0")"
GO="${GO:-$HOME/workspace/tmp/go125/go/bin/go}"
if ! command -v "$GO" >/dev/null 2>&1; then GO=go; fi
CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w" -o forge .
ls -l forge
