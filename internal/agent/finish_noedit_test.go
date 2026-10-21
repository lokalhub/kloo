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

// TestChangeRailsApply pins the predicate in isolation: changeRailsApply is
// mayWrite && taskWantsChange, i.e. the WRITE SCOPE outranks the TASK TEXT.
//
// The incident: a validation run launched with `--allow 'nothing/**'` and a prompt
// beginning "You are the VALIDATOR. Do not edit any file." degenerated into four
// repeated finish calls, each carrying the whole answer. The prompt QUOTES the spec
// it validates, so taskWantsChange matched 26 change verbs in it and the no-edit
// rail told a validator to go and make edits every write tool would refuse. The task
// text can never be a reliable signal, because a prompt that reviews code
// necessarily contains the vocabulary of changing code. The write scope can be, and
// it is already known.
//
// taskWantsChange itself is unchanged and still correct about the text — the text
// was never the problem. Only the rails that DEMAND a change are wrong when no
// change is possible; the confirm-finish rail stays sensible for a read-only run.
func TestChangeRailsApply(t *testing.T) {
	const changeTask = "add a cart resolver and fix the rounding"
	const questionTask = "what does the scope policy do?"
	cases := []struct {
		name     string
		task     string
		mayWrite bool
		want     bool
	}{
		// The gate this pins: a change-shaped task cannot be nudged toward a change
		// it is not permitted to make.
		{name: "change task, scope refuses every write", task: changeTask, mayWrite: false, want: false},
		{name: "change task, scope leaves something writable", task: changeTask, mayWrite: true, want: true},
		// A question never asks for the tree to change, so neither value arms it.
		{name: "question task, scope refuses every write", task: questionTask, mayWrite: false, want: false},
		{name: "question task, scope leaves something writable", task: questionTask, mayWrite: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := changeRailsApply(tc.task, tc.mayWrite); got != tc.want {
				t.Errorf("changeRailsApply(%q, mayWrite=%v) = %v, want %v", tc.task, tc.mayWrite, got, tc.want)
			}
		})
	}
	// The two halves must stay independently meaningful: changeRailsApply only
	// suppresses what taskWantsChange already decided, so a change task has to read
	// as a change task here. If this fails, the row above is vacuous and the test is
	// pinning nothing.
	if !taskWantsChange(changeTask) {
		t.Errorf("taskWantsChange(%q) = false; the mayWrite=true row above asserts nothing", changeTask)
	}
	if taskWantsChange(questionTask) {
		t.Errorf("taskWantsChange(%q) = true; the question rows above assert nothing", questionTask)
	}
}
