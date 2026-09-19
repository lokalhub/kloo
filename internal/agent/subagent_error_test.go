package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestOnSubagentCarriesTheChildsError reproduces the failure that cost a whole
// kloo-bench sweep: every delegated child died on its first step, and the only
// record was "reason=error". The reason says a child failed; it does not say why,
// and the cause had to be recovered by correlating timestamps across two runs.
//
// So the callback must carry the child's error, not just its terminal reason.
// Negative control: passing nil instead of rep.Err in runSubagent fails this test.
func TestOnSubagentCarriesTheChildsError(t *testing.T) {
	mocks := readSpin(t, 6) // parent explores until the legacy trigger fires
	// The child's very first completion fails, exactly as the routed child did when
	// its model could not load. 400 is not retryable, so it ends the child at once.
	mocks = append(mocks, llmtest.Mock{Status: 400, Body: `{"error":{"message":"model qwen3.8-27b-nvfp4 is not loaded"}}`})
	mocks = append(mocks, readSpin(t, 8)...) // the parent carries on alone

	loop, _ := newLoop(t, llmtest.Sequence(t, mocks...), nil, &stubBudget{tripAt: 60}, &stubChurn{})
	loop.AutoDelegate = true
	loop.LLMRetries = 0 // no retries: the child gets exactly the one failing call

	var (
		fired  int
		gotErr error
		reason Reason
	)
	loop.OnSubagent = func(_ int, r Reason, err error) {
		fired++
		reason, gotErr = r, err
	}

	if _, err := loop.Run(context.Background(), "fix it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fired != 1 {
		t.Fatalf("OnSubagent fired %d times, want 1", fired)
	}
	if reason != ReasonError {
		t.Fatalf("reason = %q, want %q — the child did not fail as intended", reason, ReasonError)
	}
	if gotErr == nil {
		t.Fatal("OnSubagent got a nil error for a failed child: the cause is discarded again")
	}
}
