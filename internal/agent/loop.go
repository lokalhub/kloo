package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/repomap"
	"github.com/lokalhub/kloo/internal/tools"
)

// Loop is the autonomous harness. Its dependencies are injected so the
// integration suite can drive it with a mocked LLM and real rails.
type Loop struct {
	Client   llm.LLMClient
	Adapter  tools.ToolAdapter
	Registry *tools.Registry

	// Rails (seams). Checkpoint may be nil (no snapshot/rollback).
	Verifier   Verifier
	Budget     Budget
	Churn      ChurnDetector
	Checkpoint Checkpointer

	// Memory is the in-process working-memory assembler. nil ⇒ the legacy
	// boundedHistory path (byte-identical to pre-P00: the repo-map budget stays
	// the full ContextTokens and the history is the bounded transcript). When
	// set, act() caps the repo map at mapBudgetTokens(ContextTokens) and routes
	// history assembly + whole-prompt compaction through it.
	Memory WorkingMemory

	// Context assembly.
	Root          string // workspace root for the repo map (empty ⇒ skip map)
	ContextTokens int    // per-step repo-map token budget
	System        string // system prompt
	Model         string
	Temperature   float64
	Now           func() time.Time // injectable clock (defaults to time.Now)
	// SessionHistory is the conversation from PRIOR runs in the same session (the
	// TUI reuses one Loop across submissions). It is seeded into working memory as
	// the oldest tail, so a follow-up ("what's the issue?", "now do the other
	// file") sees what happened before — summarized oldest-first under window
	// pressure, while the current task stays pinned. nil ⇒ a standalone run
	// (byte-identical to before; headless and one-shot set nothing).
	SessionHistory []llm.Message
	// MaxConversation bounds how many recent transcript messages (plus the
	// original task) are sent to the model per request, so the per-request prompt
	// can't grow unbounded across a long run and overflow a small model's context
	// window (the repo-map section of the system prompt is separately bounded by
	// ContextTokens). 0 ⇒ DefaultMaxConversation.
	MaxConversation int

	// OnState, if set, is called as the machine enters each state — a test seam
	// for asserting the act→apply→verify→decide→stop sequence.
	OnState func(State)

	// Observation hooks for a UI (Phase 05). All are optional and nil-gated, so
	// the loop's core behaviour is unchanged when they are unset:
	//   - OnDelta: streamed assistant content deltas. When set, act() uses the
	//     streaming client path (Stream) so the UI can render token-by-token;
	//     when nil, act() uses non-streaming Complete (unchanged).
	//   - OnTool: each dispatched tool call with its result/error.
	//   - OnProgress: a per-turn progress snapshot (step + budget counters).
	//   - OnBeforeEdit: called before dispatching an edit tool; returning false
	//     skips the edit (the approve-each "reject" path). nil ⇒ always apply.
	OnDelta      func(content string)
	OnTool       func(call tools.Call, res tools.Result, err error)
	OnProgress   func(step, maxSteps, tokens, maxTokens int)
	OnBeforeEdit func(call tools.Call) bool
}

func (l *Loop) onState(s State) {
	if l.OnState != nil {
		l.OnState(s)
	}
}

// DefaultMaxConversation bounds the transcript history per request when the loop
// does not set MaxConversation. Sized so a multi-step run keeps the per-request
// prompt small enough for an 8B–30B local model's context window; the cumulative
// token budget (budget.go) is the separate run-level ceiling.
const DefaultMaxConversation = 30

// editTools are the tool names that mutate the tree (trigger a lazy checkpoint).
func isEditTool(name string) bool {
	return name == tools.NameEditFile || name == tools.NameWriteFile
}

// maxConv returns the effective per-request transcript bound.
func (l *Loop) maxConv() int {
	if l.MaxConversation > 0 {
		return l.MaxConversation
	}
	return DefaultMaxConversation
}

// boundedHistory keeps at most max transcript messages per request: always the
// first message (the original task, so the goal is never dropped) plus the most
// recent (max-1) messages. This caps the per-request prompt size on a long run
// instead of resending an ever-growing transcript every turn.
func boundedHistory(convo []llm.Message, max int) []llm.Message {
	if max <= 0 || len(convo) <= max {
		return convo
	}
	out := make([]llm.Message, 0, max)
	out = append(out, convo[0])                      // the task
	out = append(out, convo[len(convo)-(max-1):]...) // most recent (max-1)
	return out
}

// Run drives the loop to a terminal Report. The error return is reserved for a
// programming/setup failure (e.g. a missing dependency); ordinary run outcomes
// (including failures) are carried in the Report, never as an error.
func (l *Loop) Run(ctx context.Context, task string) (*Report, error) {
	if l.Client == nil || l.Adapter == nil || l.Registry == nil || l.Verifier == nil || l.Budget == nil || l.Churn == nil {
		return nil, errors.New("agent: Loop is missing a required dependency")
	}
	now := l.Now
	if now == nil {
		now = time.Now
	}

	// Per-run state must start clean: the TUI reuses ONE Loop across many task
	// submissions, so without resetting, run N inherits run N-1's token/step totals
	// and churn streak (which made a second "hello" churn at step 1).
	l.Budget.Reset()
	l.Churn.Reset()

	convo := []llm.Message{{Role: llm.RoleUser, Content: task}}
	var (
		snap        Snapshot
		triedCkpt   bool
		lastVerify  VerifyResult
		curEditPath string // file last targeted by an edit ⇒ re-read fresh for the pin
		ignoredAll  []string
		step        int
	)

	// finish builds the report and rolls back on any non-success terminal path.
	finish := func(reason Reason, runErr error, be *BudgetEvidence, ce *ChurnEvidence) (*Report, error) {
		l.onState(StateStop)
		st := l.Budget.Stats()
		compactions := 0
		if l.Memory != nil {
			compactions = l.Memory.Stats().Compactions
		}
		rep := &Report{
			Reason:      reason,
			Steps:       step,
			FinalVerify: lastVerify,
			Budget:      be,
			Churn:       ce,
			Err:         runErr,
			TokensUsed:  st.Tokens,
			Elapsed:     st.Elapsed,
			Compactions: compactions,
			Ignored:     ignoredAll,
			Transcript:  append([]llm.Message(nil), convo...), // this run's task + steps, for the session
		}
		if reason != ReasonSuccess && snap.Taken && l.Checkpoint != nil {
			if err := l.Checkpoint.Rollback(ctx, snap); err == nil {
				rep.RolledBack = true
			}
		}
		return rep, nil
	}

	for {
		if ctx.Err() != nil {
			return finish(ReasonInterrupted, nil, nil, nil)
		}

		step++
		l.Budget.Observe(step)
		if l.OnProgress != nil {
			st := l.Budget.Stats()
			l.OnProgress(st.Steps, st.MaxSteps, st.Tokens, st.MaxTokens)
		}
		if tripped, kind := l.Budget.Check(); tripped {
			return finish(ReasonBudgetExceeded, nil, l.budgetEvidence(kind), nil)
		}
		if churned, kind := l.Churn.Check(); churned {
			return finish(ReasonChurn, nil, nil, &ChurnEvidence{Kind: kind, Artifact: l.Churn.Artifact()})
		}

		// ── ACT ─────────────────────────────────────────────────────────────
		l.onState(StateAct)
		call, ignored, usage, msg, err := l.act(ctx, task, convo, lastVerify, curEditPath)
		if err != nil {
			if ctx.Err() != nil {
				return finish(ReasonInterrupted, nil, nil, nil)
			}
			if errors.Is(err, ErrNoToolCall) {
				// Conversational reply (prose, no tool call): the answer is already
				// streamed to the transcript — stop calmly rather than error/churn.
				l.Budget.AddTokens(usage.TotalTokens)
				return finish(ReasonAnswered, nil, nil, nil)
			}
			return finish(ReasonError, err, nil, nil)
		}
		l.Budget.AddTokens(usage.TotalTokens)
		convo = append(convo, msg)
		for _, ig := range ignored {
			ignoredAll = append(ignoredAll, ig.Name)
		}

		// Track the file under edit so next turn re-reads it fresh for the pin
		// (working memory) instead of trusting the stale transcript copy.
		if isEditTool(call.Name) {
			curEditPath = str(call.Args["path"])
		}

		// Lazy checkpoint before the first edit (read-only runs take none).
		if isEditTool(call.Name) && !triedCkpt && l.Checkpoint != nil {
			triedCkpt = true
			if s, cerr := l.Checkpoint.Checkpoint(ctx); cerr == nil {
				snap = s
			}
			// A non-git workspace (ErrNotGitRepo) degrades silently to no rollback.
		}

		// ── APPLY ───────────────────────────────────────────────────────────
		l.onState(StateApply)
		var (
			result tools.Result
			derr   error
		)
		if isEditTool(call.Name) && l.OnBeforeEdit != nil && !l.OnBeforeEdit(call) {
			// approve-each rejected this edit: skip the apply, record it.
			derr = errEditRejected
		} else {
			result, derr = l.Registry.Dispatch(ctx, call)
		}
		if l.OnTool != nil {
			l.OnTool(call, result, derr)
		}
		convo = append(convo, observation(call, result, derr))

		// ── VERIFY ──────────────────────────────────────────────────────────
		l.onState(StateVerify)
		lastVerify = l.Verifier.Verify(ctx)

		// A non-runnable verify command is an error outcome, never a false pass.
		if lastVerify.Err != nil {
			if ctx.Err() != nil {
				return finish(ReasonInterrupted, nil, nil, nil)
			}
			return finish(ReasonError, fmt.Errorf("verify: %w", lastVerify.Err), nil, nil)
		}

		// Feed churn: the failing verify output (empty when passed) + the edit.
		l.Churn.Observe(Turn{
			VerifyOutput: failingOutput(lastVerify),
			Edit:         editSignature(call),
		})

		// ── DECIDE ──────────────────────────────────────────────────────────
		l.onState(StateDecide)
		if lastVerify.Passed {
			return finish(ReasonSuccess, nil, nil, nil)
		}
		// otherwise loop; budget/churn re-checked at the top of the next turn
	}
}

// act runs one model turn: assemble per-step context, call the model, and reduce
// to a single tool call (recording any extras as ignored). A malformed/no-call
// reply gets exactly one corrective re-prompt before surfacing an error.
func (l *Loop) act(ctx context.Context, task string, convo []llm.Message, lastVerify VerifyResult, curEditPath string) (tools.Call, []tools.Call, llm.Usage, llm.Message, error) {
	// Repo-map budget: the legacy path keeps the full window (byte-identical to
	// pre-P00); the memory path caps it at mapBudgetTokens so the map can no
	// longer eat the whole window (the Lead-1 fix — gated behind Memory != nil).
	mapBudget := l.ContextTokens
	if l.Memory != nil {
		mapBudget = mapBudgetTokens(l.ContextTokens)
	}
	sys := llm.Message{Role: llm.RoleSystem, Content: l.systemWithContext(task, mapBudget)}

	// History: working memory when set (pin-hot + summary + compaction under the
	// window), else the legacy bounded transcript (reused, not forked).
	var hist []llm.Message
	if l.Memory != nil {
		h, merr := l.Memory.Assemble(MemoryInput{
			Task:         task,
			Convo:        convo,
			History:      l.SessionHistory,
			LastVerify:   lastVerify,
			EditPath:     curEditPath,
			FreshFile:    l.reread(curEditPath),
			WindowTokens: l.ContextTokens,
			SystemTokens: repomap.ApproxTokens(sys.Content),
			MapBudget:    mapBudget,
		})
		if merr != nil {
			// ErrWindowTooSmall ⇒ a config error surfaced as a ReasonError stop.
			return tools.Call{}, nil, llm.Usage{}, llm.Message{}, merr
		}
		hist = h
	} else {
		hist = boundedHistory(convo, l.maxConv())
	}

	msgs := append([]llm.Message{sys}, hist...)
	req := l.Adapter.BuildRequest(llm.ChatRequest{
		Model:       l.Model,
		Messages:    msgs,
		Temperature: l.Temperature,
	}, l.Registry)

	resp, err := l.complete(ctx, req)
	if err != nil {
		return tools.Call{}, nil, llm.Usage{}, llm.Message{}, err
	}
	msg := assistantMessage(resp)
	usage := estimateUsage(resp.Usage, msgs, msg)
	calls, perr := l.Adapter.ParseAll(msg)

	if perr != nil || len(calls) == 0 {
		// One corrective re-prompt (the anti-spiral rail, mirrored from P02).
		corrective := llm.Message{Role: llm.RoleUser, Content: l.Adapter.Corrective(perr)}
		retryMsgs := append(append([]llm.Message{}, msgs...), msg, corrective)
		resp2, err2 := l.complete(ctx, llm.ChatRequest{Model: l.Model, Messages: retryMsgs, Temperature: l.Temperature})
		if err2 != nil {
			return tools.Call{}, nil, usage, msg, err2
		}
		msg2 := assistantMessage(resp2)
		usage2 := estimateUsage(resp2.Usage, retryMsgs, msg2)
		calls, perr = l.Adapter.ParseAll(msg2)
		if perr != nil {
			// A MALFORMED tool call after the nudge is a real error (the model tried to
			// act but botched the format).
			return tools.Call{}, nil, addUsage(usage, usage2), msg2, fmt.Errorf("agent: no usable tool call after re-prompt: %w", perr)
		}
		if len(calls) == 0 {
			// NO tool call at all — the model answered in prose. That's a conversational
			// reply, not a failure: surface it as a calm ReasonAnswered stop (the prose is
			// already streamed) instead of erroring/churning. ErrNoToolCall signals this.
			return tools.Call{}, nil, addUsage(usage, usage2), msg2, ErrNoToolCall
		}
		return calls[0], calls[1:], addUsage(usage, usage2), msg2, nil
	}
	return calls[0], calls[1:], usage, msg, nil
}

// estimateUsage returns the server-reported usage unchanged when it already
// reports a non-zero total (authoritative). When a turn reports zero usage —
// some OpenAI-compatible endpoints ignore include_usage — it substitutes a
// client-side estimate from the turn's prompt + completion text via the
// project's ApproxTokens heuristic, so the token counter is never frozen at
// zero. This computes a *number* only; the loop's decision logic is unchanged.
func estimateUsage(u llm.Usage, msgs []llm.Message, msg llm.Message) llm.Usage {
	if u.TotalTokens != 0 {
		return u
	}
	prompt := 0
	for _, m := range msgs {
		prompt += repomap.ApproxTokens(m.Content)
	}
	completion := repomap.ApproxTokens(msg.Content)
	for _, tc := range msg.ToolCalls {
		completion += repomap.ApproxTokens(tc.Function.Name) + repomap.ApproxTokens(tc.Function.Arguments)
	}
	u.PromptTokens = prompt
	u.CompletionTokens = completion
	u.TotalTokens = prompt + completion
	return u
}

// systemWithContext builds the per-step system prompt with a freshly curated
// repo map (so context is re-curated each turn, not accumulated unbounded).
// mapBudget is the repo-map token budget for this turn (the full window on the
// legacy path; mapBudgetTokens(window) when working memory is engaged).
func (l *Loop) systemWithContext(task string, mapBudget int) string {
	ctxStr := l.assembleContext(task, mapBudget)
	if ctxStr == "" {
		return l.System
	}
	return l.System + "\n\nRepository map (most relevant first):\n" + ctxStr
}

// assembleContext runs the Phase-03 pipeline for the task, bounded by mapBudget.
// Any failure degrades to empty context (the loop still runs).
func (l *Loop) assembleContext(task string, mapBudget int) string {
	if l.Root == "" {
		return ""
	}
	nodes, err := repomap.Walk(l.Root)
	if err != nil {
		return ""
	}
	files := repomap.Files(nodes)
	rels := make([]string, len(files))
	for i, f := range files {
		rels[i] = f.Path
	}
	syms := repomap.Extract(l.Root, rels)
	byFile := map[string][]repomap.Symbol{}
	for _, s := range syms {
		byFile[s.File] = append(byFile[s.File], s)
	}
	ranked := repomap.Rank(repomap.RankInput{Files: files, Symbols: byFile, Task: task})
	budget := mapBudget
	if budget <= 0 {
		budget = 2000
	}
	ctxStr, _ := repomap.Assemble(ranked, budget, repomap.ApproxTokens)
	return ctxStr
}

// reread returns the current content of path, freshly read through the jailed
// workspace (overview §3: re-read code from disk, never trust the stale
// transcript copy). It is bounded so a huge file can't blow the hot budget;
// the assembler truncates further under window pressure. Any failure (no path,
// no root, jail escape, missing file) degrades to "" — the file pin is simply
// omitted, never a panic.
func (l *Loop) reread(path string) string {
	if path == "" || l.Root == "" {
		return ""
	}
	ws, err := tools.NewWorkspace(l.Root)
	if err != nil {
		return ""
	}
	content, err := tools.ReadFile(ws, path)
	if err != nil {
		return ""
	}
	if max := maxKeepItemTokens * 4; len(content) > max {
		content = content[:max] + "\n…[truncated; re-read on demand]\n"
	}
	return content
}

// budgetEvidence renders the tripped budget's limit vs observed for the report.
func (l *Loop) budgetEvidence(kind BudgetKind) *BudgetEvidence {
	st := l.Budget.Stats()
	switch kind {
	case BudgetTokens:
		return &BudgetEvidence{Kind: kind, Limit: fmt.Sprint(st.MaxTokens), Observed: fmt.Sprint(st.Tokens)}
	case BudgetWallClock:
		return &BudgetEvidence{Kind: kind, Limit: st.MaxWall.String(), Observed: st.Elapsed.String()}
	default: // steps
		return &BudgetEvidence{Kind: kind, Limit: fmt.Sprint(st.MaxSteps), Observed: fmt.Sprint(st.Steps)}
	}
}

// observation renders a tool's result (or error) as a message fed back to the model.
func observation(call tools.Call, res tools.Result, err error) llm.Message {
	var b strings.Builder
	b.WriteString("tool ")
	b.WriteString(call.Name)
	if err != nil {
		b.WriteString(" error: ")
		b.WriteString(err.Error())
	} else {
		b.WriteString(" result:\n")
		b.WriteString(res.Output)
		if res.Stderr != "" {
			b.WriteString("\n[stderr]\n")
			b.WriteString(res.Stderr)
		}
		if call.Name == tools.NameRunCommand {
			b.WriteString(fmt.Sprintf("\n[exit %d]", res.ExitCode))
		}
	}
	return llm.Message{Role: llm.RoleUser, Content: b.String()}
}

// editSignature is the normalised edit a churn detector compares (empty for
// non-edit tools).
func editSignature(call tools.Call) string {
	switch call.Name {
	case tools.NameEditFile:
		return "edit_file " + str(call.Args["path"]) + "\n" + str(call.Args["diff"])
	case tools.NameWriteFile:
		return "write_file " + str(call.Args["path"]) + "\n" + str(call.Args["content"])
	default:
		return ""
	}
}

// failingOutput returns the combined output to compare for churn, or "" if the
// verify passed (progress).
func failingOutput(v VerifyResult) string {
	if v.Passed {
		return ""
	}
	return v.Stdout + "\n" + v.Stderr
}

// errEditRejected marks an edit the approve-each dial rejected (skipped).
var errEditRejected = errors.New("agent: edit rejected (approve-each)")

// ErrNoToolCall signals that the model replied in prose with no tool call (after the
// corrective re-prompt) — a conversational answer, not a failure. The loop turns it
// into a calm ReasonAnswered stop instead of ReasonError.
var ErrNoToolCall = errors.New("agent: model replied without a tool call (conversational)")

// complete runs one model call, streaming (forwarding deltas to OnDelta) when a
// delta hook is set, else non-streaming.
func (l *Loop) complete(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	if l.OnDelta == nil {
		return l.Client.Complete(ctx, req)
	}
	return l.Client.Stream(ctx, req, func(d llm.Delta) error {
		if d.Content != "" {
			l.OnDelta(d.Content)
		}
		return nil
	})
}

func assistantMessage(resp llm.ChatResponse) llm.Message {
	if len(resp.Choices) == 0 {
		return llm.Message{Role: llm.RoleAssistant}
	}
	return resp.Choices[0].Message
}

func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
	}
}

func orNoCall(err error) error {
	if err != nil {
		return err
	}
	return errors.New("no tool call in reply")
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
