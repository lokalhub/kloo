package tui

import (
	"context"
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lokalhub/kloo/internal/llm"
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

// kloo is BYO-endpoint in task 00-03: /model <name> accepts any name and task
// 00-04 will wire alias/raw-id runtime validation.
func TestSlashModelAcceptsAnyName(t *testing.T) {
	m := typeAndEnter(newSized(), "/model deepseek/deepseek-v4-flash")
	if m.modelName != "deepseek/deepseek-v4-flash" || m.status.model != "deepseek/deepseek-v4-flash" {
		t.Errorf("/model should accept any name: model=%q status=%q", m.modelName, m.status.model)
	}
	if !contains(m.View(), "model: deepseek/deepseek-v4-flash") {
		t.Errorf("expected confirmation of the model switch:\n%s", m.View())
	}
}

type fakeModelLister struct {
	models []llm.ModelInfo
	err    error
}

func (f fakeModelLister) Models(context.Context) ([]llm.ModelInfo, error) {
	return f.models, f.err
}

func TestSlashModelsPrintsLiveModels(t *testing.T) {
	m := sized(New(Config{
		Model:     "test-model",
		MaxSteps:  40,
		MaxTokens: 8000,
		ModelList: fakeModelLister{models: []llm.ModelInfo{
			{ID: "openai/gpt-4.1-mini", ContextLength: 1047000},
		}},
	}), tw, th)

	m = typeAndEnter(m, "/models")
	v := m.View()
	for _, want := range []string{
		"models:",
		"openai/gpt-4.1-mini",
		"1047k ctx",
		"live",
	} {
		if !contains(v, want) {
			t.Errorf("/models output missing %q:\n%s", want, v)
		}
	}
}

func TestSlashModelsWhenLiveFetchFails(t *testing.T) {
	m := sized(New(Config{
		Model:     "test-model",
		MaxSteps:  40,
		MaxTokens: 8000,
		ModelList: fakeModelLister{err: errors.New("upstream down")},
	}), tw, th)

	m = typeAndEnter(m, "/models")
	v := m.View()
	if !contains(v, "live models unavailable: upstream down") {
		t.Errorf("/models should show live-fetch warning:\n%s", v)
	}
	if !contains(v, "no models available") {
		t.Errorf("/models with no live models should say so:\n%s", v)
	}
}

func TestBareSlashModelOpensPickerOverlay(t *testing.T) {
	m := sized(New(Config{
		Model:     "test-model",
		MaxSteps:  40,
		MaxTokens: 8000,
		ModelList: fakeModelLister{models: []llm.ModelInfo{
			{ID: "openai/gpt-4.1-mini", ContextLength: 1047000},
		}},
	}), tw, th)

	m = typeAndEnter(m, "/model")
	if m.picker == nil {
		t.Fatal("bare /model did not open picker")
	}
	v := m.View()
	for _, want := range []string{
		"Select model for next run",
		"type to filter",
		"openai/gpt-4.1-mini",
		"1047k ctx",
		"provider current",
		"live",
		"Enter select",
		"Esc cancel",
	} {
		if !contains(v, want) {
			t.Errorf("picker render missing %q:\n%s", want, v)
		}
	}
	requireGolden(t, "model-picker.golden", v)
}

func TestModelPickerTypingFiltersAndDoesNotEditTaskInput(t *testing.T) {
	m := sized(New(Config{
		Model:     "test-model",
		MaxSteps:  40,
		MaxTokens: 8000,
		ModelList: fakeModelLister{models: []llm.ModelInfo{
			{ID: "alpha-model", ContextLength: 1000},
			{ID: "beta-model", ContextLength: 2000},
		}},
	}), tw, th)
	m = typeAndEnter(m, "/model")
	m = apply(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("beta")})

	if m.input.Value() != "" {
		t.Fatalf("picker typing should not update task input, got %q", m.input.Value())
	}
	if m.picker == nil || m.picker.filter != "beta" {
		t.Fatalf("picker filter = %#v", m.picker)
	}
	v := m.View()
	if !contains(v, "filter: beta") || !contains(v, "beta-model") {
		t.Errorf("picker should show filtered beta row:\n%s", v)
	}
	if contains(v, "alpha-model") {
		t.Errorf("picker should hide non-matching row after filter:\n%s", v)
	}
}

func TestModelPickerUpDownEnterSelectsItem(t *testing.T) {
	m := sized(New(Config{
		Model:     "test-model",
		MaxSteps:  40,
		MaxTokens: 8000,
		ModelList: fakeModelLister{models: []llm.ModelInfo{
			{ID: "alpha-model", ContextLength: 1000},
			{ID: "beta-model", ContextLength: 2000},
		}},
	}), tw, th)
	m = typeAndEnter(m, "/model")
	m = apply(m, tea.KeyMsg{Type: tea.KeyDown})
	if got := m.picker.list.SelectedItem().(modelPickerItem).ID; got != "beta-model" {
		t.Fatalf("down selected %q, want beta-model", got)
	}
	m = apply(m, tea.KeyMsg{Type: tea.KeyUp})
	if got := m.picker.list.SelectedItem().(modelPickerItem).ID; got != "alpha-model" {
		t.Fatalf("up selected %q, want alpha-model", got)
	}
	m = apply(m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyEnter})
	if m.picker != nil {
		t.Fatal("enter should close picker")
	}
	if m.modelName != "beta-model" || m.status.model != "beta-model" {
		t.Errorf("enter should select highlighted model, got model=%q status=%q", m.modelName, m.status.model)
	}
	if !contains(m.View(), "model: beta-model") {
		t.Errorf("selection should route to /model confirmation:\n%s", m.View())
	}
}

func TestModelPickerEscCancelsAndLeavesModelUnchanged(t *testing.T) {
	m := sized(New(Config{
		Model:     "test-model",
		MaxSteps:  40,
		MaxTokens: 8000,
		ModelList: fakeModelLister{models: []llm.ModelInfo{
			{ID: "alpha-model", ContextLength: 1000},
			{ID: "beta-model", ContextLength: 2000},
		}},
	}), tw, th)
	m = typeAndEnter(m, "/model")
	m = apply(m, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyEsc})
	if m.picker != nil {
		t.Fatal("esc should close picker")
	}
	if m.modelName != "test-model" || m.status.model != "test-model" {
		t.Errorf("cancel should leave model unchanged, got model=%q status=%q", m.modelName, m.status.model)
	}
	if !contains(m.View(), "model picker cancelled") {
		t.Errorf("cancel should be visible:\n%s", m.View())
	}
}

func TestSlashModelRawIDKeepsEndpointAndUsesLiveContext(t *testing.T) {
	m := sized(New(Config{
		Model:         "test-model",
		Endpoint:      "http://local/v1",
		APIKey:        "local-key",
		ContextTokens: 8000,
		ToolFormat:    "native",
		ModelList: fakeModelLister{models: []llm.ModelInfo{
			{ID: "raw-model-id", ContextLength: 32768},
		}},
	}), tw, th)

	m = typeAndEnter(m, "/model raw-model-id")
	if m.runtime.Endpoint != "http://local/v1" || m.runtime.APIKey != "local-key" {
		t.Errorf("raw id switch should keep endpoint/key: %+v", m.runtime)
	}
	if m.runtime.Model != "raw-model-id" || m.runtime.ContextTokens != 32768 {
		t.Errorf("raw id switch should update model/context: %+v", m.runtime)
	}
}

func TestSlashModelRawIDWarnsWhenLiveListMisses(t *testing.T) {
	m := sized(New(Config{
		Model:         "test-model",
		Endpoint:      "http://local/v1",
		ContextTokens: 8000,
		ModelList: fakeModelLister{models: []llm.ModelInfo{
			{ID: "other-model", ContextLength: 32768},
		}},
	}), tw, th)

	m = typeAndEnter(m, "/model unknown-model")
	if m.runtime.Model != "unknown-model" {
		t.Errorf("raw miss should still switch model: %+v", m.runtime)
	}
	if !contains(m.View(), "warning: model unknown-model not found in live model list; switching anyway") {
		t.Errorf("raw miss should show warning:\n%s", m.View())
	}
}

func TestSlashModelRawIDWarnsWhenLiveListIsEmpty(t *testing.T) {
	m := sized(New(Config{
		Model:         "test-model",
		Endpoint:      "http://local/v1",
		ContextTokens: 8000,
		ModelList:     fakeModelLister{models: []llm.ModelInfo{}},
	}), tw, th)

	m = typeAndEnter(m, "/model unknown-model")
	if m.runtime.Model != "unknown-model" {
		t.Errorf("raw miss should still switch model: %+v", m.runtime)
	}
	if !contains(m.View(), "warning: model unknown-model not found in live model list; switching anyway") {
		t.Errorf("empty live list miss should show warning:\n%s", m.View())
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
