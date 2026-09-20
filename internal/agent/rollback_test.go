package agent

import "testing"

// TestKeepWorkOnFailPreservesRealWork: measured on kloo-bench C62. kloo made the
// correct fix, verify stayed red because the verify command also names a Playwright
// spec that cannot run under vitest at all, the run ended "answered", and the fix
// was erased — the gate then scored unfixed code while grok, which does not roll
// back, passed with the same fix on disk.
func TestKeepWorkOnFailPreservesRealWork(t *testing.T) {
	t.Setenv("KLOO_KEEP_WORK_ON_FAIL", "1")
	l := &Loop{}
	for _, r := range []Reason{ReasonAnswered, ReasonExploreStop, ReasonChurn, ReasonBudgetExceeded, ReasonUnverified} {
		if l.shouldRollback(r, VerifyResult{}) {
			t.Fatalf("%s discarded the run's edits", r)
		}
	}
}

// TestUntrustworthyOutcomesStillRollBack: an internal error, an interrupt or a
// safety stop can leave the tree mid-change. Those must still be undone.
func TestUntrustworthyOutcomesStillRollBack(t *testing.T) {
	t.Setenv("KLOO_KEEP_WORK_ON_FAIL", "1")
	l := &Loop{}
	for _, r := range []Reason{ReasonError, ReasonInterrupted, ReasonSafetyStop} {
		if !l.shouldRollback(r, VerifyResult{}) {
			t.Fatalf("%s left a possibly-broken tree in place", r)
		}
	}
}

// TestSuccessNeverRollsBack, flag or no flag.
func TestSuccessNeverRollsBack(t *testing.T) {
	l := &Loop{}
	for _, on := range []string{"", "1"} {
		t.Setenv("KLOO_KEEP_WORK_ON_FAIL", on)
		if l.shouldRollback(ReasonSuccess, VerifyResult{}) {
			t.Fatal("a successful run rolled back")
		}
	}
}

// TestStockBehaviourUnchanged: with the flag off, every non-success exit rolls
// back exactly as the released binary does.
func TestKeepWorkCanBeDisabled(t *testing.T) {
	t.Setenv("KLOO_KEEP_WORK_ON_FAIL", "0")
	l := &Loop{}
	for _, r := range []Reason{ReasonAnswered, ReasonChurn, ReasonError, ReasonExploreStop} {
		if !l.shouldRollback(r, VerifyResult{}) {
			t.Fatalf("%s did not roll back with KLOO_KEEP_WORK_ON_FAIL=0", r)
		}
	}
}

// TestKeepWorkOnFailIsTheDefault: from v0.22.0 a run that merely ran out of road
// keeps what it wrote. This is the behaviour the four rollback integration tests
// used to assert the opposite of, and it is the change with the widest blast
// radius in the release — it is pinned here explicitly so nobody flips it back by
// accident.
func TestKeepWorkOnFailIsTheDefault(t *testing.T) {
	l := &Loop{}
	red := VerifyResult{Command: "vitest", Passed: false, Stdout: "AssertionError: expected 3 to be 4"}
	for _, r := range []Reason{ReasonAnswered, ReasonChurn, ReasonExploreStop, ReasonBudgetExceeded, ReasonUnverified} {
		if l.shouldRollback(r, red) {
			t.Fatalf("%s discarded the run's work under the v0.22.0 default", r)
		}
	}
	// The tree is still restored when it cannot be trusted.
	for _, r := range []Reason{ReasonError, ReasonInterrupted, ReasonSafetyStop} {
		if !l.shouldRollback(r, red) {
			t.Fatalf("%s left a possibly-broken tree in place", r)
		}
	}
}
