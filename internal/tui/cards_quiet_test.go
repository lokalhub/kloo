package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/tools"
)

// boxedLines counts the frame's box-drawing TOP edges (┌), which is one per
// bordered card drawn.
func boxedLines(frame string) int {
	n := 0
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "┌") {
			n++
		}
	}
	return n
}

// TestBodylessToolsDrawNoBox: a run of tools that each have nothing but a name and
// a path must not draw a border apiece.
//
// Reported with a screenshot: three consecutive write_file calls rendered as three
// full-width empty boxes, each containing the two words "• write_file" and nothing
// else. At forty calls a run that is working normally reads as a wall of borders.
// The frame still has the header and input boxes — what must not grow is one box
// per tool.
func TestBodylessToolsDrawNoBox(t *testing.T) {
	base := boxedLines(tall().View())
	v := apply(tall(),
		toolEventMsg{Name: "write_file", Path: "a.go", Summary: "a.go"},
		toolEventMsg{Name: "write_file", Path: "b.go", Summary: "b.go"},
		toolEventMsg{Name: "read_file", Summary: "c.go  · 12 lines"},
		toolEventMsg{Name: "run_command", Command: "go build ./...", ExitCode: 0},
		toolEventMsg{Name: "verify", Summary: "go test    passed"},
	).View()
	if got := boxedLines(v); got > base {
		t.Errorf("5 bodyless tools drew %d boxes, chrome alone draws %d — none should be bordered:\n%s", got, base, v)
	}
	for _, want := range []string{"write_file", "a.go", "b.go", "go build ./...", "exit 0"} {
		if !contains(v, want) {
			t.Errorf("de-boxing lost %q from the transcript:\n%s", want, v)
		}
	}
}

// TestFailingCommandKeepsItsBox: the border is reserved for a body worth framing.
// A command that FAILED has stderr to show and a red edge that earns its space, so
// de-boxing the bodyless cases must not take that with it.
func TestFailingCommandKeepsItsBox(t *testing.T) {
	base := boxedLines(tall().View())
	v := apply(tall(), longFailCard()).View()
	if got := boxedLines(v); got <= base {
		t.Errorf("a failing command must still be boxed (got %d boxes, chrome alone %d):\n%s", got, base, v)
	}
}

// TestRefusedToolShowsWhy: a tool call that failed renders its reason, not just its
// name. The screenshot that prompted this work showed three write_file cards with
// no path and no error — the model had been refused for not reading the file first,
// and the transcript gave the human no way to know that.
func TestRefusedToolShowsWhy(t *testing.T) {
	ev := toolEvent(
		tools.Call{Name: "write_file", Args: map[string]any{"path": "paste.go"}},
		tools.Result{},
		errors.New("refusing to shrink an unread file: read paste.go first"),
	)
	if ev.Err == "" {
		t.Fatal("toolEvent dropped the tool's failure reason")
	}
	v := apply(tall(), ev).View()
	if !contains(v, "refusing to shrink") {
		t.Errorf("the refusal reason is missing from the card:\n%s", v)
	}
}
