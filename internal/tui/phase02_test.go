package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
)

// TestTranscriptAirySpacing (task 07): blocks are separated by a blank line.
func TestTranscriptAirySpacing(t *testing.T) {
	m := apply(tall(),
		toolEventMsg{Name: "run_command", Command: "a", ExitCode: 0},
		toolEventMsg{Name: "run_command", Command: "b", ExitCode: 0},
	)
	if !strings.Contains(m.renderTranscript(), "\n\n") {
		t.Errorf("transcript blocks should be separated by a blank line:\n%s", m.renderTranscript())
	}
}

// TestPhaseComposedTranscript (task 07): the whole phase composed — reframed
// header, markdown assistant, diff card, pass + truncated-fail run cards, and a
// stop-report with a coloured verify line — renders coherently in one frame.
func TestPhaseComposedTranscript(t *testing.T) {
	m := sized(New(Config{Model: "test-model", Effort: "medium", MaxSteps: 80, MaxTokens: 200000}), tw, 60)
	m = apply(m,
		submitTaskMsg{task: "rename the three tabs"},
		progressMsg{Model: "test-model", Step: 1, MaxSteps: 80, Tokens: 400, MaxTokens: 200000},
		streamDeltaMsg{Content: "# Plan\nI'll rename the **three** tabs using `edit_file`.\n- edit tab1\n- run build"},
		streamDoneMsg{},
		toolEventMsg{Name: "edit_file", Path: "src/app/tabs/tabs.routes.ts", Search: "{ path: 'tab1' }", Replace: "{ path: 'home' }"},
		progressMsg{Model: "test-model", Step: 2, MaxSteps: 80, Tokens: 14400, MaxTokens: 200000},
		toolEventMsg{Name: "run_command", Command: "npm test", ExitCode: 0},
		longFailCard(),
		reportMsg{Reason: "error", Steps: 3, Tokens: 14400, MaxTokens: 200000, Elapsed: "31s", VerifyCmd: "npm run build", VerifyExit: 1},
	)
	v := m.View()
	requireGolden(t, "phase-composed.golden", v)

	for _, want := range []string{
		"kloo dev  test-model · medium", // reframed header lead (version "dev" for a local build)
		"● assistant", "• edit tab1",    // markdown assistant
		"✎ src/app/tabs/tabs.routes.ts", // diff card header
		"⌘ run_command", "exit 0 ✓",     // pass run card
		"ctrl+o to expand",    // truncated fail card
		"run stopped — ERROR", // stop report
		"→ exit 1 ✗",          // coloured verify line
	} {
		if !contains(v, want) {
			t.Errorf("composed frame missing %q:\n%s", want, v)
		}
	}
}

// TestPhaseComposedViaProgram (task 07): drive the composed sequence through the
// real program via teatest and toggle ctrl+o on the long fail card.
func TestPhaseComposedViaProgram(t *testing.T) {
	tm := teatest.NewTestModel(t,
		New(Config{Model: "test-model", Effort: "medium", MaxSteps: 80, MaxTokens: 200000}),
		teatest.WithInitialTermSize(tw, 60))
	for _, msg := range []tea.Msg{
		streamDeltaMsg{Content: "# Plan\nrun the build"},
		streamDoneMsg{},
		toolEventMsg{Name: "edit_file", Path: "a.ts", Search: "x", Replace: "y"},
		longFailCard(),
	} {
		tm.Send(msg)
	}
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return contains(string(b), "ctrl+o to expand")
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlO})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return contains(string(b), "line-eight")
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}
