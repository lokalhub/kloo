package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lokalhub/kloo/internal/agent"
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
	// Start runs task to completion under ctx using runtime, pumping messages into
	// the program; it returns when the run ends (it sends a terminal reportMsg).
	// runtime is the current model/client config (switchable via /model) applied
	// to this run so /model takes effect on the next task.
	// images are attachments for the task message, as data URLs; they travel
	// separately from task because they are sent as vision content parts, not text.
	Start(ctx context.Context, task string, runtime RuntimeConfig, mode Mode, contextFiles []string, images []string)

	// ContextGauge returns the window-occupancy measurement from the last assembled
	// prompt (agent.ContextGauge), for the /context command. It is a pure read of
	// what buildPrompt already measured — it assembles nothing and costs the prompt
	// nothing. ok is false when no prompt has been assembled yet (before the first
	// run), so /context can say so instead of printing zeros as if they were data.
	ContextGauge() (g agent.ContextGauge, ok bool)

	// MeasureResumed assembles the prompt a RESUMED session would send on its next
	// turn (carried SessionHistory + a placeholder task) and returns its occupancy.
	// It exists because ContextGauge is only populated after a run has assembled a
	// prompt, so on a freshly resumed session /context would otherwise report
	// "nothing assembled" even though the prior conversation is loaded and would be
	// re-sent. ok is false when there is no carried history (a fresh session), so
	// /context still says "submit a task first" rather than printing a turn-one
	// figure as if it were carried context. Assembles nothing on the wire and costs
	// the prompt nothing — no model call, no tool call, no write.
	MeasureResumed(ctx context.Context) (g agent.ContextGauge, ok bool)
}
