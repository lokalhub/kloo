package config

import "testing"

// The whole point of computing it: a flat ceiling is wrong at both ends, because a
// turn's cost is set by the window.
func TestComputeRunTokenBudgetScalesWithTheWindow(t *testing.T) {
	small := ComputeRunTokenBudget(8000, 0, 0, 0)
	mid := ComputeRunTokenBudget(32768, 0, 0, 0)
	large := ComputeRunTokenBudget(131072, 0, 0, 0)
	if !(small <= mid && mid <= large) {
		t.Fatalf("not monotonic in the window: 8k=%d 32k=%d 131k=%d", small, mid, large)
	}
	if mid <= runBudgetFloor || mid >= runBudgetCeiling {
		t.Errorf("a mid window should land between the clamps, got %d", mid)
	}
}

// The clamps carry the design, not the multiplier. A small window needs MORE turns
// (many cheap steps), so the raw product would stop a real task mid-flight.
func TestComputeRunTokenBudgetFloorProtectsSmallWindows(t *testing.T) {
	raw := int(8000 * 0.80 * 0.70 * runBudgetTurns) // 179,200 — below the floor
	if raw >= runBudgetFloor {
		t.Fatalf("test premise broken: raw %d is no longer below the floor", raw)
	}
	if got := ComputeRunTokenBudget(8000, 0, 0, 0); got != runBudgetFloor {
		t.Errorf("ctx 8000 = %d, want the floor %d", got, runBudgetFloor)
	}
}

// A large window needs FEWER turns and the multiplier runs away: 40 x 73399 is
// 2.9M, above the figure that prompted this work.
//
// Two different bounds now do that job, and which one binds depends on the
// working-set cap:
//
//   - cap ON (the default): a turn costs the cap, so 40 turns is 1.31M and the
//     budget buys the 40 turns it is denominated in.
//   - cap OFF: a turn costs the fraction, 40 of them is 2.9M, and runBudgetCeiling
//     is what keeps it off the ~2M that prompted this work.
//
// Both are asserted, because the ceiling is otherwise unreachable at the default
// cap and a test that only covered the default would let it rot.
func TestComputeRunTokenBudgetCeilingBoundsLargeWindows(t *testing.T) {
	// At a large window the CEILING is what bounds the budget, with the cap on or
	// off. That is a change: the cap used to be a flat 32768, so 40 turns landed at
	// 1.31M, under the ceiling. Now the default cap follows the window (the
	// geometric-mean curve), a turn at ctx 131072 costs 58617, and 40 of those is
	// 2.34M — above the ceiling, which is therefore the binding constraint.
	capped := ComputeRunTokenBudget(131072, 0, 0, 0)
	if capped != runBudgetCeiling {
		t.Errorf("ctx 131072 capped = %d, want the ceiling %d", capped, runBudgetCeiling)
	}
	if capped >= 2_000_000 {
		t.Errorf("budget %d does not improve on the ~2M that prompted this", capped)
	}

	uncapped := ComputeRunTokenBudget(131072, 0, 0, -1)
	if uncapped != runBudgetCeiling {
		t.Errorf("ctx 131072 with the cap off = %d, want the ceiling %d", uncapped, runBudgetCeiling)
	}

	// The cap still does its job where it is actually tighter: an EXPLICIT cap sizes
	// the per-turn cost, and the budget follows it down. This is the arm an operator
	// reaches for on a long task, so it is the one worth pinning.
	tight := ComputeRunTokenBudget(131072, 0, 0, 12_000)
	if want := 12_000 * runBudgetTurns; tight != want {
		t.Errorf("explicit cap: budget = %d, want %d (%d x %d turns)", tight, want, 12_000, runBudgetTurns)
	}
	if tight >= capped {
		t.Errorf("a tighter cap must tighten the budget: %d >= %d", tight, capped)
	}
}

// An unknown window means the endpoint reported no context length and nothing
// supplied one. A budget derived from that is a guess about a guess — keep the
// previous behaviour rather than inventing a ceiling that could stop real work.
func TestComputeRunTokenBudgetUnknownWindowIsUnbounded(t *testing.T) {
	for _, w := range []int{0, -1} {
		if got := ComputeRunTokenBudget(w, 0, 0, 0); got != 0 {
			t.Errorf("window %d = %d, want 0 (unbounded)", w, got)
		}
	}
}

// Out-of-range fractions fall back to the defaults instead of producing a nonsense
// budget, mirroring SetContextFractions.
func TestComputeRunTokenBudgetIgnoresBadFractions(t *testing.T) {
	want := ComputeRunTokenBudget(32768, 0, 0, 0)
	for _, bad := range [][2]float64{{-1, -1}, {0, 2}, {5, 0.7}} {
		if got := ComputeRunTokenBudget(32768, bad[0], bad[1], 0); got != want {
			t.Errorf("fractions %v = %d, want the default %d", bad, got, want)
		}
	}
}

// A tighter trigger means cheaper turns, so the budget must follow it down — the
// two numbers describe the same thing and must not drift apart.
func TestComputeRunTokenBudgetHonoursCustomFractions(t *testing.T) {
	tight := ComputeRunTokenBudget(32768, 0.80, 0.40, 0)
	loose := ComputeRunTokenBudget(32768, 0.80, 0.70, 0)
	if tight >= loose {
		t.Errorf("tight trigger = %d, loose = %d; a cheaper turn must mean a smaller budget", tight, loose)
	}
}
