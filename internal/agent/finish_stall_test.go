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

// TestLoopFinishUnverifiedWhenNoVerifier: in unverified mode (nil Verifier — no
// --verify and nothing auto-detected), finish stops calmly as ReasonUnverified,
// honestly distinct from success since nothing proved the change. The per-step
// verify is skipped, so a passing-verify stub is never consulted (there is none).
func TestLoopFinishUnverifiedWhenNoVerifier(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "scaffolded the app"}})})
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "build the ionic app")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonUnverified {
		t.Fatalf("reason = %q, want unverified", rep.Reason)
	}
	if rep.Steps != 1 {
		t.Errorf("steps = %d, want 1 (finish ends on the first turn)", rep.Steps)
	}
	if rep.FinalVerify.Command != "" {
		t.Errorf("unverified run should carry no verify command, got %q", rep.FinalVerify.Command)
	}
}

// TestLoopUnverifiedEditDoesNotFalseSucceed: in unverified mode an edited turn must
// NOT be reported as success — the success gate requires a real green verify, which
// is absent here. The run instead continues until another rail (here the model
// answers in prose) ends it.
func TestLoopUnverifiedEditDoesNotFalseSucceed(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "x.ts", "content": "export const x = 1\n"}})},
		llmtest.Mock{Body: proseResp(t, "Wrote x.ts; nothing to verify here.")},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, NewChurnDetector(3))

	rep, err := loop.Run(context.Background(), "add x.ts")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonSuccess {
		t.Fatalf("unverified edit must not report success; reason=%q", rep.Reason)
	}
	if rep.Reason != ReasonAnswered {
		t.Errorf("reason = %q, want answered", rep.Reason)
	}
}

// TestLoopUnverifiedShellProgressDoesNotChurn is the regression guard for the
// unverified-mode churn bug: with no verifier, the loop must not feed a synthetic
// empty verify output to the repeated-failure rail. A run that only takes shell
// actions (run_command, no edits) — e.g. scaffolding an app — must keep going to
// finish, not churn after N steps on a phantom "same red build".
func TestLoopUnverifiedShellProgressDoesNotChurn(t *testing.T) {
	rc := func(cmd string) tcSpec { return tcSpec{"run_command", map[string]any{"path": ".", "command": cmd}} }
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 3, rc("npm install -g @ionic/cli"))},
		llmtest.Mock{Body: toolResp(t, 3, rc("ionic start myApp tabs"))},
		llmtest.Mock{Body: toolResp(t, 3, rc("cd myApp && npm install"))},
		llmtest.Mock{Body: toolResp(t, 3, rc("ls myApp/src/app"))},
		llmtest.Mock{Body: toolResp(t, 3, tcSpec{"finish", map[string]any{"summary": "scaffolded"}})},
	)
	// Real churn rail at n=3 — before the fix this trips at the 4th step (churn
	// after 3 repeated phantom failures), exactly the user-reported session.
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, NewChurnDetector(3))

	rep, err := loop.Run(context.Background(), "create an ionic tabs app")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonChurn {
		t.Fatalf("unverified shell-progress run churned (the regression); steps=%d", rep.Steps)
	}
	if rep.Reason != ReasonUnverified {
		t.Errorf("reason = %q, want unverified (reached finish)", rep.Reason)
	}
}

// TestLoopStallBackstopOnGreenSpin: a model that has already acted (everActed=true)
// and then spins on no-op reads while verify stays GREEN is stopped by the stall
// backstop as ReasonAnswered — at a small N (seed + 3), far below the step budget.
// The stall only counts AFTER a real action so initial exploration is not penalised.
func TestLoopStallBackstopOnGreenSpin(t *testing.T) {
	// Model first runs a command (everActed=true), then spins on reads.
	// recordTool requires "path" in its schema, so pass it for run_command too.
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 1, tcSpec{"run_command", map[string]any{"path": "echo"}})},
		llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "a"}})},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.StallRounds = 3

	rep, err := loop.Run(context.Background(), "confirm the workspace")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered (stall backstop)", rep.Reason)
	}
	// Step 1: run_command (seeds stall, sets everActed=true)
	// Steps 2,3,4: read_file "a" (stall climbs 1,2,3 = stallLimit → stop)
	if rep.Steps != 4 {
		t.Errorf("steps = %d, want 4 (seed + stallLimit=3)", rep.Steps)
	}
}

// TestLoopStallNotTrippedBeforeFirstAction: a model that only reads files without
// acting first is governed by the explore rail (higher ceiling), not the stall
// backstop. Pre-action exploration with a green verify must never be terminated
// early by stall — that would cut off initial file-gathering before implementation.
func TestLoopStallNotTrippedBeforeFirstAction(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "a"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.StallRounds = 3
	// Disable explore rail so only stall and budget gates can fire.
	loop.ExploreNudgeRounds, loop.ExploreAbortRounds = 1000, 1000
	// Disable repetition rail so only stall and budget can stop this.
	loop.RepeatNudgeRounds, loop.RepeatAbortRounds = 1000, 1000

	rep, err := loop.Run(context.Background(), "explore the workspace")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Without a real action, stall must not fire — only the budget stops it.
	if rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want budget-exceeded (stall must not fire before everActed)", rep.Reason)
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
	// This test repeats ONE identical read to drive a long run for the budget rail;
	// disable the repetition rail (off the default 6) so it doesn't pre-empt budget.
	loop.RepeatNudgeRounds, loop.RepeatAbortRounds = 1000, 1000

	rep, err := loop.Run(context.Background(), "read everything, then fix")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want budget-exceeded (stall must not fire on a failing verify)", rep.Reason)
	}
}
