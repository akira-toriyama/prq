#!/bin/sh
# build.sh — build prq into bin/prq with the version/commit/date stamped from
# git. Used by install.sh (the from-source install channel).
set -eu
DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

VERSION="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PKG="github.com/akira-toriyama/prq/internal/version"

mkdir -p bin
GOTOOLCHAIN=local go build -trimpath \
  -ldflags "-s -w -X '${PKG}.Version=${VERSION}' -X '${PKG}.Commit=${COMMIT}' -X '${PKG}.Date=${DATE}'" \
  -o bin/prq ./cmd/prq

echo "built: $DIR/bin/prq  (${VERSION})"
