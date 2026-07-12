#!/bin/sh
# check.sh — the full local verification, runnable by you or by Claude Code with
# no TTY. Mirrors what CI enforces (build.yml → shared go-ci reusable: module
# hygiene / build / vet / race-test / lint; plus govulncheck), so a green run
# here means a green CI. The toolchain is pinned to the go.mod floor — the same
# version CI's go-version-file installs — so every step (govulncheck included)
# sees the exact stdlib CI sees. GOTOOLCHAIN=local would instead use whatever go
# is on PATH, which both diverges from CI and can carry open stdlib vulns
# (GO-2026-5856 on 1.26.0–1.26.4). First run may download the floor toolchain
# once (network); an explicit GOTOOLCHAIN in the environment wins.
set -eu
cd "$(dirname "$0")/.."
GOTOOLCHAIN="${GOTOOLCHAIN:-go$(awk '/^go [0-9]/{print $2; exit}' go.mod)}"
export GOTOOLCHAIN

echo "→ module hygiene (go mod tidy -diff + verify)"
go mod tidy -diff
go mod verify

echo "→ go build"
go build ./...

echo "→ go vet"
go vet ./...

echo "→ go test -race"
go test -race ./...

if command -v golangci-lint >/dev/null 2>&1; then
  echo "→ golangci-lint"
  golangci-lint run ./...
else
  echo "→ golangci-lint (skipped — not installed; CI runs it)"
fi

if command -v govulncheck >/dev/null 2>&1; then
  echo "→ govulncheck"
  govulncheck ./...
else
  echo "→ govulncheck (skipped — not installed; CI runs it)"
fi

echo "→ build binary for live checks"
go build -o bin/prq ./cmd/prq
BIN="$(pwd)/bin/prq"

echo "→ smoke: --version / --help"
"$BIN" --version >/dev/null
"$BIN" --help >/dev/null
echo "✓ all checks passed"
