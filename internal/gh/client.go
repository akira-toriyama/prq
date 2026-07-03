// Package gh constructs the GitHub GraphQL client (reusing gh CLI auth) and
// classifies its failures into prq's error taxonomy (docs/design.md §4.3/§4.4):
// exit 3 = auth/permission (retrying is pointless), exit 4 = transient or
// execution failure (retrying later is meaningful).
package gh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
)

// Doer is the one seam between prq and the GitHub GraphQL API. The real
// implementation wraps go-gh's *api.GraphQLClient; tests substitute a fake.
type Doer interface {
	Do(ctx context.Context, query string, variables map[string]interface{}, response interface{}) error
}

type client struct{ gql *api.GraphQLClient }

func (c client) Do(ctx context.Context, query string, variables map[string]interface{}, response interface{}) error {
	return c.gql.DoWithContext(ctx, query, variables, response)
}

// NewClient builds a GraphQL client from the ambient gh CLI auth
// (keyring/config/GH_TOKEN — the same resolution gh itself uses).
func NewClient() (Doer, error) {
	gql, err := api.NewGraphQLClient(api.ClientOptions{})
	if err != nil {
		// The dominant failure here is missing credentials.
		return nil, &Error{Code: "auth", Exit: 3, Message: err.Error(),
			Hint: "run `gh auth login` or set GH_TOKEN"}
	}
	return client{gql: gql}, nil
}

// Error is a classified failure. Code vocabulary (§4.4): auth | saml_blocked |
// not_found_or_forbidden | rate_limited | server_error | network | timeout |
// schema. Exit is 3 or 4 per §4.3.
type Error struct {
	Code       string
	Exit       int
	Message    string
	Hint       string
	RetryAfter int // seconds; 0 = unknown
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// NotFoundError: the repo/PR does not exist as far as this token can see.
// GitHub deliberately masks private-vs-missing, so this maps to exit 3.
type NotFoundError struct{ What string }

func (e *NotFoundError) Error() string { return "not found (or no access): " + e.What }

// Classify maps a transport error onto the taxonomy. what names the target
// for not-found messages (e.g. "cli/cli#13781").
func Classify(err error, what string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Code: "timeout", Exit: 4, Message: "invocation deadline exceeded",
			Hint: "raise --timeout"}
	}
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) {
		switch {
		case httpErr.StatusCode == 401:
			return &Error{Code: "auth", Exit: 3, Message: httpErr.Message,
				Hint: "run `gh auth login` or refresh GH_TOKEN"}
		case httpErr.StatusCode == 403 && strings.Contains(strings.ToLower(httpErr.Message), "saml"):
			return &Error{Code: "saml_blocked", Exit: 3, Message: httpErr.Message,
				Hint: "authorize the token for the org's SAML SSO"}
		case httpErr.StatusCode == 403 && strings.Contains(strings.ToLower(httpErr.Message), "rate limit"):
			return &Error{Code: "rate_limited", Exit: 4, Message: httpErr.Message,
				RetryAfter: retryAfter(httpErr), Hint: "retry after the delay"}
		case httpErr.StatusCode == 403:
			return &Error{Code: "auth", Exit: 3, Message: httpErr.Message}
		case httpErr.StatusCode == 404:
			return &NotFoundError{What: what}
		case httpErr.StatusCode >= 500:
			return &Error{Code: "server_error", Exit: 4, Message: httpErr.Message,
				Hint: "retry later"}
		}
		return &Error{Code: "server_error", Exit: 4, Message: httpErr.Message}
	}
	var gqlErr *api.GraphQLError
	if errors.As(err, &gqlErr) && len(gqlErr.Errors) > 0 {
		allNotFound := true
		for _, item := range gqlErr.Errors {
			switch item.Type {
			case "RATE_LIMITED":
				return &Error{Code: "rate_limited", Exit: 4, Message: item.Message,
					Hint: "retry after the delay"}
			case "FORBIDDEN":
				return &Error{Code: "not_found_or_forbidden", Exit: 3, Message: item.Message}
			case "NOT_FOUND":
			default:
				allNotFound = false
			}
		}
		if allNotFound {
			return &NotFoundError{What: what}
		}
		return &Error{Code: "schema", Exit: 4, Message: gqlErr.Error()}
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		code := "network"
		if netErr.Timeout() {
			code = "timeout"
		}
		return &Error{Code: code, Exit: 4, Message: err.Error(), Hint: "retry later"}
	}
	return &Error{Code: "network", Exit: 4, Message: err.Error(), Hint: "retry later"}
}

func retryAfter(e *api.HTTPError) int {
	if e.Headers == nil {
		return 0
	}
	n, _ := strconv.Atoi(e.Headers.Get("Retry-After"))
	return n
}

// optionalFields maps response sub-trees prq can answer without to their
// degraded-vocabulary token (design §1.1 row 15). An error confined to these
// degrades the output instead of failing the call.
var optionalFields = map[string]string{
	"reviewThreads":     "threads",
	"statusCheckRollup": "checks",
	"contexts":          "checks",
	"isRequired":        "required",
	"mergeQueueEntry":   "merge_state",
	"compare":           "behind",
	"baseRef":           "behind",
	"reviewRequests":    "review",
}

// Partial reports whether a GraphQL error only touches optional sub-fields,
// so the caller may keep the partial response (go-gh populates it before
// returning the error — source-verified) and mark the output degraded. The
// returned tokens use the design's degraded vocabulary.
func Partial(err error) (degraded bool, tokens []string) {
	var gqlErr *api.GraphQLError
	if !errors.As(err, &gqlErr) || len(gqlErr.Errors) == 0 {
		return false, nil
	}
	seen := map[string]bool{}
	for _, item := range gqlErr.Errors {
		token := ""
		for _, p := range item.Path {
			if s, ok := p.(string); ok {
				if tk, optional := optionalFields[s]; optional {
					token = tk
				}
			}
		}
		// isRequired errors surface as UNPROCESSABLE with the field in the
		// message on some shapes; catch those too.
		if token == "" && strings.Contains(item.Message, "pull request ID or pull request number") {
			token = "required"
		}
		if token == "" {
			return false, nil
		}
		if !seen[token] {
			seen[token] = true
			tokens = append(tokens, token)
		}
	}
	return true, tokens
}

// Wrap builds a generic classified error for non-transport failures.
func Wrap(code string, exit int, format string, args ...interface{}) *Error {
	return &Error{Code: code, Exit: exit, Message: fmt.Sprintf(format, args...)}
}
