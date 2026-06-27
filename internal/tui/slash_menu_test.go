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
	want := []string{"/model", "/models", "/add", "/mode"}
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
