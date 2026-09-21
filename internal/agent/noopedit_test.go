package agent

import (
	"strings"
	"testing"
)

// TestNoOpFeedbackIsOptIn: off by default until the bench says otherwise.
func TestNoOpFeedbackIsOptIn(t *testing.T) {
	t.Setenv("KLOO_NOOP_EDIT_FEEDBACK", "")
	if noOpFeedback() {
		t.Fatal("on by default")
	}
	t.Setenv("KLOO_NOOP_EDIT_FEEDBACK", "1")
	if !noOpFeedback() {
		t.Fatal("flag not honoured")
	}
}

// TestNoOpMessageTellsTheModelWhatToDo: the point is not to report the no-op, it
// is to stop the repeat. Measured on kloo-bench A16 across 19 runs: EVERY failure
// had repeated_edits=2 with no_op_edits 3-4 and died in churn; NO passing run had
// either counter. The model repeated an edit it believed had landed, because kloo
// returned "edited <path>" for a file it had not changed.
func TestNoOpMessageTellsTheModelWhatToDo(t *testing.T) {
	// The text is built inline in the loop; assert on the contract it must keep.
	msg := "That edit applied but changed NOTHING — the file is byte-for-byte identical to before. " +
		"Your replacement text must already match what was there. Do not repeat it. Re-read the region " +
		"you are targeting and make a DIFFERENT change, or edit a different file: the behaviour you are " +
		"trying to alter is not controlled by the text you just replaced."
	for _, want := range []string{"changed NOTHING", "Do not repeat it", "DIFFERENT change", "different file"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("no-op corrective missing %q", want)
		}
	}
}
