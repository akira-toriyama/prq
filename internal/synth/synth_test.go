package synth

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// open returns a minimal open-PR input; tests mutate what they care about.
func open() Input {
	return Input{
		Repo:          "o/r",
		Number:        1,
		PRState:       "OPEN",
		Mergeable:     "MERGEABLE",
		MergeState:    "CLEAN",
		BaseRef:       "main",
		RequiredKnown: true,
		ThreadsKnown:  true,
	}
}

func reqFail(name string, runID int64) Context {
	return Context{Kind: "check", Name: name, Status: "COMPLETED", Conclusion: "FAILURE", Required: true, RunID: runID}
}

func TestDecisionTable(t *testing.T) {
	cases := []struct {
		name     string
		in       func() Input
		state    string
		blockers []string
		pending  []string
		nonBlock []string
	}{
		{name: "clean", in: open, state: StateClean},
		{
			name: "merged is terminal: stale UNKNOWN and draft ignored",
			in: func() Input {
				i := open()
				i.PRState = "MERGED"
				i.IsDraft = true
				i.Mergeable = "UNKNOWN"
				i.MergeState = "UNKNOWN"
				return i
			},
			state: StateMerged,
		},
		{
			name: "closed is terminal blocked, stale BLOCKED ignored",
			in: func() Input {
				i := open()
				i.PRState = "CLOSED"
				i.MergeState = "BLOCKED"
				return i
			},
			state:    StateClosed,
			blockers: []string{"closed: PR closed without merge"},
		},
		{
			name: "has_hooks is clean with a hooks note",
			in: func() Input {
				i := open()
				i.MergeState = "HAS_HOOKS"
				return i
			},
			state:    StateClean,
			nonBlock: []string{"hooks: pre-receive hooks run on merge"},
		},
		{
			name: "unstable: optional failures stay non-blocking",
			in: func() Input {
				i := open()
				i.MergeState = "UNSTABLE"
				i.Contexts = []Context{
					{Kind: "check", Name: "bench", Status: "COMPLETED", Conclusion: "FAILURE", RunID: 7},
					{Kind: "status", Name: "ci/opt", Conclusion: "PENDING"},
				}
				return i
			},
			state:    StateUnstable,
			nonBlock: []string{"check: 'bench' FAILING (optional) -> cifail --run 7 -R o/r", "1 optional checks pending"},
		},
		{
			name: "draft blocks even when GitHub says CLEAN",
			in: func() Input {
				i := open()
				i.IsDraft = true
				return i
			},
			state:    StateBlocked,
			blockers: []string{"draft: marked as draft -> gh pr ready 1 -R o/r"},
		},
		{
			name: "blocked draft with codeowner review, precedence order",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.IsDraft = true
				i.ReviewDecision = "REVIEW_REQUIRED"
				i.CodeOwnerWait = true
				return i
			},
			state: StateBlocked,
			blockers: []string{
				"draft: marked as draft -> gh pr ready 1 -R o/r",
				"review: REVIEW_REQUIRED (codeowners)",
			},
		},
		{
			name: "failing required check emits the cifail handoff",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.Contexts = []Context{reqFail("lint", 987654)}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"check: 'lint' FAILING -> cifail --run 987654 -R o/r"},
		},
		{
			name: "non-FAILURE conclusions are spelled out",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				c := reqFail("slow", 9)
				c.Conclusion = "TIMED_OUT"
				i.Contexts = []Context{c}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"check: 'slow' TIMED_OUT -> cifail --run 9 -R o/r"},
		},
		{
			name: "ACTION_REQUIRED is not a cifail case",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				c := reqFail("deploy", 9)
				c.Conclusion = "ACTION_REQUIRED"
				i.Contexts = []Context{c}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"check: 'deploy' ACTION_REQUIRED (needs manual approval)"},
		},
		{
			name: "STALE points at re-run, not cifail",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				c := reqFail("ci", 9)
				c.Conclusion = "STALE"
				i.Contexts = []Context{c}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"check: 'ci' STALE -> re-run after push"},
		},
		{
			name: "failing required status context falls back to its URL",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.Contexts = []Context{{Kind: "status", Name: "ci/jenkins", Conclusion: "ERROR", Required: true, URL: "https://ci.example.com/42"}}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"check: 'ci/jenkins' FAILING -> https://ci.example.com/42"},
		},
		{
			name: "running and expected required contexts are pending",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.Contexts = []Context{
					{Kind: "check", Name: "build", Status: "IN_PROGRESS", Required: true},
					{Kind: "status", Name: "ci/x", Conclusion: "EXPECTED", Required: true},
				}
				return i
			},
			state:   StatePending,
			pending: []string{"check: 'build' RUNNING", "check: 'ci/x' EXPECTED"},
		},
		{
			name: "more than three running required checks collapse",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				for _, n := range []string{"a", "b", "c", "d"} {
					i.Contexts = append(i.Contexts, Context{Kind: "check", Name: n, Status: "QUEUED", Required: true})
				}
				return i
			},
			state:   StatePending,
			pending: []string{"checks: 4 required running"},
		},
		{
			name: "residual blocked names the honest cause with thread hint",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.UnresolvedThreads = 2
				return i
			},
			state:    StateBlocked,
			blockers: []string{"blocked: cause not visible to this token (hidden protection rule, required deployment, or push restriction; 2 unresolved threads)"},
		},
		{
			name: "ready to enqueue: blocked + all green + queue enabled",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.ReviewDecision = "APPROVED"
				i.QueueEnabled = true
				return i
			},
			state:    StateClean,
			nonBlock: []string{"queue: ready to enqueue (merge queue enabled)"},
		},
		{
			name: "behind with count and update action",
			in: func() Input {
				i := open()
				i.MergeState = "BEHIND"
				i.BehindBy = 7
				i.CanUpdate = true
				return i
			},
			state:    StateBlocked,
			blockers: []string{"behind: 7 commits behind main -> gh pr update-branch 1 -R o/r"},
		},
		{
			name: "behind without a count degrades honestly",
			in: func() Input {
				i := open()
				i.MergeState = "BEHIND"
				i.BehindBy = -1
				return i
			},
			state:    StateBlocked,
			blockers: []string{"behind: out of date with main"},
		},
		{
			name: "dirty and conflicting dedupe into one blocker",
			in: func() Input {
				i := open()
				i.MergeState = "DIRTY"
				i.Mergeable = "CONFLICTING"
				return i
			},
			state:    StateBlocked,
			blockers: []string{"conflict: merge conflicts with main"},
		},
		{
			name: "unknown after retries is pending but hard blockers win",
			in: func() Input {
				i := open()
				i.MergeState = "UNKNOWN"
				i.Mergeable = "UNKNOWN"
				i.IsDraft = true
				return i
			},
			state:    StateBlocked,
			blockers: []string{"draft: marked as draft -> gh pr ready 1 -R o/r"},
			pending:  []string{"mergeable: still computing (retry budget exhausted)"},
		},
		{
			name: "unknown with nothing else",
			in: func() Input {
				i := open()
				i.MergeState = "UNKNOWN"
				i.Mergeable = "UNKNOWN"
				return i
			},
			state:   StateUnknown,
			pending: []string{"mergeable: still computing (retry budget exhausted)"},
		},
		{
			name: "queue progressing is QUEUED and suppresses behind",
			in: func() Input {
				i := open()
				i.MergeState = "BEHIND"
				i.BehindBy = 3
				i.Queue = &Queue{Position: 2, State: "AWAITING_CHECKS", ETASec: 90}
				return i
			},
			state:   StateQueued,
			pending: []string{"queue: position 2 AWAITING_CHECKS (~2m)"},
		},
		{
			name: "queue unmergeable blocks",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.Queue = &Queue{Position: 1, State: "UNMERGEABLE", ETASec: -1}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"queue: entry UNMERGEABLE (will be ejected)"},
		},
		{
			name: "threads block only under a conversation-resolution rule",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.UnresolvedThreads = 4
				i.ConvResolution = true
				return i
			},
			state:    StateBlocked,
			blockers: []string{"threads: 4 unresolved -> resolve via gh pr-review"},
		},
		{
			name: "missing required contexts fire only under BLOCKED",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.MissingRequired = []string{"present", "absent"}
				i.Contexts = []Context{{Kind: "status", Name: "present", Conclusion: "SUCCESS", Required: true}}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"check: 'absent' MISSING (required, never reported)"},
		},
		{
			name: "missing required does not override a CLEAN verdict",
			in: func() Input {
				i := open()
				i.MissingRequired = []string{"stale-config"}
				return i
			},
			state: StateClean,
		},
		{
			name: "coarse pass over-blocks with the required? suffix",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.RequiredKnown = false
				i.Contexts = []Context{
					{Kind: "check", Name: "a", Status: "COMPLETED", Conclusion: "FAILURE", RunID: 5},
					{Kind: "check", Name: "b", Status: "IN_PROGRESS"},
				}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"check: 'a' FAILING (required?) -> cifail --run 5 -R o/r"},
			pending:  []string{"check: 'b' RUNNING (required?)"},
		},
		{
			name: "neutral and skipped satisfy required checks",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.ReviewDecision = "REVIEW_REQUIRED"
				i.Contexts = []Context{
					{Kind: "check", Name: "n", Status: "COMPLETED", Conclusion: "NEUTRAL", Required: true},
					{Kind: "check", Name: "s", Status: "COMPLETED", Conclusion: "SKIPPED", Required: true},
				}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"review: REVIEW_REQUIRED"},
		},
		{
			name: "duplicate context names count once",
			in: func() Input {
				i := open()
				i.MergeState = "BLOCKED"
				i.Contexts = []Context{reqFail("lint", 1), reqFail("lint", 2)}
				return i
			},
			state:    StateBlocked,
			blockers: []string{"check: 'lint' FAILING -> cifail --run 1 -R o/r"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Synthesize(tc.in())
			if r.State != tc.state {
				t.Fatalf("state = %q, want %q (report %+v)", r.State, tc.state, r)
			}
			wantBlockers := tc.blockers
			if wantBlockers == nil {
				wantBlockers = []string{}
			}
			if !reflect.DeepEqual(r.Blockers, wantBlockers) {
				t.Errorf("blockers = %q, want %q", r.Blockers, wantBlockers)
			}
			if !reflect.DeepEqual(r.Pending, tc.pending) {
				t.Errorf("pending = %q, want %q", r.Pending, tc.pending)
			}
			if !reflect.DeepEqual(r.NonBlocking, tc.nonBlock) {
				t.Errorf("non_blocking = %q, want %q", r.NonBlocking, tc.nonBlock)
			}
		})
	}
}

func TestFieldPresence(t *testing.T) {
	t.Run("merge_state omitted when equal to state", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.IsDraft = true
		if r := Synthesize(i); r.MergeState != "" {
			t.Errorf("merge_state = %q, want omitted (equals state)", r.MergeState)
		}
	})
	t.Run("merge_state shown when the verdict diverges", func(t *testing.T) {
		i := open()
		i.MergeState = "HAS_HOOKS"
		if r := Synthesize(i); r.MergeState != "HAS_HOOKS" {
			t.Errorf("merge_state = %q, want HAS_HOOKS", r.MergeState)
		}
	})
	t.Run("mergeable omitted when MERGEABLE", func(t *testing.T) {
		if r := Synthesize(open()); r.Mergeable != "" {
			t.Errorf("mergeable = %q, want omitted", r.Mergeable)
		}
	})
	t.Run("ci NONE only when blocked without a rollup", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.IsDraft = true
		if r := Synthesize(i); r.CI != "NONE" {
			t.Errorf("ci = %q, want NONE", r.CI)
		}
		if r := Synthesize(open()); r.CI != "" {
			t.Errorf("ci = %q, want omitted on clean checkless PR", r.CI)
		}
	})
	t.Run("checks gauge counts", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.Contexts = []Context{
			{Kind: "check", Name: "ok", Status: "COMPLETED", Conclusion: "SUCCESS", Required: true},
			reqFail("bad", 1),
			{Kind: "check", Name: "run", Status: "IN_PROGRESS", Required: true},
			{Kind: "check", Name: "optbad", Status: "COMPLETED", Conclusion: "FAILURE"},
			{Kind: "check", Name: "optrun", Status: "QUEUED"},
		}
		r := Synthesize(i)
		want := &Checks{ReqOK: 1, ReqFail: 1, ReqRun: 1, OptFail: 1, OptRun: 1}
		if !reflect.DeepEqual(r.Checks, want) {
			t.Errorf("checks = %+v, want %+v", r.Checks, want)
		}
	})
}

func TestCaps(t *testing.T) {
	i := open()
	i.MergeState = "BLOCKED"
	for n := 0; n < 12; n++ {
		i.Contexts = append(i.Contexts, reqFail("c-"+strings.Repeat("x", n+1), int64(n+1)))
	}
	r := Synthesize(i)
	if len(r.Blockers) != capBlockers+1 {
		t.Fatalf("blockers = %d entries, want %d + summary", len(r.Blockers), capBlockers)
	}
	if got := r.Blockers[capBlockers]; got != "+4 more" {
		t.Errorf("summary = %q, want +4 more", got)
	}

	s := Summarize(i)
	if len(s.Blockers) != capMineList+1 {
		t.Fatalf("summary blockers = %d entries, want %d + summary", len(s.Blockers), capMineList)
	}
	if got := s.Blockers[capMineList]; got != "+9 more" {
		t.Errorf("mine summary = %q, want +9 more (12 real - 3 shown)", got)
	}
}

func TestFingerprint(t *testing.T) {
	base := Synthesize(open())

	same := func(t *testing.T, mut func(*Input), why string) {
		t.Helper()
		i := open()
		mut(&i)
		if got := Synthesize(i); got.FP != base.FP {
			t.Errorf("fp changed: %s", why)
		}
	}
	diff := func(t *testing.T, mut func(*Input), why string) {
		t.Helper()
		i := open()
		mut(&i)
		if got := Synthesize(i); got.FP == base.FP {
			t.Errorf("fp did not change: %s", why)
		}
	}

	t.Run("stable across identical input", func(t *testing.T) {
		same(t, func(*Input) {}, "identical input")
	})
	t.Run("push with identical verdict does not wake the loop", func(t *testing.T) {
		// headRefOid is not part of Input-to-fp at all; nothing to vary — the
		// canonical string simply never includes it (D7). Covered by design.
	})
	t.Run("optional check churn does not wake the loop", func(t *testing.T) {
		same(t, func(i *Input) {
			i.MergeState = "UNSTABLE"
			i.Contexts = []Context{{Kind: "check", Name: "bench", Status: "COMPLETED", Conclusion: "FAILURE"}}
		}, "optional failure flipped CLEAN to UNSTABLE")
	})
	t.Run("blocker changes wake the loop", func(t *testing.T) {
		diff(t, func(i *Input) { i.IsDraft = true }, "draft blocker appeared")
	})
	t.Run("behind count churn does not wake the loop", func(t *testing.T) {
		i1 := open()
		i1.MergeState = "BEHIND"
		i1.BehindBy = 3
		i2 := open()
		i2.MergeState = "BEHIND"
		i2.BehindBy = 7
		if Synthesize(i1).FP != Synthesize(i2).FP {
			t.Error("fp changed when only the behind count moved")
		}
	})
	t.Run("queue position churn does not wake the loop", func(t *testing.T) {
		i1 := open()
		i1.Queue = &Queue{Position: 5, State: "QUEUED", ETASec: 600}
		i2 := open()
		i2.Queue = &Queue{Position: 1, State: "QUEUED", ETASec: 30}
		if Synthesize(i1).FP != Synthesize(i2).FP {
			t.Error("fp changed on queue position/ETA drift")
		}
	})
	t.Run("run id changes wake the loop", func(t *testing.T) {
		i1 := open()
		i1.MergeState = "BLOCKED"
		i1.Contexts = []Context{reqFail("lint", 1)}
		i2 := open()
		i2.MergeState = "BLOCKED"
		i2.Contexts = []Context{reqFail("lint", 2)}
		if Synthesize(i1).FP == Synthesize(i2).FP {
			t.Error("fp identical across a re-run of the failing check")
		}
	})
	t.Run("context order permutation is invariant", func(t *testing.T) {
		i1 := open()
		i1.MergeState = "BLOCKED"
		i1.Contexts = []Context{reqFail("a", 1), reqFail("b", 2)}
		i2 := open()
		i2.MergeState = "BLOCKED"
		i2.Contexts = []Context{reqFail("b", 2), reqFail("a", 1)}
		if Synthesize(i1).FP != Synthesize(i2).FP {
			t.Error("fp depends on context order")
		}
	})
	t.Run("thread count changes wake the loop", func(t *testing.T) {
		diff(t, func(i *Input) { i.UnresolvedThreads = 1 }, "unresolved thread appeared")
	})
	t.Run("degradation changes wake the loop", func(t *testing.T) {
		diff(t, func(i *Input) { i.Degraded = []string{"threads"} }, "degraded set changed")
	})
	t.Run("summarize keeps the single-mode fp", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.IsDraft = true
		if Synthesize(i).FP != Summarize(i).FP {
			t.Error("fp diverges between modes")
		}
	})
}

func TestReviewFixes(t *testing.T) {
	t.Run("worst occurrence wins the same-name dedup", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.Contexts = []Context{
			{Kind: "check", Name: "lint", Status: "COMPLETED", Conclusion: "SUCCESS", Required: true},
			reqFail("lint", 7), // a green duplicate must not shadow this
		}
		r := Synthesize(i)
		want := []string{"check: 'lint' FAILING -> cifail --run 7 -R o/r"}
		if !reflect.DeepEqual(r.Blockers, want) {
			t.Errorf("blockers = %q, want %q", r.Blockers, want)
		}
		if r.Checks == nil || r.Checks.ReqFail != 1 || r.Checks.ReqOK != 0 {
			t.Errorf("checks = %+v, want req_fail:1 only", r.Checks)
		}
	})
	t.Run("optional ACTION_REQUIRED and STALE keep the optional marker", func(t *testing.T) {
		i := open()
		i.MergeState = "UNSTABLE"
		i.Contexts = []Context{
			{Kind: "check", Name: "a", Status: "COMPLETED", Conclusion: "ACTION_REQUIRED"},
			{Kind: "check", Name: "s", Status: "COMPLETED", Conclusion: "STALE"},
		}
		r := Synthesize(i)
		want := []string{
			"check: 'a' ACTION_REQUIRED (optional) (needs manual approval)",
			"check: 's' STALE (optional) -> re-run after push",
		}
		if !reflect.DeepEqual(r.NonBlocking, want) {
			t.Errorf("non_blocking = %q, want %q", r.NonBlocking, want)
		}
	})
	t.Run("fp sees through the running-checks collapse", func(t *testing.T) {
		mk := func(names ...string) Input {
			i := open()
			i.MergeState = "BLOCKED"
			for _, n := range names {
				i.Contexts = append(i.Contexts, Context{Kind: "check", Name: n, Status: "QUEUED", Required: true})
			}
			return i
		}
		r1 := Synthesize(mk("a", "b", "c", "d"))
		r2 := Synthesize(mk("a", "b", "c", "e")) // same count, one swapped
		if len(r1.Pending) != 1 || r1.Pending[0] != "checks: 4 required running" {
			t.Fatalf("display not collapsed: %q", r1.Pending)
		}
		if r1.FP == r2.FP {
			t.Error("fp identical although the running set changed under the collapse")
		}
		if r1.FP != Synthesize(mk("d", "c", "b", "a")).FP {
			t.Error("fp depends on running-check order")
		}
	})
	t.Run("missing-required fires when merge state is unavailable", func(t *testing.T) {
		i := open()
		i.MergeState = ""
		i.Mergeable = ""
		i.MissingRequired = []string{"ci/x"}
		r := Synthesize(i)
		want := []string{"check: 'ci/x' MISSING (required, never reported)"}
		if !reflect.DeepEqual(r.Blockers, want) {
			t.Errorf("blockers = %q, want %q", r.Blockers, want)
		}
	})
}

func TestLineAndListCaps(t *testing.T) {
	t.Run("over-long action is dropped whole", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.Contexts = []Context{{Kind: "status", Name: "ci/x", Conclusion: "ERROR", Required: true,
			URL: "https://ci.example.com/" + strings.Repeat("x", 150)}}
		r := Synthesize(i)
		if len(r.Blockers) != 1 || r.Blockers[0] != "check: 'ci/x' FAILING" {
			t.Errorf("blockers = %q, want bare form without the un-pasteable URL", r.Blockers)
		}
	})
	t.Run("giant check names are truncated to the line cap", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.Contexts = []Context{{Kind: "status", Name: strings.Repeat("n", 300), Conclusion: "ERROR", Required: true}}
		r := Synthesize(i)
		if got := len([]rune(r.Blockers[0])); got > 140 {
			t.Errorf("blocker length = %d runes, cap 140", got)
		}
	})
	t.Run("fixed residual vocabulary survives the cap", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.UnresolvedThreads = 12
		r := Synthesize(i)
		if !strings.HasSuffix(r.Blockers[0], "12 unresolved threads)") {
			t.Errorf("residual line truncated: %q", r.Blockers[0])
		}
	})
	t.Run("non_blocking is recapped at 5 overall", func(t *testing.T) {
		i := open()
		i.MergeState = "HAS_HOOKS"
		for n := 0; n < 8; n++ {
			i.Contexts = append(i.Contexts, Context{Kind: "check", Name: fmt.Sprintf("opt%d", n),
				Status: "COMPLETED", Conclusion: "FAILURE"})
		}
		i.Contexts = append(i.Contexts,
			Context{Kind: "check", Name: "p1", Status: "QUEUED"},
			Context{Kind: "check", Name: "p2", Status: "QUEUED"})
		r := Synthesize(i)
		if len(r.NonBlocking) != capNonBlocking+1 {
			t.Fatalf("non_blocking = %d entries (%q), want %d + summary", len(r.NonBlocking), r.NonBlocking, capNonBlocking)
		}
		last := r.NonBlocking[len(r.NonBlocking)-1]
		if !strings.HasPrefix(last, "+") || !strings.HasSuffix(last, " more") {
			t.Errorf("summary line = %q", last)
		}
	})
	t.Run("non_blocking overflow count is accurate (single cap)", func(t *testing.T) {
		// 7 optional failing checks, nothing else: the fold line must report
		// the true hidden count (2), not an undercount from a second capping
		// pass over a summary line it could not parse.
		i := open()
		i.MergeState = "UNSTABLE"
		for n := 0; n < 7; n++ {
			i.Contexts = append(i.Contexts, Context{Kind: "check", Name: fmt.Sprintf("opt%d", n),
				Status: "COMPLETED", Conclusion: "FAILURE"})
		}
		r := Synthesize(i)
		if len(r.NonBlocking) != capNonBlocking+1 {
			t.Fatalf("non_blocking = %d entries (%q), want %d + summary", len(r.NonBlocking), r.NonBlocking, capNonBlocking)
		}
		if got := r.NonBlocking[capNonBlocking]; got != "+2 more" {
			t.Errorf("fold line = %q, want %q (7 fails - 5 shown)", got, "+2 more")
		}
	})
}

func TestQueueOutput(t *testing.T) {
	t.Run("ready to enqueue emits queue.required and CLEAN", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.ReviewDecision = "APPROVED"
		i.QueueEnabled = true
		r := Synthesize(i)
		if r.Queue == nil || !r.Queue.Required || r.Queue.Position != 0 || r.Queue.State != "" || r.Queue.ETASec != 0 {
			t.Errorf("queue = %+v, want {required:true} only", r.Queue)
		}
		if r.State != StateClean {
			t.Errorf("state = %q, want CLEAN", r.State)
		}
	})
	t.Run("progressing queue emits position+state+eta", func(t *testing.T) {
		i := open()
		i.Queue = &Queue{Position: 3, State: "AWAITING_CHECKS", ETASec: 840}
		r := Synthesize(i)
		if r.Queue == nil || r.Queue.Position != 3 || r.Queue.State != "AWAITING_CHECKS" || r.Queue.ETASec != 840 {
			t.Errorf("queue = %+v, want {3, AWAITING_CHECKS, 840}", r.Queue)
		}
		if r.Queue.Required {
			t.Error("a progressing queue must not set required")
		}
		if r.State != StateQueued {
			t.Errorf("state = %q, want QUEUED", r.State)
		}
	})
	t.Run("null eta omits eta_s", func(t *testing.T) {
		i := open()
		i.Queue = &Queue{Position: 1, State: "QUEUED", ETASec: -1}
		if r := Synthesize(i); r.Queue == nil || r.Queue.ETASec != 0 {
			t.Errorf("queue eta = %+v, want omitted on null", r.Queue)
		}
	})
	t.Run("summarize drops queue eta but keeps position/state", func(t *testing.T) {
		i := open()
		i.Queue = &Queue{Position: 3, State: "AWAITING_CHECKS", ETASec: 840}
		s := Summarize(i)
		if s.Queue == nil || s.Queue.ETASec != 0 {
			t.Errorf("mine queue = %+v, want eta dropped", s.Queue)
		}
		if s.Queue.Position != 3 || s.Queue.State != "AWAITING_CHECKS" {
			t.Errorf("mine queue lost position/state: %+v", s.Queue)
		}
	})
}

func TestFailedLineFallbacks(t *testing.T) {
	t.Run("app check with null workflowRun falls back to detailsUrl", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.Contexts = []Context{{Kind: "check", Name: "CodeQL", Status: "COMPLETED",
			Conclusion: "FAILURE", Required: true, RunID: 0,
			URL: "https://github.com/o/r/security/code-scanning"}}
		r := Synthesize(i)
		want := []string{"check: 'CodeQL' FAILING -> https://github.com/o/r/security/code-scanning"}
		if !reflect.DeepEqual(r.Blockers, want) {
			t.Errorf("blockers = %q, want %q", r.Blockers, want)
		}
	})
	t.Run("failing check with neither run id nor url is bare", func(t *testing.T) {
		i := open()
		i.MergeState = "BLOCKED"
		i.Contexts = []Context{{Kind: "check", Name: "x", Status: "COMPLETED",
			Conclusion: "FAILURE", Required: true}}
		r := Synthesize(i)
		want := []string{"check: 'x' FAILING"}
		if !reflect.DeepEqual(r.Blockers, want) {
			t.Errorf("blockers = %q, want %q", r.Blockers, want)
		}
	})
}

func TestCountOnlyThreads(t *testing.T) {
	// Unresolved threads without a conversation-resolution rule surface only as
	// the unresolved_threads field — no blocker (§2.2).
	i := open()
	i.UnresolvedThreads = 3
	i.ConvResolution = false
	r := Synthesize(i)
	if len(r.Blockers) != 0 {
		t.Errorf("blockers = %q, want none (count-only)", r.Blockers)
	}
	if r.UnresolvedThreads != 3 {
		t.Errorf("unresolved_threads = %d, want 3", r.UnresolvedThreads)
	}
	if r.State != StateClean {
		t.Errorf("state = %q, want CLEAN", r.State)
	}
}
