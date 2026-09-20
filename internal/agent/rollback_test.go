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
func TestStockBehaviourUnchanged(t *testing.T) {
	t.Setenv("KLOO_KEEP_WORK_ON_FAIL", "")
	l := &Loop{}
	for _, r := range []Reason{ReasonAnswered, ReasonChurn, ReasonError, ReasonExploreStop} {
		if !l.shouldRollback(r, VerifyResult{}) {
			t.Fatalf("%s did not roll back with the flag off", r)
		}
	}
}
