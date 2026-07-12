package gh

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cli/go-gh/v2/pkg/api"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
		wantExit int
	}{
		{"401", &api.HTTPError{StatusCode: 401, Message: "bad credentials"}, "auth", 3},
		// SAML shape stays docs-based: no SAML-enforcing org was reachable
		// from this account to capture it live (2026-07-12 probe).
		{"403 saml", &api.HTTPError{StatusCode: 403, Message: "Resource protected by organization SAML enforcement"}, "saml_blocked", 3},
		{"403 rate limit", &api.HTTPError{StatusCode: 403, Message: "API rate limit exceeded", Headers: http.Header{"Retry-After": []string{"30"}}}, "rate_limited", 4},
		{"403 other", &api.HTTPError{StatusCode: 403, Message: "forbidden"}, "auth", 3},
		{"500", &api.HTTPError{StatusCode: 502, Message: "bad gateway"}, "server_error", 4},
		{"graphql rate limited", &api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "RATE_LIMITED"}}}, "rate_limited", 4},
		{"deadline", context.DeadlineExceeded, "timeout", 4},
		{"plain error", errors.New("dial tcp: connection refused"), "network", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err, "o/r#1")
			var ghErr *Error
			if !errors.As(got, &ghErr) {
				t.Fatalf("Classify() = %T %v, want *gh.Error", got, got)
			}
			if ghErr.Code != tc.wantCode || ghErr.Exit != tc.wantExit {
				t.Errorf("got %s/%d, want %s/%d", ghErr.Code, ghErr.Exit, tc.wantCode, tc.wantExit)
			}
		})
	}

	t.Run("404 and graphql NOT_FOUND become NotFoundError", func(t *testing.T) {
		for _, err := range []error{
			&api.HTTPError{StatusCode: 404},
			&api.GraphQLError{Errors: []api.GraphQLErrorItem{{Type: "NOT_FOUND"}}},
			// Live shape captured 2026-07-12 with a fine-grained PAT
			// ("All repositories" access, default metadata-only permission)
			// against an inaccessible private repo: GitHub masks the whole
			// repository node as NOT_FOUND — the same envelope a classic
			// token gets — so no fine-grained-specific branch is needed.
			// (Same run confirmed fine-grained PATs now support GraphQL at
			// all: single-PR and --mine calls succeed on public repos.)
			&api.GraphQLError{Errors: []api.GraphQLErrorItem{{
				Type:    "NOT_FOUND",
				Path:    []interface{}{"repository"},
				Message: "Could not resolve to a Repository with the name 'akira-toriyama/projects'.",
			}}},
		} {
			var nf *NotFoundError
			if !errors.As(Classify(err, "o/r#1"), &nf) {
				t.Errorf("Classify(%v): want NotFoundError", err)
			}
		}
	})

	t.Run("rate-limit retry-after surfaces", func(t *testing.T) {
		err := Classify(&api.HTTPError{StatusCode: 403, Message: "rate limit",
			Headers: http.Header{"Retry-After": []string{"42"}}}, "x")
		var ghErr *Error
		if !errors.As(err, &ghErr) || ghErr.RetryAfter != 42 {
			t.Errorf("retryAfter = %v", err)
		}
	})
}

func TestPartial(t *testing.T) {
	t.Run("optional subtree errors degrade", func(t *testing.T) {
		err := &api.GraphQLError{Errors: []api.GraphQLErrorItem{
			{Type: "FORBIDDEN", Path: []interface{}{"repository", "pullRequest", "reviewThreads"}},
			{Type: "FORBIDDEN", Path: []interface{}{"repository", "pullRequest", "reviewThreads", float64(0)}},
		}}
		ok, tokens := Partial(err)
		if !ok || len(tokens) != 1 || tokens[0] != "threads" {
			t.Errorf("Partial = (%v, %v)", ok, tokens)
		}
	})
	t.Run("isRequired message shape degrades to required", func(t *testing.T) {
		err := &api.GraphQLError{Errors: []api.GraphQLErrorItem{
			{Type: "FORBIDDEN", Message: "A pull request ID or pull request number is required"},
		}}
		ok, tokens := Partial(err)
		if !ok || len(tokens) != 1 || tokens[0] != "required" {
			t.Errorf("Partial = (%v, %v)", ok, tokens)
		}
	})
	t.Run("core-path errors do not degrade", func(t *testing.T) {
		err := &api.GraphQLError{Errors: []api.GraphQLErrorItem{
			{Type: "FORBIDDEN", Path: []interface{}{"repository", "pullRequest"}},
		}}
		if ok, _ := Partial(err); ok {
			t.Error("pullRequest-level FORBIDDEN must not degrade")
		}
	})
	t.Run("degradation is path-based regardless of error type", func(t *testing.T) {
		// §2.4: an error confined to an optional subtree keeps the verdict
		// usable whatever its type; only the location matters.
		err := &api.GraphQLError{Errors: []api.GraphQLErrorItem{
			{Type: "SOME_ERROR", Path: []interface{}{"repository", "pullRequest", "reviewThreads"}},
		}}
		ok, tokens := Partial(err)
		if !ok || len(tokens) != 1 || tokens[0] != "threads" {
			t.Errorf("Partial = (%v, %v), want (true, [threads])", ok, tokens)
		}
	})
}

func TestPartialSilentEnrichmentLoss(t *testing.T) {
	// compare/reviewRequests are pure enrichment: their loss degrades the
	// call (usable partial data) but emits no vocabulary token.
	err := &api.GraphQLError{Errors: []api.GraphQLErrorItem{
		{Type: "SOME_ERROR", Path: []interface{}{"repository", "pullRequest", "baseRef", "compare"}},
		{Type: "FORBIDDEN", Path: []interface{}{"repository", "pullRequest", "reviewRequests"}},
	}}
	ok, tokens := Partial(err)
	if !ok || len(tokens) != 0 {
		t.Errorf("Partial = (%v, %v), want (true, none)", ok, tokens)
	}
}
