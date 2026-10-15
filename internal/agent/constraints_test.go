package agent

import (
	"strings"
	"testing"
)

// TestTaskConstraintsRestatesWhatTheUserForbade is the regression guard for a
// measured failure: the task said "do not change the template", and the model
// changed the template — adding an inline style attribute that also undid a layout
// fix from the previous turn. The sentence was in the first user message, thousands
// of tokens behind by the time the edit happened.
func TestTaskConstraintsRestatesWhatTheUserForbade(t *testing.T) {
	task := "Fix the failing unit test in login.page.spec.ts. " +
		"Diagnose it with evidence rather than guesses. " +
		"Do not weaken the test, and do not change the template."

	got := taskConstraints(task)
	if got == "" {
		t.Fatal("a task with two explicit prohibitions produced no constraints block")
	}
	if !strings.Contains(got, "do not change the template") {
		t.Errorf("the prohibition that was broken is missing:\n%s", got)
	}
	// Verbatim, not paraphrased: a restatement that drifts is worse than none.
	if !strings.Contains(got, "Do not weaken the test") {
		t.Errorf("constraints should be quoted as written:\n%s", got)
	}
	// Ordinary instructions are not constraints; including them is how the block
	// becomes noise.
	if strings.Contains(got, "Diagnose it with evidence") {
		t.Errorf("a non-prohibition sentence leaked in:\n%s", got)
	}
}

func TestTaskConstraintsCoversTheOtherShapes(t *testing.T) {
	for _, tc := range []struct{ task, want string }{
		{"Change only the stylesheet.", "only the stylesheet"},
		{"Refactor it without touching the tests.", "without touching the tests"},
		{"Never modify a migration that has shipped.", "Never modify a migration"},
		{"- do not add a dependency\n- keep it in one file", "do not add a dependency"},
	} {
		if got := taskConstraints(tc.task); !strings.Contains(got, tc.want) {
			t.Errorf("task %q: constraints %q should contain %q", tc.task, got, tc.want)
		}
	}
}

// TestTaskConstraintsEmptyForOrdinaryWork: most tasks forbid nothing, and those
// must pay nothing — no heading, no tokens, no change to the cacheable prefix.
func TestTaskConstraintsEmptyForOrdinaryWork(t *testing.T) {
	for _, task := range []string{
		"Add a health endpoint to the worker and a test for it.",
		"The build is red. Fix it.",
		"",
	} {
		if got := taskConstraints(task); got != "" {
			t.Errorf("task %q should produce no constraints block, got:\n%s", task, got)
		}
	}
}

// TestTaskConstraintsStaysShort: a wall of constraints recreates the dilution.
func TestTaskConstraintsStaysShort(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("Do not touch the file number ")
		b.WriteString(strings.Repeat("x", 15))
		b.WriteString(".\n")
	}
	got := taskConstraints(b.String())
	if n := strings.Count(got, "\n- "); n > 4 {
		t.Errorf("constraints should cap at 4 items, got %d", n)
	}
	if len(got) > 800 {
		t.Errorf("constraints block too long: %d bytes", len(got))
	}
}
