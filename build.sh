#!/bin/sh
# Build the forge binary: a static, trimmed Go build named "forge".
set -e
cd "$(dirname "$0")"

if ! command -v go >/dev/null 2>&1; then
  if [ -x "$HOME/workspace/tmp/go/bin/go" ]; then
    export PATH="$HOME/workspace/tmp/go/bin:$PATH"
  elif [ -x "/root/workspace/tmp/go/bin/go" ]; then
    export PATH="/root/workspace/tmp/go/bin:$PATH"
  fi
fi

export CGO_ENABLED=0
export GOFLAGS=-mod=mod
go build -trimpath -ldflags "-s -w" -o forge .
ls -l forge
