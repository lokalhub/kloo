package tui

import (
	"strings"
	"testing"
	"time"
)

// headerLine returns the content line of the rendered header (between borders).
func headerLine(m Model) string {
	return strings.Split(m.View(), "\n")[1]
}

// TestHeaderReframeLeadsModelEffort (task 06): the header leads with
// `kloo • model • effort` + the live token total; step is present but demoted
// (appears after the lead), and the header never leads with step.
func TestHeaderReframeLeadsModelEffort(t *testing.T) {
	m := sized(New(Config{Model: "test-model", Effort: "medium", MaxSteps: 80, MaxTokens: 200000}), tw, th)
	m = apply(m, progressMsg{Model: "test-model", Step: 18, MaxSteps: 80, Tokens: 14400, MaxTokens: 200000})
	h := headerLine(m)

	if !contains(h, "kloo  test-model · medium") {
		t.Errorf("header should lead with kloo • model • effort:\n%s", h)
	}
	if !contains(h, "14.4k/200k tok") || contains(h, "0/200k tok") {
		t.Errorf("header should show the live non-zero token total:\n%s", h)
	}
	if !contains(h, "step 18/80") {
		t.Errorf("step should still be present (demoted):\n%s", h)
	}
	if strings.Index(h, "kloo") > strings.Index(h, "step") {
		t.Errorf("model/effort must precede the demoted step:\n%s", h)
	}
	requireGolden(t, "header-reframe.golden", m.View())
}

// TestThinkingLineActivity (task 06): the thinking line carries the tool-derived
// activity phrase alongside the live token count.
func TestThinkingLineActivity(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 80, MaxTokens: 200000}), tw, th)
	m = apply(m,
		progressMsg{Tokens: 14400, MaxTokens: 200000},
		toolEventMsg{Name: "run_command", Command: "npm run build", ExitCode: 0},
	)
	m.running = true
	m.verb = "Percolating"
	m.runStart = time.Now()
	line := m.renderThinking()

	if !strings.Contains(line, "running npm run build") {
		t.Errorf("thinking line missing activity phrase: %q", line)
	}
	if !strings.Contains(line, "14.4k tok") || strings.Contains(line, "0 tok") {
		t.Errorf("thinking line missing live token count: %q", line)
	}
}

// TestActivityPhraseFromTools (task 06): the activity field is derived per tool.
func TestActivityPhraseFromTools(t *testing.T) {
	cases := []struct {
		msg  toolEventMsg
		want string
	}{
		{toolEventMsg{Name: "edit_file", Path: "src/app/app.ts"}, "editing src/app/app.ts"},
		{toolEventMsg{Name: "run_command", Command: "npm run build"}, "running npm run build"},
		{toolEventMsg{Name: "read_file", Summary: "app.go (42 lines)"}, "reading app.go (42 lines)"},
	}
	for _, tc := range cases {
		if got := activityPhrase(tc.msg); got != tc.want {
			t.Errorf("activityPhrase(%+v) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

// TestThinkingLineFrame (task 06, Product): a deterministic thinking-line frame
// with a frozen spinner glyph + elapsed, matching thinking-line.html at intent —
// verb + activity phrase + non-zero token total + esc-to-interrupt affordance.
// The frozen state (fixed verb/spinFrame, runStart 12s in the past → elapsed=12s)
// makes the golden stable the same way the static mock is.
func TestThinkingLineFrame(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 80, MaxTokens: 200000}), tw, th)
	m = apply(m,
		progressMsg{Tokens: 14400, MaxTokens: 200000},
		toolEventMsg{Name: "edit_file", Path: "src/app/app.ts", Search: "x", Replace: "y"},
	)
	m.running = true
	m.verb = "Moonwalking"
	m.spinFrame = 2                                // spinnerFrames[2] == "⠹" (matches the mock)
	m.runStart = time.Now().Add(-12 * time.Second) // elapsed renders as a stable 12s

	line := m.renderThinking()
	for _, want := range []string{"⠹ Moonwalking…", "editing src/app/app.ts", "12s", "14.4k tok", "esc to interrupt"} {
		if !strings.Contains(line, want) {
			t.Errorf("thinking-line frame missing %q: %q", want, line)
		}
	}
	requireGolden(t, "thinking-line.golden", line+"\n")
}

// TestActivityClearsOnReport (task 06): a finished run shows no stale phrase.
func TestActivityClearsOnReport(t *testing.T) {
	m := apply(tall(), toolEventMsg{Name: "run_command", Command: "npm run build", ExitCode: 0})
	if m.activity == "" {
		t.Fatal("activity should be set after a tool event")
	}
	m2 := apply(m, reportMsg{Reason: "success", Steps: 1})
	if m2.activity != "" {
		t.Errorf("activity should clear on report, got %q", m2.activity)
	}
}
