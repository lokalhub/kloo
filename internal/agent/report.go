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
		// " — verification passed" is a lie on a baseline-tolerated success: the verify
		// is still red. Say which kind of success this was (agent/baseline.go).
		if r.Baseline != nil && r.Baseline.Tolerated {
			b.WriteString(" — the change stands; the verify is still red with the SAME failures it had before the run, and they are outside the allowed edit scope")
		} else {
			b.WriteString(" — verification passed")
		}
	case ReasonBudgetExceeded:
		if r.Budget != nil {
			fmt.Fprintf(&b, " — %s budget exceeded (limit %s, observed %s)", r.Budget.Kind, r.Budget.Limit, r.Budget.Observed)
			// The memory ceiling is the one budget whose remedy is not obvious from the
			// numbers, so it gets its own line rather than being crammed into Observed
			// (which renders mid-sentence, before the limit).
			if r.Budget.Kind == BudgetMemory {
				fmt.Fprintf(&b, "\n  %s", memCeilingAdvice())
			}
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
	case ReasonUnverified:
		b.WriteString(" — finished, but no verify command was available (unverified — pass --verify to gate on one)")
	}

	// The real verify signal (never a model claim). Omitted in unverified mode,
	// where no command ran — an empty/zero verify line would just be noise.
	if r.FinalVerify.Command != "" {
		if r.FinalVerify.Err != nil {
			// "exit=0 passed=false" reads as a contradiction; say it could not run.
			fmt.Fprintf(&b, "\n  verify: %q DID NOT RUN: %v", r.FinalVerify.Command, r.FinalVerify.Err)
		} else {
			fmt.Fprintf(&b, "\n  verify: %q exit=%d passed=%v", r.FinalVerify.Command, r.FinalVerify.ExitCode, r.FinalVerify.Passed)
		}
		if out := strings.TrimSpace(r.FinalVerify.Stdout + "\n" + r.FinalVerify.Stderr); out != "" {
			fmt.Fprintf(&b, "\n  output: %s", firstLine(out))
		}
	}

	// The pre-edit baseline, when one was taken. Printed only then, so an unscoped
	// run's report is byte-identical to before.
	if bl := r.Baseline; bl != nil {
		switch {
		case !bl.Taken:
			fmt.Fprintf(&b, "\n  baseline: not taken (%s)", bl.Skipped)
		case bl.Passed:
			b.WriteString("\n  baseline: verify was GREEN before the first edit")
		default:
			fmt.Fprintf(&b, "\n  baseline: verify was ALREADY RED before the first edit (%d known failure(s))", len(bl.Failing))
			if len(bl.Unreachable) > 0 {
				fmt.Fprintf(&b, "; implicated files are outside the edit scope: %s", strings.Join(bl.Unreachable, ", "))
			}
		}
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
