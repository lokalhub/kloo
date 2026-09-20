package agent

import (
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/tools"
)

func toolCall(args map[string]any) tools.Call { return tools.Call{Name: "edit_file", Args: args} }

func longFile(n int, marker string, at int) string {
	lines := make([]string, n)
	for i := range lines {
		lines[i] = "filler line"
	}
	if at >= 0 && at < n {
		lines[at] = marker
	}
	return strings.Join(lines, "\n")
}

// TestPinWindowIsOptIn: the pin is load-bearing for edit correctness (it is what
// stops the model editing against a stale copy). Narrowing it must not happen
// until the bench says it should.
func TestPinWindowIsOptIn(t *testing.T) {
	t.Setenv("KLOO_PIN_WINDOW", "")
	if _, _, w := pinWindowOf(longFile(5000, "", -1), ""); w {
		t.Fatal("pin windowed with the flag off")
	}
}

// TestPinWindowCentresOnTheLastEdit: the region the model is working in is the
// region worth paying for every turn.
func TestPinWindowCentresOnTheLastEdit(t *testing.T) {
	t.Setenv("KLOO_PIN_WINDOW", "1")
	body, note, w := pinWindowOf(longFile(3000, "const RETENTION_MARKER = 1", 2400), "const RETENTION_MARKER = 1")
	if !w {
		t.Fatal("long file was not windowed")
	}
	if !strings.Contains(body, "RETENTION_MARKER") {
		t.Fatal("window does not contain the edit site")
	}
	if n := strings.Count(body, "\n") + 1; n > pinMaxLines {
		t.Fatalf("window is %d lines, over the %d cap", n, pinMaxLines)
	}
	if !strings.Contains(note, "of 3000") {
		t.Fatalf("note does not say what was withheld: %q", note)
	}
}

// TestPinWindowNeverSilentlyTruncates: a model that believes it has seen a whole
// file will write an edit against a function it never read. Every narrowed pin
// must say so and say how to get the rest.
func TestPinWindowNeverSilentlyTruncates(t *testing.T) {
	t.Setenv("KLOO_PIN_WINDOW", "1")
	_, note, _ := pinWindowOf(longFile(3000, "", -1), "")
	if !strings.Contains(note, "read_file") {
		t.Fatalf("note does not tell the model how to see the rest: %q", note)
	}
}

// TestShortFilePinnedWhole: the whole point is to bound a LONG file. A file that
// already fits must be untouched, flag or no flag.
func TestShortFilePinnedWhole(t *testing.T) {
	t.Setenv("KLOO_PIN_WINDOW", "1")
	if _, _, w := pinWindowOf(longFile(pinMaxLines, "", -1), ""); w {
		t.Fatal("a file within the cap was windowed")
	}
}

// TestEditAnchorFromBothEditTools: the anchor has to come out of kloo's fenced
// diff as well as grok-shaped search_replace, or the window silently falls back
// to the head of the file for whichever tool is in use.
func TestEditAnchorFromBothEditTools(t *testing.T) {
	got := editAnchorOf(toolCall(map[string]any{"old_string": "const x = 1"}))
	if got != "const x = 1" {
		t.Fatalf("search_replace anchor: %q", got)
	}
	got = editAnchorOf(toolCall(map[string]any{
		"diff": "<<<<<<< SEARCH\nconst y = 2\n=======\nconst y = 3\n>>>>>>> REPLACE"}))
	if got != "const y = 2" {
		t.Fatalf("edit_file anchor: %q", got)
	}
	if editAnchorOf(toolCall(map[string]any{"content": "whole file"})) != "" {
		t.Fatal("write_file should yield no anchor")
	}
}
