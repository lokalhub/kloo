package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// runningModel returns a sized model in the running state with a cancel recorder.
func runningModel(canceled *bool) Model {
	m := newSized()
	m.running = true
	m.cancel = func() { *canceled = true }
	return m
}

// TestInterruptCancelsRunningCtx: Esc/Ctrl-C during a run cancels the ctx and
// does NOT quit (returns no quit command).
func TestInterruptCancelsRunningCtx(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyEsc, tea.KeyCtrlC} {
		var canceled bool
		m := runningModel(&canceled)
		_, cmd := m.Update(tea.KeyMsg{Type: key})
		if !canceled {
			t.Errorf("key %v during run should cancel the ctx", key)
		}
		if cmd != nil && isQuit(cmd) {
			t.Errorf("key %v during run must NOT quit the program", key)
		}
	}
}

// TestIdleCtrlCQuits: from idle, Ctrl-C returns a quit command.
func TestIdleCtrlCQuits(t *testing.T) {
	m := newSized()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil || !isQuit(cmd) {
		t.Errorf("idle Ctrl-C should quit")
	}
}

// TestIdleQQuitsWithEmptyInput: bare `q` quits from idle only with empty input.
func TestIdleQQuitsWithEmptyInput(t *testing.T) {
	m := newSized()
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil || !isQuit(cmd) {
		t.Errorf("idle q (empty input) should quit")
	}
}

// TestInterruptReportReturnsToIdle: an interrupted report renders the interrupt
// line (distinct from the autonomous stop-report banner) and returns to idle.
func TestInterruptReportReturnsToIdle(t *testing.T) {
	var canceled bool
	m := runningModel(&canceled)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm := apply(tm.(Model), reportMsg{Reason: "interrupted"})
	if mm.running {
		t.Errorf("after an interrupted report the UI should be idle")
	}
	if !contains(mm.View(), "run interrupted") {
		t.Errorf("should render the interrupt line:\n%s", mm.View())
	}
	// Distinct from the autonomous stop banner.
	if contains(mm.View(), "run stopped —") {
		t.Errorf("interrupt must not render the autonomous stop-report banner")
	}
}

// TestInterruptFrame captures the user-initiated interrupt terminal state for
// the Product lens (screens-to-verify §A row 7): a "run interrupted" line, back
// to idle — visibly distinct from the autonomous stop-report banner.
func TestInterruptFrame(t *testing.T) {
	var canceled bool
	m := runningModel(&canceled)
	m, _ = func() (Model, tea.Cmd) { mm, c := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); return mm.(Model), c }()
	m = apply(m, reportMsg{Reason: "interrupted"})
	requireGolden(t, "interrupt.golden", m.View())
}

// isQuit runs a command and reports whether it produced a tea.QuitMsg.
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	msg := cmd()
	_, ok := msg.(tea.QuitMsg)
	return ok
}
