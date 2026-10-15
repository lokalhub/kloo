package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// The live incident: the model wrote a 150-line stylesheet for a brand band, a
// surface card and a restyled primary button, then the edit_file adding the
// matching markup failed —
//
//	edit_file: no SEARCH/REPLACE block found in diff
//
// — and the run reported SUCCESS, because the verify (unit tests plus an
// unrelated gate) was green either way. The CSS targeted elements that did not
// exist, so the page was byte-for-byte unchanged and the user was told it was
// done.
//
// NEGATIVE CONTROL: remove the rail and this fails — the run ends one request
// earlier, reporting success with the failed call unchallenged.
func TestFailedLastActionIsChallengedBeforeSuccess(t *testing.T) {
	srv := llmtest.Sequence(t,
		// A landed edit, so the run has real progress and a green verify.
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.txt", "diff": "x"}})},
		// green-verify-confirm spends its one nudge on that green edit; the model's
		// answer to it is the call that FAILS (recordTool errors with no "path").
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{}})},
		// The rail fires; the model does the work properly.
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "b.txt", "diff": "y"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "added the markup too"}})},
	)
	var vr []VerifyResult
	for i := 0; i < 10; i++ {
		vr = append(vr, passResult())
	}
	loop, _ := newLoop(t, srv, &stubVerifier{results: vr}, &stubBudget{tripAt: 60}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "give the login page a brand band")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := rep.RailFires[string(RailFailedLastAction)]; n != 1 {
		t.Fatalf("rail fired %d times, want 1 (rails: %v, steps %d, reason %v)", n, rep.RailFires, rep.Steps, rep.Reason)
	}
	var sawCorrective bool
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "most recent tool call FAILED") {
			sawCorrective = true
		}
	}
	if !sawCorrective {
		t.Error("the corrective never reached the model")
	}
}

// A run whose last call SUCCEEDED must not pay for this rail.
func TestCleanRunIsNotChallenged(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.txt", "diff": "x"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.txt"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	var vr []VerifyResult
	for i := 0; i < 10; i++ {
		vr = append(vr, passResult())
	}
	loop, _ := newLoop(t, srv, &stubVerifier{results: vr}, &stubBudget{tripAt: 60}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "give the login page a brand band")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := rep.RailFires[string(RailFailedLastAction)]; n != 0 {
		t.Errorf("rail fired %d times on a clean run, want 0", n)
	}
}

// The corrective quotes the error, because a bare "something failed" invites the
// model to re-affirm that everything is fine.
func TestFailedLastActionCorrectiveQuotesTheError(t *testing.T) {
	msg := failedLastActionCorrective("tools: edit_file login.page.ts:\n  no SEARCH/REPLACE block found in diff")
	if !strings.Contains(msg.Content, "no SEARCH/REPLACE block found in diff") {
		t.Errorf("the error text was dropped: %q", msg.Content)
	}
	if strings.Contains(msg.Content, "\n") {
		t.Errorf("the quoted error was not collapsed to one line: %q", msg.Content)
	}
}
