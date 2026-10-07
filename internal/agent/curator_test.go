package agent

import "testing"

// TestEffectiveCuratorBudget pins the clamp: the curator can never promise more
// than the prompt budget holds, and an unset cap means "no separate cap".
func TestEffectiveCuratorBudget(t *testing.T) {
	cases := []struct {
		name       string
		window     int
		configured int
		want       int
	}{
		// 8000 window ⇒ usable 6400. A 32768 cap is far above it, so it clamps and
		// the arithmetic is identical to the pre-split code.
		{"small window clamps to usable", 8000, 32768, 6400},
		{"unset cap falls back to usable", 8000, 0, 6400},
		{"negative cap falls back to usable", 8000, -1, 6400},
		{"cap below usable is honoured", 8000, 4000, 4000},
		// 900k window ⇒ usable 720000. Here the cap is what stops a quarter-million
		// token repo map from being assembled on every turn.
		{"large window honours the cap", 900_000, 32768, 32768},
		{"large window without a cap falls back", 900_000, 0, 720_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveCuratorBudget(tc.window, tc.configured); got != tc.want {
				t.Errorf("EffectiveCuratorBudget(%d, %d) = %d, want %d",
					tc.window, tc.configured, got, tc.want)
			}
		})
	}
}

// TestSplitIsByteIdenticalForSmallWindows is the regression guard for the split:
// for any window small enough that the default cap clamps, the resulting map
// budget must equal what the pre-split code computed (mapBudgetFrac × usable
// window). Small local models — kloo's core audience — must see no change.
func TestSplitIsByteIdenticalForSmallWindows(t *testing.T) {
	const defaultCap = 32768 // config.DefaultCuratorBudgetTokens (not imported: agent must not depend on cli wiring)
	for _, window := range []int{4096, 8000, 16384, 32768, 40960} {
		preSplit := mapBudgetTokens(usableWindow(window))
		postSplit := mapBudgetTokens(EffectiveCuratorBudget(window, defaultCap))
		if preSplit != postSplit {
			t.Errorf("window %d: map budget changed %d → %d; the split must be a no-op below the cap",
				window, preSplit, postSplit)
		}
	}
}

// TestSplitBoundsLargeWindows is the whole point: a huge window must not raise
// what kloo assembles each turn.
//
// The capacity half of this test was INVERTED on purpose in v0.25.6. It used to
// assert that a huge window raises the compaction trigger in proportion
// ("compaction becomes rare"), and that turned out to be the bug the user was
// reporting: at --ctx 131072 nothing was shed for twenty-five turns, the prompt
// grew 83KB → 218KB, and the run spent 1.29M tokens answering one read-only
// question. The trigger is now bounded absolutely by the working-set cap, so the
// window buys headroom for the single call that needs it rather than licence to
// accumulate. The proportional behaviour is still reachable with the cap off,
// which is asserted here so the change stays reversible.
func TestSplitBoundsLargeWindows(t *testing.T) {
	const window = 900_000
	const defaultCap = 32768

	// Capacity no longer scales with the window IN PROPORTION: the cap binds. It is
	// not a flat constant either — a window-blind 32768 meant kloo used 3.6% of a
	// 900k window, refusing context already paid for. The cap now follows the
	// geometric-mean curve, so what is asserted is the curve's value at this window,
	// and the property under test is unchanged: the trigger is far below the
	// fraction, and sublinear in the window.
	if got, want := triggerTokens(window), WorkingSetTokensFor(window); got != want {
		t.Errorf("compaction trigger = %d, want the working-set cap %d", got, want)
	}
	// Sublinear, not merely bounded: the curve must stay far under the proportional
	// trigger that caused the 1.29M-token run.
	if got := triggerTokens(window); got >= 630_000/3 {
		t.Errorf("trigger %d is not meaningfully below the proportional 630000", got)
	}
	// ...and the old proportional trigger is one flag away.
	func() {
		t.Cleanup(func() { SetWorkingSetTokens(0) })
		SetWorkingSetTokens(-1)
		if got, want := triggerTokens(window), 630_000; got != want {
			t.Errorf("compaction trigger with the cap off = %d, want %d", got, want)
		}
	}()
	// Appetite does not.
	// 6881, not 11468: the map budget is now a fraction of the COMPACTION TRIGGER
	// rather than of the curator budget directly. The old value, combined with a hot
	// budget taken from the RAW window, summed to MORE than the trigger at every
	// window size, so the two budgets alone forced a compaction every turn. See
	// TestBudgetsFitUnderTheCompactionTrigger.
	mapBudget := mapBudgetTokens(EffectiveCuratorBudget(window, defaultCap))
	want := mapBudgetTokens(defaultCap)
	if mapBudget != want {
		t.Errorf("map budget = %d, want %d", mapBudget, want)
	}
	if unbounded := mapBudgetTokens(usableWindow(window)); mapBudget >= unbounded {
		t.Errorf("map budget %d should be far below the unbounded %d", mapBudget, unbounded)
	}
	// Hot state still scales with the window: it is conversation we already have,
	// so a bigger window should keep MORE of it, not the same.
	if hotBudgetTokens(window) <= hotBudgetTokens(32768) {
		t.Error("hot budget should grow with the window, not with the curator cap")
	}
}
