package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReportRendersEachReason(t *testing.T) {
	cases := []struct {
		name   string
		report Report
		want   []string // substrings the rendering must contain
	}{
		{
			name: "success",
			report: Report{
				Reason: ReasonSuccess, Steps: 4,
				FinalVerify: VerifyResult{Command: "go test", ExitCode: 0, Passed: true},
				TokensUsed:  120, Elapsed: 2 * time.Second,
			},
			want: []string{"success", "4 step", "passed", "go test"},
		},
		{
			name: "budget-exceeded",
			report: Report{
				Reason: ReasonBudgetExceeded, Steps: 40,
				Budget:      &BudgetEvidence{Kind: BudgetSteps, Limit: "40", Observed: "41"},
				FinalVerify: VerifyResult{Command: "go test", ExitCode: 1, Passed: false},
			},
			want: []string{"budget-exceeded", "steps", "limit 40", "observed 41"},
		},
		{
			name: "churn",
			report: Report{
				Reason: ReasonChurn, Steps: 6,
				Churn:       &ChurnEvidence{Kind: ChurnRepeatedFailure, Artifact: "FAIL: x != y"},
				FinalVerify: VerifyResult{Command: "go test", ExitCode: 1, Passed: false},
			},
			want: []string{"churn", "repeated-failure", "FAIL: x != y"},
		},
		{
			name: "error",
			report: Report{
				Reason: ReasonError, Steps: 2,
				Err:         errors.New("verify: command not found"),
				FinalVerify: VerifyResult{Command: "missing-cmd"},
			},
			want: []string{"error", "command not found"},
		},
		{
			name: "interrupted",
			report: Report{
				Reason: ReasonInterrupted, Steps: 3,
				RolledBack:  true,
				FinalVerify: VerifyResult{Command: "go test", ExitCode: 1},
			},
			want: []string{"interrupted", "rolled back"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.report.String()
			for _, w := range tc.want {
				if !strings.Contains(s, w) {
					t.Errorf("rendering missing %q:\n%s", w, s)
				}
			}
			// The real verify signal is always present (the struct is the truth).
			if !strings.Contains(s, "verify:") {
				t.Errorf("rendering must surface the real verify signal:\n%s", s)
			}
		})
	}
}

func TestReportSucceeded(t *testing.T) {
	if !(&Report{Reason: ReasonSuccess}).Succeeded() {
		t.Error("success report should report Succeeded() true")
	}
	if (&Report{Reason: ReasonChurn}).Succeeded() {
		t.Error("non-success report should report Succeeded() false")
	}
}

// TestReportStructIsSourceOfTruth: the rendering is derived from the struct — a
// field change is reflected in String() (no divergent second copy).
func TestReportStructIsSourceOfTruth(t *testing.T) {
	r := Report{Reason: ReasonSuccess, Steps: 1, FinalVerify: VerifyResult{Command: "npm run build", Passed: true}}
	if !strings.Contains(r.String(), "npm run build") {
		t.Errorf("rendering should reflect the struct's command field")
	}
	r.FinalVerify.Command = "go vet"
	if !strings.Contains(r.String(), "go vet") || strings.Contains(r.String(), "npm run build") {
		t.Errorf("rendering should track the struct, not a stale copy")
	}
}
