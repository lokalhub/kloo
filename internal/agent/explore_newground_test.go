package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// On a real repo a competent model reads many files to locate a one-file change.
// kloo-bench is 22 real-commit cases with a MEDIAN OF ONE source file edited, and
// every v0.17.1 failure was this rail stopping the run at 16 read-only steps with
// nothing written. Reading a file not yet seen is exploration working, not spinning.
func TestExploreRailAllowsManyDistinctReads(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 30; i++ { // far past the abort ceiling
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": fmt.Sprintf("pkg/f%d.go", i)}})})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})})
	srv := llmtest.Sequence(t, mocks...)
	loop, calls := newLoop(t, srv, nil, &stubBudget{tripAt: 200}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "find and fix the scoping bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonExploreStop {
		t.Fatalf("30 reads of DISTINCT files tripped the explore rail at step %d; "+
			"locating a one-file change in a real repo takes many reads", rep.Steps)
	}
	if len(*calls) < 30 {
		t.Errorf("dispatched %d calls, want all 30 distinct reads to survive", len(*calls))
	}
}

// The weak-model spin the rail exists for must STILL be caught: the same target
// over and over covers no new ground however many turns it takes.
func TestExploreRailStillStopsARepeatedRead(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 30; i++ {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "same.go"}})})
	}
	srv := llmtest.Sequence(t, mocks...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 200}, &stubChurn{})
	loop.ExploreNudgeRounds, loop.ExploreAbortRounds = 3, 6

	rep, err := loop.Run(context.Background(), "review it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonExploreStop {
		t.Fatalf("reason = %q, want explore-stop: re-reading one file forever is the "+
			"spin this rail exists to catch", rep.Reason)
	}
}

// A rail-stopped run must not report as ReasonAnswered. "answered" reads as the
// model replying, which made the largest kloo-bench failure mode invisible in
// every report that keys on the reason.
func TestExploreAbortIsNotReportedAsAnswered(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 20; i++ {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "loop.go"}})})
	}
	srv := llmtest.Sequence(t, mocks...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 200}, &stubChurn{})
	loop.ExploreNudgeRounds, loop.ExploreAbortRounds = 2, 4

	rep, _ := loop.Run(context.Background(), "look at it")
	if rep.Reason == ReasonAnswered {
		t.Fatal("a run the explore rail stopped must not be reported as `answered`")
	}
	if rep.Reason != ReasonExploreStop {
		t.Fatalf("reason = %q, want explore-stop", rep.Reason)
	}
}

// The thresholds must be reachable from config, which they were not: they lived
// only in loop.go and could be changed only by importing kloo as a Go library.
func TestExploreRoundsAreConfigurable(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 12; i++ {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "one.go"}})})
	}
	srv := llmtest.Sequence(t, mocks...)
	loop, calls := newLoop(t, srv, nil, &stubBudget{tripAt: 200}, &stubChurn{})
	loop.ExploreNudgeRounds, loop.ExploreAbortRounds = 2, 3

	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := len(*calls); n > 4 {
		t.Errorf("dispatched %d calls with abort=3; the configured ceiling was ignored", n)
	}
}

// The regression the unit tests missed and a REAL bench case caught: kloo-bench
// C12 issued 42 DISTINCT searches plus 9 other reads with no edit, burning 1.2M
// tokens to the step ceiling. Every query was new ground, so the no-new-ground
// streak never fired. Distinctness proves a turn is not a repeat; it does not
// prove the run is converging on a change.
func TestExploreRailStopsAVariedSpiral(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 60; i++ { // every search DIFFERENT, as in C12
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"search", map[string]any{"query": fmt.Sprintf("approver scope %d", i)}})})
	}
	srv := llmtest.Sequence(t, mocks...)
	loop, calls := newLoop(t, srv, nil, &stubBudget{tripAt: 500}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix the approver queue scoping")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonExploreStop {
		t.Fatalf("reason = %q, want explore-stop: 60 distinct searches with no edit is a spiral", rep.Reason)
	}
	if n := len(*calls); n > DefaultExploreTotalCap+1 {
		t.Errorf("dispatched %d read-only calls, want <= %d (the total cap)", n, DefaultExploreTotalCap+1)
	}
}

// ...and the cap must NOT undo the fix: a normal locate-then-edit run, well under
// the cap, still completes.
func TestExploreTotalCapDoesNotBreakNormalLocateThenEdit(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 25; i++ { // 25 distinct reads: normal for a real repo
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": fmt.Sprintf("src/m%d.ts", i)}})})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "src/fix.ts", "diff": "d"}})})
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})})
	srv := llmtest.Sequence(t, mocks...)
	loop, calls := newLoop(t, srv, nil, &stubBudget{tripAt: 500}, &stubChurn{})

	rep, _ := loop.Run(context.Background(), "locate and fix")
	if rep.Reason == ReasonExploreStop {
		t.Fatalf("25 distinct reads before an edit tripped the cap; real cases need this")
	}
	edits := 0
	for _, c := range *calls {
		if c.Name == "edit_file" {
			edits++
		}
	}
	if edits != 1 {
		t.Errorf("edits = %d, want 1 (the run must reach its edit)", edits)
	}
}

// The nudge must keep firing while a model reads distinct files. Tying it to the
// no-new-ground streak silenced it entirely for that shape — and the nudge is what
// converts reading into an edit attempt. Measured on kloo-bench C30: stock nudged
// and the model made 2 edits; with the nudge on the streak it made 0.
func TestExploreNudgeFiresWhileReadingDistinctFiles(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 20; i++ {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": fmt.Sprintf("src/d%d.ts", i)}})})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})})
	srv := llmtest.Sequence(t, mocks...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 200}, &stubChurn{})
	loop.ExploreNudgeRounds = 5

	rep, err := loop.Run(context.Background(), "find the bug")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.RailFires[string(RailExplore)] == 0 {
		t.Fatal("no explore nudge fired across 20 distinct reads; the nudge is what " +
			"turns reading into acting and must not be silenced by novelty")
	}
	// and it RE-ARMS: 20 reads at every 5th turn is more than one nudge
	if n := rep.RailFires[string(RailExplore)]; n < 2 {
		t.Errorf("explore nudges = %d, want >= 2 (it must re-arm, not fire once per run)", n)
	}
}
