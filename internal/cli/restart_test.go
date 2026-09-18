package cli

import (
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
)

// TestOnlyRailStopsRestart: a fresh attempt can only help a run that a RAIL ended
// for going nowhere. A budget or ceiling stop has no clock left to spend, and an
// error repeats — restarting either would burn the remaining time for nothing.
//
// Measured on kloo-bench: two matched-load repeats of the same binary and config
// flipped 3 of 7 cases (A06 998s pass → 2400s fail; A16 423s pass → 1914s fail;
// A05 the other way), while grok flipped 0 of 20 over two runs. kloo passes C66
// 44% of the time, A06 48%, A16 46% — the capability is there, the convergence is
// not, and a second independent attempt is worth 4.3 → 6.7 expected passes over
// the 12 non-deterministic cases.
func TestOnlyRailStopsRestart(t *testing.T) {
	for _, tc := range []struct {
		reason agent.Reason
		want   bool
		why    string
	}{
		{agent.ReasonExploreStop, true, "the rail stopped a run that was reading without acting"},
		{agent.ReasonChurn, true, "the rail stopped a run repeating itself"},
		{agent.ReasonBudgetExceeded, false, "no budget left to spend on a second attempt"},
		{agent.ReasonError, false, "an error is not improved by repeating it"},
		{agent.ReasonSuccess, false, "nothing to retry"},
		{agent.ReasonAnswered, false, "a calm answer is not a non-convergence"},
		{agent.ReasonUnverified, false, "nothing was checked; a retry cannot change that"},
	} {
		if got := isStallReason(tc.reason); got != tc.want {
			t.Errorf("isStallReason(%q) = %v, want %v — %s", tc.reason, got, tc.want, tc.why)
		}
	}
}

// TestRestartIsOffByDefault: it changes how long a failing run takes and what it
// reports, so it does not get the default path without measurement.
func TestRestartIsOffByDefault(t *testing.T) {
	t.Setenv("KLOO_RESTART_ON_STALL", "")
	if restartOnStall() {
		t.Error("restart is on with the flag unset")
	}
	t.Setenv("KLOO_RESTART_ON_STALL", "1")
	if !restartOnStall() {
		t.Error("restart did not switch on with the flag set")
	}
}

// TestRestartBudgetDefaultsToTheBenchCeiling: the budget covers BOTH attempts, so a
// first attempt that already burned the clock leaves nothing and the restart is
// correctly skipped rather than starting a run that cannot finish.
func TestRestartBudgetDefaultsToTheBenchCeiling(t *testing.T) {
	t.Setenv("KLOO_RESTART_BUDGET_S", "")
	if got, want := restartBudget(), 2400*time.Second; got != want {
		t.Errorf("restartBudget() = %s, want %s (the kloo-bench ceiling)", got, want)
	}
	t.Setenv("KLOO_RESTART_BUDGET_S", "600")
	if got, want := restartBudget(), 600*time.Second; got != want {
		t.Errorf("restartBudget() = %s, want %s", got, want)
	}
}
