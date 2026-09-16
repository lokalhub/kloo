package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// readSpin returns n read_file mocks on DISTINCT paths, so only the explore
// TOTAL advances — the shape measured on kloo-bench.
func readSpin(t *testing.T, n int) []llmtest.Mock {
	t.Helper()
	out := make([]llmtest.Mock, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, llmtest.Mock{Body: toolResp(t, 5,
			tcSpec{"read_file", map[string]any{"path": string(rune('a'+i%26)) + ".go"}})})
	}
	return out
}

// TestAutoDelegateFiresOnTheExploreNudge: glimmer is OFFERED the task tool and
// never calls it (0 delegations across 36 calls on a lost case), so kloo must
// drive the decomposition rather than suggest it.
//
// NEGATIVE CONTROL: remove the auto-delegation block from the explore-nudge
// branch and this fails — no investigator report reaches the parent.
func TestAutoDelegateFiresOnTheExploreNudge(t *testing.T) {
	mocks := readSpin(t, 6) // parent reads to the nudge threshold
	// the investigator child: one read, then a finish carrying its report
	mocks = append(mocks,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "deep.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "EDIT src/pay.go in calcTax: the cap is applied before the exemption"}})},
	)
	mocks = append(mocks, readSpin(t, 8)...) // parent continues
	srv := llmtest.Sequence(t, mocks...)

	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 60}, &stubChurn{})
	loop.EnableSubagents = true

	rep, err := loop.Run(context.Background(), "fix the tax cap")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.AutoDelegations != 1 {
		t.Fatalf("AutoDelegations = %d, want exactly 1 (one-shot per run)", rep.ToolCounters.AutoDelegations)
	}
	if !msgWithAll(rep.Transcript, "EDIT src/pay.go in calcTax") {
		t.Error("the investigator's findings never reached the parent")
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "deep.go") {
			t.Error("the investigator's own reads leaked into the parent context")
		}
	}
}

// TestAutoDelegateIsOneShot: a second investigation is the same spin one level
// down, and would double the cost of an already-losing run.
func TestAutoDelegateIsOneShot(t *testing.T) {
	mocks := readSpin(t, 6)
	mocks = append(mocks,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "report one"}})})
	mocks = append(mocks, readSpin(t, 20)...) // spin past several more nudge rounds
	srv := llmtest.Sequence(t, mocks...)

	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 60}, &stubChurn{})
	loop.EnableSubagents = true

	rep, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.AutoDelegations > 1 {
		t.Errorf("AutoDelegations = %d, want at most 1", rep.ToolCounters.AutoDelegations)
	}
}

// TestAutoDelegateOffWhenSubagentsDisabled: default path untouched.
func TestAutoDelegateOffWhenSubagentsDisabled(t *testing.T) {
	srv := llmtest.Sequence(t, readSpin(t, 14)...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 60}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.AutoDelegations != 0 {
		t.Errorf("AutoDelegations = %d with subagents off, want 0", rep.ToolCounters.AutoDelegations)
	}
}
