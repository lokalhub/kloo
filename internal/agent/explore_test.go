package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestLoopExplorationRailStopsTheSpin: a model that inspects file after file
// without ever editing (a weak-model failure mode — distinct files each turn, so
// not repetition; no verify change, so not stall; no edit, so not churn) is nudged
// to act, then stopped (ReasonAnswered) so the human can step in.
func TestLoopExplorationRailStopsTheSpin(t *testing.T) {
	var mocks []llmtest.Mock
	for i := 0; i < 12; i++ { // more reads than the abort threshold
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": fmt.Sprintf("f%d.go", i)}})})
	}
	srv := llmtest.Sequence(t, mocks...)
	loop, calls := newLoop(t, srv, nil, &stubBudget{tripAt: 100}, &stubChurn{})
	loop.ExploreNudgeRounds, loop.ExploreAbortRounds = 4, 7 // small for the test

	rep, err := loop.Run(context.Background(), "review the app and fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered (explore rail stops the spin)", rep.Reason)
	}
	if rep.Steps != 7 {
		t.Errorf("steps = %d, want 7 (stopped at the explore abort)", rep.Steps)
	}
	if n := len(*calls); n != 7 {
		t.Errorf("dispatched %d calls, want 7 (no spinning to the budget ceiling)", n)
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
