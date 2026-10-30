{
  # prq — `nix run github:akira-toriyama/prq` or `nix profile install`.
  #
  # The primary distribution is the Homebrew cask (see .goreleaser.yaml); this
  # flake is the secondary, source-built channel. version stays "dev" on purpose
  # — a source build has no release number, so there is nothing to go stale (the
  # commit is stamped from the flake's own git rev instead).
  #
  # vendorHash pins the vendored go modules; when go.mod/go.sum change, set it
  # back to pkgs.lib.fakeHash, run `nix build`, and paste the hash nix prints
  # ("got: sha256-...").
  description = "One-call PR state synthesis for AI coding agents — why is this PR blocked?";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        version = "dev";
        rev = self.shortRev or self.dirtyShortRev or "unknown";
        v = "github.com/akira-toriyama/prq/internal/version";
      in
      {
        # The Nix sandbox cannot download toolchains, so the builder's own go IS
        # the build toolchain: this path builds with the Go the locked nixpkgs
        # ships, not with go.mod's `toolchain` line. A stdlib security fix reaches
        # it when flake.lock moves to a nixpkgs whose go is at or past that line.
        packages.default = pkgs.buildGoModule {
          pname = "prq";
          inherit version;
          src = ./.;
          vendorHash = "sha256-D7CONmxKdjSLSOKBreeexqhO3/EXl7WDDFiFSd1UvrQ=";
          ldflags = [
            "-s" "-w"
            "-X ${v}.Version=${version}"
            "-X ${v}.Commit=${rev}"
          ];
          subPackages = [ "cmd/prq" ];
          meta = with pkgs.lib; {
            description = "One-call PR state synthesis for AI coding agents — why is this PR blocked?";
            homepage = "https://github.com/akira-toriyama/prq";
            license = licenses.mit;
            mainProgram = "prq";
          };
        };

        apps.default = flake-utils.lib.mkApp {
          drv = self.packages.${system}.default;
          name = "prq";
        };

        devShells.default = pkgs.mkShell {
          # go (not a pinned go_1_xx — nixpkgs removes EOL versions): a local go
          # at or past go.mod's `toolchain` line satisfies it with no download
          # (downloaded toolchains do not run on non-FHS NixOS), so keep the lock
          # at a nixpkgs whose go is at least that line.
          packages = [ pkgs.go pkgs.golangci-lint pkgs.goreleaser pkgs.git-cliff pkgs.govulncheck ];
        };
      });
}
