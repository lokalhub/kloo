package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/edit"
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

	// Linter is the fast ADVISORY lint rail. nil ⇒ no lint step; the loop is
	// byte-identical to pre-lint behaviour (off-by-default-safe). When set, after a
	// successful edit the loop runs it on the edited file and appends its output to
	// the conversation as a model-visible observation ONLY. Lint NEVER gates success
	// (verify alone does, at the success gate below) and is NEVER fed to the churn
	// rail (Turn{} below is unchanged) — so it cannot false-churn.
	Linter Linter

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
	// ChatSystem, when non-empty, enables the conversational gate: ONE no-tools
	// model call before the agent loop that classifies the user's message as an
	// actionable task (→ run the loop) or conversation (→ reply directly, no run).
	// Empty ⇒ disabled (headless/benchmark, where every input is a real task).
	ChatSystem  string
	Model       string
	Temperature float64
	Now         func() time.Time // injectable clock (defaults to time.Now)
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

	// StallRounds is the stall backstop threshold: after this many CONSECUTIVE
	// no-progress turns (no edit landed, the file tree unchanged, AND the verify
	// result unchanged) the loop stops as ReasonAnswered — the model is spinning on
	// redundant read-only/no-op commands without calling finish. This is a
	// no-progress counter (it resets to 0 on any progress), ORTHOGONAL to MaxSteps:
	// it fires at a small N (≈ ChurnRounds), hundreds of steps before the budget
	// ceiling, so the two never overlap. 0 ⇒ DefaultStallRounds.
	StallRounds int

	// MaxRepairAttempts bounds how many ENRICHED repair observations the loop emits
	// per edit target before falling back to the bare error string. When an
	// edit_file fails as a no-match/ambiguous match (a SEARCH the model can fix
	// against the real file), the loop replaces the bare error with the file's
	// actual contents + a "fix this edit" instruction (repair.go) — but only this
	// many times per path, so a model that keeps failing terminates via the existing
	// churn/budget/stall rails rather than being enriched forever. It governs
	// ENRICHMENT only, never termination. 0 ⇒ DefaultMaxRepairAttempts.
	MaxRepairAttempts int

	// RepeatNudgeRounds / RepeatAbortRounds tune the repetition rail: how many
	// IDENTICAL consecutive tool calls (name + args) before the loop injects one
	// corrective observation, then halts the run as churn. 0 ⇒ the package
	// defaults (DefaultRepeatNudgeRounds / DefaultRepeatAbortRounds).
	RepeatNudgeRounds int
	RepeatAbortRounds int

	// ExploreNudgeRounds / ExploreAbortRounds tune the exploration rail: how many
	// CONSECUTIVE read-only turns (read_file/list_dir, no edit/run_command) before
	// the loop nudges the model to act, then stops the run. 0 ⇒ the package defaults.
	ExploreNudgeRounds int
	ExploreAbortRounds int

	// EditFailLimit is how many consecutive failed edit_file attempts (no successful
	// edit between) before the run halts as churn. 0 ⇒ DefaultEditFailLimit.
	EditFailLimit int

	// PromiseNudgeLimit is how many times a turn that narrates a next action with no
	// tool call is nudged to actually emit the call before the run accepts the calm
	// answered-stop. 0 falls back to DefaultPromiseNudgeLimit.
	PromiseNudgeLimit int

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
	//   - OnRetry: a transient model-call failure is about to be retried after a
	//     wait (so a UI can show "model timed out, retrying 1/2…"). nil ⇒ silent.
	OnDelta      func(content string)
	OnTool       func(call tools.Call, res tools.Result, err error)
	OnProgress   func(step, maxSteps, tokens, maxTokens int)
	OnBeforeEdit func(call tools.Call) bool
	OnRetry      func(attempt, max int, err error, wait time.Duration)

	// LLMRetries is how many EXTRA model-call attempts to make after the first when
	// a call fails transiently (endpoint timeout, cold model load, 5xx, dropped
	// connection). 0 ⇒ DefaultLLMRetries. A negative value disables retry.
	LLMRetries int
	// RetryBaseDelay is the first backoff wait; it doubles each attempt. 0 ⇒
	// DefaultRetryBaseDelay. (Tests set it tiny to stay fast.)
	RetryBaseDelay time.Duration
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

// DefaultStallRounds is the stall backstop used when StallRounds is unset. Kept
// small (a handful of turns) so a spinning model is caught quickly, yet large
// enough to let the model read a few files before its first edit without tripping.
const DefaultStallRounds = 3

// DefaultLLMRetries / DefaultRetryBaseDelay bound the transient-failure retry on
// model calls. Local endpoints (llama.cpp/llama-swap) routinely fail one call
// transiently — a cold model load or a slow prefill trips the stream idle timeout,
// or the connection drops — and a single such failure should NOT discard a long
// run. We retry a couple of times with exponential backoff before surfacing the
// error. See [[kloo-completion-termination]].
const (
	DefaultLLMRetries     = 2
	DefaultRetryBaseDelay = 2 * time.Second
)

// DefaultMaxRepairAttempts is the per-target edit-repair cap when MaxRepairAttempts
// is unset: up to this many CORRECTIVE observations per edit target (file contents
// for a no-match, or the grammar nudge for a malformed block) before the bare error
// returns and the churn/budget rails take over. Three gives a weak/reasoner model a
// real chance to recover its SEARCH or its block format instead of giving up.
const DefaultMaxRepairAttempts = 3

// DefaultRepeatNudgeRounds / DefaultRepeatAbortRounds bound the repetition rail:
// after a model fires the IDENTICAL tool call (name + args) this many times in a
// row, the loop first injects ONE corrective observation (nudge), then halts the
// run as churn (abort) if it keeps going. This catches degenerate repetition the
// repeated-failure/edit rails miss — most importantly a read-only spin (the same
// read_file/list_dir over and over), which leaves no edit or verify signal. Kept
// a touch above the verify/edit churn rounds so a legitimate read-twice never
// trips it. See [[kloo-churn-flail-gap]].
const (
	DefaultRepeatNudgeRounds = 3
	DefaultRepeatAbortRounds = 6
)

// DefaultExploreNudgeRounds / DefaultExploreAbortRounds bound the exploration rail:
// after this many CONSECUTIVE read-only turns (read_file/list_dir, no edit / write
// / run_command) the model is inspecting without acting — a weak model (e.g. a 2B
// ollama model) gets stuck analyzing and asking questions instead of editing, and
// no other rail catches it (different files each turn ⇒ not repetition; no verify
// change ⇒ not stall; no edit/failure ⇒ not churn). After the nudge it is told to
// act or ask-and-stop; after the abort the run stops (ReasonAnswered) so the human
// can step in. Generous so a legitimate read-many-then-edit run is never cut off.
const (
	DefaultExploreNudgeRounds = 6
	DefaultExploreAbortRounds = 10
)

// DefaultEditFailLimit bounds the failed-edit rail: after this many CONSECUTIVE
// edit_file attempts that fail to apply (malformed block, no-match, rejected) — with
// no successful edit in between (reads don't reset it) — the run halts as churn. The
// model can't produce a valid edit and is flailing (edit↔read), which no other rail
// catches (the call/edit varies → not repetition; reads break the explore streak; no
// verify signal in unverified mode). Past the repair-nudge budget (3) plus slack. See
// [[kloo-churn-flail-gap]].
const DefaultEditFailLimit = 5

// DefaultPromiseNudgeLimit bounds the promised-but-didn't-act rail: how many times a
// turn that NARRATES a next action ("let me run X") with no tool call is nudged to
// actually emit the call before the run accepts the calm answered-stop. The counter
// resets the moment the model emits a real call, so it bounds only consecutive
// all-talk turns — a model that acts between promises is never cut off.
const DefaultPromiseNudgeLimit = 3

// editTools are the tool names that mutate the tree (trigger a lazy checkpoint).
func isEditTool(name string) bool {
	return name == tools.NameEditFile || name == tools.NameWriteFile
}

// isReadOnlyTool reports whether a tool only inspects (no mutation, no shell).
func isReadOnlyTool(name string) bool {
	return name == tools.NameReadFile || name == tools.NameListDir || name == tools.NameReadDir || name == tools.NameSearch
}

// stallLimit is the effective stall backstop threshold.
func (l *Loop) stallLimit() int {
	if l.StallRounds > 0 {
		return l.StallRounds
	}
	return DefaultStallRounds
}

// repairLimit is the effective per-target repair-enrichment cap (mirrors the
// stallLimit 0-⇒-default seam).
func (l *Loop) repairLimit() int {
	if l.MaxRepairAttempts > 0 {
		return l.MaxRepairAttempts
	}
	return DefaultMaxRepairAttempts
}

// repeatNudgeRounds / repeatAbortRounds are the effective repetition-rail
// thresholds (mirror the stallLimit 0-⇒-default seam).
func (l *Loop) repeatNudgeRounds() int {
	if l.RepeatNudgeRounds > 0 {
		return l.RepeatNudgeRounds
	}
	return DefaultRepeatNudgeRounds
}

func (l *Loop) repeatAbortRounds() int {
	if l.RepeatAbortRounds > 0 {
		return l.RepeatAbortRounds
	}
	return DefaultRepeatAbortRounds
}

// exploreNudgeRounds / exploreAbortRounds are the effective exploration-rail
// thresholds (mirror the stallLimit 0-⇒-default seam).
func (l *Loop) exploreNudgeRounds() int {
	if l.ExploreNudgeRounds > 0 {
		return l.ExploreNudgeRounds
	}
	return DefaultExploreNudgeRounds
}

func (l *Loop) exploreAbortRounds() int {
	if l.ExploreAbortRounds > 0 {
		return l.ExploreAbortRounds
	}
	return DefaultExploreAbortRounds
}

func (l *Loop) editFailLimit() int {
	if l.EditFailLimit > 0 {
		return l.EditFailLimit
	}
	return DefaultEditFailLimit
}

func (l *Loop) promiseNudgeLimit() int {
	if l.PromiseNudgeLimit > 0 {
		return l.PromiseNudgeLimit
	}
	return DefaultPromiseNudgeLimit
}

// isRepairableEditFailure reports whether an edit failure is fixable by the model
// re-issuing a corrected SEARCH against the real file contents: a no-match
// (ErrSearchNotFound) or an ambiguous match (ErrAmbiguousMatch). A malformed
// block, path escape, or read error is NOT repairable-by-content — the bare error
// already tells the model to fix the block shape — so it is excluded here.
func isRepairableEditFailure(err error) bool {
	return errors.Is(err, edit.ErrSearchNotFound) || errors.Is(err, edit.ErrAmbiguousMatch)
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
	// Verifier is intentionally NOT required: a nil Verifier is "unverified mode"
	// (no verify command configured or auto-detected), where finish is honoured but
	// no run is labelled success. The other deps are always required.
	if l.Client == nil || l.Adapter == nil || l.Registry == nil || l.Budget == nil || l.Churn == nil {
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
		edited      bool // has the agent applied an edit this run? gates ReasonSuccess

		// repairAttempts counts the enriched repair observations emitted per edit
		// target this run, so enrichment is bounded to repairLimit() per path. A fresh
		// map per Run (like the other per-run vars) so the TUI's reused Loop starts
		// each task clean.
		repairAttempts = map[string]int{}

		// knownFiles are paths whose CONTENT the model has seen this run — read_file'd,
		// successfully edit_file'd, or successfully write_file'd. The write_file clobber
		// guard uses it to refuse a BLIND overwrite of an existing non-empty file the
		// model never read (which silently destroyed a real config in the wild).
		knownFiles = map[string]bool{}

		// Stall backstop state: a no-progress counter (resets on any progress) that
		// catches a model spinning on read-only/no-op commands without calling finish.
		stall       int
		stallSeeded bool
		prevFp      string // workspace tree fingerprint from the previous turn

		// Repetition rail state: the previous turn's tool-call signature, how many
		// times it has repeated identically in a row, and whether the one-shot nudge
		// for this streak has been emitted. Catches a model locked onto a single
		// identical call (e.g. re-reading one empty file) — a read-only spin the
		// edit/verify churn rails cannot see.
		repeatKeyLast string
		repeatStreak  int
		repeatNudged  bool

		// Exploration rail state: consecutive read-only turns (read_file/list_dir) with
		// no edit/run_command, and whether the one-shot nudge fired. Catches a weak
		// model that inspects+analyzes forever without acting.
		exploreStreak int
		exploreNudged bool

		// Failed-edit rail state: consecutive edit_file attempts that FAILED to apply
		// (reset by a successful edit). Catches the edit↔read flail no other rail sees.
		editFailStreak int

		// Promised-but-didn't-act rail state: how many times the model ended a turn by
		// NARRATING a next action ("let me run X") with no tool call — OR gave up in prose
		// right after a FAILING action (lastActionFailed). Reset only by a SUCCESSFUL
		// action, so a model that keeps failing-then-explaining is bounded. Catches a
		// model that talks/explains instead of acting, which the answered-stop would
		// otherwise accept as a finished reply (e.g. "a wrong command stops the run").
		promiseNudges    int
		lastActionFailed bool
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

	// Conversational gate: a no-tools turn that answers chit-chat / acknowledgments
	// directly instead of launching tool-driven work. A weak model otherwise re-does
	// the finished task on a vague input like "thanks" (the system prompt telling it
	// to just finish isn't enough). Only a TASK verdict falls through to the loop;
	// anything else is replied to and stops as a calm ReasonAnswered. Disabled when
	// ChatSystem is empty (headless/benchmark). A gate error fails OPEN — we run the
	// loop rather than block real work on a classifier hiccup.
	if l.ChatSystem != "" && ctx.Err() == nil {
		reply, conversational, usage := l.chatGate(ctx, task)
		l.Budget.AddTokens(usage.TotalTokens)
		if conversational {
			if l.OnDelta != nil {
				l.OnDelta(reply)
			}
			convo = append(convo, llm.Message{Role: llm.RoleAssistant, Content: reply})
			return finish(ReasonAnswered, nil, nil, nil)
		}
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
				l.Budget.AddTokens(usage.TotalTokens)
				// Promised-but-didn't-act rail: the model narrated a NEXT action ("let me
				// run X", "I'll check Y") but emitted no tool call, so nothing ran — the
				// pattern behind a run that keeps stopping as `answered` mid-task. Rather
				// than accept that announcement as a finished reply, nudge ONCE per episode
				// to actually emit the call, then continue. Bounded by promiseNudgeLimit so
				// a model that only ever narrates (never acts) still stops as answered; the
				// counter resets the moment it emits a real call (below), so distinct
				// promise episodes each get one rescue.
				if (promisesToAct(msg.Content) || lastActionFailed) && promiseNudges < l.promiseNudgeLimit() {
					promiseNudges++
					convo = append(convo, msg, promiseToActCorrective(lastActionFailed))
					continue
				}
				// Conversational reply (prose, no tool call): the answer is already
				// streamed to the transcript — stop calmly rather than error/churn.
				return finish(ReasonAnswered, nil, nil, nil)
			}
			return finish(ReasonError, err, nil, nil)
		}
		l.Budget.AddTokens(usage.TotalTokens)
		convo = append(convo, msg)
		for _, ig := range ignored {
			ignoredAll = append(ignoredAll, ig.Name)
		}

		// ── FINISH (explicit terminator) ─────────────────────────────────────
		// The model declared it is done. Honour it as a calm terminal stop — this
		// is how a no-edit / question task ends cleanly without spinning, which a
		// small model rarely manages via a tool-free turn (ReasonAnswered). The
		// label still hinges on verify, not self-report (kloo trusts only verify):
		// run one final verify — Success when it passes, else Answered (the model's
		// summary stands, but nothing was verified).
		if call.Name == tools.NameFinish {
			convo = append(convo, observation(call, tools.Result{Output: str(call.Args["summary"])}, nil))
			if l.Verifier == nil {
				// Unverified mode: no command to prove the change works. Honour finish
				// as a calm terminal stop, but label it UNVERIFIED — distinct from
				// success, which always requires a real green verify.
				return finish(ReasonUnverified, nil, nil, nil)
			}
			lastVerify = l.Verifier.Verify(ctx)
			if lastVerify.Err == nil && lastVerify.Passed {
				return finish(ReasonSuccess, nil, nil, nil)
			}
			return finish(ReasonAnswered, nil, nil, nil)
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
		switch {
		case isEditTool(call.Name) && l.OnBeforeEdit != nil && !l.OnBeforeEdit(call):
			// approve-each rejected this edit: skip the apply, record it.
			derr = errEditRejected
		case call.Name == tools.NameWriteFile && l.wouldClobberUnread(str(call.Args["path"]), str(call.Args["content"]), knownFiles):
			// Clobber guard: write_file would REPLACE an existing non-empty file the model
			// never read this run — a blind overwrite that has silently destroyed real
			// config. Refuse the apply and nudge the model to read-then-edit instead.
			derr = errWriteClobber
		default:
			result, derr = l.Registry.Dispatch(ctx, call)
		}
		if l.OnTool != nil {
			l.OnTool(call, result, derr)
		}

		// Track files whose CONTENT the model has now seen (read/edited/written OK), so a
		// later write_file to one of them is an informed overwrite, not a blind clobber.
		if derr == nil {
			switch call.Name {
			case tools.NameReadFile, tools.NameEditFile, tools.NameWriteFile:
				if p := str(call.Args["path"]); p != "" {
					knownFiles[p] = true
				}
			}
		}

		// Promise-rail progress signal: an action SUCCEEDED when it neither errored nor
		// (for run_command) exited non-zero. A successful action is real progress and
		// resets the promised-but-didn't-act / failure-recovery nudge budget; a FAILED
		// one arms lastActionFailed, so if the model then gives up in prose next turn it
		// is nudged to recover instead of the run silently stopping as answered (the
		// "a wrong command stops the run" gap). Read tools report ExitCode 0, so only a
		// run_command non-zero exit or a real dispatch error counts as a failure here.
		lastActionFailed = derr != nil || result.ExitCode != 0
		if !lastActionFailed {
			promiseNudges = 0
		}

		if isEditTool(call.Name) {
			if derr == nil {
				edited = true // a real change landed this run
				editFailStreak = 0
			} else {
				// An edit that FAILED to apply (malformed block, no-match, rejected).
				// Tracked across turns — reads in between don't reset it — so a model
				// that alternates failing-edit ↔ read without ever landing a valid edit
				// (the churn-flail-gap) is caught even though no single rail's signal
				// (identical call / read-only spin / repeated verify) fires.
				editFailStreak++
			}
		}

		// Repair enrichment: on a no-match/ambiguous edit_file failure under the
		// per-target cap, replace the bare error with a repair observation carrying the
		// file's ACTUAL contents + a "fix this edit" instruction (repair.go), so a weak
		// model can correct its SEARCH instead of guessing blind. The repair text goes
		// ONLY into convo (the model-facing transcript) — never into the churn feed
		// below (which still sees editSignature+verifyOut) and never affecting `edited`
		// (still set only on derr == nil), so no new false-churn/false-success source.
		obs := observation(call, result, derr)
		if call.Name == tools.NameEditFile {
			path := str(call.Args["path"])
			switch {
			case derr == nil:
				delete(repairAttempts, path) // a clean apply clears this target's repair budget
			case errors.Is(derr, edit.ErrMalformedBlock) && repairAttempts[path] < l.repairLimit():
				// Malformed block SHAPE (bad/duplicated/missing markers): nudge with the
				// exact grammar so the model retries with a correct call instead of
				// apologizing and stopping (the gpt-oss failure mode).
				obs = buildMalformedCorrection(l.Root, path)
				repairAttempts[path]++
			case isRepairableEditFailure(derr) && repairAttempts[path] < l.repairLimit():
				if rep, okRep := buildRepairObservation(l.Root, path, str(call.Args["diff"])); okRep {
					obs = rep
					repairAttempts[path]++
				}
			}
		}
		if call.Name == tools.NameWriteFile && errors.Is(derr, errWriteClobber) {
			// Replace the bare error with a guidance nudge: read the file first, then make
			// a surgical edit_file — or write_file again only to truly replace all of it.
			obs = buildClobberCorrection(l.Root, str(call.Args["path"]))
		}
		convo = append(convo, obs)

		// ── LINT (advisory) ─────────────────────────────────────────────────
		// After a SUCCESSFUL edit, run the fast lint on the edited file and feed its
		// output back to the model as an observation — and nothing else. This step
		// is nil-gated (Linter == nil ⇒ skipped, byte-identical to pre-lint) and
		// touches NONE of the decision state: not lastVerify, not edited, not the
		// Turn{} fed to churn below, not stall/prevFp, not the success gate. The
		// observation is an ordinary model-visible message; because it never reaches
		// the churn detector (which reads only Turn.VerifyOutput/Edit/Acted), a
		// linter that emits identical text every turn CANNOT false-churn a
		// progressing run (the prior constant-signal scar this plan must not redo).
		if isEditTool(call.Name) && derr == nil && l.Linter != nil {
			lr := l.Linter.Lint(ctx, []string{curEditPath})
			if lintMsg, ok := lintObservation(lr); ok { // ok == false when clean OR non-runnable
				convo = append(convo, lintMsg)
			}
		}

		// ── VERIFY ──────────────────────────────────────────────────────────
		// Unverified mode (nil Verifier) skips this entirely: lastVerify stays the
		// zero value (Passed=false), so the success gate below never fires and the
		// run can only end via finish (→ unverified), churn, budget, or answered.
		if l.Verifier != nil {
			l.onState(StateVerify)
			lastVerify = l.Verifier.Verify(ctx)

			// A non-runnable verify command is an error outcome, never a false pass.
			if lastVerify.Err != nil {
				if ctx.Err() != nil {
					return finish(ReasonInterrupted, nil, nil, nil)
				}
				return finish(ReasonError, fmt.Errorf("verify: %w", lastVerify.Err), nil, nil)
			}
		}

		// Feed churn: the failing verify output (empty when passed), the edit, and
		// whether the turn took a non-edit side-effecting action. A run_command that
		// launched (derr == nil) can mutate the tree (rm/mv/sed -i) yet leaves no edit
		// signature — without flagging it, shell-driven work is invisible to the churn
		// rail and a stuck run loops to the budget ceiling (see types.Turn.Acted).
		//
		// Unverified mode (no verifier) has NO failure signal to feed: lastVerify is
		// the zero value, and failingOutput would synthesise "\n" (a constant the
		// repeated-failure rail mis-reads as "same red build every step", churning a
		// progressing shell-driven run). Pass "" so only the repeated-EDIT rail can
		// fire — the one churn signal that still means "stuck" without a verify.
		verifyOut := ""
		if l.Verifier != nil {
			verifyOut = failingOutput(lastVerify)
		}
		l.Churn.Observe(Turn{
			VerifyOutput: verifyOut,
			Edit:         editSignature(call),
			Acted:        call.Name == tools.NameRunCommand && derr == nil,
		})

		// ── DECIDE ──────────────────────────────────────────────────────────
		l.onState(StateDecide)
		// Success means the agent's CHANGE verifies — not that an unrelated verify
		// happens to pass. A read-only run (e.g. list_dir/read to answer a question)
		// must NOT be declared COMPLETE just because `go test` trivially passes; that
		// cut the model off before it could answer. Require an edit this run; an
		// already-passing, no-edit run instead continues until the model answers
		// (ReasonAnswered) or a budget/churn rail fires.
		if lastVerify.Passed && edited {
			return finish(ReasonSuccess, nil, nil, nil)
		}

		// Failed-edit rail: the model keeps attempting edits that fail to apply (malformed
		// block, no-match) and re-reading between tries, never landing a valid edit — the
		// edit↔read flail that slips past the repetition/explore/churn rails. Halt it.
		if editFailStreak >= l.editFailLimit() {
			art := "edit_file kept failing to apply"
			if curEditPath != "" {
				art += " on " + curEditPath
			}
			art += fmt.Sprintf(" (%d attempts, no valid edit landed)", editFailStreak)
			return finish(ReasonChurn, nil, nil, &ChurnEvidence{Kind: ChurnEditFailed, Artifact: art})
		}

		// Repetition rail: a weak model can lock onto ONE identical tool call and
		// fire it over and over (the canonical case: re-reading a single empty file,
		// emitting the same prose each turn — see [[kloo-edit-silent-noop]] /
		// [[kloo-churn-flail-gap]]). The repeated-failure/edit rails never see it: a
		// read_file/list_dir leaves no edit signature and no verify change. So we
		// track the call's normalised (name + args) signature; the FIRST time it has
		// repeated repeatNudgeRounds times we inject one corrective observation (a
		// chance to recover without losing the run), and if it keeps repeating to
		// repeatAbortRounds we halt as churn. A distinct call resets the streak, so a
		// progressing run — which never fires the same call twice running — is immune.
		if key := repeatKey(call); key != "" {
			if key == repeatKeyLast {
				repeatStreak++
			} else {
				repeatKeyLast, repeatStreak, repeatNudged = key, 1, false
			}
			switch {
			case repeatStreak >= l.repeatAbortRounds():
				return finish(ReasonChurn, nil, nil, &ChurnEvidence{
					Kind:     ChurnRepeatedCall,
					Artifact: repeatArtifact(call, repeatStreak),
				})
			case repeatStreak >= l.repeatNudgeRounds() && !repeatNudged:
				repeatNudged = true
				convo = append(convo, repeatCorrective(call, repeatStreak))
			}
		}

		// Exploration rail: a weak model (e.g. a 2B ollama model) inspects file after
		// file, narrating analysis and asking the user questions, but never edits — and
		// because it reads a DIFFERENT file each turn (not repetition), with no verify
		// change (not stall) and no edit/failure (not churn), nothing else stops it; it
		// spins to the step ceiling. Count consecutive READ-ONLY turns (any edit / write
		// / run_command resets it): nudge once to act-or-ask, then stop the run
		// (ReasonAnswered) so the human can step in.
		if isReadOnlyTool(call.Name) {
			exploreStreak++
		} else {
			exploreStreak, exploreNudged = 0, false
		}
		switch {
		case exploreStreak >= l.exploreAbortRounds():
			return finish(ReasonAnswered, nil, nil, nil)
		case exploreStreak >= l.exploreNudgeRounds() && !exploreNudged:
			exploreNudged = true
			convo = append(convo, exploreCorrective(exploreStreak))
		}

		// Stall backstop: a no-progress counter, ORTHOGONAL to MaxSteps. It engages
		// ONLY when verify is PASSING — a green check with no edit and no tree change
		// for stallLimit consecutive turns means the model is spinning on redundant
		// read-only commands (re-confirming a done state with echo/ls) instead of
		// calling finish. A FAILING verify is deliberately left to churn + budget, so
		// a legitimate read-heavy run toward a fix (read many files, THEN edit) is
		// never cut off. Resets to 0 on any progress, so it fires at a small N far
		// below the step budget — the two ceilings never overlap.
		editedThisTurn := isEditTool(call.Name) && derr == nil
		fp := l.treeFingerprint()
		switch {
		case !stallSeeded:
			stallSeeded = true // first turn establishes the baseline; nothing to compare yet
		case !lastVerify.Passed:
			stall = 0 // red verify ⇒ churn/budget own this; never stall honest exploration
		case editedThisTurn || fp != prevFp:
			stall = 0 // real progress (an edit, or a run_command that changed the tree)
		default:
			stall++ // green verify, nothing changed ⇒ a confirming-spin
		}
		prevFp = fp
		if stall >= l.stallLimit() {
			return finish(ReasonAnswered, nil, nil, nil)
		}
		// otherwise loop; budget/churn re-checked at the top of the next turn
	}
}

// treeFingerprint is a cheap signature of the workspace's files (sorted
// path:size), so a run_command that mutates the tree (rm/mv/touch) registers as
// progress for the stall backstop even though it leaves no edit_file signature.
// Reuses the repo-map walker (which already runs each turn) and degrades to "" on
// any error — an empty, stable fingerprint simply makes the tree a no-op signal,
// leaving the verify-change and edit signals to drive the backstop.
func (l *Loop) treeFingerprint() string {
	if l.Root == "" {
		return ""
	}
	nodes, err := repomap.Walk(l.Root)
	if err != nil {
		return ""
	}
	h := fnv.New64a()
	for _, n := range repomap.Files(nodes) {
		fmt.Fprintf(h, "%s:%d\n", n.Path, n.Size)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// act runs one model turn: assemble per-step context, call the model, and reduce
// to a single tool call (recording any extras as ignored). A malformed/no-call
// reply gets exactly one corrective re-prompt before surfacing an error.
func (l *Loop) act(ctx context.Context, task string, convo []llm.Message, lastVerify VerifyResult, curEditPath string) (tools.Call, []tools.Call, llm.Usage, llm.Message, error) {
	// Repo-map budget: the legacy path keeps the full window (byte-identical to
	// pre-P00); the memory path caps it at mapBudgetTokens so the map can no
	// longer eat the whole window (the Lead-1 fix — gated behind Memory != nil).
	// win is the prompt-token budget. The memory path reserves headroom below the
	// model's context window (usableWindow) for the output + tool schemas +
	// estimation slack, so the assembled request stays under the server's n_ctx
	// (a full-window prompt overflowed it → 400). The legacy path keeps the full
	// window (byte-identical to pre-P00).
	win := l.ContextTokens
	mapBudget := l.ContextTokens
	if l.Memory != nil {
		win = usableWindow(l.ContextTokens)
		mapBudget = mapBudgetTokens(win)
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
			WindowTokens: win,
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

// repoMapFileCap mirrors repomap.maxMappedFileBytes (walk.go:34): a defensive
// upper bound on the size of a file whose content we read into memory for the
// graph signal. Walk already excludes files above this, but the guard keeps the
// OOM fix (171fcbf) honest at the read site too.
const repoMapFileCap = 1 << 20 // 1 MiB

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

	// Read each mapped file's content ONCE, through the jailed workspace (never a
	// raw os.ReadFile — keeps the path-jail intact), so Rank can build the
	// def→ref graph for the PageRank centrality signal. Reads are capped and
	// degrade non-fatally: a >cap or unreadable file is simply omitted (it then
	// contributes no graph references), matching assembleContext's degrade-to-
	// empty contract. (repomap excludes >1MiB at walk time; the cap here is a
	// defensive guard against re-reading a huge file into memory — the OOM fixed
	// in 171fcbf.)
	contents := map[string][]byte{}
	if ws, err := tools.NewWorkspace(l.Root); err == nil {
		for _, f := range files {
			if f.Size > repoMapFileCap {
				continue
			}
			data, err := tools.ReadFile(ws, f.Path)
			if err != nil {
				continue
			}
			contents[f.Path] = []byte(data)
		}
	}

	ranked := repomap.Rank(repomap.RankInput{Files: files, Symbols: byFile, Task: task, Contents: contents})
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

// repeatKey is the normalised signature (tool name + args) the repetition rail
// compares turn-to-turn: two calls collide only when they would do the SAME thing.
// finish is excluded — it terminates the loop on its own and never repeats. Args
// are JSON-encoded (encoding/json sorts map keys, so the bytes are stable across
// turns) then run through normalizeChurn, so volatile bits (temp paths, durations,
// hex) can't make two otherwise-identical calls look distinct.
func repeatKey(call tools.Call) string {
	if call.Name == "" || call.Name == tools.NameFinish {
		return ""
	}
	raw, err := json.Marshal(call.Args)
	if err != nil {
		raw = fmt.Appendf(nil, "%v", call.Args)
	}
	return call.Name + "\x00" + normalizeChurn(string(raw))
}

// repeatArtifact is the short "what was repeated ×N" line shown in the churn
// report when the repetition rail halts a run.
func repeatArtifact(call tools.Call, n int) string {
	target := str(call.Args["path"])
	if target == "" {
		target = str(call.Args["command"])
	}
	if target != "" {
		return fmt.Sprintf("%s %s (×%d)", call.Name, firstLine(target), n)
	}
	return fmt.Sprintf("%s (×%d)", call.Name, n)
}

// repeatCorrective is the one-shot nudge injected the first time a call repeats
// repeatNudgeRounds times: it names the stuck call and points at the usual escape
// (an empty file needs write_file, not another read), so a weak model can break
// the loop instead of riding it to the abort threshold.
func repeatCorrective(call tools.Call, n int) llm.Message {
	target := str(call.Args["path"])
	var b strings.Builder
	fmt.Fprintf(&b, "STOP — you have called %s", call.Name)
	if target != "" {
		fmt.Fprintf(&b, " on %s", target)
	}
	fmt.Fprintf(&b, " %d times in a row with the SAME arguments, and nothing changed. Repeating it will not help.\n", n)
	b.WriteString("Take a DIFFERENT action now:\n")
	if call.Name == tools.NameReadFile {
		b.WriteString("- If that file is empty or missing, create its contents with write_file — do NOT read it again.\n")
	}
	b.WriteString("- Otherwise make the change the task actually needs, or call finish if the work is already done.\n")
	return llm.Message{Role: llm.RoleUser, Content: b.String()}
}

// exploreCorrective is the one-shot nudge when the model has inspected files for
// exploreNudgeRounds turns without making any change — it has enough context;
// it should ACT, or ask the user a single short question and stop (no tool call →
// a calm answered stop) rather than keep reading.
func exploreCorrective(n int) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf(
		"You have inspected %d files in a row without making any change. You have enough "+
			"context now — STOP reading and ACT: make the edit the task requires (edit_file/"+
			"write_file) or run the needed command. If you genuinely need the user to clarify "+
			"something, reply with ONE short question and NO tool call so they can answer.", n)}
}

// promiseToActCorrective is the one-shot nudge for the promised-but-didn't-act rail.
// When lastFailed, the model gave up in prose right after a FAILING action — tell it
// the failure is not "done" and to recover. Otherwise it merely narrated a next step
// without emitting the call. Both end with: act now, or call finish if truly done —
// never accept a tool-free explanation as a finished reply.
func promiseToActCorrective(lastFailed bool) llm.Message {
	var b strings.Builder
	if lastFailed {
		b.WriteString("Your last command FAILED (non-zero exit) — that is NOT success and the task is not done. " +
			"Read the error in the previous output, then ACT this turn to recover: fix the cause and run a DIFFERENT " +
			"command, or try another approach. ")
	} else {
		b.WriteString("You described the next action in prose but did NOT call a tool, so nothing actually ran. " +
			"Do not narrate what you will do — DO it now: emit the tool call this turn (run_command to run a command, " +
			"read_file/search to inspect, edit_file/write_file to change code). ")
	}
	b.WriteString("Only call finish if the task is genuinely complete or you are truly blocked (say why). " +
		"If you need the user to decide something, ask ONE short question and call no tool.")
	return llm.Message{Role: llm.RoleUser, Content: b.String()}
}

// promiseVerbs are the action-announcing phrases a model emits right before it SHOULD
// call a tool. They are matched (lower-cased, substring) on a no-tool-call reply by
// promisesToAct. Deliberately action-verb-anchored ("let me run", not bare "let me")
// so a genuine conversational closer like "let me know if…" is NOT mis-read as a
// promise to act.
var promiseVerbs = []string{
	"let me run", "let me check", "let me try", "let me look", "let me examine",
	"let me see", "let me execute", "let me test", "let me install", "let me verify",
	"let me read", "let me search", "let me list", "let me inspect", "let me explore",
	"let me start", "let me first", "let me fix", "let me update", "let me create",
	"let me add", "let me find", "let me open", "let me build", "let me actually",
	"let's run", "let's check", "let's try", "let's see", "let's start",
	"i'll run", "i'll check", "i'll try", "i'll look", "i'll start", "i'll fix",
	"i will run", "i will check", "i'm going to", "i am going to",
	"going to run", "going to check", "try running", "now let me", "now i'll",
	"next, let me", "next i'll", "next, i'll",
}

// promisesToAct reports whether a no-tool-call reply READS like the model announced a
// next action ("let me run X") rather than delivering a final answer. Used by the
// promised-but-didn't-act rail to nudge the model to actually emit the call before
// the run accepts the calm answered-stop.
func promisesToAct(content string) bool {
	s := strings.ToLower(content)
	for _, p := range promiseVerbs {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
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

// errWriteClobber marks a write_file the clobber guard refused: it would shrink a
// substantial existing file the model never read this run (a blind overwrite).
var errWriteClobber = errors.New("agent: write_file would clobber an unread file")

// clobberMinBytes is the size at/above which an existing file is "substantial" enough
// to guard from a blind shrinking overwrite. Below it, a file is cheap to recreate and
// routinely (re)written, so guarding it would be noise; the real data-loss case in the
// wild was a ~2 KiB config replaced by a ~250-byte fabricated stub.
const clobberMinBytes = 512

// wouldClobberUnread reports whether a write_file to path would BLINDLY destroy unseen
// content: the target exists, is SUBSTANTIAL (≥ clobberMinBytes), the model has not
// read it this run (path ∉ known), AND the new content is SMALLER than what is there
// (a net shrink). A missing/empty/small file, a same-or-larger rewrite, a directory,
// an unresolved jail path, or an already-known file is NOT guarded — writing is fine.
func (l *Loop) wouldClobberUnread(path, newContent string, known map[string]bool) bool {
	if path == "" || known[path] {
		return false
	}
	ws, err := tools.NewWorkspace(l.Root)
	if err != nil {
		return false
	}
	abs, err := ws.Resolve(path)
	if err != nil {
		return false
	}
	fi, err := os.Stat(abs)
	if err != nil || fi.IsDir() {
		return false // missing/new file or a dir ⇒ not a clobber
	}
	return fi.Size() >= clobberMinBytes && int64(len(newContent)) < fi.Size()
}

// buildClobberCorrection is the model-facing nudge when the clobber guard refuses a
// write_file: read the existing file first, then make a surgical edit_file — or
// write_file again only if a full replacement is truly intended. It names the file
// and its size so the model sees what it was about to destroy.
func buildClobberCorrection(root, path string) llm.Message {
	size := int64(-1)
	if ws, err := tools.NewWorkspace(root); err == nil {
		if abs, rerr := ws.Resolve(path); rerr == nil {
			if fi, serr := os.Stat(abs); serr == nil {
				size = fi.Size()
			}
		}
	}
	where := path
	if size >= 0 {
		where = fmt.Sprintf("%s (%d bytes)", path, size)
	}
	return llm.Message{Role: llm.RoleUser, Content: fmt.Sprintf(
		"write_file was REFUSED: it would REPLACE the existing file %s with LESS content, and you have NOT read it "+
			"this run — a blind shrinking overwrite can destroy content you never saw. First call read_file %s to see "+
			"what is there, then make the change with edit_file (a surgical SEARCH/REPLACE). Only call write_file on %s "+
			"again if you genuinely intend to replace its ENTIRE contents, having read it first.", where, path, path)}
}

// ErrNoToolCall signals that the model replied in prose with no tool call (after the
// corrective re-prompt) — a conversational answer, not a failure. The loop turns it
// into a calm ReasonAnswered stop instead of ReasonError.
var ErrNoToolCall = errors.New("agent: model replied without a tool call (conversational)")

// chatSentinel is the exact token the gate model emits for an actionable task, so
// the loop proceeds. Anything else is a conversational reply shown to the user.
const chatSentinel = "TASK"

// chatGate classifies the user's message with ONE no-tools model call. A real
// coding request returns ("", false) so Run proceeds into the agent loop; anything
// conversational returns (reply, true) — the model's natural reply, generated WITH
// the session context but WITHOUT any tools, so a weak model can't mistake the
// message for "redo the work". Cheap by design: system + prior-run recap + the
// message, NO repo map. It is NON-streaming on purpose — a "TASK" verdict must
// never flash on the user's screen; the conversational reply is surfaced by the
// caller (via OnDelta) only once classification is known. Any error fails OPEN
// (returns false) so a classifier hiccup never blocks real work.
func (l *Loop) chatGate(ctx context.Context, task string) (reply string, conversational bool, usage llm.Usage) {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: l.ChatSystem}}
	msgs = append(msgs, l.SessionHistory...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: task})

	resp, err := l.Client.Complete(ctx, llm.ChatRequest{Model: l.Model, Messages: msgs, Temperature: l.Temperature})
	if err != nil {
		return "", false, llm.Usage{} // fail open: run the loop normally
	}
	msg := assistantMessage(resp)
	usage = estimateUsage(resp.Usage, msgs, msg)
	text := strings.TrimSpace(msg.Content)
	if text == "" || isTaskVerdict(text) {
		return "", false, usage
	}
	return text, true, usage
}

// isTaskVerdict reports whether the gate reply is the TASK sentinel. Lenient: the
// first whitespace-separated token, stripped of surrounding punctuation and
// upper-cased, equals TASK — tolerating "TASK", "TASK.", "Task:".
func isTaskVerdict(text string) bool {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return false
	}
	first := strings.ToUpper(strings.Trim(fields[0], ".,!?:;\"'`()[]"))
	return first == chatSentinel
}

// llmRetries / retryBaseDelay are the effective retry knobs (mirror the stallLimit
// 0-⇒-default seam; a NEGATIVE LLMRetries disables retry entirely).
func (l *Loop) llmRetries() int {
	if l.LLMRetries != 0 {
		return l.LLMRetries
	}
	return DefaultLLMRetries
}

func (l *Loop) retryBaseDelay() time.Duration {
	if l.RetryBaseDelay > 0 {
		return l.RetryBaseDelay
	}
	return DefaultRetryBaseDelay
}

// complete runs one model call, streaming (forwarding deltas to OnDelta) when a
// delta hook is set, else non-streaming. A TRANSIENT failure (endpoint timeout,
// cold model load, 5xx, dropped connection) is retried up to llmRetries() times
// with exponential backoff, so one flaky call doesn't throw away a long run. It is
// NOT retried when: the parent ctx is done (interrupt / wall-clock budget), the
// error is deterministic (4xx, auth, parse), or a stream already emitted tokens —
// retrying then would duplicate the visible output.
func (l *Loop) complete(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	attempts := max(1, l.llmRetries()+1) // a negative LLMRetries disables retry (1 try)
	var (
		resp llm.ChatResponse
		err  error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		emitted := false
		if l.OnDelta == nil {
			resp, err = l.Client.Complete(ctx, req)
		} else {
			resp, err = l.Client.Stream(ctx, req, func(d llm.Delta) error {
				if d.Content != "" {
					emitted = true
					l.OnDelta(d.Content)
				}
				return nil
			})
		}
		if err == nil || ctx.Err() != nil || attempt == attempts || emitted || !isRetryableLLMError(err) {
			return resp, err
		}
		wait := l.retryBaseDelay() << (attempt - 1) // 2s, 4s, …
		if l.OnRetry != nil {
			l.OnRetry(attempt, attempts-1, err, wait)
		}
		select {
		case <-ctx.Done():
			return resp, err
		case <-time.After(wait):
		}
	}
	return resp, err
}

// isRetryableLLMError reports whether a failed model call is a TRANSIENT endpoint
// hiccup worth retrying — versus a deterministic error (4xx auth/bad-request,
// parse) that a retry would only repeat. The parent-ctx guard lives in the caller,
// so a context.DeadlineExceeded reaching here is the request's OWN timeout (slow
// prefill / cold load), which is retryable.
func isRetryableLLMError(err error) bool {
	if err == nil {
		return false
	}
	// The classic local-endpoint hiccups: no token in time (cold load / slow
	// prefill) and a stream that ended before [DONE] (dropped connection).
	if errors.Is(err, llm.ErrStreamIdle) || errors.Is(err, llm.ErrStreamIncomplete) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Transport-level i/o timeouts.
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	// Connection reset/refused/EOF mid-flight — a server that's restarting or a
	// llama-swap mid model-swap. (no-such-host is a config error, NOT matched.)
	low := strings.ToLower(err.Error())
	for _, s := range []string{"connection reset", "connection refused", "unexpected eof", "broken pipe"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	// Upstream 5xx / 429 / 408 are server-side transient; other 4xx are not.
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500 || apiErr.StatusCode == 408 || apiErr.StatusCode == 429
	}
	return false
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
