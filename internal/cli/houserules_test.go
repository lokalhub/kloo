package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHouseRulesRepeatsTheProhibitionsLast is the regression guard for a measured
// failure: a repo whose AGENTS.md says UI chrome must use Ionic components, and which
// contains no inline style attribute anywhere, got an edit that added style="..." to
// its markup. The rule was loaded and pinned — thousands of tokens above the task. The
// fix is recency: the imperatives are restated at the very end, next to the work.
func TestHouseRulesRepeatsTheProhibitionsLast(t *testing.T) {
	dir := t.TempDir()
	md := strings.Join([]string{
		"# AGENTS.md",
		"",
		"Some prose about the project that is context, not a rule.",
		"",
		"## Conventions",
		"- **Ionic-native components** for all UI chrome — no bespoke buttons/inputs.",
		"- Angular signals throughout (standalone components).",
		"- Tests are explicit: you must never modify a test to make it pass.",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	got := agentsInstructions(dir, nil, 131072, nil)

	if !strings.Contains(got, "HOUSE RULES") {
		t.Fatalf("no house-rules block was appended:\n%s", got)
	}
	idx := strings.Index(got, "HOUSE RULES")
	if idx < strings.Index(got, "Some prose") {
		t.Error("the house rules must come LAST — recency is the whole point")
	}
	tail := got[idx:]
	if !strings.Contains(tail, "no bespoke buttons/inputs") {
		t.Errorf("the prohibition that was ignored in the real incident is missing:\n%s", tail)
	}
	if !strings.Contains(tail, "never modify a test") {
		t.Errorf("an obligation line should be restated too:\n%s", tail)
	}
	// Descriptive bullets are noise here: a long list recreates the dilution.
	if strings.Contains(tail, "Angular signals throughout") {
		t.Errorf("a bullet with no imperative should not be restated:\n%s", tail)
	}
	// Markdown emphasis is stripped so the line reads as an instruction.
	if strings.Contains(tail, "**") {
		t.Errorf("markdown emphasis should be stripped:\n%s", tail)
	}
}

// TestHouseRulesStaysShort: the block is a reminder, not a second rulebook.
func TestHouseRulesStaysShort(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("# AGENTS.md\n\n")
	for i := 0; i < 40; i++ {
		b.WriteString("- You must never do forbidden thing number ")
		b.WriteString(strings.Repeat("x", 20))
		b.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := agentsInstructions(dir, nil, 131072, nil)
	tail := got[strings.Index(got, "HOUSE RULES"):]
	if n := strings.Count(tail, "\n- "); n > 8 {
		t.Errorf("house rules should cap at 8 lines, got %d", n)
	}
	if len(tail) > 1200 {
		t.Errorf("house rules block too long: %d bytes", len(tail))
	}
}

// TestNoHouseRulesWithoutImperatives: a repo whose AGENTS.md is pure description
// gets no block at all, rather than a meaningless heading.
func TestNoHouseRulesWithoutImperatives(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"),
		[]byte("# AGENTS.md\n\n- The engine is pure and deterministic.\n- The worker is a Cloudflare Worker.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := agentsInstructions(dir, nil, 131072, nil); strings.Contains(got, "HOUSE RULES") {
		t.Errorf("description-only instructions should produce no house rules:\n%s", got)
	}
}
