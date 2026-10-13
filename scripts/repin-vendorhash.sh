#!/bin/sh
# Canonical standard file managed by fleet-sync (.github/workflows/fleet-sync.yml).
# Do NOT edit per-repo copies — edit THIS file; fleet-sync propagates it to every
# owned, non-fork, non-archived repo whose ROOT carries a flake.nix (the probe on
# its MANIFEST entry). Synced to: scripts/repin-vendorhash.sh
#
# Re-pin flake.nix's vendorHash to the go.sum in the tree, in one command.
#
# A `buildGoModule` flake pins the vendored modules with a fixed-output hash, and
# that hash is nothing dependabot can write: every gomod bump changes go.sum and
# leaves the pin stale, so `nix build` fails until a hand performs the ritual
# (set vendorHash = pkgs.lib.fakeHash, `nix build`, paste the `got:` hash). The
# hub's fleet-repin runs this on each dependabot gomod branch; a session that
# changes go.mod/go.sum by hand runs it before pushing.
#
# The optional stamp. A repo may carry `# go.sum sha256: <sha256 of go.sum>` in
# flake.nix (furrow does; its check-version-lockstep.sh compares the stamp to the
# real go.sum in CI, which catches a forgotten re-pin without nix). With a stamp
# this is a cheap no-op when it already matches go.sum, and the stamp is
# refreshed on every re-pin; without one the probe build always runs.
#
# Contract:
#   - exit 0 and no write when the stamp already matches go.sum (`--force`
#     re-derives the hash anyway; a repo without a stamp always derives);
#   - exit 1 without touching flake.nix when nix is absent, flake.nix carries no
#     single vendorHash line, or `nix build` fails for any reason other than the
#     expected fixed-output mismatch;
#   - on success flake.nix carries the derived hash (and the fresh stamp, where
#     there is one) and the final `nix build` with that hash has passed — the
#     file is never left on fakeHash;
#   - exit 2 on usage.
# Writes only flake.nix. Needs `nix` with flakes, run from anywhere inside the
# repo (it cd's to the repo root: scripts/..).
set -eu
cd "$(dirname "$0")/.."

force=false
case "${1:-}" in
  "") ;;
  --force) force=true ;;
  *) echo "usage: scripts/repin-vendorhash.sh [--force]" >&2; exit 2 ;;
esac

[ -f flake.nix ] || { echo "✖ no flake.nix at the repo root — nothing to re-pin" >&2; exit 1; }
[ -f go.sum ] || { echo "✖ no go.sum at the repo root — nothing to pin the vendorHash to" >&2; exit 1; }

if command -v shasum >/dev/null 2>&1; then
  gosum="$(shasum -a 256 go.sum | cut -d' ' -f1)"
else
  gosum="$(sha256sum go.sum | cut -d' ' -f1)"
fi
stamp="$(sed -n 's/^[[:space:]]*# go\.sum sha256: \([0-9a-f]\{64\}\).*/\1/p' flake.nix | head -1)"
current="$(sed -n 's/^[[:space:]]*vendorHash = "\(sha256-[A-Za-z0-9+\/=]*\)";.*/\1/p' flake.nix | head -1)"

if [ -n "$stamp" ] && [ "$stamp" = "$gosum" ] && [ -n "$current" ] && [ "$force" = false ]; then
  echo "ok — flake.nix vendorHash stamp already matches go.sum ($current)"
  exit 0
fi

if ! command -v nix >/dev/null 2>&1; then
  echo "✖ nix is not on PATH — the vendorHash can only be derived by nix" >&2
  exit 1
fi

# Exactly one vendorHash line, whatever it currently holds (a pinned SRI hash,
# or a fakeHash left behind by an interrupted hand ritual).
n="$(grep -c '^[[:space:]]*vendorHash = ' flake.nix || true)"
if [ "$n" != 1 ]; then
  echo "✖ flake.nix must carry exactly one 'vendorHash = ' line (found $n)" >&2
  exit 1
fi

orig="$(mktemp)"
cp flake.nix "$orig"
restore() { cp "$orig" flake.nix; rm -f "$orig"; }

sed 's/^\([[:space:]]*\)vendorHash = .*;/\1vendorHash = pkgs.lib.fakeHash;/' "$orig" > flake.nix

# The probe build is EXPECTED to fail: a fixed-output derivation built against
# fakeHash reports the real hash as `got:`. Any other failure (eval error, no
# network, a go.mod the vendoring cannot satisfy) carries no `got:` and is the
# caller's to read.
out="$(nix build .# --no-link 2>&1 || true)"
got="$(printf '%s\n' "$out" | sed -n 's/.*got:[[:space:]]*\(sha256-[A-Za-z0-9+\/=]*\).*/\1/p' | head -1)"
if [ -z "$got" ]; then
  restore
  printf '%s\n' "$out" >&2
  echo "✖ nix build printed no 'got: sha256-…' — flake.nix left untouched" >&2
  exit 1
fi

sed -e "s|^\([[:space:]]*\)vendorHash = .*;|\1vendorHash = \"$got\";|" \
    -e "s/^\([[:space:]]*# go\.sum sha256: \)[0-9a-f]\{64\}/\1$gosum/" "$orig" > flake.nix

if [ -n "$stamp" ] && ! grep -q "^[[:space:]]*# go\.sum sha256: $gosum" flake.nix; then
  restore
  echo "✖ flake.nix's '# go.sum sha256:' stamp could not be refreshed — flake.nix left untouched" >&2
  exit 1
fi

# The hash is only a pin once a build with it succeeds.
if ! nix build .# --no-link; then
  restore
  echo "✖ nix build failed with the derived vendorHash $got — flake.nix left untouched" >&2
  exit 1
fi
rm -f "$orig"

stamped="no stamp line to refresh"
[ -z "$stamp" ] || stamped="stamp refreshed to go.sum $gosum"
if [ "$got" = "$current" ]; then
  echo "re-pinned — vendorHash unchanged ($got); $stamped"
else
  echo "re-pinned — vendorHash ${current:-<none>} -> $got; $stamped"
fi
