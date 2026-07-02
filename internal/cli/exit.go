package cli

import "fmt"

const (
	benchmarkExitVerify      = 10
	benchmarkExitModel       = 11
	benchmarkExitTool        = 12
	benchmarkExitContext     = 13
	benchmarkExitRepetition  = 14
	benchmarkExitJSON        = 15
	benchmarkExitBudget      = 16
	benchmarkExitConfigError = 17
	benchmarkExitInterrupted = 18
	benchmarkExitInternal    = 19
	benchmarkExitAnswered    = 20
	benchmarkExitScope       = 21
)

type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit %d", e.code)
	}
	return e.err.Error()
}

func (e exitError) Unwrap() error { return e.err }

func benchmarkExitCode(summary runSummary) int {
	if summary.Success {
		return 0
	}
	switch summary.FailureCode {
	case "verify_failed", "unverified":
		return benchmarkExitVerify
	case "model_error":
		return benchmarkExitModel
	case "tool_call_invalid", "tool_error":
		return benchmarkExitTool
	case "context_too_small":
		return benchmarkExitContext
	case "repetition_halt", "edit_failed":
		return benchmarkExitRepetition
	case "off_scope_edit":
		return benchmarkExitScope
	case "precheck_failed", "postcheck_failed":
		// Hook gate failures are verify-family (a gate around verify did not pass);
		// the specific failure_code distinguishes them for analysis.
		return benchmarkExitVerify
	case "json_invalid":
		return benchmarkExitJSON
	case "budget_exceeded":
		return benchmarkExitBudget
	case "config_error":
		return benchmarkExitConfigError
	case "interrupted":
		return benchmarkExitInterrupted
	case "answered":
		return benchmarkExitAnswered
	default:
		return benchmarkExitInternal
	}
}
