package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// ─── Phase 02 Task 02: finish always verifies; no stale green becomes success ──

// errEditNoMatch stands in for the engine's no-match failure: the edit tool ran and
// changed no bytes, so it is not a mutation.
var errEditNoMatch = errors.New("edit: no match for SEARCH block")

// TestFinishVerifiesEvenWithNoMutation: a run of read-only steps mutates nothing,
// so the verify gate skips every one of them — and `finish` still verifies. The
// finish-path verify is the one that decides success and is never gated.
func TestFinishVerifiesEvenWithNoMutation(t *testing.T) {
	cases := []struct {
		name       string
		result     VerifyResult
		wantReason Reason
	}{
		{name: "green finish is a success", result: passResult(), wantReason: ReasonSuccess},
		{name: "red finish is answered, never success", result: failResult(), wantReason: ReasonAnswered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := llmtest.Sequence(t, mocksFor(t,
				tcSpec{"read_file", map[string]any{"path": "a.go"}},
				tcSpec{"list_dir", map[string]any{"path": "."}},
				finishSpec,
			)...)
			v := &countingVerifier{results: []VerifyResult{tc.result}}
			loop := gateLoop(t, srv, v, &stubChurn{})

			rep, err := loop.Run(context.Background(), "just look around")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rep.ToolCounters.VerifyAttempts != 1 {
				t.Errorf("VerifyAttempts = %d, want exactly 1 — the reads skip, finish must still verify",
					rep.ToolCounters.VerifyAttempts)
			}
			if rep.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", rep.Reason, tc.wantReason)
			}
		})
	}
}

// TestFinishVerifiesAfterSkippedVerifies: a mutating step runs the verify, later
// read-only steps skip it, and `finish` verifies AGAIN rather than trusting the
// result from before the reads. A red finish yields ReasonAnswered, never success.
func TestFinishVerifiesAfterSkippedVerifies(t *testing.T) {
	cases := []struct {
		name       string
		atFinish   VerifyResult
		wantReason Reason
	}{
		// The finish path's own rule — unchanged by this phase — is "green verify at
		// finish ⇒ success", independent of the mid-loop gate's `edited` requirement.
		{name: "green at finish", atFinish: passResult(), wantReason: ReasonSuccess},
		{name: "red at finish", atFinish: failResult(), wantReason: ReasonAnswered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			read := tcSpec{"read_file", map[string]any{"path": "a.go"}}
			srv := llmtest.Sequence(t, mocksFor(t,
				tcSpec{"run_command", map[string]any{"command": "mv a.go b.go"}},
				read, read, finishSpec,
			)...)
			// Red on the mutating turn (so the run does not end there), then whatever
			// the case wants at finish. `edited` is false throughout — a run_command is
			// not an edit — so the mid-loop gate never fires and the reason is decided
			// entirely by the finish verify, which is the point of the test.
			v := &countingVerifier{results: []VerifyResult{failResult(), tc.atFinish}}
			loop := gateLoop(t, srv, v, &stubChurn{})

			rep, err := loop.Run(context.Background(), "move then review then finish")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rep.ToolCounters.VerifyAttempts != 2 {
				t.Errorf("VerifyAttempts = %d, want exactly 2 — one for the mutation, one at finish; "+
					"the two reads between them skip", rep.ToolCounters.VerifyAttempts)
			}
			if tc.wantReason == ReasonAnswered && rep.Reason == ReasonSuccess {
				t.Errorf("reason = success on a RED finish verify — the finish verify decides, and it failed")
			}
			if rep.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", rep.Reason, tc.wantReason)
			}
		})
	}
}

// TestNoSuccessOnStaleGreen: the adversarial case. A mutating step verifies GREEN;
// a read-only step then skips the verify, so the run is holding a green that is now
// STALE; meanwhile the tree is broken out of band — by something kloo never
// dispatched, which is why no gate could have caught it. `finish` re-checks and the
// run is not blessed.
func TestNoSuccessOnStaleGreen(t *testing.T) {
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"run_command", map[string]any{"command": "mv a.go b.go"}},
		tcSpec{"read_file", map[string]any{"path": "b.go"}},
		finishSpec,
	)...)
	// GREEN on the mutating turn; the read carries it forward untouched; RED at
	// finish — the tree changed underneath the run between the two.
	v := &countingVerifier{results: []VerifyResult{passResult(), failResult()}}
	loop := gateLoop(t, srv, v, &stubChurn{})

	rep, err := loop.Run(context.Background(), "move the file")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 2 {
		t.Fatalf("VerifyAttempts = %d, want 2 (the mutation and the finish re-check)", rep.ToolCounters.VerifyAttempts)
	}
	if rep.Reason == ReasonSuccess {
		t.Fatalf("reason = success — a run holding a stale green must not be blessed; "+
			"final verify was %+v", rep.FinalVerify)
	}
	if rep.FinalVerify.ExitCode == 0 || rep.FinalVerify.Passed {
		t.Errorf("final verify = %+v, want the RED finish result — the finish verify must be the decider",
			rep.FinalVerify)
	}
}

// TestMidLoopSuccessRequiresPostMutationVerify: whenever the mid-loop gate returns
// ReasonSuccess, the verify that authorised it ran at or after the last applied
// edit. Asserted over a recorded event order, not by reading the code.
func TestMidLoopSuccessRequiresPostMutationVerify(t *testing.T) {
	var events []string

	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "answer.txt"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "answer.txt"}})},
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "wrong\n", "right\n", 5)},
	)
	v := &countingVerifier{results: []VerifyResult{passResult()}}
	recorder := &eventVerifier{inner: v, events: &events}

	loop, _ := newRealEditLoop(t, srv, "answer.txt", "wrong\n", recorder, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.OnTool = func(call tools.Call, res tools.Result, err error) {
		if isEditTool(call.Name) && err == nil {
			events = append(events, "edit")
		}
	}

	rep, err := loop.Run(context.Background(), "make answer.txt say right")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success (this test is about HOW success was reached)", rep.Reason)
	}

	lastEdit, lastVerify := -1, -1
	for i, e := range events {
		switch e {
		case "edit":
			lastEdit = i
		case "verify":
			lastVerify = i
		}
	}
	if lastEdit < 0 || lastVerify < 0 {
		t.Fatalf("expected both an edit and a verify, got %v", events)
	}
	if lastVerify < lastEdit {
		t.Errorf("the verify that authorised success ran BEFORE the last applied edit: %v", events)
	}
}

// TestLayeredVerifyHooksStillRunAtFinish: precheck/postcheck gates still execute on
// the finish path with the gate in place — each exactly once.
func TestLayeredVerifyHooksStillRunAtFinish(t *testing.T) {
	pre := &recordingVerifier{name: "pre", result: pass("precheck")}
	post := &recordingVerifier{name: "post", result: pass("postcheck")}
	inner := &recordingVerifier{name: "verify", result: pass("npm test")}
	layered := NewLayeredVerifier([]Verifier{pre}, inner, []Verifier{post})

	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"read_file", map[string]any{"path": "a.go"}},
		finishSpec,
	)...)
	loop := gateLoop(t, srv, layered, &stubChurn{})

	rep, err := loop.Run(context.Background(), "look then finish")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 1 {
		t.Fatalf("VerifyAttempts = %d, want 1 (the finish verify)", rep.ToolCounters.VerifyAttempts)
	}
	if pre.called != 1 {
		t.Errorf("precheck ran %d times, want exactly 1 on the finish path", pre.called)
	}
	if post.called != 1 {
		t.Errorf("postcheck ran %d times, want exactly 1 on the finish path", post.called)
	}
	if inner.called != 1 {
		t.Errorf("inner verify ran %d times, want exactly 1", inner.called)
	}
}

// eventVerifier records the position of each verify relative to the edits recorded
// by the loop's OnTool hook, so ordering can be asserted rather than inferred.
type eventVerifier struct {
	inner  Verifier
	events *[]string
}

func (e *eventVerifier) Verify(ctx context.Context) VerifyResult {
	*e.events = append(*e.events, "verify")
	return e.inner.Verify(ctx)
}
