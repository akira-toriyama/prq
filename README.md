# prq

One-call PR state synthesis for AI coding agents — *why is this PR blocked?*
(Go, reuses `gh` auth)

`gh pr view` answers with an opaque `BLOCKED` enum; diagnosing it takes 3–5
stitched API calls, unresolved review-thread counts aren't exposed at all
(cli/cli#12273), and `mergeable` starts out `UNKNOWN` so agents burn turns
re-polling (cli/cli#2544). prq does the whole loop in one call and returns a
synthesized verdict: compact, deterministic JSON with pasteable next actions.

```console
$ prq 13781 -R cli/cli
{"pr":"cli/cli#13781","state":"BLOCKED","fp":"6738178d952c5804","blockers":[
 "draft: marked as draft -> gh pr ready 13781 -R cli/cli",
 "review: REVIEW_REQUIRED"],"behind_by":7,"ci":"NONE","review":"REVIEW_REQUIRED"}
$ echo $?
1
```

## Usage

```console
prq                        # PR of the current branch (repo derived from cwd)
prq 123                    # by number (repo from cwd or -R)
prq 123 -R owner/repo
prq owner/repo#123
prq https://github.com/owner/repo/pull/123
prq my-branch -R owner/repo
prq --mine                 # my open PRs, NDJSON one line per PR (~150 B each)
prq --mine --repos a/x,b/y --limit 30
```

- **`blockers[]`** — everything an actor must do, each with a pasteable
  command where one exists (`gh pr ready`, `gh pr update-branch`,
  `cifail --run <id> -R <repo>` for failing required checks).
- **`pending[]`** — things that resolve on their own (required checks running,
  merge queue progressing, mergeability computing).
- **`fp`** — a fingerprint of the actionable state. Babysit loops compare it
  for equality; queue-position drift, base-branch advancement, optional-check
  churn, and no-op pushes don't change it.
- **`mergeable: UNKNOWN` retry built in** — slim probes (0.5/1/2/4 s ladder,
  `--retry-budget`, default 8 s) instead of wasted agent turns.
- **`unresolved_threads`** — the count `gh` doesn't expose; read/write of
  threads belongs to [gh pr-review](https://github.com/agynio/gh-pr-review).
- **Read-only.** prq never merges, re-runs, or mutates anything.

## Exit codes

| code | meaning |
|---|---|
| 0 | nothing prevents merge (`CLEAN`, `UNSTABLE`, `MERGED`) |
| 1 | blocked; an actor must act (`BLOCKED`, `CLOSED`) |
| 2 | time will change it; poll again (`PENDING`, `QUEUED`, `UNKNOWN`) |
| 3 | auth/permission failure, or the verdict is not certifiable (degraded green) |
| 4 | transient failure; retrying later is meaningful |
| 64 | usage |

Errors are one JSON object on stderr (`{"error":{"code":…,"message":…,"hint":…}}`);
stdout carries only the verdict document.

## Design

The full contract — output schema, blocker grammar, the complete
`mergeStateStatus` decision table, GraphQL queries, and the fingerprint
canonicalization — lives in [docs/design.md](docs/design.md). Facts there were
verified against the live GitHub GraphQL schema.

## Install

```sh
brew install akira-toriyama/tap/prq
# or
go install github.com/akira-toriyama/prq/cmd/prq@latest
# or from a checkout
./install.sh            # → ~/.local/bin/prq
# or with nix
nix run github:akira-toriyama/prq -- --help
```

> `go install …@latest` builds with whatever Go you have locally — Go never
> downgrades to the go.mod floor, so keep that toolchain patched (`govulncheck`
> clean). The other three channels build with a pinned, patched toolchain.

See the [Releases](https://github.com/akira-toriyama/prq/releases) page for
published versions. Auth comes from the ambient `gh` CLI login (keyring /
`GH_TOKEN`), via [go-gh](https://github.com/cli/go-gh).

## Develop

```sh
go build ./... && go vet ./... && go test ./...   # green == CI green
go build -o prq ./cmd/prq                          # local binary
```
