package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/lokal/kloo/internal/tools"
	"github.com/muesli/termenv"
)

// TestWantsNoColor: the pure decision over the NO_COLOR × TTY matrix.
func TestWantsNoColor(t *testing.T) {
	set := func(string) (string, bool) { return "", true }    // NO_COLOR present (empty value)
	unset := func(string) (string, bool) { return "", false } // NO_COLOR absent
	cases := []struct {
		name  string
		env   func(string) (string, bool)
		isTTY bool
		want  bool
	}{
		{"NO_COLOR set + TTY", set, true, true},
		{"NO_COLOR set + non-TTY", set, false, true},
		{"unset + non-TTY", unset, false, true},
		{"unset + TTY", unset, true, false},
	}
	for _, tc := range cases {
		if got := wantsNoColor(tc.env, tc.isTTY); got != tc.want {
			t.Errorf("%s: wantsNoColor = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// populatedFrame renders a transcript with a colour-bearing surface (a failed
// run_command card → danger border, a stop report) so a colour profile would
// emit ANSI escapes.
func populatedFrame(m Model) string {
	m = apply(m,
		submitTaskMsg{task: "build the thing"},
		progressMsg{Model: "snappy", Step: 1, MaxSteps: 40, Tokens: 400, MaxTokens: 8000},
		streamDeltaMsg{Content: "working on it"},
		streamDoneMsg{},
		toolEvent(tools.Call{Name: "run_command", Args: map[string]any{"command": "npm run build"}}, tools.Result{ExitCode: 1, Stderr: "boom"}),
		reportMsg{Reason: "error", Steps: 1, VerifyCmd: "npm run build", VerifyExit: 1},
	)
	return m.View()
}

// TestDegradeNoANSIEscapes: under the forced ascii profile (what the degrade
// does), a populated frame contains no ANSI escape bytes — layout/glyphs/labels
// intact, no colour. TestMain already pins termenv.Ascii, mirroring the degrade.
func TestDegradeNoANSIEscapes(t *testing.T) {
	m := sized(New(Config{Model: "snappy", MaxSteps: 40, MaxTokens: 8000}), tw, 40)
	frame := populatedFrame(m)
	if strings.Contains(frame, "\x1b[") {
		t.Errorf("degraded frame must contain no ANSI escapes, got:\n%q", frame)
	}
	// Intent survives the degrade (no-color.html): labels + glyphs intact.
	for _, want := range []string{"▸ you: build the thing", "⌘ run_command", "exit 1 ✗"} {
		if !strings.Contains(frame, want) {
			t.Errorf("degraded frame lost intent %q:\n%s", want, frame)
		}
	}
	// Commit the delivered degraded frame as a golden (Product-lens evidence for
	// the no-color.html row; regenerate with -update if the layout legitimately
	// changes).
	requireGolden(t, "no-color-degrade.golden", frame)
}

// TestDegradeActuallyStripsColour: the meaningful before/after — under a colour
// profile a styled frame emits escapes; forcing ascii (the degrade) removes them.
// The suite's ascii default is restored on exit so no other test is affected.
func TestDegradeActuallyStripsColour(t *testing.T) {
	defer lipgloss.SetColorProfile(termenv.Ascii) // restore the suite default

	m := sized(New(Config{Model: "snappy", MaxSteps: 40, MaxTokens: 8000}), tw, 40)

	lipgloss.SetColorProfile(termenv.TrueColor)
	if !strings.Contains(populatedFrame(m), "\x1b[") {
		t.Fatal("precondition: a colour profile should emit ANSI escapes for a styled frame")
	}

	lipgloss.SetColorProfile(termenv.Ascii) // the degrade
	if strings.Contains(populatedFrame(m), "\x1b[") {
		t.Error("after forcing ascii, the frame should carry no ANSI escapes")
	}
}
