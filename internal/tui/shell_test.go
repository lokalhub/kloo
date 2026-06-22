package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
)

// TestShellIdleFrame renders the idle three-region layout and golden-compares it
// to the mock's idle prompt (header + empty transcript + input with slash hints +
// dial hint line).
func TestShellIdleFrame(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	requireGolden(t, "idle.golden", m.View())
}

// TestShellCtrlCQuitsFromIdle drives the real program with teatest and asserts
// ctrl+c quits cleanly from idle (no panic).
func TestShellCtrlCQuitsFromIdle(t *testing.T) {
	tm := teatest.NewTestModel(t, New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}),
		teatest.WithInitialTermSize(tw, th))
	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

// TestShellQQuitsFromIdleEmptyInput: bare `q` quits only when the input is empty.
func TestShellQQuitsFromIdleEmptyInput(t *testing.T) {
	tm := teatest.NewTestModel(t, New(Config{Model: "test-model"}), teatest.WithInitialTermSize(tw, th))
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(2*time.Second))
}

// TestShellQTypesWhenInputNonEmpty: with text already typed, `q` is part of the
// task, not a quit — the program keeps running until we force quit.
func TestShellQDoesNotQuitWhileTyping(t *testing.T) {
	m := New(Config{Model: "test-model"})
	m = sized(m, tw, th)
	// Type "fix q" then verify the model is still idle/non-running and the input
	// retains the text (q did not quit or clear).
	m = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("fix bug q")})
	if m.input.Value() != "fix bug q" {
		t.Errorf("input = %q, want the typed text including q", m.input.Value())
	}
}
