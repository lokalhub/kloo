package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// The live failure, three times across three kloo versions, reported as
// `answered` with a green verify and zero edits:
//
//	run_command (playwright capture)  → everActed = true
//	view_image wide-login.png         → stall 1
//	view_image phone-login.png        → stall 2
//	view_image wide-lobby.png         → stall 3 ⇒ run ends
//
// DefaultStallRounds is 3, which is exactly where every one of those runs died.
// The verify was green before the run began and the artifacts are gitignored, so
// every inspection turn fell through to the confirming-spin default.
//
// NEGATIVE CONTROL: restore the case to key on everActed and this fails — the run
// ends at the third inspection having edited nothing.
func TestInspectionBeforeTheFirstEditIsNotAStall(t *testing.T) {
	srv := llmtest.Sequence(t,
		// A command first: this is what sets everActed, and on a visual task it is
		// how exploration STARTS rather than how it ends.
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"run_command", map[string]any{"path": "npx playwright test e2e/visual-capture.spec.ts"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.txt"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "b.txt"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "c.txt"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "d.txt"}})},
		// Having looked, the model finally edits — which is the whole point of
		// letting it keep looking.
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.txt", "diff": "x"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "fixed the contrast"}})},
	)
	// A verify that is green from the very first turn — the condition that makes
	// every inspection turn look like a confirming-spin.
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{
		passResult(), passResult(), passResult(), passResult(), passResult(), passResult(), passResult(),
	}}, &stubBudget{tripAt: 60}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix the contrast in the matchup bar")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success — the run was cut off mid-inspection (steps=%d)", rep.Reason, rep.Steps)
	}
	if rep.Steps < 6 {
		t.Errorf("steps = %d, want at least 6: four inspection turns must not be cut off before the edit", rep.Steps)
	}
}

// The spin the backstop was BUILT for must still be caught: repeated commands
// against a green verify, changing nothing.
func TestCommandSpinOnAGreenVerifyStillStalls(t *testing.T) {
	cmd := llmtest.Mock{Body: toolResp(t, 5, tcSpec{"run_command", map[string]any{"path": "echo still done"}})}
	srv := llmtest.Sequence(t, cmd, cmd, cmd, cmd, cmd, cmd, cmd, cmd)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{
		passResult(), passResult(), passResult(), passResult(), passResult(), passResult(), passResult(), passResult(),
	}}, &stubBudget{tripAt: 60}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix the contrast in the matchup bar")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Steps > 6 {
		t.Errorf("steps = %d — a command spin on a green verify must still be stopped by the backstop", rep.Steps)
	}
}
