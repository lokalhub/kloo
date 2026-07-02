package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
)

// noopRunner is a Runner whose Start does nothing, so submitTask marks the model
// running (for a static active-run frame) without launching a real loop goroutine.
type noopRunner struct{}

func (noopRunner) Start(context.Context, string, RuntimeConfig, Mode, []string) {}

// activeModel builds a sized, RUNNING model with a task, a pinned summary, and a
// few activity entries — the C8 active-run state used by the frame goldens.
func activeModel(t *testing.T, w, h int) Model {
	t.Helper()
	m := sized(New(Config{Model: "test-model", Effort: "medium", MaxSteps: 500, MaxTokens: 200000, Runner: noopRunner{}}), w, h)
	m = apply(m,
		submitTaskMsg{task: "add per-file scope enforcement without weakening the workspace jail"},
		progressMsg{Model: "test-model", Step: 7, MaxSteps: 500, Tokens: 14400, MaxTokens: 200000},
		toolEventMsg{Name: "read_file", Summary: "internal/tools/workspace.go  · 118 lines"},
		streamDeltaMsg{Content: "Scope checks are wired before writes; fixing read-only precedence test."},
		streamDoneMsg{},
		toolEventMsg{Name: "edit_file", Path: "internal/tools/scope.go", Search: "old", Replace: "new"},
		toolEventMsg{Name: "run_command", Command: "go test ./internal/tools", ExitCode: 1, Stderr: "FAIL"},
	)
	// Pin the (otherwise random) thinking verb and a fixed elapsed so the frame
	// golden is deterministic (mirrors reframe_test / thinking-line.golden).
	m.verb = "Cooking"
	m.runStart = time.Now().Add(-12 * time.Second)
	return m
}

// TestActiveRunFrameStandard renders the active-run layout at the standard test size
// and checks the task header, pinned summary, and activity log all appear with the
// input+hint still present (no overlap).
func TestActiveRunFrameStandard(t *testing.T) {
	m := activeModel(t, tw, th)
	v := m.View()
	requireGolden(t, "active-run.golden", v)
	for _, want := range []string{
		"Task: add per-file scope enforcement",  // task header (original request)
		"Latest: Scope checks are wired",        // pinned latest assistant summary
		"edited internal/tools/scope.go",        // activity log entry (done)
		"ran go test ./internal/tools (exit 1)", // activity log entry (fail)
		"> type a task",                         // input still present
		"Esc/Ctrl-C interrupt",                  // hint still present
	} {
		if !contains(v, want) {
			t.Errorf("active frame missing %q:\n%s", want, v)
		}
	}
}

// TestActiveRunFrameSmall: at 80x24 the header, task, summary, activity, input, and
// hint all fit with no overlap (the acceptance's small-terminal case).
func TestActiveRunFrameSmall(t *testing.T) {
	m := activeModel(t, 80, 24)
	v := m.View()
	requireGolden(t, "active-run-small.golden", v)
	// Height must not exceed the terminal: the composed view fits in 24 rows.
	if lines := strings.Count(v, "\n") + 1; lines > 24 {
		t.Fatalf("active frame at 80x24 is %d lines (must be ≤ 24):\n%s", lines, v)
	}
	for _, want := range []string{"Task: add per-file scope", "Latest:", "> type a task", "Esc/Ctrl-C interrupt"} {
		if !contains(v, want) {
			t.Errorf("small active frame missing %q:\n%s", want, v)
		}
	}
}

// TestActiveRunFrameWide: a wide terminal uses the available width and keeps the
// transcript scrollable (viewport present).
func TestActiveRunFrameWide(t *testing.T) {
	m := activeModel(t, 120, 36)
	v := m.View()
	requireGolden(t, "active-run-wide.golden", v)
	if !contains(v, "Task: add per-file scope enforcement without weakening the workspace jail") {
		t.Errorf("wide frame should show the full untruncated task:\n%s", v)
	}
}

// TestActiveRunNoColor: with the ascii profile (kloo's NO_COLOR/non-TTY degrade),
// the active frame carries no ANSI escape sequences and stays legible.
func TestActiveRunNoColor(t *testing.T) {
	m := activeModel(t, tw, th)
	v := m.View()
	if strings.Contains(v, "\x1b[") {
		t.Fatalf("active frame must carry no ANSI escapes under NO_COLOR/ascii:\n%q", v)
	}
	for _, want := range []string{"Task:", "Latest:", "✓ edited internal/tools/scope.go", "✗ ran go test"} {
		if !contains(v, want) {
			t.Errorf("no-color active frame missing %q:\n%s", want, v)
		}
	}
}

// TestActivityLogShowsRecentEntries: at least the last 3 entries are visible, and the
// log is capped to the visible window (oldest beyond the window drop off the glance).
func TestActivityLogShowsRecentEntries(t *testing.T) {
	m := activeModel(t, tw, 44)
	m = apply(m,
		toolEventMsg{Name: "read_file", Summary: "a.go · 1 line"},
		toolEventMsg{Name: "read_file", Summary: "b.go · 1 line"},
		toolEventMsg{Name: "read_file", Summary: "c.go · 1 line"},
		toolEventMsg{Name: "read_file", Summary: "d.go · 1 line"},
	)
	rr := m.renderRunningRegions()
	// The newest entries must be present…
	for _, want := range []string{"read c.go", "read d.go"} {
		if !contains(rr, want) {
			t.Errorf("activity log should show recent %q:\n%s", want, rr)
		}
	}
	// …and no more than activityLogVisible log lines are shown.
	logLines := 0
	for _, ln := range strings.Split(rr, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "✓") || strings.HasPrefix(strings.TrimSpace(ln), "✗") || strings.HasPrefix(strings.TrimSpace(ln), "→") {
			logLines++
		}
	}
	if logLines > activityLogVisible {
		t.Fatalf("activity log shows %d lines, want ≤ %d:\n%s", logLines, activityLogVisible, rr)
	}
}

// TestRunningRegionsHiddenWhenIdle: idle (not running) renders none of the C8 panels,
// so the idle layout is unchanged.
func TestRunningRegionsHiddenWhenIdle(t *testing.T) {
	m := sized(New(Config{Model: "test-model", MaxSteps: 40, MaxTokens: 8000}), tw, th)
	if rr := m.renderRunningRegions(); rr != "" {
		t.Fatalf("idle model must render no running regions, got:\n%s", rr)
	}
}

// TestActiveRunViaProgram drives a scripted run through the real program (teatest):
// progress → edit → run_command → report. While running the task header + activity
// are visible; after the report the run returns to idle.
func TestActiveRunViaProgram(t *testing.T) {
	tm := teatest.NewTestModel(t,
		New(Config{Model: "test-model", Effort: "medium", MaxSteps: 80, MaxTokens: 200000, Runner: noopRunner{}}),
		teatest.WithInitialTermSize(tw, th))

	tm.Send(submitTaskMsg{task: "rename the tabs"})
	for _, msg := range []tea.Msg{
		progressMsg{Model: "test-model", Step: 1, MaxSteps: 80, Tokens: 400, MaxTokens: 200000},
		streamDeltaMsg{Content: "Renaming the tabs now."},
		streamDoneMsg{},
		toolEventMsg{Name: "edit_file", Path: "a.ts", Search: "x", Replace: "y"},
		toolEventMsg{Name: "run_command", Command: "npm test", ExitCode: 0},
	} {
		tm.Send(msg)
	}
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		s := string(b)
		return contains(s, "Task: rename the tabs") && contains(s, "edited a.ts")
	}, teatest.WithDuration(3*time.Second))

	// The report ends the run → regions hide, stop banner shows.
	tm.Send(reportMsg{Reason: "success", Steps: 2, Tokens: 400, MaxTokens: 200000, Elapsed: "3s", VerifyCmd: "npm test", VerifyExit: 0})
	teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
		return contains(string(b), "run stopped — COMPLETE")
	}, teatest.WithDuration(3*time.Second))

	tm.Send(tea.KeyMsg{Type: tea.KeyCtrlC})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}
