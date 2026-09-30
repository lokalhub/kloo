package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
)

// TestReportRendersFinishSummary is the regression guard for a defect that made
// kloo feel broken on question tasks: the model called finish with a summary — on
// a question, that summary IS the answer — and the terminal rendered only
// steps/tokens/elapsed. The user saw a receipt for work whose result was thrown
// away. Reported after kloo spent 3m39s and 121.6k tokens on "can you help me
// identify my workspace?" and displayed nothing but the counters.
func TestReportRendersFinishSummary(t *testing.T) {
	const answer = "Your workspace is a multi-project tree with seven top-level areas."
	out := reportItem{r: reportMsg{
		Reason:  "unverified",
		Steps:   8,
		Tokens:  121600,
		Elapsed: "3m39s",
		Summary: answer,
	}}.render(100)

	if !strings.Contains(out, answer) {
		t.Fatalf("the model's answer is missing from the report:\n%s", out)
	}
	// It must come BEFORE the counters: prose under a wall of statistics reads as
	// an afterthought, and the answer is the point of the run.
	if strings.Index(out, answer) > strings.Index(out, "run stopped") {
		t.Errorf("the answer should precede the stop banner:\n%s", out)
	}
}

// TestReportWithoutSummaryIsUnchanged: a run that never called finish (a rail
// stop, an error) has no summary, and its banner must look exactly as before —
// no stray blank lines, no empty block above it.
func TestReportWithoutSummaryIsUnchanged(t *testing.T) {
	out := reportItem{r: reportMsg{Reason: "churn", Steps: 3, Tokens: 900, Elapsed: "12s"}}.render(100)
	if strings.HasPrefix(strings.TrimSpace(stripBorder(out)), "\n") {
		t.Errorf("empty summary left a blank line:\n%s", out)
	}
	if !strings.Contains(out, "run stopped") {
		t.Errorf("banner missing:\n%s", out)
	}
}

// TestReportForCarriesSummary: the bridge from the agent Report must actually pass
// it through. The field existing on the message is worthless if reportFor drops it
// — which is precisely how the original defect worked, with agent.Report.Summary
// populated all along.
func TestReportForCarriesSummary(t *testing.T) {
	rep := &agent.Report{
		Reason:     agent.ReasonUnverified,
		Steps:      2,
		TokensUsed: 10,
		Elapsed:    time.Second,
		Summary:    "the answer text",
	}
	if got := reportFor(rep, 0); got.Summary != "the answer text" {
		t.Errorf("reportFor dropped the summary, got %q", got.Summary)
	}
}

// stripBorder removes the lipgloss box drawing so the assertion is about content.
func stripBorder(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		b.WriteString(strings.Trim(line, "│┌┐└┘─ "))
		b.WriteString("\n")
	}
	return b.String()
}
