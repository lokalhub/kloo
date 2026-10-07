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

// TestHysteresisIsWorthItsValue measures what hysteresis actually controls —
// how often the fold boundary moves and how many prompt bytes stop being a
// byte-identical prefix of the previous turn when it does.
//
// This exists because a 4-pair partial bench sweep hinted that 0.60 was LOSING
// and the value should go back to 1.00. That hint was wrong, and it was nearly
// acted on. Measured deterministically over 60 turns at --ctx 131072:
//
//	frac   boundary moves   re-prefilled   peak change
//	1.00   30               3.7 MB         121 KB
//	0.85    6               0.8 MB         104 KB
//	0.70    3               0.5 MB          85 KB
//	0.60    3               0.4 MB          73 KB
//	0.50    2               0.3 MB          61 KB
//	0.35    2               0.3 MB          41 KB
//
// At 1.00 the boundary moves on every other turn — the exact oscillation the
// monotonic fold was built to stop — and costs ~9x the re-prefill. 0.60 sits on
// the flat part of the curve, so the precise value matters far less than not
// being 1.0.
//
// It is also the lesson about instruments: this runs in ~2s with zero variance,
// while the bench sweep it replaced needed hours of GPU and showed 3.2x
// wall-clock spread across four pairs — enough noise to produce exactly the
// wrong answer.
func TestHysteresisIsWorthItsValue(t *testing.T) {
	const window, turns = 131072, 60
	// Pin the working set so this measures HYSTERESIS and nothing else. The cap now
	// follows the window (workingset.go), and at this window it rose above what this
	// fixture produces — so without the pin both arms fold zero times and the test
	// silently compares nothing to nothing.
	t.Cleanup(func() { SetWorkingSetTokens(0) })
	SetWorkingSetTokens(32768)
	build := func(n int) []llm.Message {
		convo := []llm.Message{{Role: llm.RoleUser, Content: "trace the prompt assembly"}}
		for i := range n {
			convo = append(convo,
				llm.Message{Role: llm.RoleAssistant, Content: fmt.Sprintf("turn %d: reading", i)},
				llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf("tool read_file result:\n%s",
					strings.Repeat(fmt.Sprintf("line %d of internal/agent/file_%d.go\n", i, i%7), 110))})
		}
		return convo
	}
	measure := func(frac float64) (moves, reprefill int) {
		defer func(old float64) { lowWaterFrac = old }(lowWaterFrac)
		lowWaterFrac = frac
		w := NewWorkingMemory()
		var prev []byte
		prevFolded := 0
		for n := 1; n <= turns; n++ {
			in := MemoryInput{Task: "trace the prompt assembly", Convo: build(n), WindowTokens: window}
			out, err := w.Assemble(in)
			if err != nil {
				t.Fatalf("frac %v turn %d: %v", frac, n, err)
			}
			cur := promptBytes(out)
			if prev != nil {
				reprefill += len(cur) - commonPrefixLen(prev, cur)
			}
			if w.folded != prevFolded {
				moves, prevFolded = moves+1, w.folded
			}
			prev = cur
		}
		return moves, reprefill
	}

	shippedMoves, shippedBytes := measure(lowWaterFrac)
	noneMoves, noneBytes := measure(1.0)
	t.Logf("frac %.2f: %d boundary moves, %.1f MB re-prefilled", lowWaterFrac, shippedMoves, float64(shippedBytes)/1e6)
	t.Logf("frac 1.00: %d boundary moves, %.1f MB re-prefilled", noneMoves, float64(noneBytes)/1e6)

	// No hysteresis must be MATERIALLY worse, not marginally — otherwise the
	// complexity is not paying for itself and the simpler 1.0 should win.
	if noneBytes < shippedBytes*2 {
		t.Errorf("hysteresis is not earning its keep: 1.00 re-prefills %d bytes vs %d at %.2f",
			noneBytes, shippedBytes, lowWaterFrac)
	}
	if noneMoves <= shippedMoves {
		t.Errorf("1.00 moved the boundary %d times vs %d at %.2f — hysteresis should move it LESS",
			noneMoves, shippedMoves, lowWaterFrac)
	}
}
