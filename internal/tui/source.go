package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"
)

// TaskSource is the seam that feeds submitted task strings into the program as
// submitTaskMsg values. The interactive keyboard path is the ONE v1
// implementation; this abstraction exists so a future stdin/J1-provider source
// can be added by writing a new TaskSource — WITHOUT changing the model,
// Update, or View (design doc §5/§7; recorded in decisions.md).
//
// A future StdinTaskSource would implement Attach() to read tasks from stdin
// (one task per run) and emit submitTaskMsg per task; the eventual J1
// integration would, on a run's Report, emit a reportAgentResult to its sink.
// NEITHER the stdin reader NOR the reportAgentResult emission is wired in v1 —
// the seam is shape-only.
type TaskSource interface {
	// Attach returns a tea.Cmd that begins feeding tasks from this source, or nil
	// for the keyboard source (driven by the model's key handler).
	Attach() tea.Cmd
	// Name identifies the source (for status/diagnostics).
	Name() string
}

// keyboardSource is the v1 TaskSource: the textinput Enter handler emits the
// submitTaskMsg, so this source needs no background command.
type keyboardSource struct{}

func (keyboardSource) Attach() tea.Cmd { return nil }
func (keyboardSource) Name() string    { return "keyboard" }

// submitTaskMsg routes a non-slash task line through the submission path — the
// single channel every TaskSource (keyboard today, stdin tomorrow) feeds.
type submitTaskMsg struct{ task string }

// Runner launches the autonomous loop for a submitted task. The TUI is decoupled
// from internal/agent behind this interface: the runner translates the loop's
// streaming/tool/progress/report signals into the program's tea.Msg types
// (streamDeltaMsg, toolEventMsg, progressMsg, reportMsg, confirmRequestMsg) and
// sends them via the program. nil in unit tests that don't drive a real run.
type Runner interface {
	// Start runs task to completion under ctx using model, pumping messages into
	// the program; it returns when the run ends (it sends a terminal reportMsg).
	// model is the current model (the TUI's, switchable via /model) — applied to
	// this run so /model takes effect on the next task.
	Start(ctx context.Context, task, model string, mode Mode, contextFiles []string)
}
