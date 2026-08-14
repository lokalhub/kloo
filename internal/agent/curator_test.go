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

// TestSplitBoundsLargeWindows is the whole point: a huge window must raise the
// compaction trigger WITHOUT raising what kloo assembles each turn.
func TestSplitBoundsLargeWindows(t *testing.T) {
	const window = 900_000
	const defaultCap = 32768

	// Capacity scales with the window — compaction becomes rare.
	if got, want := triggerTokens(window), 630_000; got != want {
		t.Errorf("compaction trigger = %d, want %d", got, want)
	}
	// Appetite does not.
	mapBudget := mapBudgetTokens(EffectiveCuratorBudget(window, defaultCap))
	if mapBudget != 11_468 {
		t.Errorf("map budget = %d, want 11468", mapBudget)
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
