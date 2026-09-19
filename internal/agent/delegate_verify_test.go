package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestReadThresholdHandoffVerifiesTheChildsWork: a delegated child edits the SAME
// tree as its parent, but its writes never pass through the parent's dispatch, so
// the parent's "something changed" signal never fires and the verify gate stays
// shut. The parent then has no way to learn the child's edit was wrong.
//
// Measured on kloo-bench C07: the child edited the correct file, ended in churn
// with 2 of 6 tests still failing, and the parent ran verify exactly once in a
// 38-step run — never over the child's edit. It read 15 more files and the explore
// rail stopped it. The rescue handoff already set this flag; the read-threshold
// handoff, which is the path production uses, did not.
//
// Negative control: dropping `mutatedSinceVerify = true` from the read-threshold
// handoff makes this test fail with 0 verifies after the handoff.
func TestReadThresholdHandoffVerifiesTheChildsWork(t *testing.T) {
	// Parent reads to the threshold, hands off; the child spins and is capped; the
	// parent then reads on. No parent edit anywhere, so the ONLY thing that can
	// open the verify gate is the handoff itself.
	mocks := readSpin(t, 4) // parent, up to DelegateAfterReads
	mocks = append(mocks, readSpin(t, 30)...)

	loop, calls := newLoop(t, llmtest.Sequence(t, mocks...), nil, &stubBudget{tripAt: 40}, &stubChurn{})
	loop.AutoDelegate = true
	loop.DelegateAfterReads = 4
	loop.SubagentMaxSteps = 3

	var verifies int
	loop.Verifier = &stubVerifier{
		results:  []VerifyResult{{Passed: false, ExitCode: 1, Stdout: "2 of 6 tests failing"}},
		onVerify: func() { verifies++ },
	}

	var (
		handoffAt int
		delegated bool
	)
	loop.OnSubagent = func(int, Reason, error) { delegated, handoffAt = true, verifies }

	if _, err := loop.Run(context.Background(), "fix it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Premise, kept separate from the assertion: handoffAt is legitimately 0
	// whenever no verify ran BEFORE the handoff, which is the normal case, so it
	// cannot double as "delegation happened".
	if !delegated {
		t.Fatal("no delegation happened: the test proves nothing")
	}
	// Everything the parent did was read-only, so without the fix the gate never
	// opens and this is 0.
	if after := verifies - handoffAt; after == 0 {
		t.Errorf("verifies after the handoff = 0 (total %d): the parent never checked "+
			"the child's edit, so it cannot know the edit was wrong", verifies)
	}
	// Guard the premise: if a parent edit had slipped in, the gate would have opened
	// for that instead and the test would pass for the wrong reason.
	for _, c := range *calls {
		if c.Name == "edit_file" {
			t.Fatalf("parent edited (%s): the premise of this test is a read-only parent", c.Name)
		}
	}
}
