package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// The rails were tuned where nobody is watching, and ending a flailing run there is
// cheap. In front of a person it is the expensive outcome — they can see it is stuck
// and they have Esc. These tests pin the asymmetry: identical signal, different
// outcome, and headless unchanged.

// TestWatchedChurnKeepsGoing: a churn trip with a human at the prompt hands the
// model a corrective and continues instead of ending the run.
func TestWatchedChurnKeepsGoing(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.go"}})},
		// after the rescue the model does something else and finishes
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"run_command", map[string]any{"command": "go test ./...", "path": "a.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "found the real cause in b.go"}})},
	)
	churn := &stubChurn{churnAfter: 1, kind: ChurnRepeatedEdit}
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, churn)
	loop.Watched = true
	var notices []string
	loop.OnNotice = func(s string) { notices = append(notices, s) }

	rep, err := loop.Run(context.Background(), "fix the failing test")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonChurn {
		t.Fatalf("a watched run must not END on churn; reason = %q", rep.Reason)
	}
	if n := rep.RailFires[string(RailWatchedContinue)]; n < 1 {
		t.Errorf("the rescue should be tallied, fires = %v", rep.RailFires)
	}
	if len(notices) == 0 {
		t.Errorf("the user must be told the run was stuck and carried on")
	}
	if !strings.Contains(strings.Join(notices, " "), "going in circles") {
		t.Errorf("notice should say what happened in plain words: %q", notices)
	}
}

// TestUnwatchedChurnStillStops: headless and kloo-bench keep the hard stop, so no
// measured arm shifts under this change.
func TestUnwatchedChurnStillStops(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.go"}})},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{churnAfter: 1, kind: ChurnRepeatedEdit})
	// Watched deliberately left false.
	rep, err := loop.Run(context.Background(), "fix the failing test")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonChurn {
		t.Fatalf("an unwatched run must still stop on churn, got %q", rep.Reason)
	}
	if n := rep.RailFires[string(RailWatchedContinue)]; n != 0 {
		t.Errorf("no rescue may happen unwatched, fires = %v", rep.RailFires)
	}
}

// TestWatchedRescuesAreBounded: a model that will never converge must still stop —
// at the same rail, a few turns later. maxWatchedRailContinues rescues, then the
// churn stop stands.
func TestWatchedRescuesAreBounded(t *testing.T) {
	// Every turn is a read, and churn trips on every turn: the only way this run can
	// end is the bound.
	mocks := make([]llmtest.Mock, 0, 40)
	for i := 0; i < 40; i++ {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.go"}})})
	}
	srv := llmtest.Sequence(t, mocks...)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{churnAfter: 1, kind: ChurnRepeatedFailure})
	loop.Watched = true

	rep, err := loop.Run(context.Background(), "fix the failing test")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonChurn {
		t.Fatalf("the bound must still end the run on churn, got %q", rep.Reason)
	}
	if n := rep.RailFires[string(RailWatchedContinue)]; n != maxWatchedRailContinues {
		t.Errorf("rescues = %d, want exactly %d (then the stop stands)", n, maxWatchedRailContinues)
	}
}

// TestChurnRescueCorrectiveAsksForADifferentMove: the model reached the rail because
// repeating did not work, so the corrective must rule the repetition out and name
// the alternatives — not just say "try again".
func TestChurnRescueCorrectiveAsksForADifferentMove(t *testing.T) {
	msg := churnRescueCorrective(ChurnRepeatedEdit, "edit_file a.go\nwith the same diff", map[string]bool{"a.go": true})
	for _, want := range []string{"same edit", "a.go", "have NOT touched", "finish"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("corrective should mention %q:\n%s", want, msg.Content)
		}
	}
	// The artifact can be long; only its first line belongs in a prompt.
	if strings.Contains(msg.Content, "with the same diff") {
		t.Errorf("the whole artifact was pasted into the prompt:\n%s", msg.Content)
	}
}

// TestWatchedRunOutlivesTheRepeatedCallAbort is the rail a watched run dies on most
// often, and the one an end-to-end tmux run actually hit: the model locks onto ONE
// call and fires it until repeatAbortRounds. Watched and unwatched are driven with
// the IDENTICAL script, so the only difference measured is how long each survives.
func TestWatchedRunOutlivesTheRepeatedCallAbort(t *testing.T) {
	script := func() []llmtest.Mock {
		mocks := make([]llmtest.Mock, 0, 60)
		for i := 0; i < 60; i++ {
			// A MUTATING call, repeated identically: the arm with the abort.
			mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"run_command", map[string]any{"command": "go test ./...", "path": "a.go"}})})
		}
		return mocks
	}

	run := func(watched bool) *Report {
		loop, _ := newLoop(t, llmtest.Sequence(t, script()...), nil, &stubBudget{tripAt: 100}, &stubChurn{})
		loop.Watched = watched
		rep, err := loop.Run(context.Background(), "run the tests")
		if err != nil {
			t.Fatalf("Run(watched=%v): %v", watched, err)
		}
		return rep
	}

	plain, watched := run(false), run(true)

	// Both still stop — the bound holds, a hopeless model is not humoured forever.
	if plain.Reason != ReasonChurn || watched.Reason != ReasonChurn {
		t.Fatalf("both should end as churn, got %q and %q", plain.Reason, watched.Reason)
	}
	if n := watched.RailFires[string(RailWatchedContinue)]; n != maxWatchedRailContinues {
		t.Errorf("watched rescues = %d, want %d: %v", n, maxWatchedRailContinues, watched.RailFires)
	}
	if n := plain.RailFires[string(RailWatchedContinue)]; n != 0 {
		t.Errorf("unwatched must never rescue, fires = %v", plain.RailFires)
	}
	// And the rescues have to BUY something: more turns at the task.
	if watched.Steps <= plain.Steps {
		t.Errorf("watched run took %d steps, unwatched %d — the rescue bought nothing", watched.Steps, plain.Steps)
	}
}

// TestEditFailRescueTellsItHowToFixTheEdit: edits that never apply are usually one
// fixable mistake (a SEARCH block that does not match byte for byte), so the
// corrective has to name that rather than say "try something else".
func TestEditFailRescueTellsItHowToFixTheEdit(t *testing.T) {
	msg := churnRescueCorrective(ChurnEditFailed, "edit_file kept failing to apply on a.go (3 attempts, no valid edit landed)", nil)
	for _, want := range []string{"BYTE FOR BYTE", "indentation", "write_file"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("edit-fail corrective should mention %q:\n%s", want, msg.Content)
		}
	}
}
