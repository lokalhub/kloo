package agent

import "testing"

// saturated must require a FULL window. Judging from two or three samples would
// stop a run during the opening moves, where re-reading what you just listed is
// orienting, not circling.
func TestSaturatedNeedsAFullWindow(t *testing.T) {
	if saturated([]bool{true, false, false}, 8, 0.35) {
		t.Fatal("saturated on a partial window")
	}
	if saturated(nil, 8, 0.35) {
		t.Fatal("saturated on an empty window")
	}
}

func TestSaturatedRatio(t *testing.T) {
	cases := []struct {
		name   string
		window []bool
		want   bool
	}{
		{"all new ground", []bool{true, true, true, true}, false},
		{"half new ground", []bool{true, false, true, false}, false},
		{"one in four is below 0.35", []bool{true, false, false, false}, true},
		// ZERO new ground belongs to the no-new-ground streak rail, not this one.
		// Claiming it made saturation fire a step earlier than that rail and broke
		// the v0.16.7 parity scenarios.
		{"pure repetition is the OTHER rail's job", []bool{false, false, false, false}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := saturated(tc.window, 4, 0.35); got != tc.want {
				t.Errorf("saturated = %v, want %v", got, tc.want)
			}
		})
	}
}

// A disabled window (size <= 0) must never fire, whatever the samples say.
func TestSaturatedDisabled(t *testing.T) {
	if saturated([]bool{true, false, false, false}, 0, 0.35) {
		t.Fatal("fired with the rail disabled")
	}
	if saturated([]bool{true, false, false, false}, -1, 0.35) {
		t.Fatal("fired with a negative window")
	}
}

// The resolvers' sign convention: 0 means "use the built-in", NEGATIVE means
// "disabled". The usual >0 guard would leave no way to turn a rail off.
func TestExploreRailResolvers(t *testing.T) {
	var l Loop
	if got := l.exploreTokenCap(); got != DefaultExploreTokenCap {
		t.Errorf("default token cap = %d, want %d", got, DefaultExploreTokenCap)
	}
	if got := (&Loop{ExploreTokenCap: -1}).exploreTokenCap(); got != 0 {
		t.Errorf("negative token cap = %d, want 0 (disabled)", got)
	}
	if got := (&Loop{ExploreTokenCap: 500}).exploreTokenCap(); got != 500 {
		t.Errorf("explicit token cap = %d, want 500", got)
	}
	if got := l.exploreSaturationWindow(); got != DefaultExploreSaturationWindow {
		t.Errorf("default saturation window = %d, want %d", got, DefaultExploreSaturationWindow)
	}
	if got := (&Loop{ExploreSaturationWindow: -1}).exploreSaturationWindow(); got != 0 {
		t.Errorf("negative saturation window = %d, want 0 (disabled)", got)
	}
	if got := l.exploreSaturationMin(); got != DefaultExploreSaturationMin {
		t.Errorf("default saturation min = %v, want %v", got, DefaultExploreSaturationMin)
	}
}

// The token cap exists because a TURN cap means different things at different
// windows. Pin the arithmetic that motivated it, so a future edit cannot quietly
// set a value that is once again unreachable at a large window.
func TestExploreTokenCapIsReachableAtALargeWindow(t *testing.T) {
	const perStepAtCtx131k = 100_000 // ~what a full 131072 window costs per turn
	turnsToTrip := DefaultExploreTokenCap / perStepAtCtx131k
	if turnsToTrip >= DefaultExploreTotalCap {
		t.Fatalf("token cap trips after %d turns at ctx 131k, but the turn cap is %d — "+
			"the token rail would never fire first and the run is unbounded in practice",
			turnsToTrip, DefaultExploreTotalCap)
	}
}

// The corrective must name a checkable number and give both legitimate exits — a
// weak model nudged with an unfalsifiable claim argues with it instead of acting.
func TestSaturationCorrectiveNamesTheEvidenceAndTheExits(t *testing.T) {
	msg := (&Loop{}).saturationCorrective(17)
	for _, want := range []string{"17 distinct targets", "finish", "edit", "Do NOT read"} {
		if !contains(msg.Content, want) {
			t.Errorf("corrective missing %q:\n%s", want, msg.Content)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
