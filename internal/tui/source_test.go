package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeTaskSource is a NON-keyboard TaskSource: it proves the seam admits a future
// stdin source (it feeds one task without any key events).
type fakeTaskSource struct{ task string }

func (f fakeTaskSource) Attach() tea.Cmd {
	return func() tea.Msg { return submitTaskMsg{task: f.task} }
}
func (f fakeTaskSource) Name() string { return "fake" }

// TestKeyboardIsTheV1Source: by default the model uses the keyboard source.
func TestKeyboardIsTheV1Source(t *testing.T) {
	m := New(Config{Model: "snappy"})
	if _, ok := m.source.(keyboardSource); !ok {
		t.Errorf("default TaskSource = %T, want keyboardSource", m.source)
	}
	if m.source.Attach() != nil {
		t.Errorf("keyboard source needs no background command")
	}
}

// TestSeamAdmitsNonKeyboardSource: a fakeTaskSource drives a task submission
// WITHOUT keyboard input and WITHOUT changing the model/Update/View — exactly
// what a future StdinTaskSource would do.
func TestSeamAdmitsNonKeyboardSource(t *testing.T) {
	m := sized(New(Config{Model: "snappy", Source: fakeTaskSource{task: "build the tabs"}}), tw, th)

	// The source's Attach command yields the submit message (no keys involved).
	attach := m.source.Attach()
	if attach == nil {
		t.Fatal("fake source should provide an Attach command")
	}
	msg := attach()
	m = apply(m, msg)

	if !contains(m.View(), "▸ you: build the tabs") {
		t.Errorf("non-keyboard source did not drive a task submission:\n%s", m.View())
	}
}
