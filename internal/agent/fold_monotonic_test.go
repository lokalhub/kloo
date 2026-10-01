package agent

import (
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

// THE PROPERTY THE WHOLE CHANGE EXISTS FOR. Once the compaction path is entered it
// runs on EVERY turn, because the history only grows. Before this, each of those
// turns re-derived the summary from a growing cold set, so the block above the tail
// changed every turn and invalidated the entire prompt below it.
//
// Now a turn that does not need to advance the boundary must leave the summary
// byte-identical, which makes the turn a pure append.
func TestFoldBoundaryHoldsWhenItNeedNotAdvance(t *testing.T) {
	w := NewWorkingMemory()
	in := shedPressureInput()

	if _, err := w.Assemble(in); err != nil {
		t.Fatalf("first assemble: %v", err)
	}
	folded, entries, compactions := w.folded, append([]string(nil), w.foldedEntries...), w.compactions
	if folded == 0 {
		t.Fatal("premise broken: this input must force a fold")
	}

	// Re-assemble the SAME input: nothing new arrived, so nothing may be re-folded.
	if _, err := w.Assemble(in); err != nil {
		t.Fatalf("second assemble: %v", err)
	}
	if w.folded != folded {
		t.Errorf("boundary moved on an unchanged input: %d → %d", folded, w.folded)
	}
	if w.compactions != compactions {
		t.Errorf("compaction counted again for no new folding: %d → %d", compactions, w.compactions)
	}
	if len(w.foldedEntries) != len(entries) {
		t.Fatalf("summary length changed: %d → %d", len(entries), len(w.foldedEntries))
	}
	for i := range entries {
		if w.foldedEntries[i] != entries[i] {
			t.Fatalf("summary entry %d was rewritten:\n before: %q\n after:  %q", i, entries[i], w.foldedEntries[i])
		}
	}
}

// Already-folded entries are never recomputed, so growth only ever APPENDS to the
// summary. If this regresses, the prefix above the tail starts moving again.
func TestFoldOnlyAppendsAsHistoryGrows(t *testing.T) {
	w := NewWorkingMemory()
	in := shedPressureInput()
	if _, err := w.Assemble(in); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	before := append([]string(nil), w.foldedEntries...)

	// More history arrives. Only the newly-cold part may be folded.
	grown := in
	grown.Convo = append(append([]llm.Message{}, in.Convo...), shedPressureInput().Convo[1:]...)
	if _, err := w.Assemble(grown); err != nil {
		t.Fatalf("assemble grown: %v", err)
	}
	if len(w.foldedEntries) < len(before) {
		t.Fatalf("summary SHRANK: %d → %d", len(before), len(w.foldedEntries))
	}
	for i := range before {
		if w.foldedEntries[i] != before[i] {
			t.Fatalf("entry %d rewritten as history grew:\n before: %q\n after:  %q", i, before[i], w.foldedEntries[i])
		}
	}
}

// Hysteresis means the boundary overshoots what is minimally required, so the next
// turns fit without moving it. A value of 1.0 would be no hysteresis at all.
func TestLowWaterFracIsRealHysteresis(t *testing.T) {
	if lowWaterFrac >= 1.0 {
		t.Fatalf("lowWaterFrac = %v — at 1.0 the boundary advances to exactly the limit "+
			"and the next read dump crosses it again, which is the oscillation this fixes", lowWaterFrac)
	}
	if lowWaterFrac <= 0.2 {
		t.Fatalf("lowWaterFrac = %v — folding this deep discards history the model still needs", lowWaterFrac)
	}
}
