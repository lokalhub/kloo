package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/edit"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/session"
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
	maxTokens  int
	send       func(tea.Msg)
	// session is the conversation carried ACROSS submissions in one TUI session:
	// each run's transcript plus a compact outcome note, fed back as the loop's
	// SessionHistory so follow-ups ("what's the issue?", "now the other file") have
	// context. Working memory compacts it under the window each turn.
	session []llm.Message
	// store + sess persist that conversation to disk so it also survives a restart
	// (resume). Both nil ⇒ in-memory only (tests, or a workspace store that failed
	// to init); the in-session carry still works.
	store *session.Store
	sess  *session.Session
	now   func() time.Time // injectable clock for persistence timestamps
}

// NewLoopRunner builds a runner over a pre-constructed loop (deps wired by the
// caller — internal/cli). ws is the workspace jail used to read /add-pinned
// files into the loop's context; maxTokens is the token budget shown in the
// status line / stop-report. The model is passed per-run to Start (so /model
// can switch it between runs) — not fixed at construction.
func NewLoopRunner(loop *agent.Loop, ws tools.Workspace, maxTokens int) *LoopRunner {
	return &LoopRunner{loop: loop, ws: ws, baseSystem: loop.System, maxTokens: maxTokens, now: time.Now}
}

// WithSession attaches a persistent session: the runner seeds its in-memory carry
// from the session's saved messages and writes the session back after each run, so
// the conversation survives a restart. Returns the runner for chaining.
func (r *LoopRunner) WithSession(store *session.Store, sess *session.Session) *LoopRunner {
	r.store, r.sess = store, sess
	if sess != nil {
		r.session = append([]llm.Message{}, sess.Messages...)
	}
	return r
}

// setSend connects the program's message sink (called by Run).
func (r *LoopRunner) setSend(send func(tea.Msg)) { r.send = send }

// Start runs the loop for task and pumps its signals into the program. In
// approve-each mode an edit is held via a confirmRequestMsg until the user
// answers. It blocks until the run ends, then sends the terminal reportMsg.
func (r *LoopRunner) Start(ctx context.Context, task, model string, mode Mode, contextFiles []string) {
	if r.send == nil {
		return
	}

	// Apply the current model to the loop so THIS run's requests use it. The model
	// can change between runs via /model in the TUI; without this the loop kept the
	// model fixed at launch and /model only relabeled the header (the request still
	// went out under the launch model). Set per-run from the caller's current model.
	r.loop.Model = model

	// Wire /add-pinned files into the loop's per-run context: read each (jailed)
	// and inject a bounded section into the system prompt so the model always
	// sees them. Rebuilt from the base each run so a removed pin does not linger.
	r.loop.System = r.baseSystem + pinnedSection(r.ws, contextFiles)

	r.loop.OnProgress = func(step, maxSteps, tokens, maxTokens int) {
		r.send(progressMsg{Model: model, Step: step, MaxSteps: maxSteps, Tokens: tokens, MaxTokens: maxTokens})
		// Forward the working-memory compaction count over the same plumbing
		// (nil-safe: no message when memory is off, so the indicator stays hidden).
		if r.loop.Memory != nil {
			r.send(memoryMsg{Compactions: r.loop.Memory.Stats().Compactions})
		}
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

	// Carry context across submissions: seed this run with the session so far, then
	// fold this run's transcript + a compact outcome note back in for the next one.
	r.loop.SessionHistory = r.session
	rep, _ := r.loop.Run(ctx, task)
	if rep != nil {
		r.session = append(r.session, rep.Transcript...)
		r.session = append(r.session, sessionOutcome(rep))
		r.session = capSession(r.session)
		r.persist(task)
	}
	r.send(reportFor(rep, r.maxTokens))
}

// persist writes the updated session to disk (resume across restarts). No-op when
// there is no store (in-memory-only runs). A write error is non-fatal — the run
// already happened and the in-memory carry still works; we don't crash the TUI.
func (r *LoopRunner) persist(task string) {
	if r.store == nil || r.sess == nil {
		return
	}
	r.sess.Messages = r.session
	r.sess.Runs++
	r.sess.Updated = r.now()
	if r.sess.Title == "" {
		r.sess.Title = session.Title(task)
	}
	_ = r.store.Save(r.sess)
}

// maxSessionMessages bounds the carried session so a very long interactive session
// can't grow the in-memory transcript (re-assembled each turn) without limit.
// Working memory already summarises old turns under the window; this just caps the
// raw backlog, dropping the oldest beyond the limit.
const maxSessionMessages = 300

func capSession(s []llm.Message) []llm.Message {
	if len(s) <= maxSessionMessages {
		return s
	}
	return append([]llm.Message{}, s[len(s)-maxSessionMessages:]...)
}

// sessionOutcome is the compact note appended after a run so the NEXT submission
// knows how this one ended — the key to answering follow-ups like "what's the
// issue?". It carries the stop reason, any error, and the last verify (with its
// failing output), which is exactly what a user asking about a failed run needs.
func sessionOutcome(rep *agent.Report) llm.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "[Previous run ended: %s after %d step(s).", rep.Reason, rep.Steps)
	if rep.Err != nil {
		fmt.Fprintf(&b, " Error: %s.", rep.Err.Error())
	}
	if rep.FinalVerify.Command != "" {
		fmt.Fprintf(&b, " Last verify: %s (exit %d, passed=%t).", rep.FinalVerify.Command, rep.FinalVerify.ExitCode, rep.FinalVerify.Passed)
		if !rep.FinalVerify.Passed {
			if out := strings.TrimSpace(rep.FinalVerify.Stdout + "\n" + rep.FinalVerify.Stderr); out != "" {
				fmt.Fprintf(&b, "\n%s", out)
			}
		}
	}
	if rep.RolledBack {
		b.WriteString(" The workspace was rolled back to the pre-run checkpoint.")
	}
	b.WriteString("]")
	return llm.Message{Role: llm.RoleAssistant, Content: b.String()}
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
	case "read_file":
		// Read-only browsing is noise if it dumps the whole file: show the path and a
		// dim line count, not the contents.
		return toolEventMsg{Name: "read_file", Summary: pathSummary(call, res.Output, "line", "lines")}
	case "list_dir":
		return toolEventMsg{Name: "list_dir", Summary: pathSummary(call, res.Output, "entry", "entries")}
	default:
		return toolEventMsg{Name: call.Name, Summary: res.Output}
	}
}

// pathSummary renders the compact one-line summary for a read-only tool: its path
// argument (root "." shown as ".") plus a dim count of the result size — so the
// transcript reads "read_file src/app.ts · 48 lines" instead of dumping the file.
func pathSummary(call tools.Call, output, unit, units string) string {
	path := str(call.Args["path"])
	if path == "" {
		path = "."
	}
	n := 0
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	if n == 0 {
		return path
	}
	if n == 1 {
		return fmt.Sprintf("%s  · 1 %s", path, unit)
	}
	return fmt.Sprintf("%s  · %d %s", path, n, units)
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

// humanizeError turns a raw run error into a short, natural-language reason with
// an actionable hint, so the stop-report reads like a sentence instead of a Go
// error dump. The common local-endpoint failures get a tailored message; anything
// unrecognized falls back to the raw error (still better than a bare "ERROR").
func humanizeError(err error) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	low := strings.ToLower(raw)
	switch {
	case strings.Contains(low, "connection refused"):
		return "Couldn't reach the model endpoint — is the server running at your --endpoint? (connection refused)"
	case strings.Contains(low, "no such host"), strings.Contains(low, "no route to host"):
		return "Couldn't reach the model endpoint — check the --endpoint address. (host unreachable)"
	case strings.Contains(low, "no router for requested model"),
		strings.Contains(low, "unknown model"), strings.Contains(low, "model not found"):
		return "The endpoint doesn't serve that model — set --model (or /model) to a name it has. Multi-model servers like llama-swap/Ollama route by name."
	case strings.Contains(low, "context deadline exceeded"),
		strings.Contains(low, "timeout"), strings.Contains(low, "timed out"):
		return "The model endpoint timed out — it may be loading or overloaded. Retry, or give it more time."
	case strings.Contains(low, "peg-native"),
		strings.Contains(low, "does not match the expected"):
		return "The model's reply wasn't a tool call kloo could parse — it may not support tool-calling. Serve it with --jinja, or try a more capable model."
	case strings.Contains(low, "missing required scope"),
		strings.Contains(low, "unauthorized"), strings.Contains(low, "forbidden"),
		strings.Contains(low, "401"), strings.Contains(low, "invalid api key"):
		return "Authentication failed — check your API key (KLOO_API_KEY, falls back to OPENAI_API_KEY)."
	case strings.Contains(low, "rate limit"), strings.Contains(low, "429"):
		return "The endpoint rate-limited the request — wait a moment and retry, or check your plan's limits."
	case strings.Contains(low, "below the irreducible prompt floor"),
		strings.Contains(low, "window too small"):
		return "The context window is too small for the task + system prompt — raise maxContextTokens."
	case strings.Contains(low, "no usable tool call"), strings.Contains(low, "no tool call"):
		return "The model replied without a tool call kloo could use (it may have answered in prose or used an unsupported format). kloo runs actions, not chat — give it a concrete task."
	default:
		return raw
	}
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
	case rep.Err != nil:
		// Surface the failure as a short, natural-language reason with a hint
		// instead of a bare "ERROR" or a raw Go error dump.
		msg.Detail = humanizeError(rep.Err)
	case rep.Budget != nil:
		msg.Detail = string(rep.Budget.Kind) + " (" + rep.Budget.Observed + "/" + rep.Budget.Limit + ")"
	case rep.Churn != nil:
		msg.Detail = string(rep.Churn.Kind)
	}
	return msg
}
