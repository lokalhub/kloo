package tui

import (
	"context"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/lokalhub/kloo/internal/tools"
)

// scriptRunner is a deterministic fake loop: on Start it plays a fixed sequence
// of program messages (the surfaces a real run would produce), then a terminal
// report. It proves the integrated program composes tasks 01–08 without needing
// a live model.
type scriptRunner struct {
	send  func(tea.Msg)
	steps []tea.Msg
}

func (r *scriptRunner) setSend(send func(tea.Msg)) { r.send = send }
func (r *scriptRunner) Start(ctx context.Context, task string, mode Mode, files []string) {
	for _, msg := range r.steps {
		r.send(msg)
	}
}

// TestIntegrationFullRunFrame: a full synthetic run (stream → edit card →
// run_command card → done) renders the integrated transcript + advanced status.
func TestIntegrationFullRunFrame(t *testing.T) {
	// A taller terminal so the whole session fits in the viewport for the golden.
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, 44)
	m = apply(m,
		submitTaskMsg{task: "make the three tabs Home/Apps/Profile, each name centered"},
		progressMsg{Model: "test-model", Step: 1, MaxSteps: 40, Tokens: 400, MaxTokens: 8000},
		streamDeltaMsg{Content: "I'll update the tab routes first."},
		streamDoneMsg{},
		// The edit card goes through the REAL bridge parse (a fenced diff arg).
		toolEvent(tools.Call{Name: "edit_file", Args: map[string]any{"path": "src/app/tabs/tabs.routes.ts", "diff": editFileDiff}}, tools.Result{}),
		progressMsg{Model: "test-model", Step: 2, MaxSteps: 40, Tokens: 1200, MaxTokens: 8000},
		toolEvent(tools.Call{Name: "run_command", Args: map[string]any{"command": "npm run build"}}, tools.Result{ExitCode: 0}),
		reportMsg{Reason: "success", Steps: 2, Tokens: 1200, MaxTokens: 8000, Elapsed: "12s", VerifyCmd: "npm run build", VerifyExit: 0},
	)
	v := m.View()
	for _, want := range []string{"▸ you: make the three tabs", "I'll update the tab routes", "✎ src/app/tabs/tabs.routes.ts", "- { path: 'tab1'", "exit 0 ✓", "step 2/40", "run stopped — COMPLETE"} {
		if !contains(v, want) {
			t.Errorf("full-run frame missing %q:\n%s", want, v)
		}
	}
	requireGolden(t, "session-run.golden", v)
}

// TestIntegrationStopReportBudgetDistinct: an autonomous budget stop renders the
// terminal banner, visibly DISTINCT from the Esc-interrupt line.
func TestIntegrationStopReportBudgetDistinct(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	m = apply(m,
		submitTaskMsg{task: "a task that never converges"},
		reportMsg{
			Reason: "budget-exceeded", Steps: 40, Tokens: 7900, MaxTokens: 8000,
			Elapsed: "2m18s", Detail: "max-steps (40/40)",
			VerifyCmd: "npm run build", VerifyExit: 1, RolledBack: true,
		},
	)
	v := m.View()
	for _, want := range []string{"run stopped — BUDGET EXCEEDED", "max-steps (40/40)", "steps:   40", "exit 1 ✗", "rolled back to checkpoint"} {
		if !contains(v, want) {
			t.Errorf("stop-report banner missing %q:\n%s", want, v)
		}
	}
	// Distinct from the interrupt line.
	if contains(v, "run interrupted") {
		t.Errorf("autonomous stop must not render the interrupt line")
	}
	if m.running {
		t.Errorf("after a stop-report the UI should be idle")
	}
	requireGolden(t, "session-stop-budget.golden", v)
}

// TestIntegrationStopReportChurnVariant captures the churn variant of the
// autonomous stop-report banner (screens-to-verify §A: "■ run stopped — NO
// PROGRESS (churn)", reason "same failure ×3") so both banner variants have a
// frame for the Product lens.
func TestIntegrationStopReportChurnVariant(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	m = apply(m,
		submitTaskMsg{task: "a task that keeps repeating the same edit"},
		reportMsg{
			Reason: "churn", Steps: 6, Tokens: 2100, MaxTokens: 8000,
			Elapsed: "31s", Detail: "repeated-failure (same failure ×3)",
			VerifyCmd: "npm run build", VerifyExit: 1, RolledBack: true,
		},
	)
	v := m.View()
	for _, want := range []string{"run stopped — NO PROGRESS (churn)", "same failure ×3", "exit 1 ✗", "rolled back to checkpoint"} {
		if !contains(v, want) {
			t.Errorf("churn stop-report missing %q:\n%s", want, v)
		}
	}
	if contains(v, "run interrupted") {
		t.Errorf("autonomous churn stop must not render the interrupt line")
	}
	requireGolden(t, "session-stop-churn.golden", v)
}

// TestIntegrationDialDefaultAndApproveEachEndToEnd: default auto end-to-end, and
// approve-each holds an edit until confirmed.
func TestIntegrationDialDefaultAndApproveEachEndToEnd(t *testing.T) {
	m := New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000})
	if m.mode != ModeAuto {
		t.Fatalf("default dial must be auto end-to-end, got %q", m.mode)
	}
	m = sized(m, tw, th)
	m = typeAndEnter(m, "/mode approve-each")
	done := make(chan bool, 1)
	m = apply(m, confirmRequestMsg{Path: "a.ts", Search: "old", Replace: "new", Respond: func(ok bool) { done <- ok }})
	if m.confirm == nil || !contains(m.View(), "[y/n]") {
		t.Errorf("approve-each should hold an edit with a confirm prompt")
	}
	m = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if <-done != true {
		t.Errorf("y should confirm the held edit")
	}
}

// TestIntegrationFakeRunnerDrivesViaProgram: the scripted runner drives the real
// program via teatest end-to-end (the seam + wiring), reaching an idle success.
func TestIntegrationFakeRunnerDrivesViaProgram(t *testing.T) {
	runner := &scriptRunner{steps: []tea.Msg{
		streamDeltaMsg{Content: "working"},
		streamDoneMsg{},
		toolEventMsg{Name: "run_command", Command: "npm run build", ExitCode: 0},
		reportMsg{Reason: "success", Steps: 1, VerifyCmd: "npm run build", VerifyExit: 0},
	}}
	tm := teatest.NewTestModel(t, New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000, Runner: runner}),
		teatest.WithInitialTermSize(tw, th))
	runner.setSend(tm.GetProgram().Send)

	tm.Type("do the thing")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return contains(string(b), "COMPLETE") || contains(string(b), "run stopped")
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC}) // idle quit
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

// C3: a scripted run that emits a memoryMsg (a compaction occurred) drives the
// real program to its terminal report without panic — the indicator plumbing is
// exercised end-to-end alongside the existing surfaces.
func TestIntegrationMemoryIndicatorEndToEnd(t *testing.T) {
	runner := &scriptRunner{steps: []tea.Msg{
		progressMsg{Model: "test-model", Step: 1, MaxSteps: 40, Tokens: 500, MaxTokens: 8000},
		memoryMsg{Compactions: 1},
		streamDeltaMsg{Content: "working"},
		streamDoneMsg{},
		toolEventMsg{Name: "run_command", Command: "go test", ExitCode: 0},
		progressMsg{Model: "test-model", Step: 2, MaxSteps: 40, Tokens: 900, MaxTokens: 8000},
		memoryMsg{Compactions: 2},
		reportMsg{Reason: "success", Steps: 2, VerifyCmd: "go test", VerifyExit: 0},
	}}
	tm := teatest.NewTestModel(t, New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000, Runner: runner}),
		teatest.WithInitialTermSize(tw, th))
	runner.setSend(tm.GetProgram().Send)

	tm.Type("do the thing")
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return contains(string(b), "COMPLETE") || contains(string(b), "run stopped")
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC}) // idle quit
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}
