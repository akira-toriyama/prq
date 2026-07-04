// Package synth turns the raw facts of a pull request into an actionable
// verdict: what, if anything, is preventing the merge.
//
// The package is pure — stdlib only, no I/O. fetch feeds it an Input; the
// Report it returns is the CLI's entire product. The decision tables live in
// docs/design.md §2; the string vocabulary in §1.3; keep them in sync.
package synth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// States (the prq verdict enum, §2.1). Exit mapping lives in cmd/prq.
const (
	StateClean    = "CLEAN"
	StateUnstable = "UNSTABLE"
	StatePending  = "PENDING"
	StateBlocked  = "BLOCKED"
	StateQueued   = "QUEUED"
	StateMerged   = "MERGED"
	StateClosed   = "CLOSED"
	StateUnknown  = "UNKNOWN"
)

// Display caps (§1.1/§1.2). Fingerprints hash the uncapped sets, so the fp is
// identical across single and --mine modes.
const (
	capBlockers     = 8
	capPending      = 8
	capNonBlocking  = 5
	capMineList     = 3
	collapseRunning = 3 // >3 required running checks collapse into one line
)

// Queue is the PR's merge-queue entry.
type Queue struct {
	Position int
	State    string // MergeQueueEntryState
	ETASec   int    // estimatedTimeToMerge seconds; -1 = null
}

// Context is one entry of the head commit's status-check rollup.
type Context struct {
	Kind       string // "check" (CheckRun) | "status" (StatusContext)
	Name       string
	Status     string // CheckStatusState (checks only)
	Conclusion string // CheckConclusionState (checks) or StatusState (statuses)
	Required   bool
	RunID      int64  // Actions workflow run id for the cifail handoff (0 = none)
	URL        string // detailsUrl / targetUrl
}

// Input is the normalized, transport-free view of one pull request.
type Input struct {
	Repo           string // owner/name
	Number         int
	PRState        string // OPEN | CLOSED | MERGED
	IsDraft        bool
	Mergeable      string // MERGEABLE | CONFLICTING | UNKNOWN | ""
	MergeState     string // MergeStateStatus | ""
	ReviewDecision string // "" = no review requirement
	CodeOwnerWait  bool   // a review request with asCodeOwner=true is pending
	BaseRef        string
	BehindBy       int // commits behind base; -1 = unobtainable
	CanUpdate      bool
	QueueEnabled   bool
	Queue          *Queue
	RollupPresent  bool
	RollupState    string
	Contexts       []Context
	// RequiredKnown is false when required-ness was not fetched (coarse
	// multi pass) or errored; synthesis then over-blocks rather than
	// false-greens (§2.4).
	RequiredKnown bool
	// MissingRequired lists BPR requiredStatusCheckContexts absent from the
	// rollup (the fork-PR never-started case).
	MissingRequired []string
	// ConvResolution is BPR requiresConversationResolution (false when BPR
	// is unreadable — the normal non-admin case).
	ConvResolution    bool
	UnresolvedThreads int
	ThreadsKnown      bool
	Degraded          []string // design §1.1 row 15 vocabulary
}

// Checks is the check-count gauge (§1.1 row 7, single-PR only).
type Checks struct {
	ReqOK   int `json:"req_ok,omitempty"`
	ReqFail int `json:"req_fail,omitempty"`
	ReqRun  int `json:"req_run,omitempty"`
	OptFail int `json:"opt_fail,omitempty"`
	OptRun  int `json:"opt_run,omitempty"`
}

// QueueOut is the wire form of the queue field (§1.1 row 14).
type QueueOut struct {
	Position int    `json:"position,omitempty"`
	State    string `json:"state,omitempty"`
	ETASec   int    `json:"eta_s,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// Report is the output document. Struct order IS the wire order (§1.1).
type Report struct {
	PR                string    `json:"pr"`
	State             string    `json:"state"`
	FP                string    `json:"fp"`
	Blockers          []string  `json:"blockers"`
	Pending           []string  `json:"pending,omitempty"`
	NonBlocking       []string  `json:"non_blocking,omitempty"`
	Checks            *Checks   `json:"checks,omitempty"`
	UnresolvedThreads int       `json:"unresolved_threads,omitempty"`
	BehindBy          int       `json:"behind_by,omitempty"`
	CI                string    `json:"ci,omitempty"`
	Review            string    `json:"review,omitempty"`
	Mergeable         string    `json:"mergeable,omitempty"`
	MergeState        string    `json:"merge_state,omitempty"`
	Queue             *QueueOut `json:"queue,omitempty"`
	Degraded          []string  `json:"degraded,omitempty"`
	Final             bool      `json:"final,omitempty"`
}

// entry is a blocker/pending line before precedence sorting.
type entry struct {
	prec    int    // topic precedence (§1.3)
	key     string // secondary sort key (check name, bytewise)
	text    string
	running bool // a required CheckRun still executing — collapsible (§1.3)
}

const (
	precClosed = iota
	precDraft
	precReview
	precCheck
	precConflict
	precBehind
	precThreads
	precQueue
	precBlocked
)

// Synthesize applies the §2 tables to one PR.
func Synthesize(in Input) Report {
	r := Report{
		PR:       fmt.Sprintf("%s#%d", in.Repo, in.Number),
		Blockers: []string{},
		Review:   in.ReviewDecision,
		Degraded: sortedCopy(in.Degraded),
	}
	if in.Mergeable != "MERGEABLE" {
		r.Mergeable = in.Mergeable
	}
	if in.UnresolvedThreads > 0 && in.ThreadsKnown {
		r.UnresolvedThreads = in.UnresolvedThreads
	}
	if in.BehindBy > 0 {
		r.BehindBy = in.BehindBy
	}
	if in.RollupPresent {
		r.CI = in.RollupState
	}

	// Terminal gate (§2.1 rules 1-2): stale merge facts on non-OPEN PRs are
	// ignored entirely — merged PRs report mergeable UNKNOWN forever and
	// closed PRs keep a stale BLOCKED merge state.
	switch in.PRState {
	case "MERGED":
		r.State = StateMerged
		r.Final = true
		r.Mergeable = ""
		r.BehindBy = 0
		in.MergeState = ""
		finish(&r, in, nil, nil)
		return r
	case "CLOSED":
		r.State = StateClosed
		r.Final = true
		r.Mergeable = ""
		r.BehindBy = 0
		in.MergeState = ""
		r.Blockers = []string{"closed: PR closed without merge"}
		finish(&r, in, []entry{{prec: precClosed, text: r.Blockers[0]}}, nil)
		return r
	}

	blockers, pending := decompose(&r, in)

	// §2.3 BLOCKED with nothing visible: ready-to-enqueue or the honest
	// residual blocker.
	if in.MergeState == "BLOCKED" && len(blockers) == 0 && len(pending) == 0 {
		reviewOK := in.ReviewDecision == "" || in.ReviewDecision == "APPROVED"
		if in.QueueEnabled && reviewOK && in.Queue == nil {
			r.Queue = &QueueOut{Required: true}
			r.NonBlocking = append(r.NonBlocking, "queue: ready to enqueue (merge queue enabled)")
		} else {
			msg := "blocked: cause not visible to this token (hidden protection rule, required deployment, or push restriction"
			if in.UnresolvedThreads > 0 {
				msg += fmt.Sprintf("; %d unresolved threads", in.UnresolvedThreads)
			}
			msg += ")"
			blockers = append(blockers, entry{prec: precBlocked, text: msg})
		}
	}

	// State derivation (§2.1 rules 3-9, first match wins). The pending entry
	// the UNKNOWN condition itself generates does not count as rule-6
	// pendingness — otherwise rule 7 would be unreachable.
	switch {
	case in.Queue != nil && in.Queue.State == "UNMERGEABLE":
		r.State = StateBlocked
	case in.Queue != nil:
		r.State = StateQueued
	case len(blockers) > 0:
		r.State = StateBlocked
	case in.Mergeable == "UNKNOWN" && onlyComputing(pending):
		r.State = StateUnknown
	case len(pending) > 0:
		r.State = StatePending
	case in.Mergeable == "UNKNOWN":
		r.State = StateUnknown
	case in.MergeState == "UNSTABLE":
		r.State = StateUnstable
	default:
		r.State = StateClean
	}

	if !in.RollupPresent && r.State == StateBlocked {
		r.CI = "NONE"
	}
	finish(&r, in, blockers, pending)
	return r
}

// Summarize projects Synthesize onto the compact --mine row (§1.2): same
// schema minus non_blocking, checks, and queue ETA; tighter list caps. The
// fp computation is identical to single-PR mode (hashed pre-cap) — though
// the two modes can still hash different values when they saw different
// facts (the coarse pass has no compare, so its degraded set differs).
func Summarize(in Input) Report {
	r := Synthesize(in)
	r.NonBlocking = nil
	r.Checks = nil
	if r.Queue != nil {
		q := *r.Queue
		q.ETASec = 0
		r.Queue = &q
	}
	r.Blockers = recap(r.Blockers, capMineList)
	r.Pending = recap(r.Pending, capMineList)
	return r
}

// decompose runs the §2.2 additive rows, filling non_blocking/checks/queue on
// r and returning the blocker/pending candidate entries.
func decompose(r *Report, in Input) (blockers, pending []entry) {
	prNum := in.Number

	if in.IsDraft {
		blockers = append(blockers, entry{prec: precDraft,
			text: fmt.Sprintf("draft: marked as draft -> gh pr ready %d -R %s", prNum, in.Repo)})
	}

	switch in.ReviewDecision {
	case "CHANGES_REQUESTED":
		blockers = append(blockers, entry{prec: precReview, text: "review: CHANGES_REQUESTED"})
	case "REVIEW_REQUIRED":
		text := "review: REVIEW_REQUIRED"
		if in.CodeOwnerWait {
			text += " (codeowners)"
		}
		blockers = append(blockers, entry{prec: precReview, text: text})
	}

	cb, cp, counts := analyzeContexts(r, in)
	blockers = append(blockers, cb...)
	pending = append(pending, cp...)
	if counts != (Checks{}) && in.RequiredKnown {
		c := counts
		r.Checks = &c
	}

	// BPR enrichment: required contexts that never reported — the fork-PR
	// never-started case (§2.2). Only meaningful while GitHub says BLOCKED
	// (or said nothing at all — the degraded merge_state case synthesizes
	// from components, §2.4); firing it under a greener verdict GitHub
	// already computed would override it. Dedup against rollup names.
	if in.MergeState == "BLOCKED" || in.MergeState == "" {
		seen := map[string]bool{}
		for _, c := range in.Contexts {
			seen[c.Name] = true
		}
		for _, name := range in.MissingRequired {
			if !seen[name] {
				blockers = append(blockers, entry{prec: precCheck, key: name,
					text: fmt.Sprintf("check: '%s' MISSING (required, never reported)", name)})
			}
		}
	}

	if in.Mergeable == "CONFLICTING" || in.MergeState == "DIRTY" {
		blockers = append(blockers, entry{prec: precConflict,
			text: "conflict: merge conflicts with " + base(in)})
	}

	// behind: only when GitHub says so, and never while queued (the queue
	// rebases). behind_by>0 with another merge state stays a field only.
	if in.MergeState == "BEHIND" && in.Queue == nil {
		switch {
		case in.BehindBy > 0:
			text := fmt.Sprintf("behind: %d commits behind %s", in.BehindBy, base(in))
			if in.CanUpdate {
				text += fmt.Sprintf(" -> gh pr update-branch %d -R %s", prNum, in.Repo)
			}
			blockers = append(blockers, entry{prec: precBehind, text: text})
		default:
			blockers = append(blockers, entry{prec: precBehind,
				text: "behind: out of date with " + base(in)})
			r.Degraded = appendToken(r.Degraded, "behind")
		}
	}

	if in.UnresolvedThreads > 0 && in.ConvResolution {
		blockers = append(blockers, entry{prec: precThreads,
			text: fmt.Sprintf("threads: %d unresolved -> resolve via gh pr-review", in.UnresolvedThreads)})
	}

	if in.Queue != nil {
		if in.Queue.State == "UNMERGEABLE" {
			blockers = append(blockers, entry{prec: precQueue,
				text: "queue: entry UNMERGEABLE (will be ejected)"})
		} else {
			text := fmt.Sprintf("queue: position %d %s", in.Queue.Position, in.Queue.State)
			if in.Queue.ETASec > 0 {
				text += fmt.Sprintf(" (~%dm)", (in.Queue.ETASec+59)/60)
			}
			pending = append(pending, entry{prec: precQueue, text: text})
		}
		r.Queue = &QueueOut{Position: in.Queue.Position, State: in.Queue.State}
		if in.Queue.ETASec > 0 {
			r.Queue.ETASec = in.Queue.ETASec
		}
	}

	if in.PRState == "OPEN" && in.Mergeable == "UNKNOWN" {
		pending = append(pending, entry{prec: precBlocked, text: computingText})
	}

	return blockers, pending
}

// analyzeContexts classifies the rollup (§2.2 check rows). mergeable-state
// PRs (CLEAN/HAS_HOOKS/UNSTABLE) route every non-required failure to
// non_blocking; elsewhere unknown required-ness over-blocks (§2.4).
func analyzeContexts(r *Report, in Input) (blockers, pending []entry, counts Checks) {
	mergeableState := in.MergeState == "CLEAN" || in.MergeState == "HAS_HOOKS" || in.MergeState == "UNSTABLE"
	if in.MergeState == "HAS_HOOKS" {
		r.NonBlocking = append(r.NonBlocking, "hooks: pre-receive hooks run on merge")
	}

	var optFailLines []string
	optPendingCount := 0

	// Dedup by (kind, name), keeping the WORST occurrence: the rollup can
	// carry the same check name from several suites (re-runs, matrices) in
	// arbitrary order, and a green duplicate must not shadow a red one.
	var order []string
	best := map[string]Context{}
	rank := func(c Context) int {
		failing, waiting := classify(c)
		switch {
		case failing:
			return 2
		case waiting:
			return 1
		}
		return 0
	}
	for _, c := range in.Contexts {
		key := c.Kind + "\x00" + c.Name
		prev, seen := best[key]
		if !seen {
			order = append(order, key)
			best[key] = c
		} else if rank(c) > rank(prev) {
			best[key] = c
		}
	}

	for _, key := range order {
		c := best[key]
		failing, waiting := classify(c)
		required := c.Required && in.RequiredKnown
		unknownReq := !in.RequiredKnown
		suffix := ""
		if unknownReq && !mergeableState {
			// Conservative fallback: treat as required, say so.
			required = true
			suffix = " (required?)"
		}

		switch {
		case !failing && !waiting:
			if required {
				counts.ReqOK++
			}
		case required && failing:
			counts.ReqFail++
			blockers = append(blockers, entry{prec: precCheck, key: c.Name,
				text: failedLine(in, c, suffix)})
		case required && waiting:
			counts.ReqRun++
			pending = append(pending, entry{prec: precCheck, key: c.Name,
				text:    fmt.Sprintf("check: '%s' %s%s", c.Name, waitWord(c), suffix),
				running: c.Kind == "check"})
		case failing:
			counts.OptFail++
			optFailLines = append(optFailLines, failedLine(in, c, " (optional)"))
		default:
			counts.OptRun++
			optPendingCount++
		}
	}

	sort.Strings(optFailLines)
	if len(optFailLines) > capNonBlocking {
		optFailLines = append(optFailLines[:capNonBlocking],
			fmt.Sprintf("+%d more checks not shown", len(optFailLines)-capNonBlocking))
	}
	r.NonBlocking = append(r.NonBlocking, optFailLines...)
	if optPendingCount > 0 {
		r.NonBlocking = append(r.NonBlocking, fmt.Sprintf("%d optional checks pending", optPendingCount))
	}
	return blockers, pending, counts
}

// classify buckets one rollup context: exactly one of failing/waiting, or
// neither (SUCCESS, NEUTRAL, SKIPPED — all satisfy required checks).
func classify(c Context) (failing, waiting bool) {
	if c.Kind == "check" {
		if c.Status != "COMPLETED" {
			return false, true // REQUESTED | QUEUED | IN_PROGRESS | WAITING | PENDING
		}
		switch c.Conclusion {
		case "FAILURE", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
			return true, false
		}
		return false, false
	}
	switch c.Conclusion { // StatusState
	case "FAILURE", "ERROR":
		return true, false
	case "PENDING", "EXPECTED":
		return false, true
	}
	return false, false
}

// failedLine renders a failing context per the §1.3 vocabulary. ACTION_REQUIRED
// and STALE carry their own next actions — cifail cannot fix either.
func failedLine(in Input, c Context, suffix string) string {
	switch c.Conclusion {
	case "ACTION_REQUIRED":
		return fmt.Sprintf("check: '%s' ACTION_REQUIRED%s (needs manual approval)", c.Name, suffix)
	case "STALE":
		return fmt.Sprintf("check: '%s' STALE%s -> re-run after push", c.Name, suffix)
	}
	verb := "FAILING"
	if c.Kind == "check" && c.Conclusion != "FAILURE" {
		verb = c.Conclusion // TIMED_OUT | CANCELLED | STARTUP_FAILURE
	}
	head := fmt.Sprintf("check: '%s' %s%s", c.Name, verb, suffix)
	switch {
	case c.Kind == "check" && c.RunID > 0:
		return fmt.Sprintf("%s -> cifail --run %d -R %s", head, c.RunID, in.Repo)
	case c.URL != "":
		return head + " -> " + c.URL
	}
	return head
}

func waitWord(c Context) string {
	if c.Kind == "check" {
		return "RUNNING"
	}
	return c.Conclusion // PENDING | EXPECTED
}

const computingText = "mergeable: still computing (retry budget exhausted)"

// collapseRunningChecks folds >3 running required checks into one aggregate
// display line (§1.3); other pending entries pass through untouched.
func collapseRunningChecks(pending []entry) []entry {
	running := 0
	for _, e := range pending {
		if e.running {
			running++
		}
	}
	if running <= collapseRunning {
		return pending
	}
	out := make([]entry, 0, len(pending)-running+1)
	inserted := false
	for _, e := range pending {
		if !e.running {
			out = append(out, e)
			continue
		}
		if !inserted {
			inserted = true
			out = append(out, entry{prec: precCheck,
				text: fmt.Sprintf("checks: %d required running", running)})
		}
	}
	return out
}

// onlyComputing reports whether every pending entry is the one the UNKNOWN
// retry exhaustion generates.
func onlyComputing(pending []entry) bool {
	for _, e := range pending {
		if e.text != computingText {
			return false
		}
	}
	return true
}

// finish sorts, caps, fingerprints, and freezes the report. The fingerprint
// hashes the UNCOLLAPSED pending set (real check names), so progress among
// many running checks is visible even while the display shows an aggregate.
func finish(r *Report, in Input, blockers, pending []entry) {
	r.Blockers = flatten(blockers, capBlockers)
	if r.Blockers == nil {
		r.Blockers = []string{}
	}
	r.Pending = flatten(collapseRunningChecks(pending), capPending)

	if r.MergeState = in.MergeState; r.MergeState == r.State {
		r.MergeState = "" // shown only when the verdict diverges (§1.1 row 13)
	}
	if !in.ThreadsKnown {
		r.Degraded = appendToken(r.Degraded, "threads")
	}
	if !in.RequiredKnown && len(in.Contexts) > 0 {
		r.Degraded = appendToken(r.Degraded, "required")
	}
	sort.Strings(r.Degraded)
	r.FP = fingerprint(r, blockers, pending, in)
}

// flatten sorts entries by (precedence, key, text) and caps the list.
// Empty in → nil out, so omitempty fields disappear from the wire.
func flatten(entries []entry, limit int) []string {
	if len(entries) == 0 {
		return nil
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].prec != entries[j].prec {
			return entries[i].prec < entries[j].prec
		}
		if entries[i].key != entries[j].key {
			return entries[i].key < entries[j].key
		}
		return entries[i].text < entries[j].text
	})
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.text)
	}
	return recap(out, limit)
}

// recap caps a list to limit entries, folding the tail (including a previous
// "+N more" line) into one summary line.
func recap(list []string, limit int) []string {
	if len(list) <= limit {
		return list
	}
	hidden := len(list) - limit
	last := list[len(list)-1]
	if strings.HasPrefix(last, "+") && strings.HasSuffix(last, " more") {
		var n int
		fmt.Sscanf(last, "+%d more", &n)
		hidden = hidden - 1 + n
	}
	out := append([]string{}, list[:limit]...)
	return append(out, fmt.Sprintf("+%d more", hidden))
}

// fingerprint implements §1.4: a v1-prefixed canonical string over the
// pre-cap blocker set, pending check names, and the loop-relevant scalars.
// headRefOid, queue churn, behind counts, and non_blocking are all excluded.
func fingerprint(r *Report, blockers, pending []entry, in Input) string {
	normB := make([]string, 0, len(blockers))
	for _, e := range blockers {
		if e.prec == precBehind {
			normB = append(normB, "behind")
			continue
		}
		normB = append(normB, e.text)
	}
	sort.Strings(normB)

	var pendingChecks []string
	for _, e := range pending {
		if e.prec != precCheck {
			continue
		}
		if e.key != "" {
			pendingChecks = append(pendingChecks, e.key)
		} else {
			pendingChecks = append(pendingChecks, e.text)
		}
	}
	sort.Strings(pendingChecks)

	review := r.Review
	if review == "" {
		review = "-"
	}
	mergeable := in.Mergeable
	if mergeable == "" || in.PRState != "OPEN" {
		mergeable = "MERGEABLE"
	}
	// UNSTABLE differs from CLEAN only by optional-check noise, which must
	// not wake a babysit loop (§1.4 invariants); hash them as one state.
	state := r.State
	if state == StateUnstable {
		state = StateClean
	}

	canonical := "v1|" + r.PR +
		"|" + state +
		"|" + strings.Join(normB, ";") +
		"|" + strings.Join(pendingChecks, ";") +
		"|r=" + review +
		"|m=" + mergeable +
		"|t=" + strconv.Itoa(in.UnresolvedThreads) +
		"|d=" + strings.Join(r.Degraded, ",")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:8])
}

func base(in Input) string {
	if in.BaseRef == "" {
		return "base"
	}
	return in.BaseRef
}

func appendToken(list []string, token string) []string {
	for _, t := range list {
		if t == token {
			return list
		}
	}
	return append(list, token)
}

func sortedCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}
