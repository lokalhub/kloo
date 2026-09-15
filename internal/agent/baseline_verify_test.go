package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// readOnlySpin returns n read_file mocks, each on a DISTINCT path so the
// no-new-ground streak stays at 0 and only the TOTAL read-only counter advances
// — the exact shape measured on kloo-bench A29/C07.
func readOnlySpin(t *testing.T, n int) []llmtest.Mock {
	t.Helper()
	ms := make([]llmtest.Mock, 0, n)
	for i := 0; i < n; i++ {
		ms = append(ms, llmtest.Mock{Body: toolResp(t, 10,
			tcSpec{"read_file", map[string]any{"path": string(rune('a'+i%26)) + "_file.go"}})})
	}
	return ms
}

// TestBaselineVerifyRunsForAReadOnlySpin reproduces the kloo-bench trap: the
// verify gate is `mutatedSinceVerify`, so a model that only READS never verifies
// and is left to fix a failure whose text it has never seen. Measured over 41
// bench runs, every one of the 21 passing runs verified at least once and all 7
// runs that ended with verify_attempts == 0 failed.
//
// NEGATIVE CONTROL: drop the `counters.VerifyAttempts == 0` block from the
// explore-nudge branch in loop.go and this test fails on the first assertion
// (verify never runs) — it cannot pass vacuously.
func TestBaselineVerifyRunsForAReadOnlySpin(t *testing.T) {
	srv := llmtest.Sequence(t, readOnlySpin(t, 12)...)

	verifies := 0
	v := &stubVerifier{
		results:  []VerifyResult{{Command: "npx vitest run", ExitCode: 1, Passed: false, Stdout: "FAIL b15-carry-over.test.ts > carries over"}},
		onVerify: func() { verifies++ },
	}
	loop, _ := newLoop(t, srv, v, &stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "make the failing test pass")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if verifies == 0 {
		t.Fatal("verify never ran during a read-only spin: the model is asked to fix a failure it was never shown")
	}
	if rep.ToolCounters.VerifyAttempts == 0 {
		t.Errorf("VerifyAttempts = 0, want >= 1")
	}
	if !msgWithAll(rep.Transcript, "FAIL b15-carry-over.test.ts") {
		t.Error("the real failing verify output was not fed back to the model")
	}
	if !msgWithAll(rep.Transcript, "npx vitest run") {
		t.Error("the verify command was not shown to the model")
	}
}

// TestBaselineVerifyDoesNotDisplaceTheExploreNudge: when verify has ALREADY run,
// the explore nudge must still be the plain corrective. Without this, a run that
// verifies normally could silently lose its nudge.
func TestBaselineVerifyDoesNotDisplaceTheExploreNudge(t *testing.T) {
	mocks := []llmtest.Mock{{Body: toolResp(t, 10, tcSpec{"edit_file", map[string]any{"path": "a.go"}})}}
	mocks = append(mocks, readOnlySpin(t, 12)...)
	srv := llmtest.Sequence(t, mocks...)

	// Verify fails, so the edit does not end the run and the read-only spin follows.
	v := &stubVerifier{results: []VerifyResult{failResult()}}
	loop, _ := newLoop(t, srv, v, &stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !msgWithAll(rep.Transcript, "STOP reading and take action") {
		t.Error("explore corrective missing when verify had already run")
	}
}

// TestBaselineVerifyNotRunWhenUnverified: with no Verifier the loop must behave
// exactly as before — no panic, no synthesised verify.
func TestBaselineVerifyNotRunWhenUnverified(t *testing.T) {
	srv := llmtest.Sequence(t, readOnlySpin(t, 12)...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 0 {
		t.Errorf("VerifyAttempts = %d with no verifier, want 0", rep.ToolCounters.VerifyAttempts)
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "I ran the verify command for you") {
			t.Error("baseline verify corrective injected in unverified mode")
		}
	}
}
