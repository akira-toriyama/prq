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

# Pin the toolchain to the go.mod floor (a patched patch-level minor) so a
# from-source install carries the same stdlib security fixes as CI/release
# builds. GOTOOLCHAIN=local would build with whatever go is on PATH — e.g. a
# 1.26.0–1.26.4 machine would ship an unpatched crypto/tls (GO-2026-5856).
# Missing toolchains download once (network) and are cached after that. An
# explicit GOTOOLCHAIN in the environment wins (escape hatch for offline
# machines whose local go is already patched).
GOTOOLCHAIN="${GOTOOLCHAIN:-go$(awk '/^go [0-9]/{print $2; exit}' go.mod)}"
export GOTOOLCHAIN

mkdir -p bin
go build -trimpath \
  -ldflags "-s -w -X '${PKG}.Version=${VERSION}' -X '${PKG}.Commit=${COMMIT}' -X '${PKG}.Date=${DATE}'" \
  -o bin/prq ./cmd/prq

echo "built: $DIR/bin/prq  (${VERSION})"
