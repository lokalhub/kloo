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
