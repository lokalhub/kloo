package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// TestLoopRepetitionRailNudgesRepeatedReads: a model locked onto the SAME
// read_file call is CORRECTED by the repetition rail — repeatedly — but the run is
// no longer ended by it. (Before the read-only downgrade this test asserted a
// ChurnRepeatedCall abort with Class "repeated_read_file"; that abort is exactly
// what the measurement showed was cutting off recoverable runs.)
func TestLoopRepetitionRailNudgesRepeatedReads(t *testing.T) {
	read := tcSpec{"read_file", map[string]any{"path": "tab1.page.scss"}}
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, read)},
		llmtest.Mock{Body: toolResp(t, 5, read)},
		llmtest.Mock{Body: toolResp(t, 5, read)},
		llmtest.Mock{Body: toolResp(t, 5, finishCall)},
	)
	// nil verifier ⇒ no success/stall path interferes; stubChurn never fires, so
	// ONLY the repetition rail could end this run — and it no longer does.
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.RepeatNudgeRounds = 2
	loop.RepeatAbortRounds = 3

	rep, err := loop.Run(context.Background(), "center the tab labels")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonChurn {
		t.Fatalf("a repeated READ must not end the run as churn; churn=%+v", rep.Churn)
	}
	if rep.Churn != nil {
		t.Fatalf("no churn evidence expected, got %+v", rep.Churn)
	}
	if rep.Steps != 4 {
		t.Errorf("steps = %d, want 4 (three reads, then finish)", rep.Steps)
	}
	if rep.ToolCounters.RepeatedReadFile != 2 {
		t.Errorf("repeated_read_file = %d, want 2 — the diagnostic counter still counts",
			rep.ToolCounters.RepeatedReadFile)
	}
	// The corrective nudge must still have been injected into the transcript.
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
		t.Error("expected a corrective nudge in the transcript")
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

// finishCall is the model's explicit terminator, used by the repetition tests to
// end a run that the rail deliberately no longer ends.
var finishCall = tcSpec{"finish", map[string]any{"summary": "done"}}

// repeatMocks scripts n identical tool calls followed by finish. llmtest.Sequence
// repeats its LAST mock, so finish keeps being served if the loop asks again.
func repeatMocks(t *testing.T, n int, call tcSpec) []llmtest.Mock {
	t.Helper()
	mocks := make([]llmtest.Mock, 0, n+1)
	for range n {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, call)})
	}
	return append(mocks, llmtest.Mock{Body: toolResp(t, 5, finishCall)})
}

// registerReadOnlyTools adds the read-only builtins newLoop does not register, so
// the downgrade can be exercised across the whole isReadOnlyTool set rather than
// on read_file alone.
func registerReadOnlyTools(loop *Loop, calls *[]tools.Call) {
	for _, name := range []string{tools.NameListDir, tools.NameReadDir, tools.NameSearch, tools.NameCommandOutput} {
		loop.Registry.Register(recordTool{name: name, calls: calls})
	}
}

// TestRepeatedReadFileCompletesRun: the canonical benchmark failure — a model that
// re-reads one file many times before finding its way to an answer. Eight
// identical read_file calls no longer zero the run: it reaches the model's finish
// and verifies green. The repeated_read_file failure class is gone with the abort.
func TestRepeatedReadFileCompletesRun(t *testing.T) {
	read := tcSpec{"read_file", map[string]any{"path": "tab1.page.scss"}}
	srv := llmtest.Sequence(t, repeatMocks(t, 8, read)...)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "center the tab labels")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonChurn {
		t.Fatalf("8 identical reads must not churn; churn=%+v", rep.Churn)
	}
	if rep.Reason != ReasonSuccess {
		t.Errorf("reason = %q, want success (finish + a green verify)", rep.Reason)
	}
	if rep.Churn != nil && rep.Churn.Class == "repeated_read_file" {
		t.Errorf("the repeated_read_file failure class must be gone, got %+v", rep.Churn)
	}
	if rep.Steps != 9 {
		t.Errorf("steps = %d, want 9 (eight reads, then finish)", rep.Steps)
	}
	if rep.ToolCounters.RepeatedReadFile != 7 {
		t.Errorf("repeated_read_file counter = %d, want 7 — the diagnostic signal survives",
			rep.ToolCounters.RepeatedReadFile)
	}
}

// TestRepeatedReadOnlyToolsDowngraded: the downgrade is keyed on isReadOnlyTool,
// not on the literal "read_file" — every read-only builtin behaves the same way.
func TestRepeatedReadOnlyToolsDowngraded(t *testing.T) {
	cases := []struct {
		tool string
		args map[string]any
	}{
		{tools.NameReadFile, map[string]any{"path": "tab1.page.scss"}},
		{tools.NameListDir, map[string]any{"path": "src"}},
		{tools.NameReadDir, map[string]any{"path": "src"}},
		{tools.NameSearch, map[string]any{"query": "ion-content"}},
		{tools.NameCommandOutput, map[string]any{"id": "bg1"}},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			srv := llmtest.Sequence(t, repeatMocks(t, 8, tcSpec{tc.tool, tc.args})...)
			loop, calls := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
			registerReadOnlyTools(loop, calls)

			rep, err := loop.Run(context.Background(), "inspect the styles")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rep.Reason == ReasonChurn {
				t.Fatalf("8 identical %s calls must not churn; churn=%+v", tc.tool, rep.Churn)
			}
			if rep.Churn != nil {
				t.Fatalf("no churn evidence expected for %s, got %+v", tc.tool, rep.Churn)
			}
			if rep.Steps != 9 {
				t.Errorf("steps = %d, want 9 (eight %s calls, then finish)", rep.Steps, tc.tool)
			}
		})
	}
}

// TestRepeatedEditFileStillHalts: the mutating side of the rail is untouched.
// Re-firing the same edit is not exploration, and it still ends the run as churn
// with the repeated-call evidence.
func TestRepeatedEditFileStillHalts(t *testing.T) {
	edit := tcSpec{"edit_file", map[string]any{"path": "tab1.page.scss"}}
	srv := llmtest.Sequence(t, repeatMocks(t, 8, edit)...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})

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
	if rep.Steps != DefaultRepeatAbortRounds {
		t.Errorf("steps = %d, want %d (halts on the abort round)", rep.Steps, DefaultRepeatAbortRounds)
	}
}

// TestRepeatedRunCommandStillHalts: run_command stays OUT of the downgraded class
// even when the command itself only inspects — isReadOnlyTool deliberately
// excludes it, because a repeated shell call still executes a process.
func TestRepeatedRunCommandStillHalts(t *testing.T) {
	cmd := tcSpec{"run_command", map[string]any{"command": "go test ./..."}}
	srv := llmtest.Sequence(t, repeatMocks(t, 8, cmd)...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "run the suite")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonChurn {
		t.Fatalf("reason = %q, want churn", rep.Reason)
	}
	if rep.Churn == nil || rep.Churn.Kind != ChurnRepeatedCall {
		t.Fatalf("churn kind = %v, want repeated-call", rep.Churn)
	}
}

// TestDowngradedRepeatReNudges: a downgraded repeat is not silently tolerated —
// the corrective is re-armed and fires at every multiple of repeatNudgeRounds, so
// the model keeps getting a push instead of spinning unheard to the explore ceiling.
func TestDowngradedRepeatReNudges(t *testing.T) {
	read := tcSpec{"read_file", map[string]any{"path": "tab1.page.scss"}}
	srv := llmtest.Sequence(t, repeatMocks(t, 6, read)...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.RepeatNudgeRounds = 2

	rep, err := loop.Run(context.Background(), "center the tab labels")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := rep.RailFires[string(RailRepeatedCall)]; got != 3 {
		t.Errorf("rail_fires[%q] = %d, want 3 (streaks 2, 4 and 6)", RailRepeatedCall, got)
	}
	var correctives int
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "STOP — you have called") {
			correctives++
		}
	}
	if correctives < 2 {
		t.Errorf("corrective text appears %d times in the transcript, want at least 2", correctives)
	}
}

// TestRepeatedReadStillTerminates: the rail is downgraded, not removed. A model
// that ONLY re-reads, forever, is still stopped — by the exploration rail, at its
// ceiling. The step count is asserted exactly so a change to that ceiling (which
// is now the backstop for read spins) is caught here rather than in production.
func TestRepeatedReadStillTerminates(t *testing.T) {
	read := tcSpec{"read_file", map[string]any{"path": "tab1.page.scss"}}
	srv := llmtest.JSON(t, toolResp(t, 5, read)) // the same read, served forever
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 500}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "center the tab labels")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered (the exploration rail's calm stop)", rep.Reason)
	}
	if rep.Steps != DefaultExploreAbortRounds {
		t.Errorf("steps = %d, want %d (the exploration-rail ceiling)", rep.Steps, DefaultExploreAbortRounds)
	}
}

// TestTunedAbortRoundsStillApplyToEdits: the knob wired through config reaches the
// abort that SURVIVES the downgrade.
func TestTunedAbortRoundsStillApplyToEdits(t *testing.T) {
	edit := tcSpec{"edit_file", map[string]any{"path": "tab1.page.scss"}}
	srv := llmtest.Sequence(t, repeatMocks(t, 8, edit)...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.RepeatAbortRounds = 3

	rep, err := loop.Run(context.Background(), "center the tab labels")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonChurn {
		t.Fatalf("reason = %q, want churn", rep.Reason)
	}
	if rep.Steps != 3 {
		t.Errorf("steps = %d, want 3 (RepeatAbortRounds=3)", rep.Steps)
	}
}

// TestRepeatedEditEvidenceMatchesBaseline: the surviving mutating abort reports
// EXACTLY what it reported before the downgrade. The literals below were captured
// by running this same scripted harness against v0.16.7 (commit 60bf66a) on
// 2026-09-11; a live-binary diff would have been neither reproducible nor
// deterministic.
func TestRepeatedEditEvidenceMatchesBaseline(t *testing.T) {
	edit := tcSpec{"edit_file", map[string]any{"path": "tab1.page.scss"}}
	srv := llmtest.Sequence(t, repeatMocks(t, 8, edit)...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "center the tab labels")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Churn == nil {
		t.Fatalf("expected churn evidence, reason = %q", rep.Reason)
	}
	// v0.16.7 (60bf66a), captured 2026-09-11: Kind repeated-call, no Class, no Tool,
	// and the short "what was repeated ×N" artifact.
	if got, want := rep.Churn.Kind, ChurnRepeatedCall; got != want {
		t.Errorf("Kind = %q, want %q", got, want)
	}
	if got, want := rep.Churn.Class, ""; got != want {
		t.Errorf("Class = %q, want %q", got, want)
	}
	if got, want := rep.Churn.Tool, ""; got != want {
		t.Errorf("Tool = %q, want %q", got, want)
	}
	if got, want := rep.Churn.Artifact, "edit_file tab1.page.scss (×6)"; got != want {
		t.Errorf("Artifact = %q, want %q", got, want)
	}
}
