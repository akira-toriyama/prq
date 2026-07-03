// Package fetch runs the GraphQL round trips and normalizes GitHub's answer
// into a synth.Input. It owns the query text (docs/design.md §3), the
// mergeable-UNKNOWN retry ladder, pagination, and the degraded bookkeeping.
package fetch

import (
	"context"
	"fmt"
	"time"

	"github.com/akira-toriyama/prq/internal/gh"
	"github.com/akira-toriyama/prq/internal/synth"
)

// SleepFunc lets tests replace the retry backoff.
type SleepFunc func(time.Duration)

// ladder is the §3.3 retry schedule for mergeable == UNKNOWN: after the full
// query, up to 4 slim probes preceded by these sleeps, stopping when the next
// sleep would exceed the caller's budget (default 8s).
var ladder = []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second, 4 * time.Second}

// Pagination caps (§3.2): contexts 4×100, threads 5×100; past the cap the
// output carries a degraded token instead of silently truncating.
const (
	maxContextPages = 4
	maxThreadPages  = 5
)

const prCoreQuery = `
query PRState($owner: String!, $name: String!, $number: Int!, $headRef: String!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      id
      number
      state
      isDraft
      locked
      mergeable
      mergeStateStatus
      reviewDecision
      isCrossRepository
      baseRefName
      headRefOid
      isInMergeQueue
      isMergeQueueEnabled
      viewerCanUpdateBranch
      mergeQueueEntry { position state estimatedTimeToMerge }
      reviewRequests(first: 20) { nodes { asCodeOwner } }
      reviewThreads(first: 100) {
        totalCount
        pageInfo { hasNextPage endCursor }
        nodes { isResolved }
      }
      baseRef {
        compare(headRef: $headRef) { aheadBy behindBy }
        branchProtectionRule {
          requiresStrictStatusChecks
          requiresConversationResolution
          requiredStatusCheckContexts
        }
      }
      statusCheckRollup {
        state
        contexts(first: 100) {
          totalCount
          pageInfo { hasNextPage endCursor }
          nodes {
            __typename
            ... on CheckRun {
              name
              status
              conclusion
              detailsUrl
              isRequired(pullRequestNumber: $number)
              checkSuite { workflowRun { databaseId } }
            }
            ... on StatusContext {
              context
              state
              targetUrl
              isRequired(pullRequestNumber: $number)
            }
          }
        }
      }
    }
  }
}`

// probeQuery is the §3.3 slim retry probe (~120 B response, cost 1).
const probeQuery = `
query PRProbe($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { state mergeable mergeStateStatus }
  }
}`

const contextsPageQuery = `
query PRContexts($owner: String!, $name: String!, $number: Int!, $cursor: String!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      statusCheckRollup {
        contexts(first: 100, after: $cursor) {
          pageInfo { hasNextPage endCursor }
          nodes {
            __typename
            ... on CheckRun {
              name
              status
              conclusion
              detailsUrl
              isRequired(pullRequestNumber: $number)
              checkSuite { workflowRun { databaseId } }
            }
            ... on StatusContext {
              context
              state
              targetUrl
              isRequired(pullRequestNumber: $number)
            }
          }
        }
      }
    }
  }
}`

const threadsPageQuery = `
query PRThreads($owner: String!, $name: String!, $number: Int!, $cursor: String!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes { isResolved }
      }
    }
  }
}`

const currentPRQuery = `
query CurrentPR($owner: String!, $name: String!, $branch: String!) {
  repository(owner: $owner, name: $name) {
    pullRequests(headRefName: $branch, states: OPEN, first: 10,
                 orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes { number isCrossRepository viewerDidAuthor }
    }
  }
}`

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type ctxNode struct {
	Typename string `json:"__typename"`
	// CheckRun
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	IsRequired *bool  `json:"isRequired"` // nil when the coarse query omits it or it errored
	DetailsURL string `json:"detailsUrl"`
	CheckSuite *struct {
		WorkflowRun *struct {
			DatabaseID int64 `json:"databaseId"`
		} `json:"workflowRun"`
	} `json:"checkSuite"`
	// StatusContext
	Context   string `json:"context"`
	State     string `json:"state"`
	TargetURL string `json:"targetUrl"`
}

type contextsConn struct {
	TotalCount int       `json:"totalCount"`
	PageInfo   pageInfo  `json:"pageInfo"`
	Nodes      []ctxNode `json:"nodes"`
}

type rollup struct {
	State    string       `json:"state"`
	Contexts contextsConn `json:"contexts"`
}

type prNode struct {
	ID                    string `json:"id"`
	Number                int    `json:"number"`
	State                 string `json:"state"`
	IsDraft               bool   `json:"isDraft"`
	Locked                bool   `json:"locked"`
	Mergeable             string `json:"mergeable"`
	MergeStateStatus      string `json:"mergeStateStatus"`
	ReviewDecision        string `json:"reviewDecision"`
	IsCrossRepository     bool   `json:"isCrossRepository"`
	BaseRefName           string `json:"baseRefName"`
	HeadRefOid            string `json:"headRefOid"`
	IsInMergeQueue        bool   `json:"isInMergeQueue"`
	IsMergeQueueEnabled   bool   `json:"isMergeQueueEnabled"`
	ViewerCanUpdateBranch bool   `json:"viewerCanUpdateBranch"`
	Repository            *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"` // populated by the search (multi) query only
	MergeQueueEntry *struct {
		Position             int    `json:"position"`
		State                string `json:"state"`
		EstimatedTimeToMerge *int   `json:"estimatedTimeToMerge"`
	} `json:"mergeQueueEntry"`
	ReviewRequests struct {
		Nodes []struct {
			AsCodeOwner bool `json:"asCodeOwner"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	ReviewThreads struct {
		TotalCount int      `json:"totalCount"`
		PageInfo   pageInfo `json:"pageInfo"`
		Nodes      []struct {
			IsResolved bool `json:"isResolved"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	BaseRef *struct {
		Compare *struct {
			AheadBy  int `json:"aheadBy"`
			BehindBy int `json:"behindBy"`
		} `json:"compare"`
		BranchProtectionRule *struct {
			RequiresStrictStatusChecks     bool     `json:"requiresStrictStatusChecks"`
			RequiresConversationResolution bool     `json:"requiresConversationResolution"`
			RequiredStatusCheckContexts    []string `json:"requiredStatusCheckContexts"`
		} `json:"branchProtectionRule"`
	} `json:"baseRef"`
	StatusCheckRollup *rollup `json:"statusCheckRollup"`
}

type prCoreResp struct {
	Repository struct {
		PullRequest *prNode `json:"pullRequest"`
	} `json:"repository"`
}

// PR fetches and normalizes one pull request. retryBudget bounds the
// mergeable-UNKNOWN probe loop (0 disables it). Degraded conditions are
// recorded in the returned Input, not returned as errors.
func PR(ctx context.Context, c gh.Doer, sleep SleepFunc, owner, name string, number int, retryBudget time.Duration) (synth.Input, error) {
	what := fmt.Sprintf("%s/%s#%d", owner, name, number)
	vars := map[string]interface{}{
		"owner": owner, "name": name, "number": number,
		"headRef": fmt.Sprintf("refs/pull/%d/head", number),
	}

	pr, degraded, err := fetchFull(ctx, c, vars, what)
	if err != nil {
		return synth.Input{}, err
	}

	// §3.3: only OPEN PRs are worth probing — merged PRs report UNKNOWN forever.
	if pr.State == "OPEN" && pr.Mergeable == "UNKNOWN" && retryBudget > 0 {
		var spent time.Duration
		for _, d := range ladder {
			if spent+d > retryBudget {
				break
			}
			sleep(d)
			spent += d
			probe, perr := fetchProbe(ctx, c, vars, what)
			if perr != nil {
				break // keep the full snapshot we already have
			}
			if probe.State != "OPEN" || probe.Mergeable != "UNKNOWN" {
				if fresh, fdeg, ferr := fetchFull(ctx, c, vars, what); ferr == nil {
					pr, degraded = fresh, fdeg
				}
				break
			}
		}
	}

	in := normalize(pr, owner+"/"+name)
	in.Degraded = append(in.Degraded, degraded...)
	paginate(ctx, c, vars, pr, &in)
	return in, nil
}

func fetchFull(ctx context.Context, c gh.Doer, vars map[string]interface{}, what string) (*prNode, []string, error) {
	var resp prCoreResp
	var degraded []string
	if err := c.Do(ctx, prCoreQuery, vars, &resp); err != nil {
		ok, tokens := gh.Partial(err)
		if !ok {
			return nil, nil, gh.Classify(err, what)
		}
		degraded = tokens
	}
	if resp.Repository.PullRequest == nil {
		return nil, nil, &gh.NotFoundError{What: what}
	}
	return resp.Repository.PullRequest, degraded, nil
}

func fetchProbe(ctx context.Context, c gh.Doer, vars map[string]interface{}, what string) (*prNode, error) {
	pv := map[string]interface{}{"owner": vars["owner"], "name": vars["name"], "number": vars["number"]}
	var resp prCoreResp
	if err := c.Do(ctx, probeQuery, pv, &resp); err != nil {
		if ok, _ := gh.Partial(err); !ok {
			return nil, gh.Classify(err, what)
		}
	}
	if resp.Repository.PullRequest == nil {
		return nil, &gh.NotFoundError{What: what}
	}
	return resp.Repository.PullRequest, nil
}

// normalize maps the raw GraphQL node onto synth's Input.
func normalize(pr *prNode, repo string) synth.Input {
	in := synth.Input{
		Repo:           repo,
		Number:         pr.Number,
		PRState:        pr.State,
		IsDraft:        pr.IsDraft,
		Mergeable:      pr.Mergeable,
		MergeState:     pr.MergeStateStatus,
		ReviewDecision: pr.ReviewDecision,
		BaseRef:        pr.BaseRefName,
		BehindBy:       -1,
		CanUpdate:      pr.ViewerCanUpdateBranch,
		QueueEnabled:   pr.IsMergeQueueEnabled,
		RequiredKnown:  true,
		ThreadsKnown:   true,
	}
	if pr.Repository != nil {
		in.Repo = pr.Repository.NameWithOwner
	}
	for _, rr := range pr.ReviewRequests.Nodes {
		if rr.AsCodeOwner {
			in.CodeOwnerWait = true
		}
	}
	if q := pr.MergeQueueEntry; q != nil {
		eta := -1
		if q.EstimatedTimeToMerge != nil {
			eta = *q.EstimatedTimeToMerge
		}
		in.Queue = &synth.Queue{Position: q.Position, State: q.State, ETASec: eta}
	}
	for _, t := range pr.ReviewThreads.Nodes {
		if !t.IsResolved {
			in.UnresolvedThreads++
		}
	}
	if pr.BaseRef != nil {
		if cmp := pr.BaseRef.Compare; cmp != nil {
			in.BehindBy = cmp.BehindBy
		}
		if bpr := pr.BaseRef.BranchProtectionRule; bpr != nil {
			in.ConvResolution = bpr.RequiresConversationResolution
			in.MissingRequired = bpr.RequiredStatusCheckContexts // filtered in synth against the rollup
		}
	}
	if ru := pr.StatusCheckRollup; ru != nil {
		in.RollupPresent = true
		in.RollupState = ru.State
		in.Contexts = appendContexts(in.Contexts, ru.Contexts.Nodes)
	}
	return in
}

func appendContexts(dst []synth.Context, nodes []ctxNode) []synth.Context {
	for _, n := range nodes {
		c := synth.Context{}
		switch n.Typename {
		case "CheckRun":
			c.Kind = "check"
			c.Name = n.Name
			c.Status = n.Status
			c.Conclusion = n.Conclusion
			c.URL = n.DetailsURL
			if n.CheckSuite != nil && n.CheckSuite.WorkflowRun != nil {
				c.RunID = n.CheckSuite.WorkflowRun.DatabaseID
			}
		case "StatusContext":
			c.Kind = "status"
			c.Name = n.Context
			c.Conclusion = n.State
			c.URL = n.TargetURL
		default:
			continue
		}
		if n.IsRequired != nil {
			c.Required = *n.IsRequired
		}
		dst = append(dst, c)
	}
	return dst
}

// paginate follows the thread and context cursors left open by the core
// query, up to the §3.2 caps; failures and cap overruns degrade the output.
func paginate(ctx context.Context, c gh.Doer, vars map[string]interface{}, pr *prNode, in *synth.Input) {
	tp := pr.ReviewThreads.PageInfo
	for page := 2; tp.HasNextPage; page++ {
		if page > maxThreadPages {
			in.Degraded = append(in.Degraded, "threads_truncated")
			break
		}
		var resp struct {
			Repository struct {
				PullRequest *struct {
					ReviewThreads struct {
						PageInfo pageInfo `json:"pageInfo"`
						Nodes    []struct {
							IsResolved bool `json:"isResolved"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.Do(ctx, threadsPageQuery, withCursor(vars, tp.EndCursor), &resp); err != nil {
			in.Degraded = append(in.Degraded, "threads_truncated")
			break
		}
		if resp.Repository.PullRequest == nil {
			break
		}
		for _, t := range resp.Repository.PullRequest.ReviewThreads.Nodes {
			if !t.IsResolved {
				in.UnresolvedThreads++
			}
		}
		tp = resp.Repository.PullRequest.ReviewThreads.PageInfo
	}

	var cp pageInfo
	if pr.StatusCheckRollup != nil {
		cp = pr.StatusCheckRollup.Contexts.PageInfo
	}
	for page := 2; cp.HasNextPage; page++ {
		if page > maxContextPages {
			in.Degraded = append(in.Degraded, "checks_truncated")
			break
		}
		var resp struct {
			Repository struct {
				PullRequest *struct {
					StatusCheckRollup *struct {
						Contexts contextsConn `json:"contexts"`
					} `json:"statusCheckRollup"`
				} `json:"pullRequest"`
			} `json:"repository"`
		}
		if err := c.Do(ctx, contextsPageQuery, withCursor(vars, cp.EndCursor), &resp); err != nil {
			in.Degraded = append(in.Degraded, "checks_truncated")
			break
		}
		if resp.Repository.PullRequest == nil || resp.Repository.PullRequest.StatusCheckRollup == nil {
			break
		}
		ru := resp.Repository.PullRequest.StatusCheckRollup
		in.Contexts = appendContexts(in.Contexts, ru.Contexts.Nodes)
		cp = ru.Contexts.PageInfo
	}
}

func withCursor(vars map[string]interface{}, cursor string) map[string]interface{} {
	pv := map[string]interface{}{
		"owner": vars["owner"], "name": vars["name"], "number": vars["number"],
		"cursor": cursor,
	}
	return pv
}

// ResolveCurrentPR finds the open PR for a head branch, preferring a
// same-repo head over fork PRs with a colliding branch name, then the
// caller's own PR, then the newest (§4.1).
func ResolveCurrentPR(ctx context.Context, c gh.Doer, owner, name, branch string) (int, error) {
	var resp struct {
		Repository struct {
			PullRequests struct {
				Nodes []struct {
					Number            int  `json:"number"`
					IsCrossRepository bool `json:"isCrossRepository"`
					ViewerDidAuthor   bool `json:"viewerDidAuthor"`
				} `json:"nodes"`
			} `json:"pullRequests"`
		} `json:"repository"`
	}
	err := c.Do(ctx, currentPRQuery, map[string]interface{}{
		"owner": owner, "name": name, "branch": branch,
	}, &resp)
	if err != nil {
		return 0, gh.Classify(err, fmt.Sprintf("%s/%s branch %s", owner, name, branch))
	}
	nodes := resp.Repository.PullRequests.Nodes
	if len(nodes) == 0 {
		return 0, gh.Wrap("no_pr_found", 4, "no open PR for branch %q in %s/%s", branch, owner, name)
	}
	for _, n := range nodes {
		if !n.IsCrossRepository {
			return n.Number, nil
		}
	}
	for _, n := range nodes {
		if n.ViewerDidAuthor {
			return n.Number, nil
		}
	}
	return nodes[0].Number, nil
}
