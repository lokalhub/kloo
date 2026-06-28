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
	store        *session.Store
	sess         *session.Session
	now          func() time.Time // injectable clock for persistence timestamps
	statusWriter func(RuntimeConfig, *agent.Report, time.Duration) error
	beforeRun    func(context.Context, string, RuntimeConfig) string
	afterRun     func(context.Context, string, RuntimeConfig, *agent.Report, time.Duration)
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

// WithStatusWriter attaches an optional completion hook used by the CLI to write
// the same structured JSON summary as headless runs after visible TUI runs.
func (r *LoopRunner) WithStatusWriter(fn func(RuntimeConfig, *agent.Report, time.Duration) error) *LoopRunner {
	r.statusWriter = fn
	return r
}

// WithRunHooks attaches optional lifecycle hooks around visible task runs. The
// before hook may return a system-prompt section for this run; the after hook is
// called once the loop has completed. Hook failures are handled by the caller so
// the TUI package stays unaware of specific integrations such as MCP memory.
func (r *LoopRunner) WithRunHooks(before func(context.Context, string, RuntimeConfig) string, after func(context.Context, string, RuntimeConfig, *agent.Report, time.Duration)) *LoopRunner {
	r.beforeRun, r.afterRun = before, after
	return r
}

// setSend connects the program's message sink (called by Run).
func (r *LoopRunner) setSend(send func(tea.Msg)) { r.send = send }

// Start runs the loop for task and pumps its signals into the program. In
// approve-each mode an edit is held via a confirmRequestMsg until the user
// answers. It blocks until the run ends, then sends the terminal reportMsg.
func (r *LoopRunner) Start(ctx context.Context, task string, runtime RuntimeConfig, mode Mode, contextFiles []string) {
	if r.send == nil {
		return
	}

	// Apply the current runtime config to the loop so THIS run's requests use the
	// selected endpoint/key/model/tool adapter. Switches apply between runs only.
	r.applyRuntime(runtime)

	var runSystemSection string
	if r.beforeRun != nil {
		runSystemSection = r.beforeRun(ctx, task, runtime)
	}
	// Wire /add-pinned files into the loop's per-run context: read each (jailed)
	// and inject a bounded section into the system prompt so the model always
	// sees them. Rebuilt from the base each run so a removed pin does not linger.
	r.loop.System = r.baseSystem + runSystemSection + pinnedSection(r.ws, contextFiles)

	r.loop.OnProgress = func(step, maxSteps, tokens, maxTokens int) {
		r.send(progressMsg{Model: runtime.Model, Step: step, MaxSteps: maxSteps, Tokens: tokens, MaxTokens: maxTokens})
		// Forward the working-memory compaction count over the same plumbing
		// (nil-safe: no message when memory is off, so the indicator stays hidden).
		if r.loop.Memory != nil {
			r.send(memoryMsg{Compactions: r.loop.Memory.Stats().Compactions})
		}
	}
	// Capture a compact, human-readable display log of THIS run for resume: the
	// prompt, the assistant's prose per turn, and a one-line summary per tool call.
	// Separate from r.session (the model-facing recap) — this is what the TUI replays
	// so a resumed session shows the prior conversation instead of a bare banner.
	disp := []session.DisplayItem{{Kind: dispUser, Text: boundText(task)}}
	var prose strings.Builder
	flushProse := func() {
		if t := strings.TrimSpace(stripToolMarkup(prose.String())); t != "" {
			disp = append(disp, session.DisplayItem{Kind: dispAssistant, Text: boundText(t)})
		}
		prose.Reset()
	}

	r.loop.OnDelta = func(content string) {
		prose.WriteString(content)
		r.send(streamDeltaMsg{Content: content})
	}
	r.loop.OnTool = func(call tools.Call, res tools.Result, err error) {
		flushProse() // the prose streamed before this call belongs to this turn
		disp = append(disp, session.DisplayItem{Kind: dispTool, Text: toolSummary(call, res, err)})
		r.send(streamDoneMsg{}) // finalize any streamed assistant text for this turn
		r.send(toolEvent(call, res))
	}
	r.loop.OnRetry = func(attempt, max int, err error, wait time.Duration) {
		// A transient model-call failure is being retried — show it as a dim line so a
		// slow/cold endpoint reads as "retrying", not a frozen run.
		r.send(noticeMsg{text: fmt.Sprintf("⟳ retrying model call %d/%d in %s — %s",
			attempt, max, wait.Round(time.Second), humanizeError(err))})
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
	// fold in ONE compact recap of this run for the next one. We deliberately do NOT
	// replay the raw prior transcript (assistant turns + tool observations): a small
	// model parrots a replayed assistant note and re-executes prior tool calls. A
	// single user-role context recap (task + outcome + the reply) gives a follow-up
	// what it needs without derailing the model.
	r.loop.SessionHistory = r.session
	start := time.Now()
	rep, _ := r.loop.Run(ctx, task)
	elapsed := time.Since(start)
	flushProse() // the final turn's answer (prose with no trailing tool call)
	if rep != nil {
		r.session = capSession(append(r.session, sessionRecap(task, rep)))
		if r.sess != nil {
			r.sess.Transcript = capDisplay(append(r.sess.Transcript, disp...))
		}
		r.persist(task)
	}
	if r.statusWriter != nil {
		if err := r.statusWriter(runtime, rep, elapsed); err != nil {
			r.send(noticeMsg{text: "status file write failed: " + err.Error()})
		}
	}
	if r.afterRun != nil {
		r.afterRun(ctx, task, runtime, rep, elapsed)
	}
	r.send(reportFor(rep, r.maxTokens))
}

func (r *LoopRunner) applyRuntime(runtime RuntimeConfig) {
	if runtime.Model != "" {
		r.loop.Model = runtime.Model
	}
	r.loop.Endpoint = runtime.Endpoint
	if runtime.ContextTokens > 0 {
		r.loop.ContextTokens = runtime.ContextTokens
	}
	r.loop.Temperature = runtime.Temperature
	r.loop.NoThink = runtime.NoThink
	if runtime.NewClient != nil {
		r.loop.Client = runtime.NewClient(runtime.Endpoint, runtime.Model, runtime.APIKey)
	}
	if runtime.ToolFormat != "" {
		if adapter, err := tools.SelectAdapter(runtime.ToolFormat, tools.EndpointCaps{SupportsTools: true}); err == nil {
			r.loop.Adapter = adapter
		}
	}
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

// sessionRecap builds the single context message carried to the NEXT submission so
// a follow-up ("what's the issue?", "continue", "why?") has what happened — the
// task, how the run ended (reason, error, failing verify), and the reply kloo gave.
//
// It is a USER-role context message, NOT an assistant turn: a small model replays
// an assistant note as its own output (parroting it verbatim). Framing it as
// bracketed background context the user is providing keeps the model responding to
// the current task instead of echoing or continuing the prior run.
func sessionRecap(task string, rep *agent.Report) llm.Message {
	var b strings.Builder
	b.WriteString("(Background from earlier in this kloo session — context only, not a new request.)\n")
	fmt.Fprintf(&b, "Task: %s\n", oneLine(task))
	fmt.Fprintf(&b, "Outcome: %s after %d step(s).", rep.Reason, rep.Steps)
	if rep.Err != nil {
		fmt.Fprintf(&b, " Error: %s.", rep.Err.Error())
	}
	if rep.FinalVerify.Command != "" {
		fmt.Fprintf(&b, " Verify: %s exit=%d passed=%t.", rep.FinalVerify.Command, rep.FinalVerify.ExitCode, rep.FinalVerify.Passed)
		if !rep.FinalVerify.Passed {
			if out := strings.TrimSpace(rep.FinalVerify.Stdout + "\n" + rep.FinalVerify.Stderr); out != "" {
				fmt.Fprintf(&b, "\nVerify output:\n%s", out)
			}
		}
	}
	if rep.RolledBack {
		b.WriteString(" The workspace was rolled back to the pre-run checkpoint.")
	}
	if ans := lastAssistantContent(rep.Transcript); ans != "" {
		fmt.Fprintf(&b, "\nkloo's reply: %s", oneLine(ans))
	}
	return llm.Message{Role: llm.RoleUser, Content: b.String()}
}

// lastAssistantContent returns the last assistant message's prose (the reply kloo
// gave), or "" if none — used to fold the answer into the next run's recap.
func lastAssistantContent(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant {
			if c := strings.TrimSpace(msgs[i].Content); c != "" {
				return c
			}
		}
	}
	return ""
}

// oneLine collapses whitespace/newlines and bounds length for a compact recap.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:237] + "…"
	}
	return s
}

// Display-item kinds for the resumable transcript (session.DisplayItem.Kind).
const (
	dispUser      = "user"
	dispAssistant = "assistant"
	dispTool      = "tool"
)

// maxDisplayItems bounds the persisted display log so a long session's JSON stays
// small; the oldest items beyond the cap are dropped (resume shows the recent tail).
const maxDisplayItems = 400

func capDisplay(d []session.DisplayItem) []session.DisplayItem {
	if len(d) <= maxDisplayItems {
		return d
	}
	return append([]session.DisplayItem{}, d[len(d)-maxDisplayItems:]...)
}

// boundText trims and caps a display string so one turn can't bloat the session
// file. Newlines are preserved (readability); only length is bounded.
func boundText(s string) string {
	const max = 2000
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// toolSummary renders one tool call as a compact, readable line for the resume log
// (e.g. "ran: npm run build [exit 0]", "edited home.page.html", "read app.ts").
// It mirrors the live cards but flattens each to a single line.
func toolSummary(call tools.Call, res tools.Result, err error) string {
	switch call.Name {
	case "run_command":
		cmd := oneLine(str(call.Args["command"]))
		switch {
		case err != nil:
			return fmt.Sprintf("ran: %s [error]", cmd)
		default:
			return fmt.Sprintf("ran: %s [exit %d]", cmd, res.ExitCode)
		}
	case "edit_file", "write_file":
		verb := "edited"
		if call.Name == "write_file" {
			verb = "wrote"
		}
		if err != nil {
			return fmt.Sprintf("%s %s [error]", verb, str(call.Args["path"]))
		}
		return fmt.Sprintf("%s %s", verb, str(call.Args["path"]))
	case "read_file":
		return "read " + pathSummary(call, res.Output, "line", "lines")
	case "list_dir":
		return "listed " + pathSummary(call, res.Output, "entry", "entries")
	default:
		if err != nil {
			return call.Name + " [error]"
		}
		return call.Name
	}
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
	case "read_dir":
		// Bulk folder read: show "<path> · N files" (N parsed from the result head
		// "read N file(s) …"), not the whole concatenated dump.
		n := "?"
		if f := strings.Fields(res.Output); len(f) >= 2 && f[0] == "read" {
			n = f[1]
		}
		p := str(call.Args["path"])
		return toolEventMsg{Name: "read_dir", Path: p, Summary: p + "  · " + n + " files"}
	case "search":
		// "<query> · N matches" (N from the result head "found N match(es) …").
		n := "0"
		if f := strings.Fields(res.Output); len(f) >= 2 && f[0] == "found" {
			n = f[1]
		}
		return toolEventMsg{Name: "search", Summary: str(call.Args["query"]) + "  · " + n + " matches"}
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
//
// Uses ParseFlexible (NOT strict Parse) — the SAME parser the edit_file tool uses
// to APPLY the edit. Small models often emit BARE (unfenced) SEARCH/REPLACE blocks;
// strict Parse rejected those, so the card fell back to dumping the whole raw block
// (markers and all) as red `-` lines while the edit still applied. Matching the
// apply parser keeps the card's diff in sync with what actually lands.
func parseDiffEdits(path, diff string) []editPair {
	blocks, err := edit.ParseFlexible(path + "\n" + diff)
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
		if art := strings.TrimSpace(rep.Churn.Artifact); art != "" {
			if i := strings.IndexByte(art, '\n'); i >= 0 {
				art = art[:i]
			}
			msg.Detail += " (" + art + ")"
		}
	}
	return msg
}
