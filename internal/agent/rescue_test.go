package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// countingCheckpoint takes a real-looking snapshot and counts rollbacks, so a
// test can prove a child's fix was NOT undone.
type countingCheckpoint struct{ rollbacks int }

func (c *countingCheckpoint) Checkpoint(context.Context) (Snapshot, error) {
	return Snapshot{Head: "h", Taken: true}, nil
}
func (c *countingCheckpoint) Rollback(context.Context, Snapshot) error {
	c.rollbacks++
	return nil
}

// rescueScenario: the parent edits (checkpoint taken, verify fails), then re-reads
// one file until the explore rail would stop it; the child "fixes" it; afterwards
// the verifier is green. Mirrors kloo-bench C66.
func rescueScenario(t *testing.T) (*Loop, *countingCheckpoint) {
	t.Helper()
	read := tcSpec{"read_file", map[string]any{"path": "a.go"}}
	mocks := []llmtest.Mock{{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.go"}})}}
	for i := 0; i < 5; i++ { // new ground, then 4 repeats -> streak 4 = abort
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, read)})
	}
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "CHILD-FIXED-IT"}})})
	for i := 0; i < 30; i++ { // buffer: parent keeps reading if nothing stops it
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, read)})
	}
	v := &stubVerifier{results: []VerifyResult{failResult(), passResult()}}
	loop, _ := newLoop(t, llmtest.Sequence(t, mocks...), v, &stubBudget{tripAt: 60}, &stubChurn{})
	ck := &countingCheckpoint{}
	loop.Checkpoint = ck
	loop.AutoDelegate = true
	loop.ExploreNudgeRounds, loop.ExploreAbortRounds, loop.ExploreTotalCap = 100, 4, 100
	return loop, ck
}

// TestRescueHandoffAtTheRailKeepsTheChildsFix: with KLOO_DELEGATE_ON_STOP the rail
// hands off instead of stopping, the parent re-verifies, and the run SUCCEEDS
// without rolling back.
//
// NEGATIVE CONTROL: drop `mutatedSinceVerify = true` from the rescue path. The
// parent then never verifies, the rail stops it again, and kloo rolls back to the
// pre-edit checkpoint — undoing the child's fix. This test fails on that rollback.
func TestRescueHandoffAtTheRailKeepsTheChildsFix(t *testing.T) {
	t.Setenv("KLOO_DELEGATE_ON_STOP", "1")
	loop, ck := rescueScenario(t)
	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.RescueDelegations != 1 {
		t.Fatalf("RescueDelegations = %d, want 1", rep.ToolCounters.RescueDelegations)
	}
	if ck.rollbacks != 0 {
		t.Errorf("rolled back %d time(s) — the child's fix was undone", ck.rollbacks)
	}
	if rep.Reason != ReasonSuccess {
		t.Errorf("reason = %q, want success after the child fixed it and the parent re-verified", rep.Reason)
	}
}

// TestRescueHandoffOffByDefault: without the flag the rail stops the run exactly as
// before — explore-stop, and the rollback that implies.
func TestRescueHandoffOffByDefault(t *testing.T) {
	t.Setenv("KLOO_DELEGATE_ON_STOP", "")
	loop, ck := rescueScenario(t)
	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.RescueDelegations != 0 {
		t.Errorf("RescueDelegations = %d with the flag off, want 0", rep.ToolCounters.RescueDelegations)
	}
	if rep.Reason != ReasonExploreStop {
		t.Errorf("reason = %q, want explore-stop with the flag off", rep.Reason)
	}
	if ck.rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1 (unchanged default behaviour)", ck.rollbacks)
	}
}
