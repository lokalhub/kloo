package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/edit"
	"github.com/lokalhub/kloo/internal/tools"
)

// maxPinnedFileChars bounds how much of each /add-pinned file is injected into
// the loop's system prompt (keeps the per-request prompt small for local models).
const maxPinnedFileChars = 2000

// LoopRunner wires the Phase-04 autonomous loop into the TUI: it sets the loop's
// observation hooks (OnDelta/OnTool/OnProgress/OnBeforeEdit) to translate the
// loop's signals into the program's tea.Msg types, runs the loop, and emits the
// terminal reportMsg. It keeps the TUI's only dependency on internal/agent in
// this one file.
type LoopRunner struct {
	loop       *agent.Loop
	ws         tools.Workspace
	baseSystem string // the loop's system prompt without any pinned files
	model      string
	maxTokens  int
	send       func(tea.Msg)
}

// NewLoopRunner builds a runner over a pre-constructed loop (deps wired by the
// caller — internal/cli). ws is the workspace jail used to read /add-pinned
// files into the loop's context; model is the display model name; maxTokens is
// the token budget shown in the status line / stop-report.
func NewLoopRunner(loop *agent.Loop, ws tools.Workspace, model string, maxTokens int) *LoopRunner {
	return &LoopRunner{loop: loop, ws: ws, baseSystem: loop.System, model: model, maxTokens: maxTokens}
}

// setSend connects the program's message sink (called by Run).
func (r *LoopRunner) setSend(send func(tea.Msg)) { r.send = send }

// Start runs the loop for task and pumps its signals into the program. In
// approve-each mode an edit is held via a confirmRequestMsg until the user
// answers. It blocks until the run ends, then sends the terminal reportMsg.
func (r *LoopRunner) Start(ctx context.Context, task string, mode Mode, contextFiles []string) {
	if r.send == nil {
		return
	}

	// Wire /add-pinned files into the loop's per-run context: read each (jailed)
	// and inject a bounded section into the system prompt so the model always
	// sees them. Rebuilt from the base each run so a removed pin does not linger.
	r.loop.System = r.baseSystem + pinnedSection(r.ws, contextFiles)

	r.loop.OnProgress = func(step, maxSteps, tokens, maxTokens int) {
		r.send(progressMsg{Model: r.model, Step: step, MaxSteps: maxSteps, Tokens: tokens, MaxTokens: maxTokens})
	}
	r.loop.OnDelta = func(content string) {
		r.send(streamDeltaMsg{Content: content})
	}
	r.loop.OnTool = func(call tools.Call, res tools.Result, err error) {
		r.send(streamDoneMsg{}) // finalize any streamed assistant text for this turn
		r.send(toolEvent(call, res))
	}
	// approve-each: hold each edit for a y/n confirm (debug mode); auto/accept-edits apply directly.
	if mode == ModeApproveEach {
		r.loop.OnBeforeEdit = func(call tools.Call) bool {
			return r.confirmGate(ctx, call)
		}
	} else {
		r.loop.OnBeforeEdit = nil
	}

	rep, _ := r.loop.Run(ctx, task)
	r.send(reportFor(rep, r.maxTokens))
}

// confirmGate sends a confirm request for an approve-each edit and waits for the
// user's y/n decision — OR the run's context being cancelled, whichever comes
// first. Selecting on ctx.Done() means an interrupt (Esc/Ctrl-C) unblocks a held
// edit (returning rejected) so the loop goroutine never deadlocks.
func (r *LoopRunner) confirmGate(ctx context.Context, call tools.Call) bool {
	decision := make(chan bool, 1)
	path := str(call.Args["path"])
	r.send(confirmRequestMsg{
		Path:    path,
		Edits:   parseDiffEdits(path, str(call.Args["diff"])),
		Respond: func(ok bool) { decision <- ok },
	})
	select {
	case d := <-decision:
		return d
	case <-ctx.Done():
		return false
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// pinnedSection reads each /add-pinned file through the workspace jail and
// renders a bounded "Pinned context files" block for the system prompt. Empty
// when no files are pinned; an unreadable file is noted, not fatal.
func pinnedSection(ws tools.Workspace, files []string) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nPinned context files (the user added these with /add — keep them in mind):\n")
	for _, f := range files {
		content, err := tools.ReadFile(ws, f)
		if err != nil {
			fmt.Fprintf(&b, "--- %s (unreadable: %v) ---\n", f, err)
			continue
		}
		fmt.Fprintf(&b, "--- %s ---\n%s\n", f, truncate(content, maxPinnedFileChars))
	}
	return b.String()
}

// toolEvent translates a dispatched loop tool call into a card message. For
// edit_file the raw `diff` arg (a fenced SEARCH/REPLACE block) is PARSED into
// proper search/replace pairs so the card renders SEARCH as `-` and REPLACE as
// `+` (not the raw fence/markers).
func toolEvent(call tools.Call, res tools.Result) toolEventMsg {
	switch call.Name {
	case "edit_file":
		path := str(call.Args["path"])
		return toolEventMsg{Name: "edit_file", Path: path, Edits: parseDiffEdits(path, str(call.Args["diff"]))}
	case "run_command":
		return toolEventMsg{Name: "run_command", Command: str(call.Args["command"]), ExitCode: res.ExitCode, Stderr: res.Stderr}
	default:
		return toolEventMsg{Name: call.Name, Summary: res.Output}
	}
}

// parseDiffEdits parses an edit_file `diff` arg (a bare fenced SEARCH/REPLACE
// block — no filename line) into editPairs using the Phase-01 engine. The path
// is prefixed as the engine's filename line (as the edit_file tool itself does),
// covering multi-block and empty-SEARCH (new-file) cases. A malformed/unparsable
// diff degrades to showing the raw text as a single removed block.
func parseDiffEdits(path, diff string) []editPair {
	blocks, err := edit.Parse(path + "\n" + diff)
	if err != nil || len(blocks) == 0 {
		return []editPair{{search: diff}}
	}
	out := make([]editPair, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, editPair{search: b.Search, replace: b.Replace})
	}
	return out
}

// reportFor translates a Phase-04 Report into the terminal reportMsg.
func reportFor(rep *agent.Report, maxTokens int) reportMsg {
	if rep == nil {
		return reportMsg{Reason: "error", Detail: "no report"}
	}
	msg := reportMsg{
		Reason:     string(rep.Reason),
		Steps:      rep.Steps,
		Tokens:     rep.TokensUsed,
		MaxTokens:  maxTokens,
		Elapsed:    rep.Elapsed.Round(1e9).String(),
		VerifyCmd:  rep.FinalVerify.Command,
		VerifyExit: rep.FinalVerify.ExitCode,
		RolledBack: rep.RolledBack,
	}
	switch {
	case rep.Budget != nil:
		msg.Detail = string(rep.Budget.Kind) + " (" + rep.Budget.Observed + "/" + rep.Budget.Limit + ")"
	case rep.Churn != nil:
		msg.Detail = string(rep.Churn.Kind)
	}
	return msg
}
