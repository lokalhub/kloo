package agent

import (
	"strings"
	"testing"
)

// The live task shape: three enumerated defects, each with its own steps. The run
// wrote the gate for the first, never ran it, never touched the other two, and
// stopped as SUCCESS.
func TestTaskChecklistListsEnumeratedRequirements(t *testing.T) {
	task := "Do the three that remain. DEFECT 4, phone lobby rows. At 390x844 each row stacks into four lines so only two fit. " +
		"DEFECT 5, matchup bar. The subtitle is dark grey on dark green and unreadable on both widths. " +
		"DEFECT 6, phone board. The board ends near y=490 leaving 310px of empty white."
	got := taskChecklist(task)
	for _, want := range []string{"DEFECT 4", "DEFECT 5", "DEFECT 6", "phone lobby rows", "matchup bar", "phone board"} {
		if !strings.Contains(got, want) {
			t.Errorf("checklist is missing %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "\n  - ") + 1; n != 3 {
		t.Errorf("got %d items, want 3:\n%s", n, got)
	}
}

func TestTaskChecklistHandlesNumberedAndStepForms(t *testing.T) {
	for _, task := range []string{
		"1. Add the gate. 2) Make it fail. 3. Fix the page.",
		"STEP 1: write the assertion. STEP 2: fix it. STEP 3: prove it with a screenshot.",
	} {
		got := taskChecklist(task)
		if strings.Count(got, "\n  - ")+1 != 3 {
			t.Errorf("task %q gave:\n%s", task, got)
		}
	}
}

// Most tasks enumerate nothing and must keep exactly the behaviour they had — the
// generic corrective, not a half-built list.
func TestTaskChecklistIsEmptyForAnOrdinaryTask(t *testing.T) {
	for _, task := range []string{
		"fix the contrast in the matchup bar",
		"which file handles auth?",
		"Rewrite the lobby so it reads like chess.com.",
		"Bump the version to 1.2 and tag it.", // a lone "1.2" must not look like a list
	} {
		if got := taskChecklist(task); got != "" {
			t.Errorf("task %q should enumerate nothing, got:\n%s", task, got)
		}
	}
}

// A long item is quoted far enough to identify it and no further — eight of them
// have to stay scannable.
func TestTaskChecklistTruncatesLongItems(t *testing.T) {
	long := strings.Repeat("make the row shorter and tidier ", 20)
	// Items are separated by a sentence break, which is how a real task writes
	// them — the marker regex deliberately anchors there so a mid-sentence "step 2"
	// in prose does not start a list.
	got := taskChecklist("DEFECT 1, " + long + ". DEFECT 2, " + long)
	for _, line := range strings.Split(got, "\n") {
		if len(line) > checklistItemChars+40 {
			t.Errorf("line is %d chars, too long to scan: %q", len(line), line)
		}
	}
	if !strings.Contains(got, "…") {
		t.Error("a truncated item must say it was truncated")
	}
}

// The corrective names the list and insists on a per-item answer, because the
// generic version was already asked on the live run and answered yes.
func TestChecklistCorrectiveDemandsAPerItemAnswer(t *testing.T) {
	msg := checklistCorrective("  - DEFECT 4: rows\n  - DEFECT 5: contrast")
	for _, want := range []string{"DEFECT 4", "DEFECT 5", "one item at a time", "every item"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("corrective is missing %q: %s", want, msg.Content)
		}
	}
	if !strings.Contains(msg.Content, "a gate written but never run") {
		t.Error("the corrective should name the exact way the live run failed")
	}
}
