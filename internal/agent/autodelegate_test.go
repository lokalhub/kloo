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

// TestDelegationGate pins the decision directly. An integration test could not
// represent it: in the unit harness run_command errors, and an errored call never
// sets everActed, so a "command then spin" scenario silently tested nothing.
func TestDelegationGate(t *testing.T) {
	cases := []struct {
		name                       string
		everActed, edited, untilEd bool
		wantBlocked                bool
	}{
		{"default: nothing done", false, false, false, false},
		{"default: ran a command (C66) blocks", true, false, false, true},
		{"default: edited blocks", true, true, false, true},
		{"untilEdit: ran a command does NOT block", true, false, true, false},
		{"untilEdit: edited blocks", true, true, true, true},
		{"untilEdit: nothing done", false, false, true, false},
	}
	for _, c := range cases {
		if got := delegationBlocked(c.everActed, c.edited, c.untilEd); got != c.wantBlocked {
			t.Errorf("%s: blocked=%v, want %v", c.name, got, c.wantBlocked)
		}
	}
}

// TestAutoDelegateAloneIsInvisibleToTheModel: with AutoDelegate but not
// EnableSubagents, kloo can still delegate, but the task tool is NOT offered.
// Advertising it coincided with A28 going from a 6-step success to a 29-step
// explore-stop with zero delegations; glimmer never called it voluntarily anyway.
func TestAutoDelegateAloneIsInvisibleToTheModel(t *testing.T) {
	mocks := readSpin(t, 6)
	mocks = append(mocks,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "HARNESS-DELEGATED"}})})
	mocks = append(mocks, readSpin(t, 8)...)
	loop, _ := newLoop(t, llmtest.Sequence(t, mocks...), nil, &stubBudget{tripAt: 60}, &stubChurn{})
	loop.AutoDelegate = true

	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := loop.Registry.Lookup(NameTask); ok {
		t.Error("the task tool was offered to the model under AutoDelegate alone")
	}
	if rep.ToolCounters.AutoDelegations != 1 {
		t.Errorf("AutoDelegations = %d, want 1", rep.ToolCounters.AutoDelegations)
	}
	if !msgWithAll(rep.Transcript, "HARNESS-DELEGATED") {
		t.Error("the delegated report never reached the parent")
	}
}

// TestSubagentStepsCountedAndCapped: the child's steps must be recorded, and an
// explicit cap must bound them. The default budget let delegated cases reach 2381s
// and 2400s against a 2400s ceiling, and nothing recorded how many steps the child
// took, so the budget could not be sized from evidence.
func TestSubagentStepsCountedAndCapped(t *testing.T) {
	mocks := readSpin(t, 6)                   // parent reaches the nudge
	mocks = append(mocks, readSpin(t, 30)...) // child spins well past any sane cap
	srv := llmtest.Sequence(t, mocks...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 60}, &stubChurn{})
	loop.AutoDelegate = true
	loop.SubagentMaxSteps = 5

	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.SubagentSteps == 0 {
		t.Fatal("SubagentSteps = 0: the child's work was not recorded")
	}
	if rep.ToolCounters.SubagentSteps > 5 {
		t.Errorf("SubagentSteps = %d, want <= 5: the cap did not bind", rep.ToolCounters.SubagentSteps)
	}
}

// TestOnSubagentFiresOnceWithChildSteps: the child's step count must reach the log
// the moment the child finishes, so it survives the harness hard-killing kloo at
// its ceiling (which suppresses KLOO_RESULT_JSON). Exactly once: the child copies
// the parent's config, so a double fire would double-count.
func TestOnSubagentFiresOnceWithChildSteps(t *testing.T) {
	mocks := readSpin(t, 6)
	mocks = append(mocks, readSpin(t, 3)...)
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})})
	mocks = append(mocks, readSpin(t, 8)...)
	loop, _ := newLoop(t, llmtest.Sequence(t, mocks...), nil, &stubBudget{tripAt: 60}, &stubChurn{})
	loop.AutoDelegate = true

	var fired []int
	loop.OnSubagent = func(steps int, _ Reason) { fired = append(fired, steps) }

	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fired) != 1 {
		t.Fatalf("OnSubagent fired %d times, want exactly 1", len(fired))
	}
	if fired[0] != rep.ToolCounters.SubagentSteps || fired[0] == 0 {
		t.Errorf("OnSubagent steps=%d, counter=%d — must match and be non-zero", fired[0], rep.ToolCounters.SubagentSteps)
	}
}

// TestDelegateAfterReadsWaitsForTheThreshold: with a threshold of 10, the legacy
// first-nudge trigger (6 reads) must NOT fire, and delegation must happen once 10
// read-only turns have passed without an edit. At 6 the trigger fired on 92% of
// runs glimmer already solves; on A06 it handed off three reads before glimmer's
// own edit and turned a ~200s pass into a timeout.
func TestDelegateAfterReadsWaitsForTheThreshold(t *testing.T) {
	mocks := readSpin(t, 10) // parent: reads 1..10; delegation fires on the 10th
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "AFTER-TEN"}})})
	mocks = append(mocks, readSpin(t, 8)...)
	loop, calls := newLoop(t, llmtest.Sequence(t, mocks...), nil, &stubBudget{tripAt: 60}, &stubChurn{})
	loop.AutoDelegate = true
	loop.DelegateAfterReads = 10

	var delegatedAt int
	loop.OnSubagent = func(int, Reason) { delegatedAt = len(*calls) }

	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.AutoDelegations != 1 {
		t.Fatalf("AutoDelegations = %d, want 1", rep.ToolCounters.AutoDelegations)
	}
	if delegatedAt < 10 {
		t.Errorf("delegated after %d parent reads, want >= 10 (the legacy 6-read trigger fired)", delegatedAt)
	}
	if !msgWithAll(rep.Transcript, "AFTER-TEN") {
		t.Error("the delegated report never reached the parent")
	}
}

// TestDelegateAfterReadsDoesNotFireBelowThreshold: a run that stays under the
// threshold must never hand off — that is what protects the runs glimmer solves.
func TestDelegateAfterReadsDoesNotFireBelowThreshold(t *testing.T) {
	mocks := readSpin(t, 9)
	mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, finishCall)})
	loop, _ := newLoop(t, llmtest.Sequence(t, mocks...), nil, &stubBudget{tripAt: 60}, &stubChurn{})
	loop.AutoDelegate = true
	loop.DelegateAfterReads = 10

	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.AutoDelegations != 0 {
		t.Errorf("AutoDelegations = %d below the threshold, want 0", rep.ToolCounters.AutoDelegations)
	}
}
