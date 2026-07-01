package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lokalhub/kloo/internal/llm"
)

// typeRunes feeds a string as a single KeyRunes message (live typing into the
// input), without pressing Enter — so the slash menu opens/refilters.
func typeRunes(m Model, s string) Model {
	return apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func menuNames(m Model) []string {
	if m.menu == nil {
		return nil
	}
	out := make([]string, 0, len(m.menu.items))
	for _, c := range m.menu.items {
		out = append(out, c.name)
	}
	return out
}

func TestSlashMenuTypingSlashShowsAllCommands(t *testing.T) {
	m := typeRunes(newSized(), "/")
	got := menuNames(m)
	want := []string{"/model", "/models", "/provider", "/add", "/mode"}
	if len(got) != len(want) {
		t.Fatalf("typing / should show all %d commands, got %v", len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("menu[%d] = %q, want %q (%v)", i, got[i], w, got)
		}
	}
}

func TestSlashMenuFiltersByTypedText(t *testing.T) {
	m := typeRunes(newSized(), "/mo")
	got := menuNames(m)
	want := []string{"/model", "/models", "/mode"}
	if len(got) != len(want) {
		t.Fatalf("/mo should filter to %v, got %v", want, got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("filtered menu[%d] = %q, want %q (%v)", i, got[i], w, got)
		}
	}
}

func TestSlashMenuEnterOnArgCommandInserts(t *testing.T) {
	// "/mod" highlights /model (an arg-taking command, not fully typed); Enter
	// inserts "/model " for the user to type an argument — it does NOT submit.
	m := typeRunes(newSized(), "/mod")
	if names := menuNames(m); len(names) == 0 || names[0] != "/model" {
		t.Fatalf("/mod should highlight /model first, got %v", names)
	}
	m = apply(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.input.Value() != "/model " {
		t.Errorf("Enter on /model should insert %q, got %q", "/model ", m.input.Value())
	}
	if m.menu != nil {
		t.Errorf("selecting a command should close the menu")
	}
	if m.picker != nil {
		t.Errorf("inserting /model must not open the picker (no submit)")
	}
}

func TestSlashMenuEnterOnNoArgCommandExecutes(t *testing.T) {
	m := sized(New(Config{
		Model:     "test-model",
		MaxSteps:  40,
		MaxTokens: 8000,
		ModelList: fakeModelLister{models: []llm.ModelInfo{
			{ID: "openai/gpt-4.1-mini", ContextLength: 1047000},
		}},
	}), tw, th)
	// Type "/model" (highlights /model), navigate down to /models, then Enter —
	// /models takes no argument, so it EXECUTES immediately (lists models).
	m = typeRunes(m, "/model")
	m = apply(m, tea.KeyMsg{Type: tea.KeyDown})
	if got := m.menu.items[m.menu.index].name; got != "/models" {
		t.Fatalf("down should highlight /models, got %q", got)
	}
	m = apply(m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.menu != nil {
		t.Errorf("executing a command should close the menu")
	}
	if m.input.Value() != "" {
		t.Errorf("executing /models should clear the input, got %q", m.input.Value())
	}
	if !contains(m.View(), "openai/gpt-4.1-mini") {
		t.Errorf("Enter on /models should list models:\n%s", m.View())
	}
}

// TestSlashMenuModeExactMatchSubmits is the regression test for the /mode bug:
// typing "/mode" exactly and pressing Enter should SUBMIT /mode, NOT select the
// highlighted /model (which sorts first because "model" also starts with "mode").
// The positive assertion is that /mode's "invalid mode" error appears — proving
// slashMode ran, not slashModel (which would have inserted "/model " into the input).
func TestSlashMenuModeExactMatchSubmits(t *testing.T) {
	m := typeRunes(newSized(), "/mode")
	if m.menu == nil {
		t.Fatal("/mode should keep the menu open (both /model and /mode match the prefix)")
	}
	if m.menu.items[0].name != "/model" {
		t.Fatalf("first filtered item should be /model (alphabetic prefix order), got %q", m.menu.items[0].name)
	}
	m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	out := m2.(Model)
	if out.menu != nil {
		t.Error("Enter on fully-typed /mode should close the menu")
	}
	if out.input.Value() != "" {
		t.Errorf("input should be cleared after submitting /mode, got %q", out.input.Value())
	}
	// /mode with no arg runs slashMode("") which emits "invalid mode:" —
	// that message confirms /mode ran. If /model had been selected it would
	// have inserted "/model " into the input (non-empty) and shown no error.
	if !contains(out.View(), "invalid mode") {
		t.Errorf("Enter on /mode should have run slashMode (expects 'invalid mode' error for empty arg), view:\n%s", out.View())
	}
}

// TestSlashMenuModeWithArgSubmitsCorrectly: /mode approve-each (with a space,
// menu closed) still dispatches correctly and isn't confused by /model.
func TestSlashMenuModeWithArgSubmitsCorrectly(t *testing.T) {
	m := typeAndEnter(newSized(), "/mode approve-each")
	if !contains(m.View(), "approve-each") {
		t.Errorf("/mode approve-each should confirm the mode change, view:\n%s", m.View())
	}
	if m.mode != ModeApproveEach {
		t.Errorf("mode should be approve-each, got %q", m.mode)
	}
}

// TestSlashMenuProviderAppearsInMenu: /provider appears in the slash menu.
func TestSlashMenuProviderAppearsInMenu(t *testing.T) {
	m := typeRunes(newSized(), "/pro")
	if m.menu == nil {
		t.Fatal("/pro should open the slash menu")
	}
	found := false
	for _, item := range m.menu.items {
		if item.name == "/provider" {
			found = true
		}
	}
	if !found {
		t.Errorf("/provider should appear in the filtered menu for '/pro', got %v", menuNames(m))
	}
}

func TestSlashMenuEscClosesAndKeepsText(t *testing.T) {
	m := typeRunes(newSized(), "/mo")
	if m.menu == nil {
		t.Fatal("/mo should open the menu")
	}
	m = apply(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.menu != nil {
		t.Errorf("Esc should close the menu")
	}
	if m.input.Value() != "/mo" {
		t.Errorf("Esc should keep the typed text, got %q", m.input.Value())
	}
}
