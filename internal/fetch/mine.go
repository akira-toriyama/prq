package fetch

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/akira-toriyama/prq/internal/gh"
	"github.com/akira-toriyama/prq/internal/synth"
)

// Multi-PR mode (§3.4) is a compact two-phase poll: one search call for the
// caller's open PRs (isRequired cannot be evaluated under search nodes —
// live-verified), then one aliased node(id:) query fetching check detail only
// for the PRs that need it. All-green boards cost a single request.

// chunkSize caps aliases per phase-2 request (§3.4).
const chunkSize = 25

const mineQuery = `
query Mine($q: String!, $limit: Int!) {
  search(type: ISSUE, query: $q, first: $limit) {
    issueCount
    nodes {
      ... on PullRequest {
        id
        number
        repository { nameWithOwner }
        state
        isDraft
        locked
        mergeable
        mergeStateStatus
        reviewDecision
        baseRefName
        isInMergeQueue
        isMergeQueueEnabled
        mergeQueueEntry { position state }
        reviewThreads(first: 100) { totalCount nodes { isResolved } }
        statusCheckRollup { state contexts(first: 1) { totalCount } }
      }
    }
  }
}`

type mineResp struct {
	Search struct {
		IssueCount int      `json:"issueCount"`
		Nodes      []prNode `json:"nodes"`
	} `json:"search"`
}

// MineResult is the multi-PR poll outcome.
type MineResult struct {
	Inputs    []synth.Input
	Total     int // issueCount from search; > len(Inputs) means truncation
	Truncated bool
}

// Mine fetches the authenticated user's open PRs, optionally scoped to repos
// ("owner/name" forms), at most limit of them.
func Mine(ctx context.Context, c gh.Doer, repos []string, limit int) (MineResult, error) {
	var q strings.Builder
	q.WriteString("is:pr is:open author:@me archived:false sort:updated-desc")
	for _, r := range repos {
		q.WriteString(" repo:")
		q.WriteString(r)
	}

	var resp mineResp
	var degraded []string
	err := c.Do(ctx, mineQuery, map[string]interface{}{"q": q.String(), "limit": limit}, &resp)
	if err != nil {
		ok, tokens := gh.Partial(err)
		if !ok {
			return MineResult{}, gh.Classify(err, "search: "+q.String())
		}
		degraded = tokens
	}

	res := MineResult{Total: resp.Search.IssueCount}
	ids := make([]string, 0, len(resp.Search.Nodes))
	for i := range resp.Search.Nodes {
		pr := &resp.Search.Nodes[i]
		if pr.Number == 0 { // non-PR search node (defensive)
			continue
		}
		in := normalize(pr, "")
		// The coarse pass fetches no context nodes; required-ness (and the
		// context list itself) arrives in phase 2 for the PRs that need it.
		in.RequiredKnown = false
		in.Degraded = append(in.Degraded, degraded...)
		res.Inputs = append(res.Inputs, in)
		ids = append(ids, pr.ID)
	}
	res.Truncated = res.Total > len(res.Inputs)

	resolveDetail(ctx, c, res.Inputs, ids)

	sort.SliceStable(res.Inputs, func(i, j int) bool {
		if res.Inputs[i].Repo != res.Inputs[j].Repo {
			return res.Inputs[i].Repo < res.Inputs[j].Repo
		}
		return res.Inputs[i].Number < res.Inputs[j].Number
	})
	return res, nil
}

// needsDetail says whether a PR's verdict depends on per-context data: any
// non-green rollup, or a BLOCKED state whose cause may be a check.
func needsDetail(in synth.Input) bool {
	if !in.RollupPresent {
		return false
	}
	return in.RollupState != "SUCCESS" || in.MergeState == "BLOCKED"
}

// resolveDetail runs phase 2: aliased node(id:) queries (chunks of 25) that
// fetch context nodes with isRequired for the PRs needsDetail selects.
// Failures degrade the affected PRs instead of failing the poll.
func resolveDetail(ctx context.Context, c gh.Doer, inputs []synth.Input, ids []string) {
	var idx []int
	for i := range inputs {
		if needsDetail(inputs[i]) {
			idx = append(idx, i)
		}
	}
	for start := 0; start < len(idx); start += chunkSize {
		chunk := idx[start:min(start+chunkSize, len(idx))]

		var b strings.Builder
		b.WriteString("query MineDetail {\n")
		for k, i := range chunk {
			fmt.Fprintf(&b, `a%d: node(id: %q) { ... on PullRequest {
  statusCheckRollup { contexts(first: 50) {
    totalCount
    pageInfo { hasNextPage }
    nodes {
      __typename
      ... on CheckRun { name status conclusion detailsUrl
        isRequired(pullRequestId: %q)
        checkSuite { workflowRun { databaseId } } }
      ... on StatusContext { context state targetUrl
        isRequired(pullRequestId: %q) }
    }
  } }
} }
`, k, ids[i], ids[i], ids[i])
		}
		b.WriteString("}")

		resp := map[string]*struct {
			StatusCheckRollup *rollup `json:"statusCheckRollup"`
		}{}
		if err := c.Do(ctx, b.String(), nil, &resp); err != nil {
			if ok, _ := gh.Partial(err); !ok {
				for _, i := range chunk {
					inputs[i].Degraded = append(inputs[i].Degraded, "required")
				}
				continue
			}
		}
		for k, i := range chunk {
			alias := resp[fmt.Sprintf("a%d", k)]
			if alias == nil || alias.StatusCheckRollup == nil {
				inputs[i].Degraded = append(inputs[i].Degraded, "required")
				continue
			}
			ru := alias.StatusCheckRollup
			inputs[i].Contexts = appendContexts(nil, ru.Contexts.Nodes)
			inputs[i].RequiredKnown = true
			if ru.Contexts.PageInfo.HasNextPage {
				inputs[i].Degraded = append(inputs[i].Degraded, "checks_truncated")
			}
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
