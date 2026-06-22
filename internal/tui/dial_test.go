package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestDialDefaultIsAuto: the locked default (design doc §2 / master plan §5) on
// initial model state AND the idle status line.
func TestDialDefaultIsAuto(t *testing.T) {
	m := New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000})
	if m.mode != ModeAuto {
		t.Fatalf("default dial = %q, want auto", m.mode)
	}
	if !contains(sized(m, tw, th).View(), "auto") {
		t.Errorf("idle status line should show auto")
	}
}

// TestDialAutoAppliesWithoutPrompt: in auto, an edit event renders a card with no
// confirm prompt.
func TestDialAutoAppliesWithoutPrompt(t *testing.T) {
	m := apply(newSized(), toolEventMsg{Name: "edit_file", Path: "a.ts", Search: "old", Replace: "new"})
	if m.confirm != nil {
		t.Errorf("auto mode must not raise a confirm")
	}
	if contains(m.View(), "[y/n]") {
		t.Errorf("auto mode must not render a confirm prompt:\n%s", m.View())
	}
}

// TestDialApproveEachHoldsAndApplies: in approve-each, a confirmRequest holds the
// edit with a prompt; `y` applies and answers the responder.
func TestDialApproveEachHoldsAndApplies(t *testing.T) {
	m := typeAndEnter(newSized(), "/mode approve-each")

	answered := make(chan bool, 1)
	m = apply(m, confirmRequestMsg{Path: "a.ts", Search: "old", Replace: "new", Respond: func(ok bool) { answered <- ok }})
	if m.confirm == nil {
		t.Fatal("approve-each should hold the edit with a pending confirm")
	}
	if !contains(m.View(), "apply this edit? [y/n]") {
		t.Errorf("confirm prompt should render:\n%s", m.View())
	}

	m = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if m.confirm != nil {
		t.Errorf("after y the confirm should clear")
	}
	if got := <-answered; got != true {
		t.Errorf("y should answer the responder with true")
	}
	if !contains(m.View(), "edit applied") {
		t.Errorf("y should render 'edit applied':\n%s", m.View())
	}
}

// TestDialApproveEachReject: `n` rejects and reports false to the loop.
func TestDialApproveEachReject(t *testing.T) {
	m := typeAndEnter(newSized(), "/mode approve-each")
	answered := make(chan bool, 1)
	m = apply(m, confirmRequestMsg{Path: "a.ts", Search: "old", Replace: "new", Respond: func(ok bool) { answered <- ok }})
	m = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if got := <-answered; got != false {
		t.Errorf("n should answer the responder with false")
	}
	if !contains(m.View(), "edit rejected") {
		t.Errorf("n should render 'edit rejected':\n%s", m.View())
	}
}

// TestInterruptWhileConfirmPending: pressing Esc while an edit is HELD for an
// approve-each confirm must resolve the pending responder (rejected) and clear
// the confirm — otherwise the loop goroutine deadlocks on <-decision and the run
// never returns to idle (the shipped-mode interrupt-DoD bug).
func TestInterruptWhileConfirmPending(t *testing.T) {
	m := typeAndEnter(newSized(), "/mode approve-each")
	m.running = true
	canceled := false
	m.cancel = func() { canceled = true }

	answered := make(chan bool, 1)
	m = apply(m, confirmRequestMsg{Path: "a.ts", Search: "old", Respond: func(ok bool) { answered <- ok }})
	if m.confirm == nil {
		t.Fatal("an edit should be held for confirm in approve-each")
	}

	// Esc while running + a confirm pending → interrupt path.
	tm, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	mm := tm.(Model)
	if !canceled {
		t.Error("Esc should cancel the run ctx")
	}
	if mm.confirm != nil {
		t.Error("Esc should clear the pending confirm")
	}
	select {
	case d := <-answered:
		if d != false {
			t.Errorf("the held edit should be resolved as rejected, got %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the held responder was never resolved → deadlock")
	}
}

// TestDialSwitchBackToAuto: /mode auto restores auto and the status line reflects it.
func TestDialSwitchBackToAuto(t *testing.T) {
	m := typeAndEnter(newSized(), "/mode approve-each")
	m = typeAndEnter(m, "/mode auto")
	if m.mode != ModeAuto {
		t.Errorf("mode = %q, want auto", m.mode)
	}
}
