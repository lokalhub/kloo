package agent

import (
	"testing"

	"github.com/lokalhub/kloo/internal/config"
)

// TestRunBudgetMatchesAgent is the test runbudget.go's comment has named since it
// was written, and which did not exist.
//
// internal/config cannot import internal/agent (the dependency runs the other
// way), so the fractions and the working-set curve are DUPLICATED there. A
// duplicate with nothing holding it in place is a copy that silently drifts, and
// the drift is invisible: the budget keeps being computed, just from a per-turn
// cost the loop no longer uses. This lives in agent, which can see both.
func TestRunBudgetMatchesAgent(t *testing.T) {
	t.Cleanup(func() { SetWorkingSetTokens(0) })
	SetWorkingSetTokens(0)

	for _, window := range []int{8000, 32768, 65536, 131072, 262144, 900_000, 1 << 20} {
		// The agent's own per-turn cost: the compaction trigger at this window.
		usable := UsableWindow(window)
		agentPerTurn := CompactTriggerTokens(usable)

		// What config computes the budget from, recovered by dividing out the turn
		// count — below the floor/ceiling the budget is exactly turns x perTurn.
		got := config.ComputeRunTokenBudget(window, 0, 0, 0)
		want := config.ComputeRunTokenBudget(window, 0, 0, agentPerTurn)
		// Tolerance of one token per turn: the two sides reach the same number through
		// different float->int truncations (config multiplies the fractions in one
		// expression, the agent rounds the usable window first). A DRIFT would be a
		// different per-turn cost entirely, which is thousands of tokens, not forty.
		if diff := got - want; diff > 40 || diff < -40 {
			t.Errorf("ctx %d: config's own per-turn cost disagrees with the agent's (%d): budget %d vs %d",
				window, agentPerTurn, got, want)
		}
	}
}

// TestWorkingSetCurveIsSublinearAndNeverExceedsTheFraction pins the two properties
// that make the curve the right shape, independently of its exact value:
//
//   - it never RAISES the trigger above what the fraction already allowed, so no
//     window gets looser than it was before the cap existed;
//   - it grows with the window but takes a SMALLER share of it as the window grows,
//     which is the whole claim — effective context does not keep up with declared.
func TestWorkingSetCurveIsSublinearAndNeverExceedsTheFraction(t *testing.T) {
	t.Cleanup(func() { SetWorkingSetTokens(0) })
	SetWorkingSetTokens(0)

	windows := []int{8000, 16384, 32768, 65536, 131072, 262144, 524288, 1 << 20}
	var prevTokens int
	var prevShare float64
	for i, w := range windows {
		usable := UsableWindow(w)
		fraction := int(compactTriggerFrac * float64(usable))
		trigger := CompactTriggerTokens(usable)

		if trigger > fraction {
			t.Errorf("ctx %d: trigger %d exceeds the fraction %d — the cap must only ever lower it", w, trigger, fraction)
		}
		share := float64(trigger) / float64(w)
		if i > 0 {
			if trigger < prevTokens {
				t.Errorf("ctx %d: trigger %d is below the smaller window's %d — it must grow", w, trigger, prevTokens)
			}
			// Non-increasing, not strictly decreasing: below ~65k the fraction is
			// tighter than the curve, so the share is a flat 56% there by design. The
			// tolerance absorbs integer truncation at those small windows.
			if share > prevShare+1e-3 {
				t.Errorf("ctx %d: share %.3f rose from %.3f — growth must be sublinear", w, share, prevShare)
			}
		}
		t.Logf("ctx %-8d usable %-8d trigger %-8d (%.0f%% of declared)", w, usable, trigger, share*100)
		prevTokens, prevShare = trigger, share
	}
}
