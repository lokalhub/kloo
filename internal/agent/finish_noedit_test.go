package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestTaskWantsChange covers the heuristic in isolation. The false-positive rows
// matter most: each one costs a real run an extra step telling a model to change
// something nobody asked it to change.
func TestTaskWantsChange(t *testing.T) {
	cases := []struct {
		task string
		want bool
	}{
		{"fix the six visual defects listed below", true},
		{"add a profile tab", true},
		{"make the lobby read like chess.com", true},
		{"rewrite the gate so it fails today", true},
		{"improve the contrast in the matchup bar", true},

		// Questions and read-only asks must never trip it.
		{"is it set up?", false}, // the task string in kloo's own retry tests
		{"which file handles auth?", false},
		{"what does calcTax do?", false},
		{"explain the redaction rule", false},
		{"thanks", false},
		{"", false},
	}
	for _, c := range cases {
		if got := taskWantsChange(c.task); got != c.want {
			t.Errorf("taskWantsChange(%q) = %v, want %v", c.task, got, c.want)
		}
	}
}

// The incident this rail exists for: a task listing defects to fix, a run that
// explores and looks, a finish with the tree untouched — and a verify that was
// already green, which turns the whole thing into a reported SUCCESS.
//
// NEGATIVE CONTROL: drop the rail from the finish branch and this test fails —
// the run ends at the first finish, reporting success, having edited nothing.
func TestNoEditFinishIsRefusedOnceOnAChangeTask(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.txt"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "looked at it"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "nothing needed changing because the rule already applies"}})},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult(), passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix the contrast in the matchup bar")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := rep.RailFires[string(RailNoEditFinish)]; n != 1 {
		t.Errorf("rail fired %d times, want exactly 1 (it is one-shot)", n)
	}
	if n := len(srv.Requests()); n != 3 {
		t.Errorf("requests = %d, want 3 (read, refused finish, accepted finish)", n)
	}
	// The second finish is honoured: a model that genuinely has nothing to change
	// must be able to say so and stop, or the rail is a trap rather than a check.
	if rep.Reason != ReasonSuccess {
		t.Errorf("reason = %q, want success on the second finish", rep.Reason)
	}
	var sawCorrective bool
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "has not changed a single file") {
			sawCorrective = true
		}
	}
	if !sawCorrective {
		t.Error("the corrective never reached the model")
	}
}

// A question finishing without an edit is CORRECT, and must cost nothing.
func TestNoEditFinishDoesNotFireOnAQuestion(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.txt"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "auth.go handles it"}})},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "which file handles auth?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := rep.RailFires[string(RailNoEditFinish)]; n != 0 {
		t.Errorf("rail fired %d times on a question, want 0", n)
	}
	if n := len(srv.Requests()); n != 2 {
		t.Errorf("requests = %d, want 2 — a question must cost no extra turn", n)
	}
}

// A run that DID edit finishes immediately: the rail must never add a turn to a
// run that was already doing its job.
func TestNoEditFinishDoesNotFireAfterAnEdit(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.txt", "diff": "x"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "changed a.txt"}})},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult(), passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix the contrast in the matchup bar")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := rep.RailFires[string(RailNoEditFinish)]; n != 0 {
		t.Errorf("rail fired %d times after a real edit, want 0", n)
	}
}

// The shape the LIVE incident actually took, and the one the first version of
// this rail missed: the model never calls finish at all. It runs tools, then
// stops with a prose turn narrating what it found. confirm-finish spends its one
// nudge on "you did not call finish"; the model answers with more prose; the run
// ends as ReasonAnswered with a green verify and an untouched tree.
//
// The session record is what identified it: `Outcome: answered after 8 step(s).
// Verify: exit=0 passed=true.` A finish-TOOL stop with a passing verify returns
// ReasonSuccess, so ReasonAnswered + green verify can only be the prose path.
//
// NEGATIVE CONTROL: remove the rail from the prose-stop branch and this fails —
// the run ends one request earlier, having edited nothing.
func TestNoEditStopIsRefusedOnTheProsePath(t *testing.T) {
	prose := func(text string) llmtest.Mock { return llmtest.Mock{Body: proseResp(t, text)} }
	narrate := prose("The matchup bar subtitle is dark grey on dark green, so it is unreadable.")
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.txt"}})},
		// FOUR prose turns, not one. A bare prose reply to a native-FC request is
		// first treated as a recoverable no-usable-content hiccup and retried, so a
		// turn only reaches the stop branch after the model has narrated more than
		// once. confirm-finish then spends the first stop that gets there, and THIS
		// rail answers the second — which is exactly the live sequence.
		narrate, narrate, narrate, narrate,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.txt", "diff": "x"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "fixed the contrast"}})},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult(), passResult(), passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix the contrast in the matchup bar")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := rep.RailFires[string(RailNoEditFinish)]; n != 1 {
		t.Fatalf("no-edit rail fired %d times on the prose path, want 1 (rails: %v)", n, rep.RailFires)
	}
	if n := rep.RailFires[string(RailConfirmFinish)]; n != 1 {
		t.Errorf("confirm-finish fired %d times, want 1 — the two rails answer different questions and must not share a budget", n)
	}
	if rep.Reason != ReasonSuccess {
		t.Errorf("reason = %q, want success: the nudge should have driven a real edit", rep.Reason)
	}
}
