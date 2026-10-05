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
	if got != defaultWorkingSetTokens {
		t.Fatalf("trigger %d, want the cap %d", got, defaultWorkingSetTokens)
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
