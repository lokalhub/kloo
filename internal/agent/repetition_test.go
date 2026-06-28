package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestLoopRepetitionRailHaltsIdenticalCalls: a model locked onto the SAME tool
// call (here read_file on one path, over and over — the canonical empty-file
// flail) is caught by the repetition rail even though stubChurn never fires and
// no edit/verify signal exists. It first injects a corrective nudge, then halts
// the run as ChurnRepeatedCall.
func TestLoopRepetitionRailHaltsIdenticalCalls(t *testing.T) {
	read := tcSpec{"read_file", map[string]any{"path": "tab1.page.scss"}}
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, read)},
		llmtest.Mock{Body: toolResp(t, 5, read)},
		llmtest.Mock{Body: toolResp(t, 5, read)},
		llmtest.Mock{Body: toolResp(t, 5, read)}, // spare; the rail should halt before this
	)
	// nil verifier ⇒ no success/stall path interferes; stubChurn never fires, so
	// ONLY the repetition rail can end this run.
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.RepeatNudgeRounds = 2
	loop.RepeatAbortRounds = 3

	rep, err := loop.Run(context.Background(), "center the tab labels")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonChurn {
		t.Fatalf("reason = %q, want churn", rep.Reason)
	}
	if rep.Churn == nil || rep.Churn.Kind != ChurnRepeatedCall {
		t.Fatalf("churn kind = %v, want repeated-call", rep.Churn)
	}
	if rep.Churn.Class != "repeated_read_file" || rep.Churn.Tool != "read_file" {
		t.Fatalf("repeated-read detail missing: %+v", rep.Churn)
	}
	if rep.Steps != 3 {
		t.Errorf("steps = %d, want 3 (halts on the 3rd identical call)", rep.Steps)
	}
	if !strings.Contains(rep.Churn.Artifact, "read_file") {
		t.Errorf("artifact should name the repeated call, got %q", rep.Churn.Artifact)
	}
	// The one-shot corrective nudge must have been injected into the transcript.
	var nudged bool
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "times in a row") &&
			strings.Contains(m.Content, "tab1.page.scss") &&
			strings.Contains(m.Content, "write_file") &&
			strings.Contains(m.Content, "edit_file") {
			nudged = true
		}
	}
	if !nudged {
		t.Error("expected a corrective nudge in the transcript before the abort")
	}
}

// TestLoopRepetitionRailIgnoresDistinctCalls: alternating DISTINCT calls are
// honest progress — the streak resets each time, so the rail never fires. The run
// ends on the model's finish, not as churn.
func TestLoopRepetitionRailIgnoresDistinctCalls(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.scss"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "b.scss"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.scss"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.RepeatNudgeRounds = 2
	loop.RepeatAbortRounds = 3

	rep, err := loop.Run(context.Background(), "inspect the styles")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonChurn {
		t.Fatalf("distinct calls must not churn; reason=%q artifact=%q", rep.Reason, rep.Churn.Artifact)
	}
}

// TestBuildRepairObservation_EmptyFile: an edit_file no-match against an EMPTY
// file gets a tailored message — "the file is EMPTY, use write_file" — instead of
// the generic (unsatisfiable) "make your SEARCH match the contents" instruction.
// This kills the empty-file read/edit flail at its seed.
func TestBuildRepairObservation_EmptyFile(t *testing.T) {
	root, path := writeTemp(t, "tab1.page.scss", "") // empty file
	diff := diffBlock("ion-content {\n", "ion-content { text-align: center;\n")

	msg, ok := buildRepairObservation(root, path, diff)
	if !ok {
		t.Fatal("expected ok=true for an empty-file edit failure")
	}
	if msg.Role != llm.RoleUser {
		t.Errorf("Role = %q, want %q", msg.Role, llm.RoleUser)
	}
	for _, want := range []string{"EMPTY", "write_file"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("empty-file observation missing %q\n---\n%s", want, msg.Content)
		}
	}
	// It must NOT hand back the impossible "match the contents exactly" instruction.
	if strings.Contains(msg.Content, "Fix this edit") {
		t.Errorf("empty-file path should not emit the generic match instruction\n---\n%s", msg.Content)
	}
}
