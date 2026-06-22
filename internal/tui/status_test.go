package tui

import (
	"strings"
	"testing"
)

func TestStatusIdleDefaults(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	v := m.View()
	for _, want := range []string{"test-model", "step 0/40", "0/8k tok", "auto"} {
		if !contains(v, want) {
			t.Errorf("idle header missing %q:\n%s", want, v)
		}
	}
	requireGolden(t, "status-idle.golden", v)
}

func TestStatusUpdatesOnProgress(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	m = apply(m, progressMsg{Model: "test-model", Step: 3, MaxSteps: 40, Tokens: 1200, MaxTokens: 8000})
	v := m.View()
	for _, want := range []string{"test-model", "step 3/40", "1.2k/8k tok", "auto"} {
		if !contains(v, want) {
			t.Errorf("running header missing %q:\n%s", want, v)
		}
	}
	requireGolden(t, "status-running.golden", v)

	// A second snapshot advances the counters.
	m2 := apply(m, progressMsg{Model: "test-model", Step: 4, MaxSteps: 40, Tokens: 1800, MaxTokens: 8000})
	if !contains(m2.View(), "step 4/40") || !contains(m2.View(), "1.8k/8k") {
		t.Errorf("counters did not advance:\n%s", m2.View())
	}
}

func TestStatusNarrowElidesGracefully(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), 30, 12)
	// Should not panic and the header line should fit the width.
	header := strings.Split(m.View(), "\n")[1] // the content line (index 1, between borders)
	if len([]rune(header)) > 30 {
		t.Errorf("narrow header overflows: %q", header)
	}
}

// C2: the status line gains a working-memory compaction indicator that
// increments on a memoryMsg; with no compaction it renders exactly as today
// (the unchanged status-running.golden above is the regression for the 0 case).
func TestStatusCompactionIndicator(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	m = apply(m, progressMsg{Model: "test-model", Step: 3, MaxSteps: 40, Tokens: 1200, MaxTokens: 8000})

	// No compaction yet ⇒ no ⟲ marker (the live token total is still shown).
	if contains(m.View(), "⟲") {
		t.Errorf("no compaction should render no ⟲ marker:\n%s", m.View())
	}
	if !contains(m.View(), "1.2k/8k") {
		t.Errorf("the live token total must remain:\n%s", m.View())
	}

	// A compaction memoryMsg surfaces ⟲N and subsequent ones update it.
	m = apply(m, memoryMsg{Compactions: 2})
	if !contains(m.View(), "⟲2") {
		t.Errorf("compaction indicator should show ⟲2:\n%s", m.View())
	}
	if !contains(m.View(), "1.2k/8k") {
		t.Errorf("the token total must stay alongside the indicator:\n%s", m.View())
	}
	// Product-lens evidence: the delivered after-compaction status line (matches
	// the ASCII mock in the plan §6, bottom).
	requireGolden(t, "status-compaction.golden", m.View())
	m = apply(m, memoryMsg{Compactions: 5})
	if !contains(m.View(), "⟲5") {
		t.Errorf("indicator should advance to ⟲5:\n%s", m.View())
	}
}
