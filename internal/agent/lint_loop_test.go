package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// fakeLinter is a deterministic Linter for the loop tests: it returns a fixed
// LintResult and records how many times it ran and the paths it was given.
type fakeLinter struct {
	res   LintResult
	calls int
	paths [][]string
}

func (f *fakeLinter) Lint(ctx context.Context, paths []string) LintResult {
	f.calls++
	f.paths = append(f.paths, paths)
	return f.res
}

const lintAdvisoryLabel = "advisory — does NOT decide success"

// 1) Advisory surfaces, no success: a turn edits, the linter returns content, and
// verify is RED. The lint observation appears in the transcript, but the run does
// NOT end ReasonSuccess (verify is the only success gate).
func TestLintAdvisorySurfacesNoSuccess(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "still-wrong\n", "")})
	cfg := config.Config{MaxSteps: 1, ChurnRounds: 100} // one edit, then budget trips
	loop := buildLoop(t, root, srv, cfg)
	fl := &fakeLinter{res: LintResult{Command: "gofmt -l answer.txt", Stdout: "answer.txt\n"}}
	loop.Linter = fl

	rep, err := loop.Run(context.Background(), "edit but leave verify red")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonSuccess {
		t.Fatalf("a red verify must not be success, even with a lint observation: %s", rep.String())
	}
	if fl.calls != 1 {
		t.Errorf("linter should run once on the edited file, ran %d times", fl.calls)
	}
	if !msgWithAll(rep.Transcript, lintAdvisoryLabel, "answer.txt") {
		t.Errorf("the advisory lint observation should be in the transcript")
	}
}

// 2) No false-churn (MANDATORY): N+2 edit turns whose lint output is BYTE-IDENTICAL
// every turn, edits are DISTINCT, verify stays red. The run must NOT stop with
// ReasonChurn — lint never reaches the churn rail, and distinct edits keep the edit
// rail from firing — so it runs to the step budget instead.
func TestLintNoFalseChurn(t *testing.T) {
	root := seedRepo(t)
	// N = ChurnRounds (3) → N+2 = 5 DISTINCT edits, none of them "right\n" (verify
	// stays red). If lint could churn, the identical lint output would trip it.
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: writeFileCall(t, "wrong-a\n", "")},
		llmtest.Mock{Body: writeFileCall(t, "wrong-b\n", "")},
		llmtest.Mock{Body: writeFileCall(t, "wrong-c\n", "")},
		llmtest.Mock{Body: writeFileCall(t, "wrong-d\n", "")},
		llmtest.Mock{Body: writeFileCall(t, "wrong-e\n", "")},
	)
	cfg := config.Config{MaxSteps: 5, ChurnRounds: 3}
	loop := buildLoop(t, root, srv, cfg)
	// Byte-identical lint output every single turn.
	loop.Linter = &fakeLinter{res: LintResult{Command: "gofmt -l answer.txt", Stdout: "answer.txt\n"}}

	rep, err := loop.Run(context.Background(), "distinct edits, identical lint")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonChurn {
		t.Fatalf("identical lint output across turns must NOT cause churn: %s", rep.String())
	}
	if rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("distinct edits + red verify should run to the step budget, got %s", rep.String())
	}
	// Prove the identical lint signal really was present every edit turn (≥ ChurnRounds).
	if n := countMsgsContaining(rep.Transcript, lintAdvisoryLabel); n < cfg.ChurnRounds {
		t.Errorf("expected the identical lint observation on every edit turn, saw %d", n)
	}
}

// 2-variant) identical lint output + a passing verify still reaches ReasonSuccess —
// lint never blocks the success a green verify earns.
func TestLintIdenticalOutputStillSucceeds(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "right\n", "")}) // green verify
	cfg := config.Config{MaxSteps: 10, ChurnRounds: 3}
	loop := buildLoop(t, root, srv, cfg)
	loop.Linter = &fakeLinter{res: LintResult{Command: "gofmt -l answer.txt", Stdout: "answer.txt\n"}}

	rep, err := loop.Run(context.Background(), "green verify with a lint finding")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("a green verify + edit must succeed despite a lint finding: %s", rep.String())
	}
}

// 3) Nil-linter parity: a clean linter (empty output ⇒ no observation) produces a
// transcript and terminal Reason byte-identical to a nil linter (the pre-feature
// path). This proves the lint code adds ZERO bytes when it has nothing to say.
func TestLintNilParity(t *testing.T) {
	run := func(linter Linter) *Report {
		root := seedRepo(t)
		srv := llmtest.Sequence(t,
			llmtest.Mock{Body: writeFileCall(t, "wrong-a\n", "")},
			llmtest.Mock{Body: writeFileCall(t, "wrong-b\n", "")},
			llmtest.Mock{Body: writeFileCall(t, "wrong-c\n", "")},
		)
		cfg := config.Config{MaxSteps: 3, ChurnRounds: 100}
		loop := buildLoop(t, root, srv, cfg)
		loop.Linter = linter
		rep, err := loop.Run(context.Background(), "identical script")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return rep
	}

	nilRep := run(nil)
	cleanRep := run(&fakeLinter{res: LintResult{}}) // clean ⇒ appends nothing

	if nilRep.Reason != cleanRep.Reason {
		t.Fatalf("reason differs: nil=%s clean=%s", nilRep.Reason, cleanRep.Reason)
	}
	if len(nilRep.Transcript) != len(cleanRep.Transcript) {
		t.Fatalf("transcript length differs: nil=%d clean=%d (a clean lint must add no messages)",
			len(nilRep.Transcript), len(cleanRep.Transcript))
	}
	for i := range nilRep.Transcript {
		if nilRep.Transcript[i].Role != cleanRep.Transcript[i].Role ||
			nilRep.Transcript[i].Content != cleanRep.Transcript[i].Content {
			t.Errorf("transcript message %d differs:\n nil=%q\n clean=%q",
				i, nilRep.Transcript[i].Content, cleanRep.Transcript[i].Content)
		}
	}
}

// 4) Verify-only gate, three facets:
//
//	(a) RED lint + GREEN verify + edit ⇒ ReasonSuccess.
//	(b) clean lint + RED verify ⇒ not success.
//	(c) a lint that ERRORS (non-runnable) never yields ReasonError/ReasonChurn by itself.
func TestLintVerifyOnlyGate(t *testing.T) {
	t.Run("red lint + green verify ⇒ success", func(t *testing.T) {
		root := seedRepo(t)
		srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "right\n", "")})
		cfg := config.Config{MaxSteps: 10, ChurnRounds: 10}
		loop := buildLoop(t, root, srv, cfg)
		loop.Linter = &fakeLinter{res: LintResult{Command: "gofmt -l answer.txt", Stdout: "answer.txt\n", ExitCode: 0}}

		rep, err := loop.Run(context.Background(), "green verify, red lint")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rep.Reason != ReasonSuccess {
			t.Fatalf("red lint must not block a green-verify success: %s", rep.String())
		}
		if !msgWithAll(rep.Transcript, lintAdvisoryLabel) {
			t.Errorf("the advisory lint observation should still be surfaced")
		}
	})

	t.Run("clean lint + red verify ⇒ not success", func(t *testing.T) {
		root := seedRepo(t)
		srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "still-wrong\n", "")})
		cfg := config.Config{MaxSteps: 1, ChurnRounds: 100}
		loop := buildLoop(t, root, srv, cfg)
		loop.Linter = &fakeLinter{res: LintResult{}} // clean

		rep, err := loop.Run(context.Background(), "clean lint, red verify")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rep.Reason == ReasonSuccess {
			t.Fatalf("a clean lint cannot manufacture success on a red verify: %s", rep.String())
		}
	})

	t.Run("erroring lint ⇒ no error/churn by itself", func(t *testing.T) {
		root := seedRepo(t)
		srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "still-wrong\n", "")})
		cfg := config.Config{MaxSteps: 1, ChurnRounds: 100}
		loop := buildLoop(t, root, srv, cfg)
		loop.Linter = &fakeLinter{res: LintResult{Err: errLintNotRunnable}}

		rep, err := loop.Run(context.Background(), "lint errors out")
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rep.Reason == ReasonError || rep.Reason == ReasonChurn {
			t.Fatalf("a lint error must not drive ReasonError/ReasonChurn: %s", rep.String())
		}
		if rep.Reason != ReasonBudgetExceeded {
			t.Errorf("expected the normal budget stop, got %s", rep.String())
		}
	})
}

// TestLintRunsOnEditedPathOnly: the linter is called with exactly the edited file
// path (not the whole tree), confirming the §3.3 "edited file(s) only" contract.
func TestLintRunsOnEditedPathOnly(t *testing.T) {
	root := seedRepo(t)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeFileCall(t, "still-wrong\n", "")})
	cfg := config.Config{MaxSteps: 1, ChurnRounds: 100}
	loop := buildLoop(t, root, srv, cfg)
	fl := &fakeLinter{res: LintResult{}}
	loop.Linter = fl

	if _, err := loop.Run(context.Background(), "edit one file"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fl.calls != 1 || len(fl.paths) != 1 || len(fl.paths[0]) != 1 || fl.paths[0][0] != "answer.txt" {
		t.Errorf("lint should run once on the single edited path [answer.txt], got %v", fl.paths)
	}
}
