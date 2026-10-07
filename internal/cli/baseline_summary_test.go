package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
)

// TestRunSummaryCarriesTheVerifyBaseline: a run closed on a STILL-RED verify, because
// the red was already there and was never the agent's to fix, is a different outcome
// from a green one — and a harness has to be able to tell them apart WITHOUT parsing
// prose. See agent/baseline.go for the incident: kloo scored a correct scoped patch
// as a failure because an out-of-scope committed-red spec was still red.
func TestRunSummaryCarriesTheVerifyBaseline(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonSuccess,
		FinalVerify: agent.VerifyResult{
			Command: "npm test", ExitCode: 1, Passed: false,
			Stdout: "LoginPage constrains the login form width FAILED\n",
		},
		Baseline: &agent.BaselineEvidence{
			Attempted:   true,
			Taken:       true,
			Failing:     []string{"constrains the login form width"},
			Unreachable: []string{"frontend/src/app/pages/login/login.page.spec.ts"},
			Tolerated:   true,
		},
	}
	s := buildRunSummary(config.Config{Model: "m"}, "npm test", rep, time.Second, nil)
	if s.Verify == nil || s.Verify.Baseline == nil {
		t.Fatal("the summary carries no verify.baseline block")
	}
	bl := s.Verify.Baseline
	if !bl.Attempted || !bl.Taken || !bl.Tolerated {
		t.Fatalf("baseline block = %+v, want attempted+taken+tolerated", bl)
	}
	if len(bl.Unreachable) != 1 {
		t.Fatalf("baseline block loses the unreachable files: %+v", bl)
	}
	// final_reason must distinguish it from an ordinary green success.
	if s.FinalReason != "success_baseline_tolerated" {
		t.Fatalf("final_reason = %q, want success_baseline_tolerated", s.FinalReason)
	}
	// And it must actually serialise under verify.baseline.
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"baseline":`, `"tolerated":true`, `"unreachable":`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("KLOO_RESULT_JSON does not contain %s:\n%s", want, b)
		}
	}
}

// TestRunSummaryOmitsTheBaselineWhenNoneWasTaken: an unscoped run takes no baseline,
// and its JSON must be exactly what it was before baselines existed.
func TestRunSummaryOmitsTheBaselineWhenNoneWasTaken(t *testing.T) {
	rep := &agent.Report{Reason: agent.ReasonSuccess, FinalVerify: agent.VerifyResult{Command: "go test ./...", Passed: true}}
	s := buildRunSummary(config.Config{Model: "m"}, "go test ./...", rep, time.Second, nil)
	if s.Verify != nil && s.Verify.Baseline != nil {
		t.Fatalf("baseline block present without a baseline: %+v", s.Verify.Baseline)
	}
	if s.FinalReason != "success" {
		t.Fatalf("final_reason = %q, want success", s.FinalReason)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), `"baseline"`) {
		t.Fatalf("baseline key leaked into a run that took none:\n%s", b)
	}
}

// TestFailureDetailQuotesAFailingLine: failure_detail.message quoted the FIRST
// non-empty line of verify output, which in every real test runner is a banner or a
// green tick — so a captured KLOO_RESULT_JSON reported a PASSING assertion as the
// reason the run failed.
func TestFailureDetailQuotesAFailingLine(t *testing.T) {
	out := "Chrome Headless 140.0.0.0 (Linux x86_64) LoginPage shows the form SUCCESS\n" +
		"Chrome Headless 140.0.0.0 (Linux x86_64) LoginPage constrains the login form width FAILED\n"
	rep := &agent.Report{
		Reason:      agent.ReasonAnswered,
		FinalVerify: agent.VerifyResult{Command: "npm test", ExitCode: 1, Passed: false, Stdout: out},
	}
	code, detail := classifyFailure(rep, nil)
	if code != "verify_failed" {
		t.Fatalf("failure_code = %q, want verify_failed", code)
	}
	if strings.Contains(detail.Message, "SUCCESS") {
		t.Fatalf("failure_detail.message quotes a PASSING line: %q", detail.Message)
	}
	if !strings.Contains(detail.Message, "FAILED") {
		t.Fatalf("failure_detail.message does not quote the failing line: %q", detail.Message)
	}
}

// TestFirstFailingLineFallsBackToTheFirstLine: when nothing in the output looks like
// a failure at all, the old behaviour stands rather than reporting nothing.
func TestFirstFailingLineFallsBackToTheFirstLine(t *testing.T) {
	if got := firstFailingLine("", "exit status 2\n"); got != "exit status 2" {
		t.Fatalf("firstFailingLine = %q, want the fallback first line", got)
	}
	if got := firstFailingLine("✓ all good\nsomething neutral\n"); got != "something neutral" {
		t.Fatalf("firstFailingLine = %q; a green tick must not be reported as the failure", got)
	}
}

// TestBothLoopSitesCarryTheScopeAndVerifyCommand: the loop can only consult the write
// scope if the construction sites give it one, and VerifyCmd was missing from the TUI
// literal entirely — silently disabling protectedByVerify (so the model could edit
// the very test it is graded on), failingTestSource and subsetTestWarning in
// INTERACTIVE use, which is the path a human actually runs.
func TestBothLoopSitesCarryTheScopeAndVerifyCommand(t *testing.T) {
	for _, file := range []string{"headless.go", "tui.go"} {
		keys := loopLiteralKeys(t, file)
		for _, want := range []string{"Scope", "VerifyCmd"} {
			if !keys[want] {
				t.Errorf("%s: agent.Loop literal does not set %s", file, want)
			}
		}
	}
}
