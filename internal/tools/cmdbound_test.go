package tools

import (
	"strings"
	"testing"
)

// TestCommandOutputBoundKeepsBothEnds: measured on kloo-bench A06, one vitest run
// grew the next prompt by ~69,700 tokens and the run died on the wall clock after
// 14 steps at 171s each. Bounding it must keep the HEAD (the first failure) and the
// TAIL (the summary line) — truncating either end hides half of what a test run is
// for.
func TestCommandOutputBoundKeepsBothEnds(t *testing.T) {
	t.Setenv("KLOO_BOUND_CMD_OUTPUT", "1")
	body := "FIRST-FAILURE" + strings.Repeat("x", modelOutputCap*2) + "SUMMARY-LINE"
	got, truncated := boundForModel(body)
	if !truncated {
		t.Fatal("oversize output was not bounded")
	}
	if !strings.Contains(got, "FIRST-FAILURE") {
		t.Fatal("head dropped — the first failure is invisible")
	}
	if !strings.Contains(got, "SUMMARY-LINE") {
		t.Fatal("tail dropped — the verdict is invisible")
	}
	if len(got) > modelOutputCap+400 {
		t.Fatalf("bounded output is still %d bytes", len(got))
	}
	if !strings.Contains(got, "omitted") {
		t.Fatal("truncation is silent — the model cannot tell it is missing output")
	}
}

// TestSmallOutputUntouched: the common case must be byte-identical, or every
// command result changes shape for no reason.
func TestSmallOutputUntouched(t *testing.T) {
	t.Setenv("KLOO_BOUND_CMD_OUTPUT", "1")
	got, truncated := boundForModel("2 passed, 0 failed")
	if truncated || got != "2 passed, 0 failed" {
		t.Fatalf("small output was altered: %q", got)
	}
}

// TestCommandBoundIsOptIn.
func TestCommandBoundIsOptIn(t *testing.T) {
	t.Setenv("KLOO_BOUND_CMD_OUTPUT", "")
	big := strings.Repeat("y", modelOutputCap*2)
	if got, truncated := boundForModel(big); truncated || len(got) != len(big) {
		t.Fatal("bounding active with the flag off")
	}
}
