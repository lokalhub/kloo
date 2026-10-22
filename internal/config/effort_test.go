package config

import "testing"

// TestDefaultTierMatchesFlatDefaults pins the invariant that the default effort
// tier and the flat Default* constants agree. The two live in different files
// (effort.go vs config.go) and both feed Resolve — the tier seeds the loop
// budgets, the flat constants seed the Config literal — so if one is edited
// without the other, the default run silently changes depending on which path
// wins. This test makes that drift a build failure instead of a surprise.
func TestDefaultTierMatchesFlatDefaults(t *testing.T) {
	tier := lookupEffort(DefaultEffort)

	if tier.MaxSteps != DefaultMaxSteps {
		t.Errorf("default tier MaxSteps = %d, want DefaultMaxSteps = %d", tier.MaxSteps, DefaultMaxSteps)
	}
	if tier.ChurnRounds != DefaultChurnRounds {
		t.Errorf("default tier ChurnRounds = %d, want DefaultChurnRounds = %d", tier.ChurnRounds, DefaultChurnRounds)
	}
	if tier.MaxTokens != DefaultMaxTokens {
		t.Errorf("default tier MaxTokens = %d, want DefaultMaxTokens = %d", tier.MaxTokens, DefaultMaxTokens)
	}
	if tier.MaxWallClockSeconds != DefaultMaxWallClockSeconds {
		t.Errorf("default tier MaxWallClockSeconds = %d, want DefaultMaxWallClockSeconds = %d",
			tier.MaxWallClockSeconds, DefaultMaxWallClockSeconds)
	}
}

// TestEffortTiersIncreaseInIntensity guards the documented ordering of the
// shipped tiers: steps and wall-clock must be non-decreasing fast -> medium ->
// heavy, so "heavy" is never less patient than "fast".
func TestEffortTiersIncreaseInIntensity(t *testing.T) {
	fast := lookupEffort("fast")
	medium := lookupEffort("medium")
	heavy := lookupEffort("heavy")

	if !(fast.MaxSteps <= medium.MaxSteps && medium.MaxSteps <= heavy.MaxSteps) {
		t.Errorf("MaxSteps not non-decreasing: fast=%d medium=%d heavy=%d",
			fast.MaxSteps, medium.MaxSteps, heavy.MaxSteps)
	}
	if !(fast.MaxWallClockSeconds <= medium.MaxWallClockSeconds && medium.MaxWallClockSeconds <= heavy.MaxWallClockSeconds) {
		t.Errorf("MaxWallClockSeconds not non-decreasing: fast=%d medium=%d heavy=%d",
			fast.MaxWallClockSeconds, medium.MaxWallClockSeconds, heavy.MaxWallClockSeconds)
	}
	if !(fast.ChurnRounds <= medium.ChurnRounds && medium.ChurnRounds <= heavy.ChurnRounds) {
		t.Errorf("ChurnRounds not non-decreasing: fast=%d medium=%d heavy=%d",
			fast.ChurnRounds, medium.ChurnRounds, heavy.ChurnRounds)
	}
}
