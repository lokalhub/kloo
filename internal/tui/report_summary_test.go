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
	// And now it is the ONLY substantial thing there. The counters that used to sit
	// under every answer are kloo's bookkeeping, not the reply, and they moved to
	// /verbose — so the defect this test was written for cannot come back in the
	// weaker form either (an answer buried under a receipt).
	for _, machinery := range []string{"run stopped", "steps:", "121.6k", "tokens:"} {
		if strings.Contains(out, machinery) {
			t.Errorf("the default outcome should not carry %q:\n%s", machinery, out)
		}
	}
	// "unverified" is not silence: the work was done with nothing to check it.
	if !strings.Contains(out, "no build or test command") {
		t.Errorf("an unverified run should say so in one line:\n%s", out)
	}
	// Elapsed survives as a dim footer — one fact, not a table.
	if !strings.Contains(out, "3m39s") {
		t.Errorf("elapsed should remain in the footer:\n%s", out)
	}
}

// TestVerboseReportKeepsTheCounterBanner: /verbose is the escape hatch, so the old
// banner must still be reachable in full — this is the view a stopped run, a budget
// being tuned, or a benchmark is read in.
func TestVerboseReportKeepsTheCounterBanner(t *testing.T) {
	out := reportItem{verbose: true, r: reportMsg{
		Reason: "budget-exceeded", Steps: 40, Tokens: 7900, MaxTokens: 8000,
		Elapsed: "2m18s", Detail: "max-steps (40/40)", Rails: "explore-nudge×2",
	}}.render(100)
	for _, want := range []string{"run stopped — BUDGET EXCEEDED", "max-steps (40/40)", "steps:   40", "7.9k/8k", "self-corrections: explore-nudge×2"} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose report missing %q:\n%s", want, out)
		}
	}
}

// TestQuietStopNotesSpeakAboutTheTask: one sentence per outcome, in the user's
// terms. The rule being pinned is that NO outcome prints a tripped-counter name
// ("BUDGET EXCEEDED", "steps 40/40", "churn") at this surface, and that a finished
// run prints no commentary at all.
func TestQuietStopNotesSpeakAboutTheTask(t *testing.T) {
	cases := []struct {
		r    reportMsg
		want string // "" = nothing to say
	}{
		{reportMsg{Reason: "success"}, ""},
		{reportMsg{Reason: "answered"}, ""},
		{reportMsg{Reason: "interrupted"}, ""},
		{reportMsg{Reason: "budget-exceeded", Detail: "max-steps (40/40)"}, `Type "continue"`},
		{reportMsg{Reason: "budget-exceeded", Detail: "memory (limit 8.0 GiB, observed 8.1 GiB resident)"}, "out of memory"},
		{reportMsg{Reason: "churn", Detail: "repeated-failure"}, "repeating itself"},
		{reportMsg{Reason: "unverified"}, "no build or test command"},
		{reportMsg{Reason: "error", Detail: "Couldn't reach the model endpoint — is the server running?"}, "Couldn't reach the model endpoint"},
	}
	for _, tc := range cases {
		got := quietStopNote(tc.r)
		if tc.want == "" {
			if got != "" {
				t.Errorf("%s: expected no commentary, got %q", tc.r.Reason, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: note %q should mention %q", tc.r.Reason, got, tc.want)
		}
		for _, jargon := range []string{"BUDGET", "40/40", "churn", "rail"} {
			if strings.Contains(got, jargon) {
				t.Errorf("%s: the note leaks internals (%q): %q", tc.r.Reason, jargon, got)
			}
		}
	}
}

// TestReportWithoutSummaryIsUnchanged: a run that never called finish (a rail
// stop, an error) has no summary — no stray blank lines, no empty block, and the
// one-sentence note standing on its own.
func TestReportWithoutSummaryIsUnchanged(t *testing.T) {
	out := reportItem{r: reportMsg{Reason: "churn", Steps: 3, Tokens: 900, Elapsed: "12s"}}.render(100)
	if strings.HasPrefix(strings.TrimSpace(stripBorder(out)), "\n") {
		t.Errorf("empty summary left a blank line:\n%s", out)
	}
	// With no answer to show, the sentence about what happened is the whole output —
	// and it has to be there, or a stopped run would end in silence.
	if !strings.Contains(out, "repeating itself") {
		t.Errorf("a stopped run with no summary must still say what happened:\n%s", out)
	}
	if strings.Contains(out, "run stopped") {
		t.Errorf("the counter banner belongs to /verbose now:\n%s", out)
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
