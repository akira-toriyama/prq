# prq design

Final synthesized design — the implementer follows this verbatim. All API facts below were live-verified against github.com GraphQL on 2026-07-03 (gh 2.80.0) across the three designer verification logs, plus judge re-verification of the two tie-breakers noted in Decisions. Items no designer could verify live are in the final section.

## Decisions

Where proposals A (api-correctness), B (agent-ux), C (robustness) conflicted:

| # | topic | decision | rationale |
|---|---|---|---|
| D1 | go-gh version | pin **v2.12.2** | judge re-verified: v2.13.0 `.mod` requires go 1.25.0; local toolchain is go1.23.3 (A+B verified, C's v2.13.0 pin would not build) |
| D2 | behind-by fetch | in the main query via `pullRequest.baseRef.compare(headRef:"refs/pull/N/head")` (B) | judge re-verified live on fork PR cli/cli#13761: `behindBy:13`, cost 1 — one round trip, fork-proof, no base-name pre-resolve; drops A/C's conditional follow-up and A's `--behind` flag |
| D3 | `state` field | prq-derived verdict enum (B), not raw `mergeStateStatus` passthrough (A) | the synthesized verdict is the product; raw value still available as `merge_state` when it differs |
| D4 | multi-PR output | **NDJSON**, one object per line (B) | per-line `fp` diff with `grep -F`/`cmp`, no reflow, partial consumption; A/C's wrapper object adds bytes for no agent value |
| D5 | terminal-state gate | consult `pr.state` before everything; never retry UNKNOWN on non-OPEN (C) | C live-verified: merged PRs report `mergeable:UNKNOWN` forever; closed PRs report stale `mergeStateStatus:BLOCKED` |
| D6 | UNKNOWN retry | slim GraphQL probes only, no REST trigger; ladder 0.5/1/2/4 s, budget 8 s (C) | A's own test showed the REST trigger did not resolve a stuck PR in 60 s; querying is itself a trigger; drop the extra dependency on REST |
| D7 | fingerprint | 16-hex sha256 of normalized canonical string, **excluding** `headRefOid` (B, backed by A) | a push that yields an identical verdict (still all-pending) should not wake a babysit loop — the action is unchanged; run-id changes are included and do wake it |
| D8 | blocker grammar | `topic: detail [ -> action]` — ` -> ` only when a pasteable command/URL exists (merge of B+C) | B's mandatory `->` forces invented non-actions ("await review"); C's optional action keeps every emitted `->` executable |
| D9 | HAS_HOOKS | state CLEAN, exit 0, non_blocking note (B+C majority) | GitHub semantics: mergeable now, hooks run at merge time; A's exit 2 would spin loops on GHES-style repos forever |
| D10 | threads blocking | blocker only when BPR visible ∧ `requiresConversationResolution` ∧ unresolved>0; otherwise count-only, mentioned inside the residual-BLOCKED blocker (B+C, A's residual mention) | the setting is unreadable for non-admins; never fabricate a blocker, but surface the count where it is the likeliest cause |
| D11 | `--mine` phase 2 | `node(id:)` aliases with `isRequired(pullRequestId:)`, only for PRs needing check detail (C) | C live-verified the node(id) pattern; all-green boards cost 1 request; server-issued IDs avoid any injection concern |
| D12 | hard-failure exits | 3 = auth/permission (incl. masked NOT_FOUND, degraded-green); 4 = transient/execution; 64 = usage (C's split + B's degraded-green rule) | retrying later is meaningful for 4, pointless for 3; C verified NOT_FOUND masks private-vs-missing |
| D13 | `ACTION_REQUIRED` | its own blocker text (`needs manual approval`), not lumped into FAILING (C) | cifail can't fix a workflow-approval gate; wrong handoff wastes an agent turn |
| D14 | `EXPECTED` StatusContext | pending[] (wait), not blocker (A+B majority) | it can still start; the never-reported case is covered by the BPR `MISSING` enrichment and the residual blocker |
| D15 | pagination caps | contexts 4×100=400; threads 5×100=500 | majority values; aggregates (`checkRunCountsByState`, `totalCount`) keep truncated output honest |
| D16 | cobra | no — stdlib `flag` | unanimous |
| D17 | check-count gauge | keep B's `checks` object (single-PR only) | ~50 B buys visible 3/4→4/4 progress without names; sourced free from aggregates |
| D18 | `ci` field | raw rollup state verbatim, `"NONE"` when rollup null (A) | never invent synonyms for enums agents already know; C's PASSING/FAILING remap adds a translation layer |
| D19 | `STALE` conclusion | blocker `check: '<name>' STALE -> re-run after push` (A) | B's table missed it entirely; C's lump into FAILING sends cifail at a non-failure |

---

## 1. Output JSON contract

### 1.1 Single-PR mode

One JSON object, one line, `\n`-terminated, UTF-8, no ANSI, stdout only. Field order is normative (Go struct order, `json.Encoder` with `SetEscapeHTML(false)`). Two runs against identical facts are byte-identical.

```json
{"pr":"cli/cli#13781","state":"BLOCKED","fp":"9f3a1c2b7e4d5a60","blockers":["draft: marked as draft -> gh pr ready 13781 -R cli/cli","review: REVIEW_REQUIRED (codeowners)","check: 'lint' FAILING -> cifail --run 987654 -R cli/cli","behind: 3 commits behind trunk -> gh pr update-branch 13781 -R cli/cli"],"pending":["check: 'build' RUNNING"],"non_blocking":["check: 'benchmark' FAILING (optional) -> cifail --run 987700 -R cli/cli"],"checks":{"req_ok":2,"req_fail":1,"req_run":1,"opt_fail":1},"unresolved_threads":2,"behind_by":3,"ci":"FAILURE","review":"REVIEW_REQUIRED","mergeable":"MERGEABLE","merge_state":"BLOCKED","queue":{"position":3,"state":"AWAITING_CHECKS","eta_s":840},"degraded":["threads"],"final":true,"fp_note":"none of these last two appear together; sample shows shape only"}
```

(`fp_note` is illustrative commentary, not a real field.)

| # | field | type | presence | semantics |
|---|-------|------|----------|-----------|
| 1 | `pr` | string `owner/repo#N` | always | canonical key; URL is derivable — no `url` field |
| 2 | `state` | enum | always | prq verdict: `CLEAN` \| `UNSTABLE` \| `PENDING` \| `BLOCKED` \| `QUEUED` \| `MERGED` \| `CLOSED` \| `UNKNOWN` (§2.1) |
| 3 | `fp` | 16 lowercase hex | always | fingerprint (§1.4) |
| 4 | `blockers` | []string | **always, even `[]`** | needs action; grammar §1.3; cap 8, then `"+N more"` |
| 5 | `pending` | []string | omit if empty | blocks merge but resolves on its own (required checks running, mergeable computing); cap 8 + `"+N more"` |
| 6 | `non_blocking` | []string | omit if empty | advisory, excluded from fp: optional failing checks (with cifail handoff), `"<k> optional checks pending"` aggregate, hooks note, truncation notices; cap 5 |
| 7 | `checks` | object | omit if no checks | ints `req_ok,req_fail,req_run,opt_fail,opt_run`, each omitted when 0; sourced from per-context walk + `checkRunCountsByState` aggregates |
| 8 | `unresolved_threads` | int | omit when 0 **and** not degraded | client-side count of `isResolved==false` |
| 9 | `behind_by` | int | omit when 0 or unobtainable | from `baseRef.compare` (D2) |
| 10 | `ci` | string | omit when no rollup and state not BLOCKED | raw `statusCheckRollup.state` verbatim; literal `"NONE"` when rollup is null (verified null happens: cli/cli#13781) |
| 11 | `review` | enum | omit when null | raw `reviewDecision`: `APPROVED\|CHANGES_REQUESTED\|REVIEW_REQUIRED`; null = no review requirement (verified null happens) |
| 12 | `mergeable` | enum | **omit when `MERGEABLE`** | `CONFLICTING` \| `UNKNOWN` only; absence == MERGEABLE |
| 13 | `merge_state` | enum | omit when equal to `state` | raw `mergeStateStatus`, for transparency when the verdict diverges (e.g. state `CLEAN`, merge_state `HAS_HOOKS`) |
| 14 | `queue` | object | omit unless relevant | `{"position":N,"state":"<MergeQueueEntryState>","eta_s":N}` (`eta_s` omitted when API null); special form `{"required":true}` when ready-to-enqueue (§2.3 BLOCKED row) |
| 15 | `degraded` | []string | omit if empty | unavailable signals: `"threads"`, `"required"`, `"merge_state"`, `"checks_truncated"`, `"threads_truncated"`, `"behind"`, `"ghes"` |
| 16 | `final` | bool | omit unless true | only on MERGED/CLOSED: stop polling regardless of exit code |

Omission contract: **absence of an omit-when-empty field means its zero value, unless the corresponding signal is listed in `degraded`.**

Never emitted: `url`, `title`, timestamps, actor names (bytes + determinism).

### 1.2 Multi-PR mode (`--mine`) — NDJSON

One object per line, sorted by `pr` (repo asc, number asc). Same schema minus `non_blocking`, `checks`, `queue.eta_s` (act on a specific PR via a single-PR call). Per-PR failures are error lines; they do not fail the invocation:

```
{"pr":"a/x#12","state":"PENDING","fp":"1a2b3c4d5e6f7a8b","blockers":[],"pending":["check: 'build' RUNNING"]}
{"pr":"a/y#7","state":"BLOCKED","fp":"c0ffee0012345678","blockers":["review: CHANGES_REQUESTED"],"unresolved_threads":3}
{"pr":"a/z#3","error":{"code":"not_found_or_forbidden"}}
```

`blockers`/`pending` capped at 3 entries + `"+N more"` per line. No summary line — the exit code carries the rollup. If discovery finds more PRs than `--limit`, the final line is `{"truncated":true,"total":N}`.

### 1.3 blockers[] string grammar

```
entry  := topic ": " detail [ " -> " action ]
topic  := "draft" | "review" | "check" | "checks" | "conflict" | "behind"
        | "threads" | "queue" | "blocked" | "closed"
action := pasteable command or URL only, ≤ 120 chars
```

Rules: at most one ` -> ` (ASCII, the safe split token); check names in single quotes, emitted verbatim (only free text — agents must not parse inside quotes); `-R owner/repo` and PR number always embedded in pasteable actions. Entries sorted by fixed topic precedence (`closed < draft < review < check/checks < conflict < behind < threads < queue < blocked`) then bytewise by check name — deterministic.

Complete vocabulary (synthesis emits nothing else):

```
closed: PR closed without merge
draft: marked as draft -> gh pr ready <N> -R <o/r>
review: CHANGES_REQUESTED
review: REVIEW_REQUIRED
review: REVIEW_REQUIRED (codeowners)
check: '<name>' FAILING -> cifail --run <runid> -R <o/r>
check: '<name>' FAILING -> <detailsUrl|targetUrl>          (workflowRun null / StatusContext)
check: '<name>' FAILING                                    (no run id, no url)
check: '<name>' TIMED_OUT|CANCELLED|STARTUP_FAILURE -> …   (conclusion spelled out when ≠ FAILURE)
check: '<name>' ACTION_REQUIRED (needs manual approval)
check: '<name>' STALE -> re-run after push
check: '<name>' MISSING (required, never reported)         (BPR enrichment only)
conflict: merge conflicts with <base>
behind: <N> commits behind <base> -> gh pr update-branch <N> -R <o/r>   (action only if viewerCanUpdateBranch)
behind: out of date with <base>                             (compare failed; degraded note added)
threads: <N> unresolved -> resolve via gh pr-review
queue: entry UNMERGEABLE (will be ejected)
blocked: cause not visible to this token (hidden protection rule, required deployment, or push restriction; <K> unresolved threads)
```

pending[] vocabulary: `check: '<name>' RUNNING`, `check: '<name>' PENDING`, `check: '<name>' EXPECTED`, `checks: <N> required running` (collapse when >3), `queue: position <N> <STATE> (~<X>m)`, `mergeable: still computing (retry budget exhausted)`.

non_blocking[] vocabulary: `check: '<name>' FAILING (optional) -> …`, `<k> optional checks pending`, `hooks: pre-receive hooks run on merge`, `queue: ready to enqueue (merge queue enabled)`, `+<n> more checks not shown`.

### 1.4 Fingerprint (`fp`)

`fp = hex(sha256(canonical))[0:16]` where

```
canonical = "v1|" + pr
          + "|" + state
          + "|" + join(sort(norm(blockers)), ";")
          + "|" + join(sort(pendingCheckNames), ";")
          + "|r=" + (review // "-")
          + "|m=" + (mergeable // "MERGEABLE")
          + "|t=" + itoa(unresolvedThreads)
          + "|d=" + join(sort(degraded), ",")
```

Normalization (`norm`, hashing only, not display): `behind: <N> commits…` → `behind`; pending entries reduce to check name only; the `queue` object is excluded entirely (position/ETA churn is noise; queue entry/exit changes `state`, which is hashed; UNMERGEABLE surfaces as a hashed blocker). Blockers otherwise hashed **verbatim, including cifail run ids** — a re-run of a failing check yields a new fp and wakes the loop to inspect the new failure instance.

Invariant under: field order, pagination order (checks sorted before synthesis), base-branch advancement, queue position/ETA drift, optional-check churn, pushes that don't change the verdict (`headRefOid` excluded — D7), timestamps (never fetched). Changes on: blocker-set change, required-check re-run, state transition, review decision, unresolved-thread count, degradation set. `v1|` prefix: any grammar change bumps to `v2|` and wakes every loop exactly once. Contract: compare for equality only, never decode.

### 1.5 Byte budget (measured by designers; enforced by caps + tests)

Single clean ≈ 90 B; typical blocked (4 blockers + scalars) ≈ 470 B; hard ceiling ≈ 1.1 KB (caps: blockers ≤8, pending ≤8, non_blocking ≤5, strings ≤120 chars). `--mine` line 130–210 B. Golden test asserts busy-PR fixture ≤ 1024 B and multi-line ≤ 256 B.

---

## 2. Blocker synthesis rules

Pure function `synth.Synthesize(Facts) Verdict`. Evaluation: **terminal gate → per-fact decomposition → mergeStateStatus fallback**.

### 2.1 Terminal gate + state derivation (first match wins)

1. `pr.state==MERGED` → `MERGED`, `blockers:[]`, `final:true`, exit 0. No retry, no decomposition (verified: merged PRs report `mergeable:UNKNOWN` forever — cli/cli#13786).
2. `pr.state==CLOSED` → `CLOSED`, blocker `closed: PR closed without merge`, `final:true`, exit 1. Stale `mergeStateStatus` ignored (verified: cli/cli#13782 reports stale BLOCKED).
3. `mergeQueueEntry.state==UNMERGEABLE` → `BLOCKED` + queue blocker.
4. `mergeQueueEntry != null` otherwise → `QUEUED`, queue pending entry + `queue{}` object; the `behind` blocker is suppressed (the queue rebases).
5. blockers[] non-empty (per §2.2) → `BLOCKED`.
6. pending[] non-empty → `PENDING`.
7. `mergeable==UNKNOWN` after retry budget → `UNKNOWN`.
8. `mergeStateStatus==UNSTABLE` → `UNSTABLE`.
9. else → `CLEAN`.

### 2.2 Decision table (OPEN PRs; rows additive; dedup rules at end)

| input | condition | output |
|---|---|---|
| `isDraft` | true | blocker `draft: … -> gh pr ready` |
| `reviewDecision` | `CHANGES_REQUESTED` | blocker `review: CHANGES_REQUESTED` |
| | `REVIEW_REQUIRED` | blocker `review: REVIEW_REQUIRED`; suffix ` (codeowners)` iff any `reviewRequests.nodes[].asCodeOwner==true` |
| | `APPROVED` / null | nothing (field carries it) |
| required CheckRun | conclusion `SUCCESS`/`NEUTRAL`/`SKIPPED` | nothing (counted in `checks.req_ok`) |
| | conclusion `FAILURE`/`TIMED_OUT`/`CANCELLED`/`STARTUP_FAILURE` | blocker `check: '<n>' FAILING -> cifail --run <checkSuite.workflowRun.databaseId> -R o/r`; conclusion spelled out when ≠ FAILURE; `workflowRun` null (verified on app checks, e.g. CodeQL) → `-> <detailsUrl>`; no url → bare |
| | conclusion `ACTION_REQUIRED` | blocker `check: '<n>' ACTION_REQUIRED (needs manual approval)` |
| | conclusion `STALE` | blocker `check: '<n>' STALE -> re-run after push` |
| | status ∈ `QUEUED,IN_PROGRESS,WAITING,PENDING,REQUESTED` | pending `check: '<n>' RUNNING` (collapse >3 → `checks: N required running`) |
| required StatusContext | state `FAILURE`/`ERROR` | blocker `check: '<ctx>' FAILING -> <targetUrl>` (no run id exists for commit statuses) |
| | state `PENDING` | pending `check: '<ctx>' PENDING` |
| | state `EXPECTED` | pending `check: '<ctx>' EXPECTED` (D14) |
| optional context failing | any failure state | non_blocking `check: '<n>' FAILING (optional) -> …` (cap 5, then aggregate) |
| optional contexts pending | any | non_blocking `<k> optional checks pending` (one aggregate entry) |
| `statusCheckRollup==null` | | `ci:"NONE"`; no check rows (verified: cli/cli#13781) |
| BPR visible: names in `requiredStatusCheckContexts` absent from rollup | | blocker `check: '<n>' MISSING (required, never reported)` — the fork-PR never-started case (observed shape: cli/cli#13787/#13665: BLOCKED, all-green or zero required contexts) |
| `mergeable==CONFLICTING` | | blocker `conflict: merge conflicts with <base>` |
| `mergeStateStatus==BEHIND` | compare succeeded | blocker `behind: <N> commits behind <base>` (+ update-branch action iff `viewerCanUpdateBranch`) |
| | compare failed | blocker `behind: out of date with <base>` + `degraded:["behind"]` |
| `behind_by>0`, mss ≠ BEHIND | | field only, no blocker |
| unresolved>0 | BPR visible ∧ `requiresConversationResolution` | blocker `threads: <N> unresolved -> resolve via gh pr-review` |
| | otherwise | count-only in `unresolved_threads` |
| `locked==true` (OPEN) | | nothing — locking does not block merge |

Dedup: (a) DIRTY/CONFLICTING emit one conflict blocker; (b) queue membership suppresses `behind`; (c) `MISSING` never duplicates a name already reported as EXPECTED/pending.

### 2.3 `mergeStateStatus` — every enum value (live enum: `DIRTY, UNKNOWN, BLOCKED, BEHIND, UNSTABLE, HAS_HOOKS, CLEAN`; no `DRAFT` in live schema — draft comes from `isDraft`)

| value | handling | exit |
|---|---|---|
| `CLEAN` | no blocker; state CLEAN | 0 |
| `HAS_HOOKS` | state CLEAN; non_blocking `hooks: pre-receive hooks run on merge`; `merge_state` field surfaces it (D9) | 0 |
| `UNSTABLE` | state UNSTABLE; optional failing/pending → non_blocking; mergeable right now | 0 |
| `BEHIND` | `behind` blocker → BLOCKED | 1 |
| `DIRTY` | conflict blocker (deduped with `mergeable==CONFLICTING`) → BLOCKED | 1 |
| `BLOCKED` | **no direct blocker.** Full §2.2 decomposition. If it yields nothing and pending[] is empty: (a) if `isMergeQueueEnabled` ∧ rollup ok ∧ review ok → state CLEAN + `queue:{"required":true}` + non_blocking `queue: ready to enqueue …` (next verb is enqueue, not merge — UNVERIFIED inference, see final section); (b) else residual blocker `blocked: cause not visible to this token (…; <K> unresolved threads)` — the honest answer to cli/cli#10775, verified real on cli/cli#13665 (APPROVED + all-green + still BLOCKED) | 1/2/0 |
| `UNKNOWN` | OPEN → retry loop §3.3; non-OPEN → ignored. After budget: component blockers win if present (BLOCKED/1); else state UNKNOWN + pending `mergeable: still computing …` | 1/2 |

### 2.4 Degraded handling

go-gh populates the response struct **before** returning `*GraphQLError` (source-verified in v2.12.2), so prq always synthesizes from whatever arrived:

- `reviewThreads` errored → omit `unresolved_threads`, `degraded:["threads"]`, threads blocker disabled.
- `isRequired` errored on all contexts → `degraded:["required"]`; **conservative fallback**: every failing check becomes a blocker (detail suffixed `(required?)`), every pending check → pending[]. Over-block rather than false-CLEAN.
- `mergeStateStatus`/`mergeable` null → `degraded:["merge_state"]`, synthesize from components only.
- Pagination caps hit → `degraded:["checks_truncated"|"threads_truncated"]` + non_blocking notice; `ci`/`checks` stay exact via aggregates and `totalCount`; `unresolved_threads` becomes a lower bound.
- BPR null (the normal non-admin case — verified) is **not** degradation; enrichment rows simply don't fire.

Degradation changes the exit code only when it undermines a green/wait verdict (§4.3).

---

## 3. GraphQL design

### 3.1 Single-PR query — 1 HTTP request, cost 1 (verified)

`$headRef` = `"refs/pull/<N>/head"`, constructed from the PR number (judge re-verified: fork PR compare works, cost 1).

```graphql
query PRState($owner:String!,$name:String!,$number:Int!,$headRef:String!) {
  repository(owner:$owner,name:$name) {
    pullRequest(number:$number) {
      id number state isDraft locked
      mergeable mergeStateStatus reviewDecision
      isCrossRepository baseRefName headRefOid
      isInMergeQueue isMergeQueueEnabled viewerCanUpdateBranch
      mergeQueueEntry { position state estimatedTimeToMerge }
      reviewRequests(first:20) { nodes { asCodeOwner } }
      reviewThreads(first:100) {
        totalCount pageInfo { hasNextPage endCursor } nodes { isResolved }
      }
      baseRef {
        compare(headRef:$headRef) { aheadBy behindBy }
        branchProtectionRule {
          requiresStrictStatusChecks requiresConversationResolution
          requiresCodeOwnerReviews requiredStatusCheckContexts
        }
      }
      statusCheckRollup {
        state
        contexts(first:100) {
          totalCount
          checkRunCountsByState { state count }
          statusContextCountsByState { state count }
          pageInfo { hasNextPage endCursor }
          nodes {
            __typename
            ... on CheckRun {
              name status conclusion detailsUrl
              isRequired(pullRequestNumber:$number)
              checkSuite { workflowRun { databaseId } }
            }
            ... on StatusContext {
              context state targetUrl
              isRequired(pullRequestNumber:$number)
            }
          }
        }
      }
    }
  }
}
```

Verified load-bearing facts: `PullRequest.statusCheckRollup` exists directly (no `commits(last:1)` hop); `isRequired` **requires** `pullRequestId` or `pullRequestNumber` even PR-rooted (error: "A pull request ID or pull request number is required", node nulled, partial data delivered); `mergeStateStatus` needs no preview header; `branchProtectionRule` returns silently-null for non-admins; `contexts(first:101)` → `EXCESSIVE_PAGINATION` **and nulls the rollup subtree** — never over-ask.

### 3.2 Pagination

- **contexts**: page size 100 (hard max); follow `endCursor` up to 3 more pages (400 total), contexts-only follow-up query with the same `isRequired` arg. Past cap: `degraded:["checks_truncated"]`, non_blocking `+N more checks not shown`; per-state aggregates keep `ci`/`checks` exact.
- **reviewThreads**: 100/page, up to 5 pages (500). Past cap: `degraded:["threads_truncated"]`, `unresolved_threads` = lower bound, `totalCount` exact.
- `--mine` never paginates; per-line truncation flags.

### 3.3 UNKNOWN-mergeable retry policy

- Applies **only** when `pr.state==OPEN && mergeable==UNKNOWN` (D5/D6). Verified: fresh PRs pair `mergeable:UNKNOWN` with `mergeStateStatus:UNKNOWN` — never trust the rest of an UNKNOWN response's merge state.
- **5 attempts total** (1 full + up to 4 slim). Slim probe fetches only `pullRequest { state mergeable mergeStateStatus }` (cost 1, ~120 B). Sleeps before probes: **0.5 s, 1 s, 2 s, 4 s** (±10% jitter). Worst-case added wall time 7.5 s.
- `--retry-budget <dur>` (default `8s`; `0` disables, alias `--no-retry`): stop when the next sleep would exceed the budget.
- On resolution: re-run the full query once and synthesize from the fresh snapshot.
- Exhaustion is not an error: `mergeable:"UNKNOWN"` in output, pending entry, state UNKNOWN, exit 2 — unless other blockers exist (blockers win, exit 1). fp includes `m=UNKNOWN`, so the next poll detects resolution. Verified: UNKNOWN can persist >60 s on real PRs; the budget bounds the spin.
- `--mine`: no per-PR loops — one slim batch re-query for all UNKNOWN OPEN PRs after a single 2 s wait, then accept.

### 3.4 `--mine` — exactly 2 HTTP requests (3+ only when chunking)

`isRequired` cannot be evaluated inside `search()` nodes and its argument cannot vary per node in a static selection (verified) — hence two phases:

**Request 1 — discovery + cheap facts** (`$q = "is:pr is:open author:@me archived:false repo:a/x repo:a/y"`; multiple `repo:` qualifiers OR — verified):

```graphql
query Mine($q:String!,$limit:Int!) {
  search(type:ISSUE, query:$q, first:$limit) {
    issueCount
    nodes { ... on PullRequest {
      id number repository { nameWithOwner }
      state isDraft mergeable mergeStateStatus reviewDecision
      isInMergeQueue mergeQueueEntry { position state }
      reviewThreads(first:100) { totalCount nodes { isResolved } }
      statusCheckRollup { state contexts(first:1) {
        checkRunCountsByState { state count }
        statusContextCountsByState { state count } } }
    } }
  }
}
```

**Request 2 — check detail**, only for PRs where rollup ≠ SUCCESS or merge_state == BLOCKED (all-green boards skip it): dynamically built aliased document, **25 aliases per chunk**, using server-issued node IDs (verified pattern):

```graphql
query {
  p0: node(id:"PR_kwDO…") { ... on PullRequest {
    statusCheckRollup { contexts(first:50) { nodes {
      __typename
      ... on CheckRun { name status conclusion detailsUrl
        isRequired(pullRequestId:"PR_kwDO…")
        checkSuite { workflowRun { databaseId } } }
      ... on StatusContext { context state targetUrl
        isRequired(pullRequestId:"PR_kwDO…") }
    } } } } }
  p1: …
}
```

IDs are JSON-string-escaped defensively despite being server-issued. `issueCount > limit` → trailing `{"truncated":true,"total":N}` line.

---

## 4. CLI surface

### 4.1 Invocation

```
prq                        # current branch → PR (git remote + headRefName lookup)
prq 123                    # number; repo from cwd / GH_REPO
prq 123 -R owner/repo
prq owner/repo#123
prq https://github.com/owner/repo/pull/123
prq my-branch -R o/r
prq --mine [--repos a/x,a/y] [--limit 30]
```

Branch resolution: `pullRequests(headRefName:…, states:OPEN, first:10)`; prefer same-head-repo match, then `@me`-authored, else newest (fork head-name collisions are real — verified). No PR found → stderr `no_pr_found`, exit 4.

### 4.2 Flags (complete set)

| flag | default | purpose |
|---|---|---|
| `-R, --repo o/r` | cwd remote | repo context |
| `--mine` | off | NDJSON multi-PR mode |
| `--repos a/x,a/y` | current repo | scope for `--mine` (implies it); omitted with `--mine` → all `author:@me` |
| `--limit N` | 30 (max 100) | `--mine` discovery cap |
| `--retry-budget dur` | `8s` | UNKNOWN retry wall budget; `0` disables |
| `--no-retry` | off | alias for `--retry-budget 0` |
| `--timeout dur` | `30s` | whole-invocation deadline |
| `--version` | | print version, exit 0 |

No `--json` (JSON is the output), no color/TTY handling, no `--watch` (loops live outside; fp exists for them), no subcommands.

### 4.3 Exit codes

| code | meaning | exact condition |
|---|---|---|
| **0** | nothing prevents merge | state ∈ {CLEAN, UNSTABLE, MERGED} and rule-3 below doesn't fire. MERGED = 0 + `final:true` |
| **1** | blocked; an actor must act | state ∈ {BLOCKED, CLOSED} (incl. queue UNMERGEABLE). **Degraded inputs never downgrade a 1** — visible blockers are trustworthy |
| **2** | time will change it; poll again | state ∈ {PENDING, QUEUED, UNKNOWN} with `degraded` empty |
| **3** | verdict not certifiable / auth | (a) `degraded` non-empty **and** computed state ∈ {CLEAN, UNSTABLE, PENDING, QUEUED, UNKNOWN} — a loop must never treat an unverifiable green as mergeable; (b) hard permission failures: HTTP 401, `FORBIDDEN` on the pullRequest path, SAML enforcement, repo/PR `NOT_FOUND` (masks private-vs-missing — verified) |
| **4** | no answer; retry later is meaningful | rate-limit exhausted after one honored `Retry-After` wait (cap 30 s), 5xx, network failure, `--timeout` exceeded, `no_pr_found`, unparsable response |
| **64** | usage | bad flags/selector; stdout empty |

`--mine` rollup: 4 if the whole invocation failed; else 3 if any line would exit 3; else 1 if any 1; else 2 if any 2; else 0. (Certifiability > blockedness > pendingness.) Per-PR failures become error lines, not invocation failures, unless all targets failed.

Agent next-action map: 0 → merge (or enqueue if `queue.required`) / stop; 1 → execute blockers in order (each carries its own command where one exists); 2 → sleep, re-poll, diff `fp`; 3 → fix auth / escalate; 4 → retry later or read stderr; 64 → fix invocation.

### 4.4 stderr contract

stdout: valid JSON or empty, always. On exits 4/64 and hard-3 (case b): stdout empty, exactly one JSON line on stderr:

```json
{"error":{"code":"rate_limited","message":"secondary rate limit","retry_after_s":30,"hint":"retry after the delay; consider --retry-budget 0"}}
```

`code` ∈ `usage | no_pr_found | not_found_or_forbidden | auth | saml_blocked | rate_limited | server_error | network | timeout | schema`. `hint` = next command to try, when one exists. On soft-3 (degraded verdict): stdout carries the verdict, stderr stays empty — degradation is data, not an error. `GH_DEBUG` passthrough excepted, stderr is otherwise silent.

GHES: out of scope for v1. Host ≠ github.com → proceed best-effort with `degraded:["ghes"]`; schema validation errors on missing fields degrade per §2.4 rather than hard-fail where partial data arrived.

---

## 5. Go architecture

**Dependencies: stdlib + `github.com/cli/go-gh/v2` pinned `v2.12.2`** (D1). **No cobra** (D16): one command, ≤8 flags; stdlib `flag`. When the toolchain moves to ≥1.25, bump go-gh to v2.13+ (used API surface unchanged).

```
cmd/prq/main.go          # thin: os.Exit(run.Run(ctx, cfg, os.Stdout, os.Stderr, client, clock))
internal/cli/            # flag parsing (stdlib flag), selector parsing (number/URL/o#N/branch), config
internal/ghx/            # type Doer interface { Do(ctx, query string, vars map[string]any, out any) error }
                         # prod impl wraps go-gh api.GraphQLClient.DoWithContext; classifies
                         # *api.HTTPError / *api.GraphQLError (with .Match) → typed prq errors
internal/query/          # query constants, typed response structs, pagination walker,
                         # --mine two-phase + 25-alias chunk builder, UNKNOWN slim probe
internal/synth/          # PURE: Synthesize(Facts) Verdict — the §2 tables; zero I/O
internal/output/         # ordered structs → deterministic JSON (SetEscapeHTML(false)),
                         # fp canonicalization + sha256, NDJSON encoder
internal/run/            # orchestration: fetch → retry loop (injected clock) → synth → print;
                         # returns exit code as int
```

Test seams (both used):
1. `synth.Synthesize` takes plain structs — the whole decision table tests with zero HTTP.
2. End-to-end: real go-gh client via `api.NewGraphQLClient(api.ClientOptions{AuthToken:"test", Host:…, Transport: fake})` (Transport is the documented test hook; go-gh delivers partial data before typed errors — source-verified). The fake/httptest server is a **script**: ordered steps `{match: operation-name or query substring, status, bodyFile, assertVars}`; unconsumed or over-consumed steps fail the test. Fixtures are verbatim response bodies captured from the live probes (checkless #13781, blocked-all-green #13665/#13787, merged #13786, closed #13782, NOT_FOUND shape, EXCESSIVE_PAGINATION shape).
3. Sleeps via injected clock; `run.Run` returns the exit code — no subprocesses. Golden files under `testdata/golden/` compared byte-exact (`-update` to regenerate).

### 5.1 Test plan (ordered; all headless; `go build ./... && go vet ./... && go test ./...` green)

| # | case | fixture/source | asserts |
|---|---|---|---|
| 1 | clean PR | synthetic | golden stdout, `blockers:[]`, exit 0, ≤120 B |
| 2 | draft + failing required + behind + threads | synthetic busy | 4 blockers in precedence order, exact cifail string, len ≤ 1024 |
| 3 | checkless (`statusCheckRollup:null`) | live #13781 capture | `ci:"NONE"`, draft blocker only |
| 4 | BLOCKED with all-green / zero required contexts | live #13665/#13787 captures | residual `blocked:` blocker; with BPR fixture → `MISSING` blockers instead |
| 5 | app check with null `workflowRun` | mutated #13665 | detailsUrl fallback form |
| 6 | merged PR | live #13786 capture | MERGED, `final:true`, exit 0, **zero retry requests** despite UNKNOWN |
| 7 | closed PR w/ stale BLOCKED | live #13782 capture | CLOSED, exit 1, stale merge_state ignored |
| 8 | every MergeStateStatus × exit code | table on `synth.Synthesize` | §2.3 rows |
| 9 | every CheckConclusionState / CheckStatusState / StatusState | table | §2.2 mapping incl. STALE, ACTION_REQUIRED, EXPECTED |
| 10 | UNKNOWN resolves on attempt 3 | scripted responses | exactly 3 HTTP calls, fake-clock sleeps 0.5 s + 1 s, verdict from fresh full query |
| 11 | UNKNOWN exhausted | scripted 5× UNKNOWN | 5 calls, pending entry, state UNKNOWN, exit 2; with a coexisting blocker → exit 1 |
| 12 | queue entry × 5 states | synthetic | QUEUED/AWAITING_CHECKS/MERGEABLE/LOCKED → exit 2; UNMERGEABLE → blocker, exit 1; `behind` suppressed while queued |
| 13 | ready-to-enqueue inference | synthetic (BLOCKED, all green, queue enabled) | state CLEAN, `queue:{"required":true}`, exit 0 |
| 14 | threads: gated blocker vs count-only | BPR-visible vs BPR-null fixtures | blocker fires only with `requiresConversationResolution` |
| 15 | contexts 150 across 2 pages | scripted pages | merged verdict; page-2 request carries cursor |
| 16 | contexts > 400 / threads > 500 | scripted | truncation degraded notes; `ci` from aggregates; lower-bound thread count |
| 17 | degraded threads (partial data + FORBIDDEN item) | fixture | field omitted, `degraded:["threads"]`, exit 3 when otherwise clean, exit 1 when blocked |
| 18 | degraded isRequired | UNPROCESSABLE items | conservative blockers with `(required?)`, `degraded:["required"]` |
| 19 | fp invariants | property-style pairs | check-order permutation ⇒ equal; behind 3→7 ⇒ equal; optional check added ⇒ equal; push w/ identical verdict ⇒ equal; run-id change / thread resolved / approval ⇒ differ; `v1|` prefix present |
| 20 | `--mine`: 3 PRs / 2 repos / 1 needs detail | scripted | exactly 2 HTTP requests; NDJSON sorted; per-line fp; rollup exit; ≤256 B/line; 25-alias chunking at 30 PRs |
| 21 | `--mine` partial failure + truncation | scripted | error line + `truncated` line, outcome exit preserved |
| 22 | rate limit 403 → Retry-After → 200; then exhausted variant | scripted, fake clock | one wait then success; exhausted → error object, exit 4 |
| 23 | 401 / SAML / NOT_FOUND | fixtures | exit 3, codes `auth`/`saml_blocked`/`not_found_or_forbidden` |
| 24 | branch resolution fork collision | fixture | prefers same-head-repo, then @me |
| 25 | stdout purity + determinism | all error cases; any fixture ×2 | stdout empty on failure, stderr object parses; byte-identical reruns |
| 26 | usage errors | arg table | exit 64 |

---

## Unverified / implementation-time checks

1. **Live merge-queue entry payload** — `mergeQueueEntry{position,state,estimatedTimeToMerge}` is introspection-verified (`position`/`state` NON_NULL; description says `estimatedTimeToMerge` is **seconds**), but no populated public queue was caught; runtime nullness of `estimatedTimeToMerge` unproven. *Check:* keep a `//go:build live` smoke test targeting a queued PR (github/docs, bevyengine/bevy drain fast — poll during implementation); parse defensively (`eta_s` omitted on null).
2. **`NEUTRAL`/`SKIPPED` satisfying required checks** — docs-based. *Check:* create a required-but-skipped check in a scratch repo during implementation; if wrong, the residual-BLOCKED path still fires (fail-safe).
3. **Ready-to-enqueue inference** (BLOCKED + all green + `isMergeQueueEnabled` ⇒ CLEAN + `queue.required`) — matches observed behavior, but BLOCKED semantics may drift (that opacity is cli/cli#10775). *Check:* validate on a real queue-enabled repo before release; the `blocked:` residual fallback guarantees no unexplained empty-blocker BLOCKED either way.
4. **`HAS_HOOKS` in the wild** — mapping to CLEAN is by enum description (GHES pre-receive). *Check:* none possible on github.com; revisit if a GHES user reports.
5. **Fine-grained PAT / SAML error shapes** — classifier is built on go-gh's typed `HTTPError`/`GraphQLError.Match` with path-based degradation regardless of error `type`. *Check:* run once with a fine-grained token during implementation; the classifier is one function with fixtures, cheap to correct.
6. **`isRequired` under repository rulesets** (vs classic branch protection) and the positive fork case (fork PR with *reported* required checks) — expected to work (evaluated against base-repo rules). *Check:* one live probe on a ruleset-protected repo during implementation.
7. **`refs/pull/N/head` compare on just-closed PRs / GC'd refs** — verified on open PRs incl. forks. Fallback already specified: compare failure → `behind: out of date` form + `degraded:["behind"]`.
8. **reviewThreads page-size max = 100** — assumed from the contexts `EXCESSIVE_PAGINATION` proof; not separately probed. *Check:* one `first:101` probe; if higher, nothing changes (100 stays the page size).
9. **`--mine` at scale** — 25-alias chunks with 100-check PRs not stress-tested against node budgets; discovery beyond `first:100` unpaginated by design (`truncated` line). *Check:* one synthetic 25×100 request during implementation; shrink chunk size if the node-limit error appears.

---

## v1 implementation deviations (deliberate, small)

1. **No jitter on the retry ladder** — a singleton CLI has no thundering-herd
   problem; determinism keeps tests byte-exact. Ladder/budget otherwise as §3.3.
2. **`--mine` skips the one-shot UNKNOWN batch re-probe** — UNKNOWN lines carry
   `m=UNKNOWN` in their fp and exit 2; the next poll self-heals. (§3.4 note)
3. **No automatic `Retry-After` wait on rate limits** — classified as
   `rate_limited`/exit 4 with `retry_after_s` surfaced when the header is
   present; the caller owns the sleep.
4. **`unresolved_threads` is omitted when 0 or unknown** — the `degraded`
   token carries the unknown case (§1.1 row 8 simplification).
5. **`checks` counts come from the walked contexts only** (≤400); past the cap
   `checks_truncated` is the honest signal (aggregate reconciliation deferred),
   and no `+N more checks not shown` non_blocking notice accompanies it — the
   degraded token alone carries the fact.
6. **Degraded vocabulary gains `"checks"`** — a wholly unreadable
   `statusCheckRollup` subtree (distinct from `required`, which means the
   contexts arrived but their required-ness didn't). Pure-enrichment subtrees
   (`compare`, `reviewRequests`) degrade silently: synthesis re-reports the
   behind case itself and the codeowner suffix is best-effort.
7. **Ready-to-enqueue reads "rollup ok" as "no required check failing or
   pending"** (i.e. blockers and pending both empty), not "rollup state ==
   SUCCESS" — optional-check failures must not demote an enqueueable PR to the
   residual blocker.
