package agent

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// ─── Phase 02 Task 03: the other rails must stop on the same turn as v0.16.7 ──
//
// This file is SELF-CONTAINED on purpose: it is dropped verbatim into a v0.16.7
// worktree to capture the expected literals, so it may not reference anything the
// gate introduced.

// parityTool dispatches with a caller-chosen outcome and no real side effects.
type parityTool struct {
	name string
	res  tools.Result
	err  error
}

func (t parityTool) Name() string        { return t.name }
func (t parityTool) Description() string { return "parity" }
func (t parityTool) Schema() tools.ParamSchema {
	return tools.ParamSchema{Properties: map[string]tools.Property{
		"path": {Type: "string"}, "command": {Type: "string"}, "diff": {Type: "string"},
	}}
}
func (t parityTool) Invoke(ctx context.Context, c tools.Call) (tools.Result, error) {
	return t.res, t.err
}

// parityVerifier cycles a fixed script of results, like the production verifier
// would over a run that never recovers.
type parityVerifier struct {
	results []VerifyResult
	calls   int
}

func (v *parityVerifier) Verify(ctx context.Context) VerifyResult {
	r := v.results[minInt(v.calls, len(v.results)-1)]
	v.calls++
	return r
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// parityOutcome is what each scripted run is compared on.
type parityOutcome struct {
	steps    int
	reason   Reason
	evidence string
	verifies int
}

func (o parityOutcome) line(name string) string {
	return fmt.Sprintf("%s: step=%d reason=%s evidence=%s", name, o.steps, o.reason, o.evidence)
}

// railScenario is one scripted run: the tool calls the model emits, the verify
// results, and the loop knobs the rail under test needs.
type railScenario struct {
	name    string
	calls   []tcSpec
	results []VerifyResult
	setup   func(l *Loop)
}

func railScenarios() []railScenario {
	redA := VerifyResult{Command: "npm test", ExitCode: 1, Passed: false, Stdout: "FAIL: one"}
	green := VerifyResult{Command: "npm test", ExitCode: 0, Passed: true}
	rm := tcSpec{"run_command", map[string]any{"command": "rm -f x"}}
	sameEdit := tcSpec{"edit_file", map[string]any{"path": "a.go", "diff": "D1"}}
	read := tcSpec{"read_file", map[string]any{"path": "a.go"}}

	repeat := func(s tcSpec, n int) []tcSpec {
		out := make([]tcSpec, 0, n)
		for range n {
			out = append(out, s)
		}
		return out
	}

	return []railScenario{
		{
			// Repeated-failure churn: the same shell mutation, the same red verify,
			// no edits — the canonical "stuck redoing a broken fix" run.
			name: "churn-repeated-failure", calls: repeat(rm, 12), results: []VerifyResult{redA},
			setup: func(l *Loop) {
				l.Churn = NewChurnDetector(3)
				l.RepeatAbortRounds = 50
				l.RepeatNudgeRounds = 40
				l.StallRounds = 40
			},
		},
		{
			// Repeated-edit churn: the identical edit over and over.
			name: "churn-repeated-edit", calls: repeat(sameEdit, 12), results: []VerifyResult{redA},
			setup: func(l *Loop) {
				l.Churn = NewChurnDetector(3)
				l.RepeatAbortRounds = 50
				l.RepeatNudgeRounds = 40
				l.StallRounds = 40
			},
		},
		{
			// Stall backstop: green verify, a real action each turn, nothing changing.
			name: "stall-confirming-spin", calls: repeat(rm, 12), results: []VerifyResult{green},
			setup: func(l *Loop) {
				l.Churn = &stubChurn{}
				l.StallRounds = 4
				l.RepeatAbortRounds = 50
				l.RepeatNudgeRounds = 40
			},
		},
		{
			// A7 repeated-verify stop at 3 identical failures.
			name: "repeated-verify-stop", calls: repeat(rm, 12), results: []VerifyResult{redA},
			setup: func(l *Loop) {
				l.Churn = &stubChurn{}
				l.StopOn = StopPolicy{RepeatedVerify: 3}
				l.StallRounds = 40
				l.RepeatAbortRounds = 50
				l.RepeatNudgeRounds = 40
			},
		},
		{
			// The legitimate run: read a few files, then edit. Must trip nothing.
			name: "read-then-edit", calls: []tcSpec{read,
				{"read_file", map[string]any{"path": "b.go"}},
				{"read_file", map[string]any{"path": "c.go"}},
				sameEdit,
				{"finish", map[string]any{"summary": "done"}}},
			results: []VerifyResult{green},
			setup: func(l *Loop) {
				l.Churn = NewChurnDetector(3)
				l.StallRounds = 40
				l.RepeatAbortRounds = 50
				l.RepeatNudgeRounds = 40
			},
		},
		{
			// Pure exploration: the explore rail owns this one.
			name: "explore-read-only", calls: repeat(read, 30), results: []VerifyResult{green},
			setup: func(l *Loop) {
				l.Churn = &stubChurn{}
				l.StallRounds = 40
				l.RepeatAbortRounds = 50
				l.RepeatNudgeRounds = 40
			},
		},
	}
}

func runRailScenario(t *testing.T, sc railScenario) parityOutcome {
	t.Helper()
	mocks := make([]llmtest.Mock, 0, len(sc.calls))
	for _, c := range sc.calls {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, c)})
	}
	srv := llmtest.Sequence(t, mocks...)

	v := &parityVerifier{results: sc.results}
	loop, calls := newLoop(t, srv, v, &stubBudget{tripAt: 100}, &stubChurn{})
	for _, name := range []string{tools.NameListDir, tools.NameReadDir, tools.NameSearch, tools.NameCommandOutput} {
		loop.Registry.Register(recordTool{name: name, calls: calls})
	}
	loop.Registry.Register(parityTool{name: tools.NameRunCommand, res: tools.Result{Output: "ok"}})
	loop.Registry.Register(parityTool{name: tools.NameEditFile, res: tools.Result{Output: "edited"}})
	sc.setup(loop)

	rep, err := loop.Run(context.Background(), "scripted parity run")
	if err != nil {
		t.Fatalf("%s: Run: %v", sc.name, err)
	}
	ev := "-"
	switch {
	case rep.Churn != nil:
		ev = string(rep.Churn.Kind)
	case rep.Safety != nil:
		ev = rep.Safety.Class
	}
	return parityOutcome{steps: rep.Steps, reason: rep.Reason, evidence: ev, verifies: rep.ToolCounters.VerifyAttempts}
}

func scenarioByName(t *testing.T, name string) railScenario {
	t.Helper()
	for _, sc := range railScenarios() {
		if sc.name == name {
			return sc
		}
	}
	t.Fatalf("no scenario named %q", name)
	return railScenario{}
}

// assertParity compares one scripted run against the literal captured from
// v0.16.7. The literals below were produced by running THIS FILE, unchanged, in a
// clean v0.16.7 worktree (commit 60bf66a) on 2026-09-11 — see
// tasks/03-verify-gate-rail-interactions/artifacts/baseline-turns.txt.
func assertParity(t *testing.T, name string, wantStep int, wantReason Reason, wantEvidence string) {
	t.Helper()
	got := runRailScenario(t, scenarioByName(t, name))
	if got.steps != wantStep || got.reason != wantReason || got.evidence != wantEvidence {
		t.Errorf("%s stopped differently than at v0.16.7:\n got: %s\nwant: %s",
			name, got.line(name), parityOutcome{steps: wantStep, reason: wantReason, evidence: wantEvidence}.line(name))
	}
}

// TestChurnRailUnchangedWithVerifyGate: both churn runs stop on the same turn with
// the same kind. Table-driven over the two churn scenarios.
func TestChurnRailUnchangedWithVerifyGate(t *testing.T) {
	cases := []struct {
		name     string
		step     int
		reason   Reason
		evidence string
	}{
		{name: "churn-repeated-failure", step: 4, reason: ReasonChurn, evidence: string(ChurnRepeatedFailure)},
		{name: "churn-repeated-edit", step: 4, reason: ReasonChurn, evidence: string(ChurnRepeatedEdit)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertParity(t, tc.name, tc.step, tc.reason, tc.evidence) })
	}
}

func TestStallBackstopUnchangedWithVerifyGate(t *testing.T) {
	assertParity(t, "stall-confirming-spin", 5, ReasonAnswered, "-")
}

func TestRepeatedVerifyStopUnchanged(t *testing.T) {
	assertParity(t, "repeated-verify-stop", 3, ReasonSafetyStop, "repeated_verify_failure")
}

func TestExploreRailUnchangedWithVerifyGate(t *testing.T) {
	// DELIBERATE drift from v0.16.7, not regression. Two changes, both intended:
	//   step 16 -> 17: the first read of a target is now NEW GROUND and resets the
	//     streak, so the 30 identical reads trip the ceiling one turn later.
	//   answered -> explore-stop: a run a rail killed with no edits is not the model
	//     answering. Reporting it as "answered" hid the largest kloo-bench failure
	//     mode behind a success-flavoured word.
	// The rail still STOPS this case, which is what the parity check is protecting.
	assertParity(t, "explore-read-only", 17, ReasonExploreStop, "-")
}

func TestReadThenEditRunTripsNothing(t *testing.T) {
	assertParity(t, "read-then-edit", 4, ReasonSuccess, "-")
}

// TestCaptureRailBaseline prints one line per scripted run. It is the generator for
// artifacts/baseline-turns.txt, and it runs in the v0.16.7 worktree unchanged.
func TestCaptureRailBaseline(t *testing.T) {
	if os.Getenv("KLOO_RAIL_BASELINE") == "" {
		t.Skip("set KLOO_RAIL_BASELINE=1 to print the parity lines")
	}
	for _, sc := range railScenarios() {
		got := runRailScenario(t, sc)
		fmt.Printf("%s verifies=%d\n", got.line(sc.name), got.verifies)
	}
}
