package agent

import "testing"

// The cap must be a STRICT NO-OP wherever the fractional trigger is already
// tighter. Every bench result kloo has was measured at a small --ctx, and a cap
// that moved those triggers would invalidate all of it.
func TestWorkingSetCapIsANoOpOnSmallWindows(t *testing.T) {
	t.Cleanup(func() { SetWorkingSetTokens(0) })
	SetWorkingSetTokens(0)
	for _, declared := range []int{4096, 8000, 16384, 32768} {
		usable := UsableWindow(declared)
		got := CompactTriggerTokens(usable)
		want := int(compactTriggerFrac * float64(usable))
		if got != want {
			t.Fatalf("declared %d: trigger %d, want the unchanged fraction %d", declared, got, want)
		}
	}
}

// The point of the change: a large window stops meaning "accumulate until the
// prompt is enormous".
func TestWorkingSetCapBindsOnLargeWindows(t *testing.T) {
	t.Cleanup(func() { SetWorkingSetTokens(0) })
	SetWorkingSetTokens(0)
	usable := UsableWindow(131072)
	frac := int(compactTriggerFrac * float64(usable))
	got := CompactTriggerTokens(usable)
	if want := WorkingSetTokensFor(usable); got != want {
		t.Fatalf("trigger %d, want the cap %d at this window", got, want)
	}
	if got >= frac {
		t.Fatalf("cap %d did not lower the fractional trigger %d", got, frac)
	}
}

// Sign convention, as everywhere else in kloo: 0 is the built-in, negative is off.
// Negative must restore the pre-cap trigger exactly, so a bad default can always
// be backed out in the field without a new build.
func TestWorkingSetCapSignConvention(t *testing.T) {
	t.Cleanup(func() { SetWorkingSetTokens(0) })
	usable := UsableWindow(131072)
	frac := int(compactTriggerFrac * float64(usable))

	SetWorkingSetTokens(-1)
	if got := CompactTriggerTokens(usable); got != frac {
		t.Fatalf("disabled: trigger %d, want the fraction %d", got, frac)
	}
	if WorkingSetTokens() > 0 {
		t.Fatalf("WorkingSetTokens() = %d, want non-positive when disabled", WorkingSetTokens())
	}

	SetWorkingSetTokens(0)
	if got := WorkingSetTokens(); got != defaultWorkingSetTokens {
		t.Fatalf("zero: cap %d, want the built-in %d", got, defaultWorkingSetTokens)
	}

	SetWorkingSetTokens(12345)
	if got := CompactTriggerTokens(usable); got != 12345 {
		t.Fatalf("explicit: trigger %d, want 12345", got)
	}
}

// The cap only ever LOWERS. A cap above the fractional trigger must not raise it
// past the headroom usableWindowFrac reserves for the completion and schemas.
func TestWorkingSetCapNeverRaisesTheTrigger(t *testing.T) {
	t.Cleanup(func() { SetWorkingSetTokens(0) })
	usable := UsableWindow(8000)
	frac := int(compactTriggerFrac * float64(usable))
	SetWorkingSetTokens(1_000_000)
	if got := CompactTriggerTokens(usable); got != frac {
		t.Fatalf("huge cap: trigger %d, want the unchanged fraction %d", got, frac)
	}
}

// Both halves of the fix must be backable-out WITHOUT a new build. lowWaterFrac
// shipped in v0.25.5 with no switch and promptly looked wrong under a paired
// bench, which left no way to revert it in the field.
func TestSummaryBudgetHasAnEscapeHatch(t *testing.T) {
	t.Cleanup(func() { SetSummaryBudgetFrac(summaryBudgetFrac) })

	if got := SummaryBudgetFrac(); got != summaryBudgetFrac {
		t.Fatalf("default = %v, want the built-in %v", got, summaryBudgetFrac)
	}
	// Out of (0,1] disables rather than producing a summary budget of zero tokens,
	// which would collapse the summary to nothing on every turn.
	for _, off := range []float64{0, -1, 1.5} {
		SetSummaryBudgetFrac(off)
		if got := SummaryBudgetFrac(); got != 0 {
			t.Fatalf("SetSummaryBudgetFrac(%v) = %v, want 0 (disabled)", off, got)
		}
	}
	SetSummaryBudgetFrac(0.5)
	if got := SummaryBudgetFrac(); got != 0.5 {
		t.Fatalf("explicit 0.5 = %v", got)
	}
}

// Disabling the budget must restore the unbounded summary exactly — the
// pre-v0.25.6 behaviour — so a revert is a revert and not a third mode.
func TestDisablingTheSummaryBudgetLeavesTheSummaryUnbounded(t *testing.T) {
	t.Cleanup(func() { SetSummaryBudgetFrac(summaryBudgetFrac) })
	SetSummaryBudgetFrac(-1)

	// Compute the budget the way Assemble does, rather than passing one in: the
	// disable works by the FRACTION resolving to 0, so a test that supplies its own
	// budget bypasses the thing it is checking.
	budget := int(float64(CompactTriggerTokens(UsableWindow(131072))) * SummaryBudgetFrac())
	if budget != 0 {
		t.Fatalf("disabled fraction still produced a budget of %d", budget)
	}

	w := &workingMemory{foldedEntries: []string{"OBS one", "OBS two", "OBS three"}}
	if w.collapseSummary(budget, func(s string) int { return len(s) }, nil) {
		t.Fatal("collapsed with the budget disabled")
	}
	if len(w.foldedEntries) != 3 || w.droppedEntries != 0 {
		t.Fatalf("entries touched with the budget disabled: %q dropped=%d", w.foldedEntries, w.droppedEntries)
	}
}
