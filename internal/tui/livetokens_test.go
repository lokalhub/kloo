package tui

import (
	"strings"
	"testing"
	"time"
)

// TestHeaderShowsLiveTokens: after a progress snapshot with non-zero tokens, the
// header renders the humanized total and never "0/" for the token field — the
// user complaint ("tokens 0/200000 for the whole run") is resolved.
func TestHeaderShowsLiveTokens(t *testing.T) {
	m := sized(New(Config{Model: "snappy", MaxSteps: 80, MaxTokens: 200000}), tw, th)
	m = apply(m, progressMsg{Model: "snappy", Step: 18, MaxSteps: 80, Tokens: 14400, MaxTokens: 200000})
	v := m.View()
	if !contains(v, "14.4k/200k tok") {
		t.Errorf("header missing live token total %q:\n%s", "14.4k/200k tok", v)
	}
	if contains(v, "0/200k tok") {
		t.Errorf("header still shows a zero token total:\n%s", v)
	}
	requireGolden(t, "header-live-tokens.golden", v)
}

// TestHeaderTokensIncrease: successive snapshots advance the displayed total.
func TestHeaderTokensIncrease(t *testing.T) {
	m := sized(New(Config{Model: "snappy", MaxSteps: 80, MaxTokens: 200000}), tw, th)
	m = apply(m, progressMsg{Model: "snappy", Step: 18, MaxSteps: 80, Tokens: 14400, MaxTokens: 200000})
	if !contains(m.View(), "14.4k/200k") {
		t.Fatalf("first snapshot not shown:\n%s", m.View())
	}
	m2 := apply(m, progressMsg{Model: "snappy", Step: 20, MaxSteps: 80, Tokens: 21000, MaxTokens: 200000})
	if !contains(m2.View(), "21k/200k") {
		t.Errorf("token total did not increase to 21k:\n%s", m2.View())
	}
}

// TestThinkingLineShowsLiveTokens: while a run is active the thinking line shows
// the live non-zero token count, never "0 tok".
func TestThinkingLineShowsLiveTokens(t *testing.T) {
	m := sized(New(Config{Model: "snappy", MaxSteps: 80, MaxTokens: 200000}), tw, th)
	m = apply(m, progressMsg{Model: "snappy", Step: 18, MaxSteps: 80, Tokens: 14400, MaxTokens: 200000})
	m.running = true
	m.verb = "Cooking"
	m.runStart = time.Now()
	line := m.renderThinking()
	if !strings.Contains(line, "14.4k tok") {
		t.Errorf("thinking line missing live tokens %q: %q", "14.4k tok", line)
	}
	if strings.Contains(line, "0 tok") {
		t.Errorf("thinking line still shows 0 tok: %q", line)
	}
}

// TestIdleHeaderNotFalselyNonZero: before any run, the header shows a genuine
// zero — the fix surfaces real usage, it does not hardcode a number.
func TestIdleHeaderNotFalselyNonZero(t *testing.T) {
	m := sized(New(Config{Model: "snappy", MaxSteps: 80, MaxTokens: 200000}), tw, th)
	if !contains(m.View(), "0/200k tok") {
		t.Errorf("idle header should show a real zero token count:\n%s", m.View())
	}
}
