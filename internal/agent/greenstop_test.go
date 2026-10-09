package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// Defect A: a landed edit with a green verify used to END the run on the spot. That
// is right for a one-requirement task and wrong for every other kind — the run was
// declared a success at its FIRST verified change, and whatever else the task asked
// for was silently dropped. These tests pin the replacement: one completion probe,
// one-shot, and a latch so the probe can never cost a run the success it had.

// TestGreenVerifyProbeRecoversADroppedRequirement is the defect itself: the task
// wants an edit AND a command, the edit verifies green on turn 1, and before the
// fix the command never ran.
func TestGreenVerifyProbeRecoversADroppedRequirement(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.go"}})},
		// The probe's turn: the model remembers the second requirement.
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"run_command", map[string]any{"command": "./deploy.sh", "path": "a.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "edited and deployed"}})},
	)
	loop, calls := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix a.go and then run ./deploy.sh")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success", rep.Reason)
	}
	if n := rep.RailFires[string(RailGreenVerifyConfirm)]; n != 1 {
		t.Fatalf("rail fires = %d, want 1", n)
	}
	var ran bool
	for _, c := range *calls {
		if c.Name == tools.NameRunCommand {
			ran = true
		}
	}
	if !ran {
		t.Errorf("the second requirement was dropped again, calls = %v", *calls)
	}
}

// TestGreenVerifyProbeIsOneShot: a model that just keeps editing is not asked twice.
// The cost of the probe is one step per run, not one step per green verify.
func TestGreenVerifyProbeIsOneShot(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.go"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success", rep.Reason)
	}
	if n := rep.RailFires[string(RailGreenVerifyConfirm)]; n != 1 {
		t.Fatalf("rail fires = %d, want exactly 1 (one-shot)", n)
	}
	// Turn 1 edits, turn 2 is the probe (the mock replays the same edit), and the
	// second green verify ends the run — no third turn.
	if rep.Steps != 2 {
		t.Fatalf("steps = %d, want 2", rep.Steps)
	}
}

// TestGreenVerifyProbeNeverLosesAnEarnedSuccess is the safety invariant that makes
// the change strictly additive: the probe spends a step, so a run on its last step
// could have had its success turned into a budget stop. It must not. Before the
// probe existed this run reported success; it still must.
func TestGreenVerifyProbeNeverLosesAnEarnedSuccess(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.go"}})})
	// The budget trips on the step the probe costs.
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 2, kind: BudgetSteps}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success — the probe must never cost a run the success it had earned", rep.Reason)
	}
	if rep.Budget != nil {
		t.Errorf("a converted stop must not report budget evidence: %+v", rep.Budget)
	}
}

// TestGreenVerifyProbeKeepsInterruptHonest: the latch converts a budget or rail stop
// into the success the run had already earned, but NOT a user interrupt. Esc means
// the user stopped it, whatever the tree looks like.
func TestGreenVerifyProbeKeepsInterruptHonest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.go"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	// Cancel as soon as the probe is injected, so the next loop head sees a dead ctx.
	loop.OnState = func(s State) {
		if s == StateDecide {
			cancel()
		}
	}

	rep, err := loop.Run(ctx, "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonInterrupted {
		t.Fatalf("reason = %q, want interrupted (the user's Esc is not a finished task)", rep.Reason)
	}
}
