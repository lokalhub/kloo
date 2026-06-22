package agent

import (
	"fmt"
	"strings"
)

// The Report struct (the source of truth, in types.go) is produced by the loop
// on every termination path. This file adds the human-readable rendering — which
// is DERIVED from the struct, never a second copy of the data — so the CLI/non-
// TUI path can print it while the TUI (Phase 05) consumes the struct directly.

// Succeeded reports whether the run ended in success.
func (r *Report) Succeeded() bool { return r.Reason == ReasonSuccess }

// String renders the report for a human, naming the reason and its evidence.
func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "kloo run %s after %d step(s)", r.Reason, r.Steps)

	switch r.Reason {
	case ReasonSuccess:
		b.WriteString(" — verification passed")
	case ReasonBudgetExceeded:
		if r.Budget != nil {
			fmt.Fprintf(&b, " — %s budget exceeded (limit %s, observed %s)", r.Budget.Kind, r.Budget.Limit, r.Budget.Observed)
		}
	case ReasonChurn:
		if r.Churn != nil {
			fmt.Fprintf(&b, " — churn: %s", r.Churn.Kind)
			if art := strings.TrimSpace(r.Churn.Artifact); art != "" {
				fmt.Fprintf(&b, " (repeated: %s)", firstLine(art))
			}
		}
	case ReasonError:
		if r.Err != nil {
			fmt.Fprintf(&b, " — error: %v", r.Err)
		}
	case ReasonInterrupted:
		b.WriteString(" — interrupted")
	}

	// The real verify signal (never a model claim).
	fmt.Fprintf(&b, "\n  verify: %q exit=%d passed=%v", r.FinalVerify.Command, r.FinalVerify.ExitCode, r.FinalVerify.Passed)
	if out := strings.TrimSpace(r.FinalVerify.Stdout + "\n" + r.FinalVerify.Stderr); out != "" {
		fmt.Fprintf(&b, "\n  output: %s", firstLine(out))
	}

	fmt.Fprintf(&b, "\n  tokens=%d elapsed=%s", r.TokensUsed, r.Elapsed)
	if r.RolledBack {
		b.WriteString("\n  working tree rolled back to checkpoint")
	}
	if len(r.Ignored) > 0 {
		fmt.Fprintf(&b, "\n  ignored %d extra tool call(s): %s", len(r.Ignored), strings.Join(r.Ignored, ", "))
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
