package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// typeAndEnter types a line into the input and presses Enter.
func typeAndEnter(m Model, line string) Model {
	m = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(line)})
	return apply(m, tea.KeyMsg{Type: tea.KeyEnter})
}

func TestSlashModelTakesEffect(t *testing.T) {
	m := typeAndEnter(newSized(), "/model alt-model")
	if m.modelName != "alt-model" || m.status.model != "alt-model" {
		t.Errorf("/model alt-model did not switch state: model=%q status=%q", m.modelName, m.status.model)
	}
	if !contains(m.View(), "alt-model") {
		t.Errorf("status line should show alt-model:\n%s", m.View())
	}
}

// kloo is BYO-endpoint: /model accepts any name the server understands, with no
// allowlist (the endpoint, not kloo, decides what's valid).
func TestSlashModelAcceptsAnyName(t *testing.T) {
	m := typeAndEnter(newSized(), "/model deepseek/deepseek-v4-flash")
	if m.modelName != "deepseek/deepseek-v4-flash" || m.status.model != "deepseek/deepseek-v4-flash" {
		t.Errorf("/model should accept any name: model=%q status=%q", m.modelName, m.status.model)
	}
	if !contains(m.View(), "model: deepseek/deepseek-v4-flash") {
		t.Errorf("expected confirmation of the model switch:\n%s", m.View())
	}
}

func TestSlashModeTakesEffect(t *testing.T) {
	m := typeAndEnter(newSized(), "/mode approve-each")
	if m.mode != ModeApproveEach {
		t.Errorf("/mode approve-each did not set the dial, got %q", m.mode)
	}
	if !contains(m.View(), "approve-each") {
		t.Errorf("status line should show approve-each:\n%s", m.View())
	}
}

func TestSlashModeInvalid(t *testing.T) {
	m := typeAndEnter(newSized(), "/mode bananas")
	if m.mode != ModeAuto {
		t.Errorf("invalid mode should leave the dial unchanged, got %q", m.mode)
	}
	if !contains(m.View(), "invalid mode") {
		t.Errorf("expected a clear invalid-mode message:\n%s", m.View())
	}
}

func TestSlashAddTakesEffect(t *testing.T) {
	m := typeAndEnter(newSized(), "/add internal/app.go")
	if len(m.contextFiles) != 1 || m.contextFiles[0] != "internal/app.go" {
		t.Errorf("/add did not add to context: %v", m.contextFiles)
	}
	if !contains(m.View(), "added internal/app.go") {
		t.Errorf("expected an add confirmation:\n%s", m.View())
	}
}

func TestSlashAddMissingPath(t *testing.T) {
	m := typeAndEnter(newSized(), "/add")
	if len(m.contextFiles) != 0 {
		t.Errorf("/add with no path should not add anything")
	}
	if !contains(m.View(), "/add needs a path") {
		t.Errorf("expected a missing-path message:\n%s", m.View())
	}
}

func TestSlashDiffAfterEdit(t *testing.T) {
	m := newSized()
	m = apply(m, toolEventMsg{Name: "edit_file", Path: "a.ts", Search: "old", Replace: "new"})
	m = typeAndEnter(m, "/diff")
	v := m.View()
	if !contains(v, "pending diff:") || !contains(v, "- old") || !contains(v, "+ new") {
		t.Errorf("/diff should render the pending diff:\n%s", v)
	}
}

func TestSlashDiffEmpty(t *testing.T) {
	m := typeAndEnter(newSized(), "/diff")
	if !contains(m.View(), "no pending diff") {
		t.Errorf("/diff with nothing pending should say so:\n%s", m.View())
	}
}

func TestSlashStopNothingRunning(t *testing.T) {
	m := typeAndEnter(newSized(), "/stop")
	if !contains(m.View(), "nothing to stop") {
		t.Errorf("/stop with nothing running should say so:\n%s", m.View())
	}
}

func TestSlashUnknown(t *testing.T) {
	m := typeAndEnter(newSized(), "/bogus")
	if !contains(m.View(), "unknown command: /bogus") {
		t.Errorf("unknown slash should produce a clear message:\n%s", m.View())
	}
	// State unchanged, nothing submitted as a task.
	if m.running {
		t.Errorf("unknown slash must not start a run")
	}
}

// TestNonSlashRoutesAsTask: a non-slash submission routes to the task path (a
// submitTaskMsg → userItem), not a slash handler.
func TestNonSlashRoutesAsTask(t *testing.T) {
	m := newSized()
	m = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("make the tabs")})
	// Enter emits a submitTaskMsg cmd; simulate the message it produces.
	m = apply(m, submitTaskMsg{task: "make the tabs"})
	if !contains(m.View(), "▸ you: make the tabs") {
		t.Errorf("non-slash input should render as a user task:\n%s", m.View())
	}
}
