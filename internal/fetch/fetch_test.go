package fetch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/akira-toriyama/prq/internal/gh"
)

// fakeDoer replays canned responses in order; a response may carry both a
// partial body and an error, exactly like go-gh's GraphQL client.
type fakeDoer struct {
	t         *testing.T
	responses []fakeResp
	queries   []string
	vars      []map[string]interface{}
}

type fakeResp struct {
	body string
	err  error
}

func (f *fakeDoer) Do(_ context.Context, query string, vars map[string]interface{}, response interface{}) error {
	f.queries = append(f.queries, query)
	f.vars = append(f.vars, vars)
	if len(f.responses) == 0 {
		f.t.Fatalf("unexpected call #%d:\n%s", len(f.queries), query)
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	if r.body != "" {
		if err := json.Unmarshal([]byte(r.body), response); err != nil {
			f.t.Fatalf("bad fixture: %v", err)
		}
	}
	return r.err
}

func noSleep(time.Duration) {}

func ctxTest() context.Context { return context.Background() }

func prBody(state, mergeable, mergeState string) string {
	return `{"repository":{"pullRequest":{
		"id":"PR_x","number":1,"state":"` + state + `","isDraft":false,
		"mergeable":"` + mergeable + `","mergeStateStatus":"` + mergeState + `",
		"reviewDecision":"","baseRefName":"main","headRefOid":"abc",
		"baseRef":{"compare":{"aheadBy":1,"behindBy":0},"branchProtectionRule":null},
		"reviewThreads":{"totalCount":0,"pageInfo":{"hasNextPage":false},"nodes":[]},
		"statusCheckRollup":null}}}`
}

func TestPRRetryLadder(t *testing.T) {
	var slept []time.Duration
	d := &fakeDoer{t: t, responses: []fakeResp{
		{body: prBody("OPEN", "UNKNOWN", "UNKNOWN")},                                      // full
		{body: `{"repository":{"pullRequest":{"state":"OPEN","mergeable":"UNKNOWN"}}}`},   // probe 1
		{body: `{"repository":{"pullRequest":{"state":"OPEN","mergeable":"MERGEABLE"}}}`}, // probe 2 resolves
		{body: prBody("OPEN", "MERGEABLE", "CLEAN")},                                      // fresh full
	}}
	in, err := PR(ctxTest(), d, func(s time.Duration) { slept = append(slept, s) }, "o", "r", 1, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if in.Mergeable != "MERGEABLE" || in.MergeState != "CLEAN" {
		t.Errorf("got %s/%s, want fresh MERGEABLE/CLEAN", in.Mergeable, in.MergeState)
	}
	if len(d.queries) != 4 {
		t.Errorf("made %d calls, want 4 (full, probe, probe, full)", len(d.queries))
	}
	want := []time.Duration{500 * time.Millisecond, time.Second}
	if len(slept) != 2 || slept[0] != want[0] || slept[1] != want[1] {
		t.Errorf("ladder = %v, want %v", slept, want)
	}
	if !strings.Contains(d.queries[1], "PRProbe") {
		t.Error("retry should use the slim probe query")
	}
}

func TestPRRetryBudgetExhausted(t *testing.T) {
	d := &fakeDoer{t: t}
	d.responses = append(d.responses, fakeResp{body: prBody("OPEN", "UNKNOWN", "UNKNOWN")})
	for i := 0; i < 4; i++ {
		d.responses = append(d.responses, fakeResp{body: `{"repository":{"pullRequest":{"state":"OPEN","mergeable":"UNKNOWN"}}}`})
	}
	var slept time.Duration
	in, err := PR(ctxTest(), d, func(s time.Duration) { slept += s }, "o", "r", 1, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.queries) != 5 {
		t.Errorf("made %d calls, want 5 (1 full + 4 probes)", len(d.queries))
	}
	if slept != 7500*time.Millisecond {
		t.Errorf("total sleep = %v, want 7.5s", slept)
	}
	if in.Mergeable != "UNKNOWN" {
		t.Errorf("mergeable = %q, want UNKNOWN passthrough", in.Mergeable)
	}
}

func TestPRNoRetryOnZeroBudgetAndNonOpen(t *testing.T) {
	d := &fakeDoer{t: t, responses: []fakeResp{{body: prBody("OPEN", "UNKNOWN", "UNKNOWN")}}}
	if _, err := PR(ctxTest(), d, noSleep, "o", "r", 1, 0); err != nil {
		t.Fatal(err)
	}
	if len(d.queries) != 1 {
		t.Errorf("budget 0: made %d calls, want 1", len(d.queries))
	}

	// Merged PRs report mergeable UNKNOWN forever — never probe them.
	d2 := &fakeDoer{t: t, responses: []fakeResp{{body: prBody("MERGED", "UNKNOWN", "UNKNOWN")}}}
	in, err := PR(ctxTest(), d2, noSleep, "o", "r", 1, 8*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(d2.queries) != 1 {
		t.Errorf("merged PR: made %d calls, want 1 (no probes)", len(d2.queries))
	}
	if in.PRState != "MERGED" {
		t.Errorf("state = %q", in.PRState)
	}
}

func TestPRNotFound(t *testing.T) {
	d := &fakeDoer{t: t, responses: []fakeResp{{body: `{"repository":{"pullRequest":null}}`}}}
	_, err := PR(ctxTest(), d, noSleep, "o", "r", 404, 0)
	var nf *gh.NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want NotFoundError", err)
	}
}

func TestPRPartialResponseDegrades(t *testing.T) {
	gqlErr := &api.GraphQLError{Errors: []api.GraphQLErrorItem{{
		Type: "FORBIDDEN",
		Path: []interface{}{"repository", "pullRequest", "reviewThreads"},
	}}}
	d := &fakeDoer{t: t, responses: []fakeResp{{body: prBody("OPEN", "MERGEABLE", "CLEAN"), err: gqlErr}}}
	in, err := PR(ctxTest(), d, noSleep, "o", "r", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Degraded) != 1 || in.Degraded[0] != "threads" {
		t.Errorf("degraded = %q, want [threads]", in.Degraded)
	}
}

func TestPRHardErrorClassified(t *testing.T) {
	d := &fakeDoer{t: t, responses: []fakeResp{{err: &api.HTTPError{StatusCode: 401}}}}
	_, err := PR(ctxTest(), d, noSleep, "o", "r", 1, 0)
	var ghErr *gh.Error
	if !errors.As(err, &ghErr) || ghErr.Code != "auth" || ghErr.Exit != 3 {
		t.Fatalf("err = %v, want auth/exit3", err)
	}
}

func TestPRNormalization(t *testing.T) {
	body := `{"repository":{"pullRequest":{
		"id":"PR_x","number":9,"state":"OPEN","isDraft":true,
		"mergeable":"MERGEABLE","mergeStateStatus":"BEHIND",
		"reviewDecision":"REVIEW_REQUIRED","baseRefName":"trunk","headRefOid":"abc",
		"isMergeQueueEnabled":true,"viewerCanUpdateBranch":true,
		"mergeQueueEntry":{"position":2,"state":"AWAITING_CHECKS","estimatedTimeToMerge":300},
		"reviewRequests":{"nodes":[{"asCodeOwner":true}]},
		"reviewThreads":{"totalCount":3,"pageInfo":{"hasNextPage":false},
			"nodes":[{"isResolved":true},{"isResolved":false},{"isResolved":false}]},
		"baseRef":{"compare":{"aheadBy":2,"behindBy":7},
			"branchProtectionRule":{"requiresConversationResolution":true,"requiredStatusCheckContexts":["ci/x"]}},
		"statusCheckRollup":{"state":"FAILURE","contexts":{
			"totalCount":1,"pageInfo":{"hasNextPage":false},
			"nodes":[{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"FAILURE",
				"isRequired":true,"checkSuite":{"workflowRun":{"databaseId":42}}}]}}}}}`
	d := &fakeDoer{t: t, responses: []fakeResp{{body: body}}}
	in, err := PR(ctxTest(), d, noSleep, "o", "r", 9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !in.IsDraft || !in.CodeOwnerWait || !in.CanUpdate || !in.QueueEnabled || !in.ConvResolution {
		t.Errorf("flags not normalized: %+v", in)
	}
	if in.BehindBy != 7 {
		t.Errorf("behindBy = %d, want 7 (in-query compare)", in.BehindBy)
	}
	if in.UnresolvedThreads != 2 {
		t.Errorf("unresolved = %d, want 2", in.UnresolvedThreads)
	}
	if in.Queue == nil || in.Queue.Position != 2 || in.Queue.ETASec != 300 {
		t.Errorf("queue = %+v", in.Queue)
	}
	if len(in.Contexts) != 1 || !in.Contexts[0].Required || in.Contexts[0].RunID != 42 {
		t.Errorf("contexts = %+v", in.Contexts)
	}
	if len(in.MissingRequired) != 1 || in.MissingRequired[0] != "ci/x" {
		t.Errorf("missingRequired = %v", in.MissingRequired)
	}
	if v, _ := d.vars[0]["headRef"].(string); v != "refs/pull/9/head" {
		t.Errorf("headRef var = %q", v)
	}
}

func TestPRPaginatesContexts(t *testing.T) {
	page1 := `{"repository":{"pullRequest":{
		"id":"PR_x","number":1,"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED",
		"baseRefName":"main","headRefOid":"abc",
		"reviewThreads":{"totalCount":0,"pageInfo":{"hasNextPage":false},"nodes":[]},
		"statusCheckRollup":{"state":"FAILURE","contexts":{
			"totalCount":101,"pageInfo":{"hasNextPage":true,"endCursor":"C1"},
			"nodes":[{"__typename":"CheckRun","name":"a","status":"COMPLETED","conclusion":"SUCCESS","isRequired":true}]}}}}}`
	page2 := `{"repository":{"pullRequest":{"statusCheckRollup":{"contexts":{
		"pageInfo":{"hasNextPage":false},
		"nodes":[{"__typename":"CheckRun","name":"b","status":"COMPLETED","conclusion":"FAILURE","isRequired":true,
			"checkSuite":{"workflowRun":{"databaseId":42}}}]}}}}}`
	d := &fakeDoer{t: t, responses: []fakeResp{{body: page1}, {body: page2}}}
	in, err := PR(ctxTest(), d, noSleep, "o", "r", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Contexts) != 2 || in.Contexts[1].Name != "b" || in.Contexts[1].RunID != 42 {
		t.Fatalf("stitched contexts = %+v", in.Contexts)
	}
	if !strings.Contains(d.queries[1], "PRContexts") {
		t.Error("expected the contexts page query")
	}
	if c, _ := d.vars[1]["cursor"].(string); c != "C1" {
		t.Errorf("cursor = %q", c)
	}
}

func TestPRContextPageCap(t *testing.T) {
	page := func(hasNext bool) string {
		next := "false"
		if hasNext {
			next = "true"
		}
		return `{"repository":{"pullRequest":{"statusCheckRollup":{"contexts":{
			"pageInfo":{"hasNextPage":` + next + `,"endCursor":"C"},"nodes":[]}}}}}`
	}
	first := `{"repository":{"pullRequest":{
		"id":"PR_x","number":1,"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED",
		"baseRefName":"main","headRefOid":"abc",
		"reviewThreads":{"totalCount":0,"pageInfo":{"hasNextPage":false},"nodes":[]},
		"statusCheckRollup":{"state":"FAILURE","contexts":{
			"totalCount":600,"pageInfo":{"hasNextPage":true,"endCursor":"C"},"nodes":[]}}}}}`
	d := &fakeDoer{t: t, responses: []fakeResp{
		{body: first}, {body: page(true)}, {body: page(true)}, {body: page(true)},
	}}
	in, err := PR(ctxTest(), d, noSleep, "o", "r", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.queries) != 4 { // full + pages 2..4; page 5 hits the cap
		t.Errorf("made %d calls, want 4", len(d.queries))
	}
	found := false
	for _, tok := range in.Degraded {
		if tok == "checks_truncated" {
			found = true
		}
	}
	if !found {
		t.Errorf("degraded = %v, want checks_truncated", in.Degraded)
	}
}

func TestResolveCurrentPR(t *testing.T) {
	t.Run("prefers same-repo head over fork collisions", func(t *testing.T) {
		d := &fakeDoer{t: t, responses: []fakeResp{{body: `{"repository":{"pullRequests":{"nodes":[
			{"number":30,"isCrossRepository":true,"viewerDidAuthor":false},
			{"number":7,"isCrossRepository":false,"viewerDidAuthor":false}]}}}`}}}
		n, err := ResolveCurrentPR(ctxTest(), d, "o", "r", "feat")
		if err != nil || n != 7 {
			t.Fatalf("got (%d, %v), want (7, nil)", n, err)
		}
	})
	t.Run("falls back to own PR then newest", func(t *testing.T) {
		d := &fakeDoer{t: t, responses: []fakeResp{{body: `{"repository":{"pullRequests":{"nodes":[
			{"number":30,"isCrossRepository":true,"viewerDidAuthor":false},
			{"number":8,"isCrossRepository":true,"viewerDidAuthor":true}]}}}`}}}
		n, err := ResolveCurrentPR(ctxTest(), d, "o", "r", "feat")
		if err != nil || n != 8 {
			t.Fatalf("got (%d, %v), want (8, nil)", n, err)
		}
	})
	t.Run("no PR is a typed no_pr_found", func(t *testing.T) {
		d := &fakeDoer{t: t, responses: []fakeResp{{body: `{"repository":{"pullRequests":{"nodes":[]}}}`}}}
		_, err := ResolveCurrentPR(ctxTest(), d, "o", "r", "gone")
		var ghErr *gh.Error
		if !errors.As(err, &ghErr) || ghErr.Code != "no_pr_found" || ghErr.Exit != 4 {
			t.Fatalf("err = %v, want no_pr_found/exit4", err)
		}
	})
}

const mineSearchBody = `{"search":{"issueCount":2,"nodes":[
	{"id":"PR_a","number":5,"repository":{"nameWithOwner":"o/x"},"state":"OPEN",
	 "mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","reviewDecision":"","baseRefName":"main",
	 "reviewThreads":{"totalCount":0,"nodes":[]},
	 "statusCheckRollup":{"state":"FAILURE","contexts":{"totalCount":1}}},
	{"id":"PR_b","number":6,"repository":{"nameWithOwner":"o/y"},"state":"OPEN",
	 "mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":"","baseRefName":"main",
	 "reviewThreads":{"totalCount":0,"nodes":[]},
	 "statusCheckRollup":{"state":"SUCCESS","contexts":{"totalCount":1}}}
]}}`

func TestMineTwoPhase(t *testing.T) {
	detail := `{"a0":{"statusCheckRollup":{"contexts":{"totalCount":1,"pageInfo":{"hasNextPage":false},
		"nodes":[{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"FAILURE",
			"isRequired":true,"checkSuite":{"workflowRun":{"databaseId":99}}}]}}}}`
	d := &fakeDoer{t: t, responses: []fakeResp{{body: mineSearchBody}, {body: detail}}}

	res, err := Mine(ctxTest(), d, []string{"o/x", "o/y"}, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Inputs) != 2 || res.Truncated {
		t.Fatalf("inputs = %d truncated=%v", len(res.Inputs), res.Truncated)
	}
	if q, _ := d.vars[0]["q"].(string); !strings.Contains(q, "repo:o/x repo:o/y") {
		t.Errorf("search query missing repo scope: %q", q)
	}
	if len(d.queries) != 2 || !strings.Contains(d.queries[1], "MineDetail") {
		t.Fatalf("expected exactly one detail query, got %d calls", len(d.queries))
	}
	if !strings.Contains(d.queries[1], `isRequired(pullRequestId: "PR_a")`) {
		t.Error("detail query should use node-id isRequired")
	}
	blocked := res.Inputs[0] // sorted: o/x#5 first
	if !blocked.RequiredKnown || len(blocked.Contexts) != 1 || !blocked.Contexts[0].Required {
		t.Errorf("detail not applied: %+v", blocked)
	}
	clean := res.Inputs[1]
	if clean.RequiredKnown || len(clean.Contexts) != 0 {
		t.Errorf("clean PR should stay coarse: %+v", clean)
	}
}

func TestMineAllGreenSkipsPhaseTwo(t *testing.T) {
	body := `{"search":{"issueCount":1,"nodes":[
		{"id":"PR_b","number":6,"repository":{"nameWithOwner":"o/y"},"state":"OPEN",
		 "mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","baseRefName":"main",
		 "reviewThreads":{"totalCount":0,"nodes":[]},
		 "statusCheckRollup":{"state":"SUCCESS","contexts":{"totalCount":1}}}]}}`
	d := &fakeDoer{t: t, responses: []fakeResp{{body: body}}}
	if _, err := Mine(ctxTest(), d, nil, 30); err != nil {
		t.Fatal(err)
	}
	if len(d.queries) != 1 {
		t.Errorf("made %d calls, want 1", len(d.queries))
	}
}

func TestMineDetailFailureDegrades(t *testing.T) {
	d := &fakeDoer{t: t, responses: []fakeResp{
		{body: mineSearchBody},
		{err: &api.HTTPError{StatusCode: 502}},
	}}
	res, err := Mine(ctxTest(), d, nil, 30)
	if err != nil {
		t.Fatal(err)
	}
	blocked := res.Inputs[0]
	found := false
	for _, tok := range blocked.Degraded {
		if tok == "required" {
			found = true
		}
	}
	if !found {
		t.Errorf("degraded = %v, want required", blocked.Degraded)
	}
}

func TestMineTruncation(t *testing.T) {
	body := `{"search":{"issueCount":80,"nodes":[
		{"id":"PR_b","number":6,"repository":{"nameWithOwner":"o/y"},"state":"OPEN",
		 "mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","baseRefName":"main",
		 "reviewThreads":{"totalCount":0,"nodes":[]},"statusCheckRollup":null}]}}`
	d := &fakeDoer{t: t, responses: []fakeResp{{body: body}}}
	res, err := Mine(ctxTest(), d, nil, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.Total != 80 {
		t.Errorf("truncated=%v total=%d, want true/80", res.Truncated, res.Total)
	}
}
