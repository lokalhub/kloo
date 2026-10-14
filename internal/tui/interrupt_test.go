package tui

import (
	"strings"
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

// ── second-press behaviour ───────────────────────────────────────────────────
//
// What made Esc feel broken was not the cancel — measured against a stalled
// endpoint the context is cancelled in ~0.1s — but everything after it: no
// acknowledgement on screen, a second press that did nothing, and a Ctrl-C that
// could not quit while `running` was still set. These tests pin all three.

// TestFirstInterruptIsVisibleImmediately: the press changes the screen at once,
// without waiting for the loop's report.
func TestFirstInterruptIsVisibleImmediately(t *testing.T) {
	var canceled bool
	m := runningModel(&canceled)
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm := tm.(Model)
	if !mm.interrupting {
		t.Fatalf("Esc during a run should mark the run as stopping")
	}
	if !mm.running {
		t.Errorf("the run is not over until it reports; running must stay set")
	}
	if !contains(mm.View(), "interrupting") {
		t.Errorf("the press must be visible on screen:\n%s", mm.View())
	}
	if !contains(mm.View(), "esc again") {
		t.Errorf("the screen must offer the way out of the wait:\n%s", mm.View())
	}
}

// TestSecondEscDropsTheRun: Esc again while the run is still stopping hands the
// prompt back, instead of being a dead key.
func TestSecondEscDropsTheRun(t *testing.T) {
	var cancels int
	m := newSized()
	m.running = true
	m.cancel = func() { cancels++ }
	m = apply(m, tea.KeyMsg{Type: tea.KeyEsc}, tea.KeyMsg{Type: tea.KeyEsc})
	if m.running || m.interrupting {
		t.Fatalf("after the second Esc the UI must be back at the prompt (running=%v interrupting=%v)", m.running, m.interrupting)
	}
	if !m.abandoned {
		t.Errorf("the dropped run's goroutine is still live; it must be marked abandoned")
	}
	if cancels < 2 {
		t.Errorf("both presses must reach the cancel func, got %d (the func was being dropped after the first)", cancels)
	}
	if !contains(m.View(), "run dropped") {
		t.Errorf("the drop must be rendered:\n%s", m.View())
	}
}

// TestCtrlCQuitsAWedgedRun: with a run that will not stop, Ctrl-C quits. Before
// this, Ctrl-C while running only ever interrupted — and interrupt had already
// dropped the cancel func — so the program was unquittable from the keyboard.
func TestCtrlCQuitsAWedgedRun(t *testing.T) {
	var canceled bool
	m := runningModel(&canceled)
	m = apply(m, tea.KeyMsg{Type: tea.KeyEsc})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil || !isQuit(cmd) {
		t.Fatalf("Ctrl-C on a run that is still stopping must quit kloo")
	}
}

// TestDroppedRunsLateSignalsAreIgnored: the abandoned goroutine keeps emitting —
// deltas, cards, its report. None of it may land under the live prompt.
func TestDroppedRunsLateSignalsAreIgnored(t *testing.T) {
	var canceled bool
	m := runningModel(&canceled)
	m = apply(m, tea.KeyMsg{Type: tea.KeyEsc}, tea.KeyMsg{Type: tea.KeyEsc})
	before := len(m.transcript)
	m = apply(m,
		streamDeltaMsg{Content: "still talking"},
		toolEventMsg{Name: "run_command", Command: "npm test"},
		noticeMsg{text: "⟳ retrying model call"},
	)
	if len(m.transcript) != before {
		t.Errorf("late signals from a dropped run must not reach the transcript (+%d items)", len(m.transcript)-before)
	}
	if contains(m.View(), "still talking") {
		t.Errorf("a dropped run's stream reopened on screen:\n%s", m.View())
	}
	// Its report prints nothing and clears the abandoned flag.
	m = apply(m, reportMsg{Reason: "interrupted"})
	if m.abandoned {
		t.Errorf("the report should clear the abandoned flag")
	}
	if n := strings.Count(m.View(), "run interrupted"); n != 0 {
		t.Errorf("the dropped run's report must not print a banner:\n%s", m.View())
	}
}

// TestDroppedRunBlocksANewTaskUntilItReports: one Loop is shared across
// submissions, so a second run cannot start while the first is still unwinding.
func TestDroppedRunBlocksANewTaskUntilItReports(t *testing.T) {
	var canceled bool
	m := runningModel(&canceled)
	m = apply(m, tea.KeyMsg{Type: tea.KeyEsc}, tea.KeyMsg{Type: tea.KeyEsc})
	m = apply(m, submitTaskMsg{task: "do the next thing"})
	if !contains(m.View(), "hasn't finished stopping") {
		t.Errorf("a task submitted while the dropped run is live should be refused with a reason:\n%s", m.View())
	}
	// Once it reports, submission works again.
	m = apply(m, reportMsg{Reason: "interrupted"})
	m = apply(m, submitTaskMsg{task: "now the next thing"})
	if !contains(m.View(), "now the next thing") {
		t.Errorf("after the dropped run reported, a new task must be accepted:\n%s", m.View())
	}
}

// TestInterruptClearsTheActivityPhrase: "editing x.go" is not what kloo is doing
// once the run has been told to stop.
func TestInterruptClearsTheActivityPhrase(t *testing.T) {
	var canceled bool
	m := runningModel(&canceled)
	m.activity = "editing src/app.ts"
	m = apply(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.activity != "" {
		t.Errorf("activity = %q, want cleared", m.activity)
	}
}
