package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/akira-toriyama/prq/internal/fetch"
	"github.com/akira-toriyama/prq/internal/gh"
	"github.com/akira-toriyama/prq/internal/synth"
	"github.com/akira-toriyama/prq/internal/version"
)

const usage = `prq — one-call PR state synthesis: why is this PR blocked?

usage:
  prq [<number> | <pr-url> | owner/repo#N | <branch>] [-R owner/repo]
  prq --mine [--repos owner/a,owner/b] [--limit N]

flags:
  -R, --repo owner/repo    target repository (default: derived from the cwd checkout)
      --mine               my open PRs, NDJSON one line per PR
      --repos a/x,b/y      comma-separated repo scope (implies --mine)
      --limit N            --mine discovery cap (default 30, max 100)
      --retry-budget dur   mergeable-UNKNOWN probe budget (default 8s; 0 disables)
      --no-retry           alias for --retry-budget 0
      --timeout dur        whole-invocation deadline (default 30s)
      --version            print version

output: JSON on stdout; errors as one JSON object on stderr.
exit: 0 clean/merged · 1 blocked/closed · 2 pending/queued/unknown ·
      3 auth or unverifiable verdict · 4 transient failure · 64 usage
`

// deps carries every ambient dependency so tests can fake the world.
type deps struct {
	client        func() (gh.Doer, error)
	currentRepo   func() (host, owner, name string, err error)
	currentBranch func() (string, error)
	sleep         fetch.SleepFunc
}

func run(args []string, stdout, stderr io.Writer, d deps) int {
	fs := flag.NewFlagSet("prq", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var repoFlag, reposFlag string
	var mine, noRetry, showVersion bool
	var limit int
	var retryBudget, timeout time.Duration
	fs.StringVar(&repoFlag, "R", "", "")
	fs.StringVar(&repoFlag, "repo", "", "")
	fs.BoolVar(&mine, "mine", false, "")
	fs.StringVar(&reposFlag, "repos", "", "")
	fs.IntVar(&limit, "limit", 30, "")
	fs.DurationVar(&retryBudget, "retry-budget", 8*time.Second, "")
	fs.BoolVar(&noRetry, "no-retry", false, "")
	fs.DurationVar(&timeout, "timeout", 30*time.Second, "")
	fs.BoolVar(&showVersion, "version", false, "")

	// stdlib flag stops at the first positional; re-parse the remainder so
	// `prq 13781 -R cli/cli` works too (flags and target in any order).
	var positionals []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprint(stdout, usage)
				return 0
			}
			return emitUsage(stderr, err.Error())
		}
		if fs.NArg() == 0 {
			break
		}
		positionals = append(positionals, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if showVersion {
		fmt.Fprintln(stdout, "prq "+version.Resolve().String())
		return 0
	}
	if noRetry {
		retryBudget = 0
	}
	if reposFlag != "" {
		mine = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if mine {
		if len(positionals) > 0 {
			return emitUsage(stderr, "--mine takes no positional argument")
		}
		if limit < 1 || limit > 100 {
			return emitUsage(stderr, "--limit must be 1..100")
		}
		return runMine(ctx, stdout, stderr, d, reposFlag, limit)
	}
	if len(positionals) > 1 {
		return emitUsage(stderr, "at most one target: a PR number, URL, owner/repo#N, or branch")
	}
	target := ""
	if len(positionals) == 1 {
		target = positionals[0]
	}
	return runSingle(ctx, stdout, stderr, d, repoFlag, target, retryBudget)
}

func runSingle(ctx context.Context, stdout, stderr io.Writer, d deps, repoFlag, target string, retryBudget time.Duration) int {
	owner, name, number, branch, ghes, err := resolveTarget(d, repoFlag, target)
	if err != nil {
		return emit(stderr, err)
	}

	client, err := d.client()
	if err != nil {
		return emit(stderr, err)
	}
	if number == 0 {
		number, err = fetch.ResolveCurrentPR(ctx, client, owner, name, branch)
		if err != nil {
			return emit(stderr, err)
		}
	}

	in, err := fetch.PR(ctx, client, d.sleep, owner, name, number, retryBudget)
	if err != nil {
		return emit(stderr, err)
	}
	if ghes {
		in.Degraded = append(in.Degraded, "ghes")
	}
	report := synth.Synthesize(in)
	if err := writeJSON(stdout, report); err != nil {
		return emit(stderr, err)
	}
	return exitForReport(report)
}

func runMine(ctx context.Context, stdout, stderr io.Writer, d deps, reposFlag string, limit int) int {
	var repos []string
	for _, r := range strings.Split(reposFlag, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !strings.Contains(r, "/") {
			return emitUsage(stderr, fmt.Sprintf("--repos entries must be owner/repo, got %q", r))
		}
		repos = append(repos, r)
	}
	if reposFlag != "" && len(repos) == 0 {
		// "--repos ," must not silently widen the scope to every repo.
		return emitUsage(stderr, "--repos contains no owner/repo entries")
	}

	client, err := d.client()
	if err != nil {
		return emit(stderr, err)
	}
	res, err := fetch.Mine(ctx, client, repos, limit)
	if err != nil {
		return emit(stderr, err)
	}

	worst := 0
	for _, in := range res.Inputs {
		row := synth.Summarize(in)
		if err := writeJSON(stdout, row); err != nil {
			return emit(stderr, err)
		}
		// Certifiability > blockedness > pendingness (§4.3).
		if c := exitForReport(row); rollupRank(c) > rollupRank(worst) {
			worst = c
		}
	}
	if res.Truncated {
		// Struct, not map: §1.1 makes field order normative and encoding/json
		// sorts map keys.
		line := struct {
			Truncated bool `json:"truncated"`
			Total     int  `json:"total"`
		}{true, res.Total}
		if err := writeJSON(stdout, line); err != nil {
			return emit(stderr, err)
		}
	}
	return worst
}

// exitForReport maps a report to its exit code, including the §4.3 rule that
// a degraded green/wait verdict is not certifiable (soft 3): a loop must
// never treat an unverifiable CLEAN as mergeable. Terminal PRs are exempt —
// MERGED is final whatever the token could not read.
func exitForReport(r synth.Report) int {
	var code int
	switch r.State {
	case synth.StateBlocked, synth.StateClosed:
		code = 1
	case synth.StatePending, synth.StateQueued, synth.StateUnknown:
		code = 2
	default: // CLEAN, UNSTABLE, MERGED
		code = 0
	}
	if len(r.Degraded) > 0 && code != 1 && !r.Final {
		return 3
	}
	return code
}

// rollupRank orders exit codes by --mine severity: 3 > 1 > 2 > 0.
func rollupRank(code int) int {
	switch code {
	case 3:
		return 3
	case 1:
		return 2
	case 2:
		return 1
	}
	return 0
}

// resolveTarget turns (repo flag, positional, ambient git state) into a
// concrete owner/name plus either a PR number or a branch to resolve.
func resolveTarget(d deps, repoFlag, target string) (owner, name string, number int, branch string, ghes bool, err error) {
	if strings.Contains(target, "github.com/") {
		owner, name, number, err = parsePRURL(target)
		return owner, name, number, "", false, err
	}
	if o, n, num, ok := parseRepoNumber(target); ok {
		return o, n, num, "", false, nil
	}

	if repoFlag != "" {
		var ok bool
		owner, name, ok = strings.Cut(repoFlag, "/")
		if !ok || owner == "" || name == "" {
			return "", "", 0, "", false, &usageError{msg: fmt.Sprintf("-R must be owner/repo, got %q", repoFlag)}
		}
	} else {
		var host string
		host, owner, name, err = d.currentRepo()
		if err != nil {
			return "", "", 0, "", false, &usageError{msg: fmt.Sprintf("no -R and no repo detected from cwd: %v", err)}
		}
		ghes = host != "" && host != "github.com"
	}

	switch {
	case target == "":
		branch, err = d.currentBranch()
		if err != nil {
			return "", "", 0, "", false, &usageError{msg: fmt.Sprintf("no target and no current branch: %v", err)}
		}
	case isDigits(target):
		number, err = strconv.Atoi(target)
		if err != nil || number <= 0 {
			return "", "", 0, "", false, &usageError{msg: fmt.Sprintf("invalid PR number %q", target)}
		}
	default:
		branch = target
	}
	return owner, name, number, branch, ghes, nil
}

// parsePRURL accepts https://github.com/OWNER/REPO/pull/123 with optional
// suffixes (/files, #discussion_r1, ?diff=split).
func parsePRURL(raw string) (owner, name string, number int, err error) {
	_, rest, ok := strings.Cut(raw, "github.com/")
	if !ok {
		return "", "", 0, &usageError{msg: fmt.Sprintf("cannot parse PR URL %q", raw)}
	}
	if i := strings.IndexAny(rest, "?#"); i >= 0 {
		rest = rest[:i]
	}
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 4 || parts[2] != "pull" {
		return "", "", 0, &usageError{msg: fmt.Sprintf("cannot parse PR URL %q (want …github.com/owner/repo/pull/N)", raw)}
	}
	number, aerr := strconv.Atoi(parts[3])
	if aerr != nil || number <= 0 {
		return "", "", 0, &usageError{msg: fmt.Sprintf("cannot parse PR number from URL %q", raw)}
	}
	return parts[0], parts[1], number, nil
}

// parseRepoNumber accepts the owner/repo#N selector.
func parseRepoNumber(s string) (owner, name string, number int, ok bool) {
	repo, num, found := strings.Cut(s, "#")
	if !found {
		return "", "", 0, false
	}
	owner, name, found = strings.Cut(repo, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", "", 0, false
	}
	return owner, name, n, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

type errBody struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter int    `json:"retry_after_s,omitempty"`
	Hint       string `json:"hint,omitempty"`
}

func writeError(w io.Writer, body errBody) {
	_ = writeJSON(w, map[string]errBody{"error": body})
}

func emitUsage(stderr io.Writer, msg string) int {
	writeError(stderr, errBody{Code: "usage", Message: msg, Hint: "see prq -h"})
	return 64
}

// emit maps an error to the exit-code taxonomy and writes the error object.
func emit(stderr io.Writer, err error) int {
	var ghErr *gh.Error
	var nfErr *gh.NotFoundError
	var uErr *usageError
	switch {
	case errors.As(err, &uErr):
		return emitUsage(stderr, uErr.msg)
	case errors.As(err, &nfErr):
		// GitHub masks private-vs-missing; retrying cannot help (§4.3).
		writeError(stderr, errBody{Code: "not_found_or_forbidden", Message: err.Error()})
		return 3
	case errors.As(err, &ghErr):
		writeError(stderr, errBody{Code: ghErr.Code, Message: ghErr.Message,
			RetryAfter: ghErr.RetryAfter, Hint: ghErr.Hint})
		return ghErr.Exit
	default:
		writeError(stderr, errBody{Code: "network", Message: err.Error(), Hint: "retry later"})
		return 4
	}
}

func writeJSON(w io.Writer, v interface{}) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
