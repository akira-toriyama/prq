package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/akira-toriyama/prq/internal/gh"
)

type fakeDoer struct {
	t         *testing.T
	responses []fakeResp
}

type fakeResp struct {
	body string
	err  error
}

func (f *fakeDoer) Do(_ context.Context, query string, _ map[string]interface{}, response interface{}) error {
	if len(f.responses) == 0 {
		f.t.Fatalf("unexpected call:\n%s", query)
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

func testDeps(t *testing.T, doer *fakeDoer) deps {
	return deps{
		client:        func() (gh.Doer, error) { return doer, nil },
		currentRepo:   func() (string, string, string, error) { return "github.com", "cur", "rent", nil },
		currentBranch: func() (string, error) { return "feat/x", nil },
		sleep:         func(time.Duration) {},
	}
}

const draftBlockedBody = `{"repository":{"pullRequest":{
	"id":"PR_x","number":13781,"state":"OPEN","isDraft":true,
	"mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED",
	"reviewDecision":"REVIEW_REQUIRED","baseRefName":"trunk","headRefOid":"ea979d1",
	"baseRef":{"compare":{"aheadBy":2,"behindBy":0},"branchProtectionRule":null},
	"reviewThreads":{"totalCount":0,"pageInfo":{"hasNextPage":false},"nodes":[]},
	"statusCheckRollup":null}}}`

func TestRunSingleGolden(t *testing.T) {
	var stdout, stderr bytes.Buffer
	d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: draftBlockedBody}}})

	code := run([]string{"13781", "-R", "cli/cli"}, &stdout, &stderr, d)

	if code != 1 {
		t.Fatalf("exit = %d, want 1 (blocked); stderr: %s", code, stderr.String())
	}
	got := stdout.String()
	var doc struct {
		PR       string   `json:"pr"`
		State    string   `json:"state"`
		FP       string   `json:"fp"`
		Blockers []string `json:"blockers"`
		CI       string   `json:"ci"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.PR != "cli/cli#13781" || doc.State != "BLOCKED" || doc.CI != "NONE" {
		t.Errorf("doc = %+v", doc)
	}
	wantBlockers := []string{
		"draft: marked as draft -> gh pr ready 13781 -R cli/cli",
		"review: REVIEW_REQUIRED",
	}
	if len(doc.Blockers) != 2 || doc.Blockers[0] != wantBlockers[0] || doc.Blockers[1] != wantBlockers[1] {
		t.Errorf("blockers = %q", doc.Blockers)
	}
	if len(doc.FP) != 16 {
		t.Errorf("fp = %q, want 16 hex chars", doc.FP)
	}
	// Determinism: field order fixed, one line, LF-terminated.
	if !strings.HasPrefix(got, `{"pr":"cli/cli#13781","state":"BLOCKED","fp":"`) || !strings.HasSuffix(got, "}\n") {
		t.Errorf("wire shape drifted: %s", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr not empty: %s", stderr.String())
	}
	if len(got) > 1024 {
		t.Errorf("output %d bytes, budget 1024", len(got))
	}
}

func TestRunDeterministicBytes(t *testing.T) {
	out := func() string {
		var stdout, stderr bytes.Buffer
		d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: draftBlockedBody}}})
		run([]string{"13781", "-R", "cli/cli"}, &stdout, &stderr, d)
		return stdout.String()
	}
	if out() != out() {
		t.Error("identical facts produced different bytes")
	}
}

func TestRunSelectorForms(t *testing.T) {
	for _, args := range [][]string{
		{"-R", "cli/cli", "13781"},
		{"https://github.com/cli/cli/pull/13781"},
		{"cli/cli#13781"},
	} {
		var stdout, stderr bytes.Buffer
		d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: draftBlockedBody}}})
		if code := run(args, &stdout, &stderr, d); code != 1 {
			t.Errorf("%v: exit = %d, want 1; stderr: %s", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"pr":"cli/cli#13781"`) {
			t.Errorf("%v: wrong pr key: %s", args, stdout.String())
		}
	}
}

func TestRunCurrentBranchResolution(t *testing.T) {
	var stdout, stderr bytes.Buffer
	resolve := `{"repository":{"pullRequests":{"nodes":[{"number":13781,"isCrossRepository":false}]}}}`
	d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: resolve}, {body: draftBlockedBody}}})
	if code := run(nil, &stdout, &stderr, d); code != 1 {
		t.Fatalf("exit = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"pr":"cur/rent#13781"`) {
		t.Errorf("current-branch PR not resolved: %s", stdout.String())
	}
}

func TestRunDegradedGreenIsExitThree(t *testing.T) {
	cleanBody := `{"repository":{"pullRequest":{
		"id":"PR_x","number":1,"state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN",
		"baseRefName":"main","headRefOid":"abc",
		"reviewThreads":{"totalCount":0,"pageInfo":{"hasNextPage":false},"nodes":[]},
		"statusCheckRollup":null}}}`
	gqlErr := &api.GraphQLError{Errors: []api.GraphQLErrorItem{{
		Type: "FORBIDDEN", Path: []interface{}{"repository", "pullRequest", "reviewThreads"},
	}}}
	var stdout, stderr bytes.Buffer
	d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: cleanBody, err: gqlErr}}})

	code := run([]string{"1", "-R", "o/r"}, &stdout, &stderr, d)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (unverifiable green)", code)
	}
	if !strings.Contains(stdout.String(), `"state":"CLEAN"`) || !strings.Contains(stdout.String(), `"degraded":["threads"]`) {
		t.Errorf("stdout should carry the degraded verdict: %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("soft-3 must keep stderr empty: %s", stderr.String())
	}
}

func TestRunDegradedBlockedStaysExitOne(t *testing.T) {
	body := `{"repository":{"pullRequest":{
		"id":"PR_x","number":1,"state":"OPEN","isDraft":true,"mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED",
		"baseRefName":"main","headRefOid":"abc",
		"reviewThreads":{"totalCount":0,"pageInfo":{"hasNextPage":false},"nodes":[]},
		"statusCheckRollup":null}}}`
	gqlErr := &api.GraphQLError{Errors: []api.GraphQLErrorItem{{
		Type: "FORBIDDEN", Path: []interface{}{"repository", "pullRequest", "reviewThreads"},
	}}}
	var stdout, stderr bytes.Buffer
	d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: body, err: gqlErr}}})
	if code := run([]string{"1", "-R", "o/r"}, &stdout, &stderr, d); code != 1 {
		t.Fatalf("exit = %d, want 1 (visible blockers are trustworthy)", code)
	}
}

func TestRunMineNDJSON(t *testing.T) {
	search := `{"search":{"issueCount":40,"nodes":[
		{"id":"PR_a","number":5,"repository":{"nameWithOwner":"o/x"},"state":"OPEN",
		 "mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","baseRefName":"main",
		 "reviewThreads":{"totalCount":0,"nodes":[]},"statusCheckRollup":null},
		{"id":"PR_b","number":7,"repository":{"nameWithOwner":"o/x"},"state":"OPEN","isDraft":true,
		 "mergeable":"MERGEABLE","mergeStateStatus":"BLOCKED","baseRefName":"main",
		 "reviewThreads":{"totalCount":0,"nodes":[]},"statusCheckRollup":null}]}}`
	var stdout, stderr bytes.Buffer
	d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: search}}})

	code := run([]string{"--mine", "--repos", "o/x", "--limit", "30"}, &stdout, &stderr, d)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (rollup: blocked beats clean); stderr: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 3 { // 2 PRs + truncated
		t.Fatalf("lines = %d, want 3:\n%s", len(lines), stdout.String())
	}
	for i, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Errorf("line %d is not valid JSON: %s", i, l)
		}
		if len(l) > 256 {
			t.Errorf("line %d is %d bytes, budget 256", i, len(l))
		}
	}
	if !strings.Contains(lines[2], `"truncated":true`) || !strings.Contains(lines[2], `"total":40`) {
		t.Errorf("missing truncation line: %s", lines[2])
	}
	if strings.Contains(stdout.String(), "non_blocking") || strings.Contains(stdout.String(), `"checks"`) {
		t.Error("--mine rows must omit non_blocking and checks")
	}
}

func TestRunUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"mine with positional", []string{"--mine", "123"}},
		{"two targets", []string{"1", "2"}},
		{"bad repo flag", []string{"1", "-R", "nodash"}},
		{"bad url", []string{"https://github.com/cli/cli/issues/1"}},
		{"unknown flag", []string{"--nope"}},
		{"bad limit", []string{"--mine", "--limit", "0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			d := testDeps(t, &fakeDoer{t: t})
			if code := run(tc.args, &stdout, &stderr, d); code != 64 {
				t.Fatalf("exit = %d, want 64", code)
			}
			var e struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &e); err != nil {
				t.Fatalf("stderr is not an error object: %s", stderr.String())
			}
			if e.Error.Code != "usage" {
				t.Errorf("code = %q, want usage", e.Error.Code)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout not empty on usage error: %s", stdout.String())
			}
		})
	}
}

func TestRunErrorTaxonomy(t *testing.T) {
	t.Run("auth exit 3", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		d := testDeps(t, nil)
		d.client = func() (gh.Doer, error) {
			return nil, &gh.Error{Code: "auth", Exit: 3, Message: "no token"}
		}
		if code := run([]string{"1", "-R", "o/r"}, &stdout, &stderr, d); code != 3 {
			t.Fatalf("exit = %d, want 3", code)
		}
		if !strings.Contains(stderr.String(), `"code":"auth"`) {
			t.Errorf("stderr = %s", stderr.String())
		}
	})
	t.Run("not found exit 3 (masks private-vs-missing)", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: `{"repository":{"pullRequest":null}}`}}})
		if code := run([]string{"1", "-R", "o/r"}, &stdout, &stderr, d); code != 3 {
			t.Fatalf("exit = %d, want 3", code)
		}
		if !strings.Contains(stderr.String(), `"code":"not_found_or_forbidden"`) {
			t.Errorf("stderr = %s", stderr.String())
		}
	})
	t.Run("no PR for branch exit 4", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: `{"repository":{"pullRequests":{"nodes":[]}}}`}}})
		if code := run([]string{"gone-branch", "-R", "o/r"}, &stdout, &stderr, d); code != 4 {
			t.Fatalf("exit = %d, want 4", code)
		}
		if !strings.Contains(stderr.String(), `"code":"no_pr_found"`) {
			t.Errorf("stderr = %s", stderr.String())
		}
	})
	t.Run("rate limited exit 4 with retry hint", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		d := testDeps(t, nil)
		d.client = func() (gh.Doer, error) {
			return nil, &gh.Error{Code: "rate_limited", Exit: 4, Message: "slow down", RetryAfter: 30}
		}
		if code := run([]string{"1", "-R", "o/r"}, &stdout, &stderr, d); code != 4 {
			t.Fatalf("exit = %d, want 4", code)
		}
		if !strings.Contains(stderr.String(), `"retry_after_s":30`) {
			t.Errorf("stderr = %s", stderr.String())
		}
	})
}

func TestRunVersionAndHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	d := testDeps(t, &fakeDoer{t: t})
	if code := run([]string{"--version"}, &stdout, &stderr, d); code != 0 {
		t.Fatalf("--version exit = %d", code)
	}
	if !strings.HasPrefix(stdout.String(), "prq ") {
		t.Errorf("version output = %q", stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"-h"}, &stdout, &stderr, d); code != 0 {
		t.Fatalf("-h exit = %d", code)
	}
	if !strings.Contains(stdout.String(), "usage:") {
		t.Errorf("help output = %q", stdout.String())
	}
}

func TestParseSelectors(t *testing.T) {
	urls := []struct {
		raw    string
		owner  string
		number int
		ok     bool
	}{
		{"https://github.com/cli/cli/pull/13781", "cli", 13781, true},
		{"https://github.com/cli/cli/pull/13781/files", "cli", 13781, true},
		{"https://github.com/cli/cli/pull/13781#discussion_r1", "cli", 13781, true},
		{"https://github.com/cli/cli/pull/13781?diff=split", "cli", 13781, true},
		{"github.com/cli/cli/pull/2", "cli", 2, true},
		{"https://github.com/cli/cli/issues/1", "", 0, false},
		{"https://github.com/cli/cli", "", 0, false},
		{"https://github.com/cli/cli/pull/zero", "", 0, false},
	}
	for _, tc := range urls {
		owner, _, number, err := parsePRURL(tc.raw)
		if tc.ok && (err != nil || owner != tc.owner || number != tc.number) {
			t.Errorf("parsePRURL(%q) = (%s, %d, %v)", tc.raw, owner, number, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("parsePRURL(%q) succeeded, want error", tc.raw)
		}
	}

	repoNums := []struct {
		raw string
		ok  bool
	}{
		{"cli/cli#13781", true},
		{"o/r#1", true},
		{"o/r#0", false},
		{"o#1", false},
		{"o/r/x#1", false},
		{"plain-branch", false},
		{"feature#test", false},
	}
	for _, tc := range repoNums {
		_, _, _, ok := parseRepoNumber(tc.raw)
		if ok != tc.ok {
			t.Errorf("parseRepoNumber(%q) ok = %v, want %v", tc.raw, ok, tc.ok)
		}
	}
}

func TestRunMergedDegradedStaysExitZero(t *testing.T) {
	mergedBody := `{"repository":{"pullRequest":{
		"id":"PR_x","number":2,"state":"MERGED","mergeable":"UNKNOWN","mergeStateStatus":"UNKNOWN",
		"baseRefName":"main","headRefOid":"abc",
		"reviewThreads":{"totalCount":0,"pageInfo":{"hasNextPage":false},"nodes":[]},
		"statusCheckRollup":null}}}`
	gqlErr := &api.GraphQLError{Errors: []api.GraphQLErrorItem{{
		Type: "FORBIDDEN", Path: []interface{}{"repository", "pullRequest", "reviewThreads"},
	}}}
	var stdout, stderr bytes.Buffer
	d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: mergedBody, err: gqlErr}}})
	if code := run([]string{"2", "-R", "o/r"}, &stdout, &stderr, d); code != 0 {
		t.Fatalf("exit = %d, want 0 (MERGED is final, degradation irrelevant)", code)
	}
	if !strings.Contains(stdout.String(), `"final":true`) {
		t.Errorf("stdout = %s", stdout.String())
	}
}

func TestParseOriginURL(t *testing.T) {
	cases := []struct {
		url   string
		owner string
		name  string
		ok    bool
	}{
		{"git@github.com:cli/cli.git", "cli", "cli", true},
		{"github.com.akira-toriyama:akira-toriyama/prq.git", "akira-toriyama", "prq", true},
		{"git@github.com.work:o/r.git", "o", "r", true},
		{"ssh://git@github.com/o/r.git", "o", "r", true},
		{"https://github.com/o/r", "o", "r", true},
		{"https://github.com/o/r.git", "o", "r", true},
		{"git@gitlab.com:o/r.git", "", "", false},
		{"gh-alias:o/r.git", "", "", false},
		{"github.community:o/r.git", "", "", false},
		{"git@github.com:broken", "", "", false},
	}
	for _, tc := range cases {
		host, owner, name, err := parseOriginURL(tc.url)
		if tc.ok && (err != nil || owner != tc.owner || name != tc.name || host != "github.com") {
			t.Errorf("parseOriginURL(%q) = (%s, %s, %s, %v)", tc.url, host, owner, name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseOriginURL(%q) succeeded, want error", tc.url)
		}
	}
}

func TestRunMineTruncatedLineByteOrder(t *testing.T) {
	search := `{"search":{"issueCount":40,"nodes":[
		{"id":"PR_a","number":5,"repository":{"nameWithOwner":"o/x"},"state":"OPEN",
		 "mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","baseRefName":"main",
		 "reviewThreads":{"totalCount":0,"nodes":[]},"statusCheckRollup":null}]}}`
	var stdout, stderr bytes.Buffer
	d := testDeps(t, &fakeDoer{t: t, responses: []fakeResp{{body: search}}})
	run([]string{"--mine", "--repos", "o/x"}, &stdout, &stderr, d)
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	last := lines[len(lines)-1]
	if last != `{"truncated":true,"total":40}` {
		t.Errorf("truncated line = %q, want normative key order", last)
	}
}

func TestRunReposWithOnlyEmptyEntriesIsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	d := testDeps(t, &fakeDoer{t: t})
	if code := run([]string{"--mine", "--repos", ","}, &stdout, &stderr, d); code != 64 {
		t.Fatalf("exit = %d, want 64 (must not silently widen scope)", code)
	}
}
