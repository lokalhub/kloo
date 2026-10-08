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
		// The pre-split arithmetic, spelled out rather than called: mapBudgetTokens no
		// longer takes a pre-resolved budget, because handing it one is what let the
		// compaction trigger be RECONSTRUCTED from a fraction instead of read from
		// triggerTokens. Writing the old formula here is what makes this a regression
		// guard rather than a tautology against whatever the function now does.
		preSplit := int(float64(usableWindow(window)) * triggerFrac * mapBudgetFrac)
		if got := mapBudgetTokens(window, defaultCap); got != preSplit {
			t.Errorf("window %d: map budget changed %d → %d; the split must be a no-op below the cap",
				window, preSplit, got)
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
		// `defer`, not `t.Cleanup`: Cleanup registers against the TEST, so it ran at the
		// end of the whole function and left the working-set cap DISABLED for every
		// assertion after this block. That is how a map budget asserted below came out
		// as the proportional 151,199 — the figure this test exists to rule out — while
		// the test still passed, because the comparison it was making had been made
		// against an equally uncapped number.
		defer SetWorkingSetTokens(0)
		SetWorkingSetTokens(-1)
		if got, want := triggerTokens(window), 630_000; got != want {
			t.Errorf("compaction trigger with the cap off = %d, want %d", got, want)
		}
	}()
	// Appetite does not. The map budget is mapBudgetFrac of whichever is SMALLER, the
	// compaction trigger or the curator budget — and at a 900k window the curator cap
	// is smaller by a factor of five, so the window does not reach the map at all.
	// Stated as an equality against a window 7x smaller, which is the property
	// ("appetite is chosen by us, capacity is discovered") rather than the number.
	mapBudget := mapBudgetTokens(window, defaultCap)
	if want := mapBudgetTokens(131072, defaultCap); mapBudget != want {
		t.Errorf("map budget at --ctx %d = %d, but %d at --ctx 131072 — appetite followed the window", window, mapBudget, want)
	}
	// With the curator cap removed (set to the usable window, i.e. no separate cap)
	// the trigger is what bounds the map, and the working-set curve keeps that far
	// below the proportional figure. Measured 2026-10-07: 46,080 here, against
	// 151,200 under the pre-change `curator x triggerFrac x mapBudgetFrac`, which is
	// the "900k window authorises an enormous map" failure the first of this
	// function's two doc comments existed to prevent.
	unbounded := mapBudgetTokens(window, usableWindow(window))
	if mapBudget >= unbounded {
		t.Errorf("map budget %d should be far below the uncapped %d", mapBudget, unbounded)
	}
	if proportional := int(float64(usableWindow(window)) * triggerFrac * mapBudgetFrac); unbounded >= proportional/3 {
		t.Errorf("uncapped map budget %d is not meaningfully below the proportional %d", unbounded, proportional)
	}
	// Hot state still scales with the window: it is conversation we already have,
	// so a bigger window should keep MORE of it, not the same.
	if hotBudgetTokens(window) <= hotBudgetTokens(32768) {
		t.Error("hot budget should grow with the window, not with the curator cap")
	}
}

// TestMapBudgetIsBoundedByTheRealTrigger pins the INVARIANT, not the numbers: the
// repo-map budget is the SMALLER of mapBudgetFrac of the compaction trigger the loop
// actually enforces and the appetite figure the curator budget has always produced.
//
// It is the invariant rather than a table of values because the numbers moved for a
// reason a value table would have hidden. mapBudgetTokens used to compute the
// appetite figure ALONE — `curator x triggerFrac x mapBudgetFrac`, a trigger
// RECONSTRUCTED from a fraction — while its sibling hotBudgetTokens read the real one
// through capWorkingSet. Since the working set went window-adaptive (v0.26.0) the two
// diverge exactly where the cap binds, which is every window above ~47k declared, and
// the divergence is invisible to any test that asserts a constant.
//
// So each case names WHICH arm is expected to bind, and the expected binding is
// itself asserted. A case that silently stopped being the working-set-bound one would
// otherwise go on passing.
//
// Note the shape of the two arms: the trigger arm can only bind where the WORKING-SET
// CAP bit, because below ~47k declared trigger == triggerFrac x usable and
// curator <= usable, so the appetite arm is always the smaller one there. That is the
// mechanism behind the "small windows are byte-identical" guarantee, stated as a
// property instead of as a list of windows.
func TestMapBudgetIsBoundedByTheRealTrigger(t *testing.T) {
	const bindsTrigger, bindsAppetite = "trigger", "appetite"
	for _, tc := range []struct {
		name       string
		window     int
		curator    int // --curator-budget; 0 ⇒ no separate cap
		workingSet int // --working-set-tokens; 0 ⇒ the built-in curve
		binding    string
	}{
		{"8k: the cap is a strict no-op, so appetite binds", 8000, 0, 0, bindsAppetite},
		{"32k: still appetite", 32768, 0, 0, bindsAppetite},
		{"131k, no curator cap: the working-set CURVE binds", 131072, 0, 0, bindsTrigger},
		{"131k, explicit --working-set-tokens 12000: the cap binds", 131072, 0, 12000, bindsTrigger},
		{"131k, the stock curator cap: appetite is the smaller number", 131072, 32768, 0, bindsAppetite},
		{"131k, the muse-glimmer-30b curator cap: still appetite", 131072, 52428, 0, bindsAppetite},
		{"131k, a curator cap at the usable window: the curve binds", 131072, 104857, 0, bindsTrigger},
		{"900k, the stock curator cap: capacity never reaches the map", 900000, 32768, 0, bindsAppetite},
		{"1M, the curator cap lifted: the curve binds hard", 1 << 20, 838860, 0, bindsTrigger},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer SetWorkingSetTokens(0)
			SetWorkingSetTokens(tc.workingSet)

			trigger := triggerTokens(usableWindow(tc.window))
			fromTrigger := int(float64(trigger) * mapBudgetFrac)
			fromAppetite := int(float64(EffectiveCuratorBudget(tc.window, tc.curator)) * triggerFrac * mapBudgetFrac)
			got := mapBudgetTokens(tc.window, tc.curator)

			want := fromTrigger
			if tc.binding == bindsAppetite {
				want = fromAppetite
				if fromAppetite > fromTrigger {
					t.Fatalf("this case claims appetite binds, but appetite %d > trigger arm %d", fromAppetite, fromTrigger)
				}
			} else if fromTrigger > fromAppetite {
				t.Fatalf("this case claims the trigger binds, but trigger arm %d > appetite %d", fromTrigger, fromAppetite)
			}
			if got != want {
				t.Errorf("map budget = %d, want %d (the %s arm)", got, want, tc.binding)
			}
			// As a SHARE of the real trigger, which is the form that survives a retune of
			// the working-set curve or of the fractions. mapBudgetFrac is the CEILING, met
			// exactly when the trigger arm binds and below it otherwise.
			share := float64(got) / float64(trigger)
			if share > mapBudgetFrac+0.001 {
				t.Errorf("map budget is %.4f of the trigger, above the %.2f ceiling", share, mapBudgetFrac)
			}
			if tc.binding == bindsTrigger && share < mapBudgetFrac-0.001 {
				t.Errorf("the trigger arm binds, so the share should be %.2f, got %.4f", mapBudgetFrac, share)
			}
		})
	}
}

// TestMapBudgetNeverExceedsTheAppetiteFormula is the guarantee that makes budgeting
// the map against the real trigger safe to ship: the change is MONOTONE DOWNWARD.
//
// Taking mapBudgetFrac of the trigger and nothing else is the coherent formula and it
// RAISES the budget wherever the curator budget is the smaller input — +43%, which at
// the stock curator cap and --ctx 131072 is 6,881 to 9,830, measured end to end as
// +9,706 bytes of repo map on every turn (+32.6% of the assembled prompt). A ~22k map
// re-prefilling every call was the worst performance bug kloo has had, and nothing
// measured says a bigger map helps. So a correctness fix must not pay for itself in
// tokens, and this test is what says so for every configuration rather than for the
// ones someone thought to tabulate.
//
// 1,496 combinations swept. Largest reduction at the built-in working-set curve:
// 281,981 tokens, at a 2M declared window with no curator cap, where the appetite arm
// authorised 352,321 tokens of repo map and the curve bounds it to 70,340.
func TestMapBudgetNeverExceedsTheAppetiteFormula(t *testing.T) {
	defer SetWorkingSetTokens(0)
	for _, window := range []int{1000, 2000, 4096, 8000, 16384, 32768, 40960, 65536, 100000, 131072, 200000, 262144, 400000, 600000, 900_000, 1 << 20, 2 << 20} {
		for _, curator := range []int{0, 1, 1024, 4096, 32768, 52428, 104857, 500_000, 838_860, 1 << 20, 1 << 22} {
			for _, ws := range []int{0, -1, 1, 8000, 12000, 32768, 65536, 200_000} {
				SetWorkingSetTokens(ws)
				// The formula that shipped in v0.27.0, written out rather than called: this
				// is a comparison against HISTORY, so it cannot be expressed in terms of the
				// function under test.
				appetite := int(float64(EffectiveCuratorBudget(window, curator)) * triggerFrac * mapBudgetFrac)
				if got := mapBudgetTokens(window, curator); got > appetite {
					t.Fatalf("ctx %d curator %d working-set %d: map budget %d exceeds the %d v0.27.0 shipped — "+
						"this change must never make the prompt bigger", window, curator, ws, got, appetite)
				}
				SetWorkingSetTokens(0)
			}
		}
	}
}

// TestMapBudgetCanNeverForceACompactionByItself is the guarantee mapBudgetTokens'
// doc comment claimed and the code did not keep.
//
// Measured before this change at --ctx 131072 --working-set-tokens 12000: trigger
// 12,000, map budget 22,019 — 184% of the trigger. The map alone guaranteed a
// compaction on turn one, every turn, and `kloo context` showed the assembled prompt
// 14,370 tokens past the trigger on a one-word task. The sweep is deliberately
// wider than the configurations anyone has run, because the defect appeared only in
// the corner where the working-set cap bound and nothing was looking there.
func TestMapBudgetCanNeverForceACompactionByItself(t *testing.T) {
	defer SetWorkingSetTokens(0)
	for _, window := range []int{2000, 4096, 8000, 16384, 32768, 65536, 131072, 262144, 900_000, 1 << 20} {
		for _, curator := range []int{0, 4096, 32768, 104857, 1 << 20} {
			for _, ws := range []int{0, -1, 12000, 32768, 200_000} {
				SetWorkingSetTokens(ws)
				got := mapBudgetTokens(window, curator)
				trigger := triggerTokens(usableWindow(window))
				if trigger <= 0 {
					continue // compaction off: there is nothing to be forced
				}
				if got >= trigger {
					t.Fatalf("ctx %d curator %d working-set %d: map budget %d >= compaction trigger %d — "+
						"the map alone forces a compaction every turn", window, curator, ws, got, trigger)
				}
				// Not merely under it: at most mapBudgetFrac of it, so the hot set and the
				// fresh conversation keep the shares mapBudgetFrac/hotBudgetFrac promise.
				// +1 absorbs the truncation in the int conversion.
				if float64(got) > mapBudgetFrac*float64(trigger)+1 {
					t.Fatalf("ctx %d curator %d working-set %d: map budget %d exceeds %.2f of the trigger %d",
						window, curator, ws, got, mapBudgetFrac, trigger)
				}
			}
		}
	}
}
