#!/bin/sh
# Build the statically linked Go binary committed alongside the sources.
# Requires Go 1.25+. Produces ./forge (stripped, ~6.5 MB, fits the 8 MiB
# per-file entry cap).
set -e
cd "$(dirname "$0")"
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
ls -la forge
