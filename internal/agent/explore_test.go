package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestLoopExplorationRailStopsTheSpin: a model that inspects file after file
// without ever editing (a weak-model failure mode — distinct files each turn, so
// not repetition; no verify change, so not stall; no edit, so not churn) is nudged
// to act, then stopped (ReasonAnswered) so the human can step in.
func TestLoopExplorationRailStopsTheSpin(t *testing.T) {
	// The SAME target each turn. This test used to read a DISTINCT file per turn,
	// but distinct reads are now treated as new ground and no longer trip the rail:
	// on a real repo, reading many files to locate a one-file change is the job, not
	// a spin (kloo-bench, 22 real-commit cases). The spin this rail exists to catch
	// is re-reading without learning anything, which is what this now exercises.
	// Distinct-read survival is pinned by TestExploreRailAllowsManyDistinctReads.
	var mocks []llmtest.Mock
	for i := 0; i < 12; i++ { // more reads than the abort threshold
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "same.go"}})})
	}
	srv := llmtest.Sequence(t, mocks...)
	loop, calls := newLoop(t, srv, nil, &stubBudget{tripAt: 100}, &stubChurn{})
	loop.ExploreNudgeRounds, loop.ExploreAbortRounds = 4, 7 // small for the test

	rep, err := loop.Run(context.Background(), "review the app and fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// ReasonExploreStop, not ReasonAnswered: the rail KILLED this run with no edits,
	// and calling that "answered" made a stopped run read as a clean one.
	if rep.Reason != ReasonExploreStop {
		t.Fatalf("reason = %q, want explore-stop (explore rail stops the spin)", rep.Reason)
	}
	if rep.Steps != 8 {
		t.Errorf("steps = %d, want 8 (abort=7 repeats, +1 for the first read that was new ground)", rep.Steps)
	}
	if n := len(*calls); n != 8 {
		t.Errorf("dispatched %d calls, want 8 (no spinning to the budget ceiling)", n)
	}
	var nudged bool
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "without making any change") {
			nudged = true
		}
	}
	if !nudged {
		t.Error("expected the explore nudge before the abort")
	}
}

// TestLoopExplorationRailResetsOnEdit: reads interleaved with an edit do NOT trip
// the rail — acting resets the streak, so a legitimate read-then-edit run is safe.
func TestLoopExplorationRailResetsOnEdit(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "b.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "c.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.go", "diff": "x"}})}, // acts → resets
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "d.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 100}, &stubChurn{})
	loop.ExploreNudgeRounds, loop.ExploreAbortRounds = 3, 4 // tight: would trip if the edit didn't reset

	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonAnswered && rep.Steps < 6 {
		t.Fatalf("reason=%q steps=%d — the edit should have reset the explore streak, not tripped it", rep.Reason, rep.Steps)
	}
}
