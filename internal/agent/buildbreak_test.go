package agent

import (
	"strings"
	"testing"
)

const a05Output = `Transform failed with 2 errors:
/w/src/modules-v2/attendance/calendar-helpers.ts:555:12: ERROR: The symbol "restDay" has already been declared
/w/src/modules-v2/attendance/calendar-helpers.ts:556:12: ERROR: The symbol "isHoliday" has already been declared`

// TestBuildBreakDetectedFromRealOutput: the exact vitest output from kloo-bench
// A05, where an edit introduced duplicate declarations and the gate then collected
// ZERO tests.
func TestBuildBreakDetectedFromRealOutput(t *testing.T) {
	broken, detail := buildBreak(a05Output)
	if !broken {
		t.Fatal("a transform failure was not recognised as a broken build")
	}
	if !strings.Contains(detail, "restDay") || !strings.Contains(detail, "isHoliday") {
		t.Fatalf("detail does not quote the errors: %q", detail)
	}
}

// TestFailingTestIsNotABuildBreak: an ordinary assertion failure is information,
// not damage. Treating it as a break would fire the urgent path on every red test
// and make the message worthless.
func TestFailingTestIsNotABuildBreak(t *testing.T) {
	out := `FAIL tests/a.test.ts > counts each non-cache read
AssertionError: expected 3 to be 4
  - Expected
  + Received`
	if broken, _ := buildBreak(out); broken {
		t.Fatal("a normal assertion failure was treated as a broken build")
	}
}

// TestUnbuildableTreeIsRolledBackEvenWhenKeepingWork: keeping partial work is
// right; handing back a tree that does not compile is not. A05's run left the file
// untransformable, which is strictly worse than the state it started in.
func TestUnbuildableTreeIsRolledBackEvenWhenKeepingWork(t *testing.T) {
	t.Setenv("KLOO_KEEP_WORK_ON_FAIL", "1")
	t.Setenv("KLOO_BUILD_BREAK_GUARD", "1")
	l := &Loop{}
	broken := VerifyResult{Command: "vitest", Passed: false, Stdout: a05Output}
	if !l.shouldRollback(ReasonExploreStop, broken) {
		t.Fatal("an unbuildable tree was handed back to the user")
	}
	// A run that merely failed its tests still keeps its work.
	red := VerifyResult{Command: "vitest", Passed: false, Stdout: "AssertionError: expected 3 to be 4"}
	if l.shouldRollback(ReasonExploreStop, red) {
		t.Fatal("a legitimate partial fix was discarded")
	}
}

// TestBuildBreakGuardIsOptIn.
func TestBuildBreakGuardIsOptIn(t *testing.T) {
	t.Setenv("KLOO_KEEP_WORK_ON_FAIL", "1")
	t.Setenv("KLOO_BUILD_BREAK_GUARD", "")
	l := &Loop{}
	broken := VerifyResult{Command: "vitest", Passed: false, Stdout: a05Output}
	if l.shouldRollback(ReasonExploreStop, broken) {
		t.Fatal("guard active with the flag off")
	}
}
