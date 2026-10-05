package agent

import (
	"fmt"
	"strings"
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
// summary — UNTIL the summary hits its own budget (summaryBudgetFrac), at which
// point the oldest incidental entries collapse into a counted placeholder.
//
// Append-only was the v0.25.5 invariant and it is what keeps the prefix cacheable,
// but taken alone it made the summary unbounded: compaction folded 161 messages
// into 161 entries of the same size and reclaimed nothing. This test therefore
// pins the append-only property BELOW the summary budget, where it is what matters
// for the cache, and workingset_test.go / ws_assemble_test.go own the bounded
// behaviour above it. The cap is switched off here so the subject is not masked.
func TestFoldOnlyAppendsAsHistoryGrows(t *testing.T) {
	t.Cleanup(func() { SetWorkingSetTokens(0) })
	SetWorkingSetTokens(-1)
	w := NewWorkingMemory()
	in := shedPressureInput()
	if _, err := w.Assemble(in); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	before := append([]string(nil), w.foldedEntries...)

	// More history arrives. Only the newly-cold part may be folded.
	//
	// The new turns carry a DISTINCT marker. Appending a second copy of
	// shedPressureInput() produces entries textually identical to the first copy's,
	// which makes "is this entry a survivor or a new arrival?" undecidable and lets
	// the assertion below pass without testing anything.
	grown := in
	grown.Convo = append([]llm.Message{}, in.Convo...)
	for i := range 40 {
		grown.Convo = append(grown.Convo,
			assistantMsg(fmt.Sprintf("later turn %d: still inspecting", i)),
			userMsg(fmt.Sprintf("tool read_file result:\n%s",
				strings.Repeat(fmt.Sprintf("LATER line %d of a long file dump\n", i), 12))),
		)
	}
	if _, err := w.Assemble(grown); err != nil {
		t.Fatalf("assemble grown: %v", err)
	}
	// The invariant that matters for the prefix cache is that surviving entries are
	// never REWRITTEN or reordered — not that the list only ever grows. The summary
	// may now shed its oldest incidental entries when it exceeds its own budget, so
	// what remains must be a SUBSEQUENCE of what was there, in order, byte-identical.
	// A rewrite would move the prefix above the tail and cost a full re-prefill;
	// a bounded drop from the front does not.
	was := map[string]bool{}
	for _, e := range before {
		was[e] = true
	}
	// Survivors keep their relative order, and nothing that was there is rewritten:
	// every current entry is either one of `before` (unchanged) or a new arrival,
	// and the survivors form an in-order subsequence of `before`.
	j, survivors := 0, 0
	for _, e := range w.foldedEntries {
		if !was[e] {
			continue // newly folded
		}
		survivors++
		for j < len(before) && before[j] != e {
			j++
		}
		if j == len(before) {
			t.Fatalf("survivor %q is out of order relative to the previous summary:\n before: %q\n after:  %q",
				e, before, w.foldedEntries)
		}
		j++
	}
	if survivors == 0 {
		t.Fatalf("no entry survived the growth; the whole summary was rewritten:\n before: %q\n after:  %q",
			before, w.foldedEntries)
	}
	// And the drop must be bounded, not wholesale: something from the first
	// assembly has to still be there for the model to use.
	if len(w.foldedEntries) == 0 {
		t.Fatal("summary emptied itself")
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
