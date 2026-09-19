package cli

import (
	"testing"

	"github.com/lokalhub/kloo/internal/agent"
)

// TestExploreStopIsARailNotAnInternalError: the exploration rail is the most common
// way a failing run ends, and it was falling through the classifier's default arm —
// reported as failure_code "internal_error", class "unknown_reason". Measured across
// 386 recorded kloo-bench failures, 228 (59%) were exactly this: the single most
// frequent outcome, labelled "unknown" and blamed on an internal fault.
func TestExploreStopIsARailNotAnInternalError(t *testing.T) {
	rep := &agent.Report{Reason: agent.ReasonExploreStop}
	code, detail := classifyFailure(rep, nil)
	if code == "internal_error" {
		t.Error("explore-stop still classifies as internal_error: a rail decision is not a kloo fault")
	}
	if code != "exploration_halt" {
		t.Errorf("failure_code = %q, want %q", code, "exploration_halt")
	}
	if detail.Class == "unknown_reason" {
		t.Error(`class is still "unknown_reason": the most common failure mode stays unlabelled`)
	}
	if detail.Source != "rail" {
		t.Errorf("source = %q, want %q", detail.Source, "rail")
	}
}

// TestExploreStopPrefersTheVerifyStory: when a verify actually ran and failed, that
// is the more specific and more useful classification, matching the precedence the
// answered/unverified arms already use. Without this the rail label would mask a
// real test failure.
func TestExploreStopPrefersTheVerifyStory(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonExploreStop,
		FinalVerify: agent.VerifyResult{
			Command: "npx vitest run", Passed: false, ExitCode: 1,
		},
	}
	code, detail := classifyFailure(rep, nil)
	if code == "exploration_halt" {
		t.Error("a failing verify was masked by the rail label")
	}
	if detail.Source != "verify" {
		t.Errorf("source = %q, want %q", detail.Source, "verify")
	}
}
