package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestLoopFinishToolSucceedsWhenVerifyPasses: the explicit finish tool ends the
// run immediately, and since verify passes the stop is a clean ReasonSuccess —
// the model's chosen terminator, with the label still hinging on verify.
func TestLoopFinishToolSucceedsWhenVerifyPasses(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "all done"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "is the workspace set up?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success", rep.Reason)
	}
	if rep.Steps != 1 {
		t.Errorf("steps = %d, want 1 (finish ends on the first turn)", rep.Steps)
	}
}

// TestLoopFinishToolAnswersWhenVerifyFails: finish on a failing/irrelevant verify
// still stops cleanly, but as ReasonAnswered — the model's summary stands while
// nothing was verified (kloo never reports a green success it can't confirm).
func TestLoopFinishToolAnswersWhenVerifyFails(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "removed the files"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "remove the go files")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered", rep.Reason)
	}
}

// TestLoopStallBackstopOnGreenSpin: a model that never calls finish and instead
// spins on a no-op read while verify stays GREEN (no edit, no tree change) is
// stopped by the stall backstop as ReasonAnswered — at a small N (seed + 3), far
// below the step budget, proving the two ceilings never overlap.
func TestLoopStallBackstopOnGreenSpin(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "a"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.StallRounds = 3

	rep, err := loop.Run(context.Background(), "confirm the workspace")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered (stall backstop)", rep.Reason)
	}
	if rep.Steps != 4 { // turn 1 seeds; turns 2,3,4 climb to stallLimit=3
		t.Errorf("steps = %d, want 4 (fires at seed + stallLimit, not the budget)", rep.Steps)
	}
}

// TestLoopStallNotTrippedOnRedVerify guards the DoD false-positive: a failing
// verify must NEVER trip the stall backstop, so a legitimate read-heavy run toward
// a fix (read many files, then edit) is left to churn + budget. Here verify always
// fails and churn never fires, so only the step budget stops it.
func TestLoopStallNotTrippedOnRedVerify(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "a"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult()}}, &stubBudget{tripAt: 8}, &stubChurn{})
	loop.StallRounds = 3

	rep, err := loop.Run(context.Background(), "read everything, then fix")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want budget-exceeded (stall must not fire on a failing verify)", rep.Reason)
	}
}
