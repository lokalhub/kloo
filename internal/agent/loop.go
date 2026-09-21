package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/edit"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/repomap"
	"github.com/lokalhub/kloo/internal/tokens"
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
	ContextTokens int    // the MODEL'S context window — what it can hold
	// CuratorTokens caps what kloo chooses to ASSEMBLE per step (the repo map),
	// as opposed to ContextTokens, which is what the model can hold. Keeping them
	// separate is the point: a 900k-window model should stop compacting, but must
	// not thereby authorise a 252k-token repo map on every turn. 0 ⇒ fall back to
	// the usable window (the pre-split behaviour).
	CuratorTokens int
	System        string // system prompt
	// ChatSystem, when non-empty, enables the conversational gate: ONE no-tools
	// model call before the agent loop that classifies the user's message as an
	// actionable task (→ run the loop) or conversation (→ reply directly, no run).
	// Empty ⇒ disabled (headless/benchmark, where every input is a real task).
	ChatSystem  string
	Endpoint    string
	Model       string
	Temperature float64
	NoThink     bool

	// Subagents. SubagentDepth is how many levels may delegate (0 ⇒
	// DefaultMaxSubagentDepth = 1: the top level delegates, children may not).
	// SubagentLimit caps TOTAL children per run (0 ⇒ DefaultMaxSubagents).
	// EnableSubagents registers the task tool at all; without it the vocabulary is
	// unchanged, so the default path is byte-identical.
	EnableSubagents bool
	// AutoDelegate lets kloo hand work to a subagent ITSELF, without offering the
	// task tool to the model or changing the prompt. Measured on kloo-bench,
	// glimmer never calls the task tool when offered it (0 of 36 calls), so every
	// useful delegation was kloo-initiated anyway — while advertising the tool and
	// its prompt directive coincided with A28 going from a 6-step success to a
	// 29-step explore-stop and A04 from 12 to 25, both with zero delegations. With
	// AutoDelegate alone, a run that never delegates is identical to the default,
	// so any difference is attributable to delegation.
	AutoDelegate bool
	// SubagentMaxSteps caps a delegated child's steps (0 ⇒ half the parent's).
	SubagentMaxSteps int
	// MaxHandoffs caps harness-initiated delegations per run (0 ⇒ 1, the old
	// one-shot behaviour). Children are sequential and never nested.
	MaxHandoffs int
	// DelegateAfterReads hands off to a subagent once the model has made this many
	// read-only turns without an edit (0 ⇒ the legacy trigger: the first explore
	// nudge, 6 turns). Sized from kloo-bench glimmer-only runs: passing runs read a
	// median of 12 before their first edit, failing runs 26. A trigger at 6 fired on
	// 92% of runs glimmer ALREADY solves — on A06 it handed off at read 6 when
	// glimmer edits at read 9 and passes in ~200s, and the handoff then timed out.
	DelegateAfterReads int
	// OnSubagent fires as soon as a delegated child finishes, with its step count,
	// terminal reason, and the error that ended it (nil unless reason is
	// ReasonError). Written to the log immediately so it survives a hard kill: the
	// bench harness SIGTERMs kloo at its 2400s ceiling, kloo has no signal handling,
	// and KLOO_RESULT_JSON is never printed — so subagent_steps was lost on exactly
	// the timeout cases the child budget needs sizing against.
	//
	// The error is passed because reason alone is not diagnosable. On kloo-bench a
	// run of children all died at step 1 with reason=error and no further detail;
	// the cause (two concurrent arms contending for one GPU, so the routed child's
	// model could not load) had to be recovered by correlating timestamps across
	// arms. The reason says a child failed; only the error says why.
	OnSubagent    func(steps int, reason Reason, err error)
	SubagentDepth int
	SubagentLimit int
	// SubagentModel / SubagentEndpoint route delegated work to a DIFFERENT model
	// than the parent loop. Empty ⇒ the child uses the parent's.
	//
	// Measured on kloo-bench: of the 9 cases kloo loses to grok on glimmer, kloo
	// passes 5 on qwen with the identical harness (A06 A22 A29 C65 C66). The
	// harness can solve them; the model will not act. Routing the delegated subtask
	// to a model that does act is the honest use of that finding — a cheap model
	// drives the loop, a capable one does the work that needs doing.
	SubagentModel    string
	SubagentEndpoint string
	// NewSubagentClient builds the child's client when routing to another model.
	// Injected by the CLI, which owns the API key and timeout options — the agent
	// package must not have to know about credentials. Nil ⇒ routing is skipped
	// and the child shares the parent's client, so a misconfiguration degrades to
	// current behaviour instead of producing an unauthenticated child.
	NewSubagentClient func(endpoint, model string) llm.LLMClient
	// subagentDepth is THIS loop's own nesting level, set when a parent builds a
	// child. Unexported: callers configure the limit, not the position. Without
	// it a child re-registered the task tool at depth 0 on its own Run and could
	// delegate forever — the depth limit was cosmetic.
	subagentDepth int
	// subagentSpawns is the run-wide child counter, shared with children so the
	// limit counts TOTAL descendants rather than resetting per level.
	subagentSpawns *int
	Now            func() time.Time // injectable clock (defaults to time.Now)
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

	// PromptCache, when true, asks the provider to cache the stable prompt prefix
	// by marking its last message with a cache breakpoint. Resolved upstream in
	// internal/config (mode + provider allowlist); this layer only places the
	// marker and never learns a provider name.
	PromptCache bool

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
	// ExploreTotalCap bounds consecutive read-only turns regardless of novelty;
	// 0 ⇒ DefaultExploreTotalCap.
	ExploreTotalCap int

	// ctxShrinks counts overflow recoveries performed this run.
	ctxShrinks int

	// EditFailLimit is how many consecutive failed edit_file attempts (no successful
	// edit between) before the run halts as churn. 0 ⇒ DefaultEditFailLimit.
	EditFailLimit int

	// StopOn is the A7 hard-stop policy: detectable early terminations for off-scope
	// edits, read-only writes, and repeated verifier failures. The zero value (no
	// rule active) leaves all existing churn/budget behaviour unchanged.
	StopOn StopPolicy

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

	// LLMRetries is the resolved number of EXTRA model-call attempts to make after
	// the first when a call fails transiently (endpoint timeout, cold model load,
	// 5xx, dropped connection). Config resolution supplies the default; 0 disables
	// retry.
	LLMRetries int
	// RetryBaseDelay is the first backoff wait; it doubles each attempt. 0 ⇒
	// DefaultRetryBaseDelay. (Tests set it tiny to stay fast.)
	RetryBaseDelay time.Duration
	// RetryMaxDelay caps exponential retry backoff. 0 ⇒ no cap.
	RetryMaxDelay time.Duration
	// ColdStartPatience is how long to keep retrying when the endpoint explicitly
	// reports it is STARTING ("powering on", "scheduler_busy"), beyond the normal
	// retry count. 0 ⇒ DefaultColdStartPatience. A generic 503 does not qualify: a
	// server that is down, not starting, must still exhaust and fail.
	ColdStartPatience time.Duration
	// RetryableStatusCodes controls which HTTP statuses retry. nil ⇒ defaults.
	RetryableStatusCodes []int

	// Tokens is the calibrating token estimator. It learns the model's real
	// chars-per-token ratio from reported usage, so budgeting stops relying on a
	// fixed guess. nil ⇒ the package default estimate (still entropy-aware).
	Tokens *tokens.Calibrator

	// MapPosition controls where the curated repo map is placed in the prompt:
	// MapPositionTail (default, cache-friendly) or MapPositionSystem (legacy).
	MapPosition string

	// Per-run prompt-cache accounting, reset at the top of every Run (the TUI
	// reuses one Loop across submissions). Unexported: the Report is the contract.
	promptTokens       int
	cachedPromptTokens int
	// VerifyCmd is the verify command as configured, used to identify the files
	// that are the SPECIFICATION for this run (see protectedByVerify). Kept
	// separate from VerifyResult.Command because protection must work before the
	// first verify has ever run.
	VerifyCmd string
	// curEditAnchor is the text the most recent edit targeted; the file pin centres
	// its window on it (KLOO_PIN_WINDOW).
	curEditAnchor string
	// editOnlyLeft is the force-edit rail's remaining budget: while it is > 0, act()
	// advertises only the tree-changing tools AND the loop refuses any other call
	// instead of dispatching it. An edit releases it immediately; otherwise it
	// decays, so a model that will not edit can never be trapped. See forceEdit().
	editOnlyLeft int
	// lastPromptChars is the character count of the request act() just built,
	// paired with the prompt_tokens the provider reports back to calibrate the
	// estimator. Consumed (and cleared) by observeUsage, so a turn whose chars we
	// did not measure — the chat gate builds its own messages — teaches nothing
	// rather than teaching a zero.
	lastPromptChars int
	// toolCharsCache memoises the marshalled size of the tool schemas, which do
	// not change within a run.
	toolCharsCache int
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
	// DefaultLLMRetries is deliberately patient. The cost is ASYMMETRIC: a retry
	// costs seconds of backoff, while giving up discards the whole run — minutes
	// of work and every edit already made. Measured on kloo-bench, one loss was a
	// 502 that exhausted 2 retries (~6s of patience) on a gateway that recovers in
	// tens of seconds. With the 2s/30s backoff this is roughly a minute.
	DefaultLLMRetries     = 5
	DefaultRetryBaseDelay = 2 * time.Second
	// DefaultColdStartPatience bounds retrying an endpoint that says it is powering
	// on. Measured on kloo-bench: a serverless worker cold start rejected requests
	// for longer than the ~60s normal retry window, and four cases across both arms
	// of a paired run ended as errors at step 1 without running at all.
	DefaultColdStartPatience = 10 * time.Minute
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
	DefaultExploreAbortRounds = 16
	// DefaultExploreTotalCap bounds TOTAL consecutive read-only turns. 16 distinct
	// reads is normal on a real repo; 40 without a single edit is a spiral.
	DefaultExploreTotalCap = 40
	// maxContextShrinks bounds overflow recovery ACROSS the run. The per-call flag
	// alone was not enough: each rebuild is a fresh call, so it reset every time and
	// a non-converging shrink span 1536 times.
	maxContextShrinks = 4
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

// mutated reports whether the dispatched call may have CHANGED the tree, and so
// whether the verify gate owes a re-check. It is the COMPLEMENT of read-only, not
// an allowlist of edit tools: MCP-bridged tools register into the same
// tools.Registry under arbitrary server-supplied names (internal/mcp/bridge.go), so
// an allowlist would skip the regression check after a real mutation until some
// later edit_file happened to run. An unrecognised tool therefore defaults to
// MUTATING — the same conservative default the shell-command classifier in
// internal/tools/readonly_command.go already takes, and for the same reason: a miss
// costs one extra verify, a false "read-only" costs a missed regression.
func mutated(readOnlyTurn, timedOut bool, derr error) bool {
	if readOnlyTurn {
		return false
	}
	// A command that was KILLED for exceeding its timeout still RAN: run_command
	// returns the partial result alongside ErrCommandTimeout, so a half-finished
	// `npm install` or `sed -i` may already have changed the tree. derr != nil does
	// not mean "nothing landed" here.
	return derr == nil || timedOut
}

// isReadOnlyTool reports whether a tool only inspects (no mutation, no shell).
func isReadOnlyTool(name string) bool {
	return name == tools.NameReadFile || name == tools.NameListDir ||
		name == tools.NameReadDir || name == tools.NameSearch ||
		name == tools.NameCommandOutput
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

// exploreTotalCap bounds consecutive read-only turns however varied they are.
// Sized for a real repo: kloo-bench cases need 20-30 reads to locate a one-file
// change, so the cap must clear that comfortably while still stopping a spiral.
func (l *Loop) exploreTotalCap() int {
	if l.ExploreTotalCap > 0 {
		return l.ExploreTotalCap
	}
	return DefaultExploreTotalCap
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

// smartChurn reports whether malformation-aware churn is on (KLOO_SMART_CHURN=1).
// A repeated CORRECTABLE edit failure (malformed block, unmatched SEARCH,
// edit-on-missing) is the model trying to fix its FORMAT/tool-use, not repeating a
// wrong APPROACH — so it should be bounded by the per-target repair budget
// (repairLimit) via a corrective, NOT counted toward the no-progress churn rail
// that halts at N. Off ⇒ stock behavior (correctable failures still churn at N).
func smartChurn() bool {
	v := os.Getenv("KLOO_SMART_CHURN")
	return v != "" && v != "0"
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
	l.promptTokens, l.cachedPromptTokens, l.lastPromptChars = 0, 0, 0
	l.toolCharsCache = 0

	// Subagents: register the delegation tool for THIS run. Opt-in, so a loop that
	// does not enable it has a byte-identical vocabulary to before. spawned is
	// shared by every taskTool in the run so the cap counts total children, not
	// children per call site.
	if l.EnableSubagents && l.Registry != nil && l.subagentDepth < l.maxSubagentDepth() {
		if l.subagentSpawns == nil {
			n := 0
			l.subagentSpawns = &n
		}
		l.Registry.Register(taskTool{parent: l, depth: l.subagentDepth, spawns: l.subagentSpawns})
	}

	convo := []llm.Message{{Role: llm.RoleUser, Content: task}}
	// One-time, at step 0: show the test this run is graded against. See
	// failingTestSource — the brief names the test but never shows it, which is
	// circular when the required behaviour IS the bug.
	if td := l.Registry.Todos(); td != nil {
		_ = td // the list is pinned per-turn below; nothing to seed at step 0
	}
	if ts := l.failingTestSource(); ts != "" {
		convo = append(convo, llm.Message{Role: llm.RoleUser, Content: ts})
	}
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
		// for this streak has been emitted (the latch governs the MUTATING path only —
		// a downgraded read-only repeat re-nudges instead of latching). Catches a model
		// locked onto a single identical call (e.g. re-reading one empty file) — a
		// read-only spin the edit/verify churn rails cannot see.
		// mutatedSinceVerify gates the verify step: true once a dispatched call may
		// have changed the tree, cleared only when a verify actually runs. See the
		// `mutated` predicate and the gate in the VERIFY block.
		mutatedSinceVerify bool

		repeatKeyLast string
		repeatStreak  int
		repeatNudged  bool
		editSigLast   string

		// Exploration rail state: consecutive read-only turns (read_file/list_dir) with
		// no edit/run_command, and whether the one-shot nudge fired. Catches a weak
		// model that inspects+analyzes forever without acting.
		exploreStreak int
		// The turn count at which the nudge last fired, so it re-arms at every
		// multiple instead of once per run: one nudge at turn 6 is easy to ignore.
		exploreNudgedAt int
		// exploreNudges counts no-edit explore nudges this run, so the force-edit
		// rail can hold back on the first one.
		exploreNudges int
		// emptyTurns counts empty completions this run has already recovered from.
		emptyTurns int
		// Read targets already visited this run. Revisiting one is not new ground.
		seenTargets  = map[string]bool{}
		exploreTotal int

		// Failed-edit rail state: consecutive edit_file attempts that FAILED to apply
		// (reset by a successful edit). Catches the edit↔read flail no other rail sees.
		editFailStreak int

		// A7 safety-stop state. safetyEv/lastScopeDenial are copied onto the Report at
		// finish; verifyFailStreak/verifyFailKey track consecutive identical verifier
		// failures for the repeated-verify stop rule (independent of the churn rail).
		safetyEv         *SafetyEvidence
		lastScopeDenial  *ScopeDenial
		patchOnlyReject  *ToolReject
		verifyFailStreak int
		verifyFailKey    string

		// Promised-but-didn't-act rail state: how many times the model ended a turn by
		// NARRATING a next action ("let me run X") with no tool call — OR gave up in prose
		// right after a FAILING action (lastActionFailed). Reset only by a SUCCESSFUL
		// action, so a model that keeps failing-then-explaining is bounded. Catches a
		// model that talks/explains instead of acting, which the answered-stop would
		// otherwise accept as a finished reply (e.g. "a wrong command stops the run").
		promiseNudges    int
		lastActionFailed bool

		// Confirm-finish rail state. everActed records whether the model ran a REAL
		// action (run_command / edit / write) this run; confirmFinishNudged makes the
		// nudge one-shot. Together they catch the premature `answered` stop where a model
		// doing multi-step EXECUTION work (e.g. a deploy) completes one step then stops
		// with a bare prose "done" instead of calling finish — seen live with dsv4
		// (registered a version, then stopped before upgrading the instance).
		everActed           bool
		confirmFinishNudged bool

		// railFires tallies each SOFT rail that injected a corrective this run, copied
		// into Report.RailFires at finish (nil when empty). Makes self-corrections
		// observable in the summary/JSON. recordRail bumps a name's count.
		railFires = map[string]int{}
		// finishSummary is what the model passed to finish, surfaced on the Report so
		// a parent agent can read a delegated child's result without its transcript.
		finishSummary string
		// autoDelegated makes the investigator one-shot per run.
		// handoffs counts harness-initiated delegations this run. The limit was one
		// per run by choice, never measured. The nesting bug accidentally showed the
		// alternative: C17 passed with ~65 child steps spread over three children and
		// fails with a single 25-step child. Sequential handoffs are the legitimate
		// form of that — each child fresh, bounded, and never nested.
		handoffs int
		// readsSinceEdit counts read-only turns since the last successful edit; unlike
		// exploreTotal it is NOT reset by running a command.
		readsSinceEdit int
		counters       ToolCounters
	)
	recordRail := func(r Rail) { railFires[string(r)]++ }

	// finish builds the report and rolls back on any non-success terminal path.
	finish := func(reason Reason, runErr error, be *BudgetEvidence, ce *ChurnEvidence) (*Report, error) {
		l.onState(StateStop)
		l.Registry.StopBackground() // kill any background servers this run started (no leaks across runs)
		st := l.Budget.Stats()
		compactions := 0
		if l.Memory != nil {
			compactions = l.Memory.Stats().Compactions
		}
		rep := &Report{
			Reason:             reason,
			Steps:              step,
			FinalVerify:        lastVerify,
			Budget:             be,
			Churn:              ce,
			Err:                runErr,
			TokensUsed:         st.Tokens,
			PromptTokens:       l.promptTokens,
			CachedPromptTokens: l.cachedPromptTokens,
			TokenRatio:         l.tokenRatio(),
			Elapsed:            st.Elapsed,
			Compactions:        compactions,
			Ignored:            ignoredAll,
			Transcript:         append([]llm.Message(nil), convo...), // this run's task + steps, for the session
			ToolCounters:       counters,
			Summary:            finishSummary,
		}
		if len(railFires) > 0 {
			rep.RailFires = railFires
		}
		rep.Safety = safetyEv
		rep.LastScopeDenial = lastScopeDenial
		rep.PatchOnlyReject = patchOnlyReject
		if l.shouldRollback(reason, lastVerify) && snap.Taken && l.Checkpoint != nil {
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
		reply, conversational, usage, gateErr := l.chatGate(ctx, task)
		l.observeUsage(usage)
		if gateErr != nil {
			return finish(ReasonError, gateErr, nil, nil)
		}
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
			// The prompt overflowed the server's context limit and the window has been
			// reduced to the figure the SERVER reported. Rebuild this step under the new
			// budget instead of ending the run: the old request can never succeed, but
			// the shrunk one usually does. Does not consume a step — no work happened.
			if errors.Is(err, errContextShrunk) {
				l.observeUsage(usage)
				step-- // no work happened; a forced rebuild must not eat the step budget
				continue
			}
			// EXHAUSTED EMPTY TURNS. The retry classifier already treats an empty
			// completion as a hiccup and retries it; when those retries are spent, the
			// run used to die as internal_error. Measured on kloo-bench C62: that threw
			// away 14 good steps AND rolled back a legitimate source edit with 45 steps
			// still on the budget, on an endpoint that answered normally minutes later.
			// One blank response from a local model is not a reason to lose the work.
			//
			// Bounded, because a model that returns nothing FOREVER must still stop:
			// after emptyTurnRecoveries the error is fatal exactly as before.
			if errors.Is(err, ErrNoUsableContent) && emptyTurnRecovery() && emptyTurns < maxEmptyTurnRecoveries {
				emptyTurns++
				l.observeUsage(usage)
				convo = append(convo, llm.Message{Role: llm.RoleUser, Content: "Your last turn came back empty — " +
					"no tool call and no message. That is a transport hiccup, not a problem with the task. " +
					"Continue from where you were and emit your next tool call."})
				step-- // nothing happened; a blank turn must not eat the step budget
				continue
			}
			if errors.Is(err, ErrNoToolCall) {
				l.observeUsage(usage)
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
					recordRail(RailPromiseToAct)
					convo = append(convo, msg, promiseToActCorrective(lastActionFailed))
					continue
				}
				// Confirm-finish rail: the model has called tools this run but stops with
				// a bare prose turn — no tool call, no finish, and not an already-green
				// edit (lastVerify.Passed && edited, which would have ended as success).
				// Previously gated on everActed (edit/write/run_command only), but a model
				// that reads task files and then narrates findings without making edits hits
				// the same premature-stop pattern — seen live with dsv4-flash on coding
				// tasks (reads 4 files, reports "task 01 is done, task 02 needs work",
				// stops without calling edit_file). Extend to any tool call (step > 1) so
				// the one-shot nudge fires whenever the model has seen tool results but
				// stops in prose. The one-shot flag means a run that genuinely has nothing
				// left still stops calmly on the very next bare turn.
				if (everActed || step > 1) && !confirmFinishNudged && !(lastVerify.Passed && edited) {
					confirmFinishNudged = true
					recordRail(RailConfirmFinish)
					convo = append(convo, msg, confirmFinishCorrective())
					continue
				}
				// Conversational reply (prose, no tool call): the answer is already
				// streamed to the transcript — stop calmly rather than error/churn.
				return finish(ReasonAnswered, nil, nil, nil)
			}
			if errors.Is(err, tools.ErrMalformedToolCall) || strings.Contains(err.Error(), "no usable tool call") {
				counters.InvalidToolCalls++
			}
			return finish(ReasonError, err, nil, nil)
		}
		l.observeUsage(usage)
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
			finishSummary = str(call.Args["summary"])
			convo = append(convo, observation(call, tools.Result{Output: finishSummary}, nil))
			if l.Verifier == nil {
				// Unverified mode: no command to prove the change works. Honour finish
				// as a calm terminal stop, but label it UNVERIFIED — distinct from
				// success, which always requires a real green verify.
				return finish(ReasonUnverified, nil, nil, nil)
			}
			// UNCONDITIONAL, and it must stay that way: this is the one verify that
			// decides success, so it may never be gated — not on the verify step's
			// mutation flag, not on anything. That step skips turns that could not have
			// changed the tree; that is only safe because the tree is re-checked here
			// before any run is called successful. Adding a gate would let a stale
			// green become a false pass, and an out-of-band mutation (a run_command
			// that breaks the build after the last edit) would go unnoticed.
			// TestFinishVerifiesEvenWithNoMutation and TestNoSuccessOnStaleGreen fail
			// if this is ever made conditional.
			lastVerify = l.Verifier.Verify(ctx)
			counters.VerifyAttempts++
			if lastVerify.Err == nil && lastVerify.Passed {
				return finish(ReasonSuccess, nil, nil, nil)
			}
			return finish(ReasonAnswered, nil, nil, nil)
		}

		// Track the file under edit so next turn re-reads it fresh for the pin
		// (working memory) instead of trusting the stale transcript copy.
		if isEditTool(call.Name) {
			curEditPath = str(call.Args["path"])
			// Remember WHAT was edited, not just where: the pin window centres on it
			// so a long file's pin follows the model to the region it is working in.
			l.curEditAnchor = editAnchorOf(call)
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
			result   tools.Result
			derr     error
			before   string
			beforeOK bool
			noOpEdit bool // the edit applied cleanly but the file is byte-identical
		)
		if isEditTool(call.Name) {
			before, beforeOK = l.currentFileContents(str(call.Args["path"]))
		}
		switch {
		case isEditTool(call.Name) && l.protectedByVerify(str(call.Args["path"])) ||
			isEditTool(call.Name) && l.protectedByVerify(str(call.Args["file_path"])):
			// The files the verify command NAMES are the specification the work is
			// checked against. Editing them is how an agent "passes" by moving the
			// goalposts. Measured on kloo-bench C30: the model edited
			// undertime-overbreak-grace.test.ts three times (every one a no-op) and
			// never touched a source file, and the run churned to a repetition halt.
			// The force-edit rail makes this MORE likely, not less — told to edit
			// something, a stuck model reaches for the file it has most recently read,
			// which is the failing test.
			derr = errProtectedPath
		case l.editOnlyLeft > 0 && !isEditTool(call.Name) &&
			!(call.Name == tools.NameFinish && lastVerify.Passed) &&
			!coversNewGround(call, seenTargets):
			// NOT refused when the call covers NEW GROUND. kloo learned this the hard
			// way in v0.17.1: an over-eager explore rail stopped runs at 16 read-only
			// steps with nothing written, and reading a file not yet seen is
			// exploration working, not spinning (TestExploreRailAllowsManyDistinctReads
			// pins it). The spin worth refusing is RE-reading what the model has
			// already seen, which is what every trace behind this rail actually shows:
			// C66 re-read one file until a rail killed the run; A05 logged 10 repeated
			// reads.
			//
			// FORCE-EDIT RAIL. Withheld tools stay dispatchable so a stray call is
			// never an unrecoverable unknown-tool error — but "dispatchable" must not
			// mean "executed", or the restriction has no teeth at all. Measured on
			// kloo-bench C66: the narrowed tool list alone changed nothing, because
			// the model kept emitting read_file from the vocabulary it had already
			// seen and the loop kept running it. Refuse, say why, and hold.
			derr = errEditOnlyTurn
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
		if derr != nil {
			counters.ToolErrors++
			if errors.Is(derr, tools.ErrUnknownTool) || errors.Is(derr, tools.ErrInvalidArgs) {
				counters.InvalidToolCalls++
			}
		}

		// A7 scope observation + hard stops. A denied edit/write, or a scoped
		// run_command rejection, surfaces as a *tools.ScopeError (wrapping ErrOffScope).
		// Count it (B3), record the latest denial for the JSON classifier, and — when a
		// matching --stop-on rule is configured — terminalize IMMEDIATELY (before the
		// observation is even fed back), with no file mutation having occurred.
		var scopeErr *tools.ScopeError
		if errors.As(derr, &scopeErr) {
			counters.OffScopeEdits++
			if scopeErr.Class == tools.ScopeClassReadOnly {
				counters.ReadOnlyEdits++
			}
			lastScopeDenial = &ScopeDenial{
				Class:   scopeErr.Class,
				Tool:    scopeErr.Tool,
				Path:    scopeErr.Path,
				Rule:    scopeErr.Rule,
				Message: scopeErr.Message,
			}
			readOnlyHit := scopeErr.Class == tools.ScopeClassReadOnly
			if l.StopOn.OffScopeEdit || (l.StopOn.ReadOnlyEdit && readOnlyHit) {
				rule := "off-scope-edit"
				if readOnlyHit && l.StopOn.ReadOnlyEdit && !l.StopOn.OffScopeEdit {
					rule = "read-only-edit"
				}
				safetyEv = &SafetyEvidence{
					Rule:    rule,
					Class:   scopeErr.Class,
					Tool:    scopeErr.Tool,
					Path:    scopeErr.Path,
					Message: scopeErr.Message,
				}
				convo = append(convo, observation(call, result, derr))
				return finish(ReasonSafetyStop, nil, nil, nil)
			}
		}

		// A4 patch-only rejection: a model-facing run_command withheld by patch-only
		// mode (no scope policy) surfaces as ErrPatchOnlyForbidden. It IS an invalid
		// tool call for this run mode, so count it and record it for the JSON
		// classifier (a run that ends calmly afterwards reports tool_call_invalid /
		// class patch_only_forbidden_tool). The run continues — patch-only has no hard
		// stop of its own.
		if errors.Is(derr, tools.ErrPatchOnlyForbidden) {
			counters.InvalidToolCalls++
			patchOnlyReject = &ToolReject{Tool: call.Name, Class: patchOnlyForbiddenClass, Message: derr.Error()}
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

		// A real action (command / edit / write — anything but a read) dispatched
		// without a tool-level error counts as EXECUTION work, arming the confirm-finish
		// rail: a run that has acted may have unfinished steps, so a later bare prose
		// stop must be challenged rather than accepted as a calm answer.
		if !isReadOnlyTool(call.Name) && derr == nil {
			everActed = true
		}

		if isEditTool(call.Name) {
			if derr == nil {
				edited = true // a real change landed this run
				editFailStreak = 0
				if beforeOK {
					if after, ok := l.currentFileContents(str(call.Args["path"])); ok && after == before {
						counters.NoOpEdits++
						noOpEdit = true
					}
				}
			} else {
				// An edit that FAILED to apply (malformed block, no-match, rejected).
				// Tracked across turns — reads in between don't reset it — so a model
				// that alternates failing-edit ↔ read without ever landing a valid edit
				// (the churn-flail-gap) is caught even though no single rail's signal
				// (identical call / read-only spin / repeated verify) fires.
				editFailStreak++
				counters.FailedEdits++
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
		correctableEdit := false // #2: this turn's edit failed a CORRECTABLE way (got a corrective within the repair budget)
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
				correctableEdit = true
			case errors.Is(derr, os.ErrNotExist) && repairAttempts[path] < l.repairLimit():
				// edit_file on a file that doesn't exist yet: a correctable tool-use slip
				// — bound it by the repair budget (repairLimit), not the churn rail.
				repairAttempts[path]++
				correctableEdit = true
			case isRepairableEditFailure(derr) && repairAttempts[path] < l.repairLimit():
				if rep, okRep := buildRepairObservation(l.Root, path, str(call.Args["diff"])); okRep {
					obs = rep
					repairAttempts[path]++
				}
				correctableEdit = true
			}
		}
		if l.editOnlyLeft > 0 {
			// Only a SUCCESSFUL edit satisfies the rail. Releasing on the call alone
			// would let a refused or failed edit buy the model its read tools back,
			// which is the behaviour the rail exists to prevent.
			if isEditTool(call.Name) && derr == nil {
				l.editOnlyLeft = 0
			} else {
				l.editOnlyLeft--
			}
		}
		if w := l.subsetTestWarning(call, result, derr); w != "" {
			convo = append(convo, obs)
			obs = llm.Message{Role: llm.RoleUser, Content: w}
		}
		// A NO-OP EDIT REPORTED AS SUCCESS is a trap. kloo counted these and said
		// nothing: the tool returned "edited <path>" while the file was unchanged, so
		// the model believed its fix had landed, saw the test still failing, concluded
		// the cause was elsewhere, and repeated the same edit until the churn rail
		// killed the run.
		//
		// Measured on kloo-bench A16 across 19 runs: EVERY failure had
		// repeated_edits=2 with no_op_edits 3-4 and ended in churn; NO passing run had
		// either counter. That is the whole difference between kloo's 4/8 and grok's
		// 8/8 on that case.
		if noOpEdit && noOpFeedback() {
			obs = llm.Message{Role: llm.RoleUser, Content: "That edit applied but changed NOTHING — the file is " +
				"byte-for-byte identical to before. Your replacement text must already match what was there. " +
				"Do not repeat it. Re-read the region you are targeting and make a DIFFERENT change, or edit a " +
				"different file: the behaviour you are trying to alter is not controlled by the text you just " +
				"replaced."}
		}
		if errors.Is(derr, errProtectedPath) {
			target := str(call.Args["path"])
			if target == "" {
				target = str(call.Args["file_path"])
			}
			msg := "That file is part of the verification command — it is the specification your work is " +
				"checked against, not something to change. Editing it cannot make the task correct. " +
				"Change the SOURCE code the test exercises instead."
			// Name the files that test actually imports. Measured on kloo-bench A33:
			// the model tried to edit the statutory test — so it had located the right
			// AREA — then spent the rest of the run editing payroll files while the red
			// assertion was about statutory dedup. It knew where the problem was and
			// could not find the code behind it. A refusal that only says "no" leaves
			// it exactly where it was.
			if imps := l.importsOf(target); len(imps) > 0 {
				msg += " That test imports these, and the fix is most likely in one of them:\n  - " +
					strings.Join(imps, "\n  - ")
			}
			obs = llm.Message{Role: llm.RoleUser, Content: msg}
		}
		if errors.Is(derr, errEditOnlyTurn) {
			obs = llm.Message{Role: llm.RoleUser, Content: "That tool is unavailable on this turn. " +
				"You have read enough and the code still does not pass its test: the only thing that can " +
				"move this task forward now is a change to the source. Call the edit tool with your best " +
				"attempt at the fix. If it is wrong, the test will say so and you can revise it."}
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

		// A run_command that only INSPECTS (go test, git diff, ls) is exploration,
		// not action: re-running the suite after an edit is the most natural move an
		// agent makes, and counting it as a no-progress round killed real runs. The
		// classifier is conservative — anything unrecognised stays acting.
		//
		// Computed HERE, above the verify gate, because both the gate and the churn
		// feed below need it. It is a pure function of the call, so its position
		// changes nothing — and there is exactly one definition of it.
		readOnlyTurn := isReadOnlyTool(call.Name) ||
			(call.Name == tools.NameRunCommand && tools.IsReadOnlyCommand(str(call.Args["command"])))
		// Sticky, not a bare `mutatedSinceVerify = mutated(...)`: the flag means "a
		// mutation has happened since the last verify", and only the verify step may
		// clear it. A plain assignment would clear a pending mutation on the next
		// read-only turn, and the mutation would never be checked.
		if mutated(readOnlyTurn, result.TimedOut, derr) {
			mutatedSinceVerify = true
		}

		// ── VERIFY ──────────────────────────────────────────────────────────
		// Unverified mode (nil Verifier) skips this entirely: lastVerify stays the
		// zero value (Passed=false), so the success gate below never fires and the
		// run can only end via finish (→ unverified), churn, budget, or answered.
		//
		// GATED on a mutation since the last verify. Measurement (KLOO-VS-GROK.md,
		// 2026-09-11): verify_attempts >= steps in 78 of 83 runs — 1,023 full vitest
		// invocations across 83 cases, re-running the suite after steps that only
		// READ and could not have changed the result. That is the wall-clock tax, and
		// six concurrent lanes of it exhausted swap with 154 workerd processes.
		//
		// Skipping is safe because it can never become a false pass: `finish` verifies
		// UNCONDITIONALLY (see the finish branch), and the mid-loop success gate needs
		// `edited`, which always sets mutatedSinceVerify — so the green that authorises
		// success was always produced after the last mutation.
		verifySkipped := l.Verifier != nil && !mutatedSinceVerify
		if l.Verifier != nil && mutatedSinceVerify {
			l.onState(StateVerify)
			lastVerify = l.Verifier.Verify(ctx)
			counters.VerifyAttempts++
			mutatedSinceVerify = false

			// A non-runnable verify command is an error outcome, never a false pass.
			if lastVerify.Err != nil {
				if ctx.Err() != nil {
					return finish(ReasonInterrupted, nil, nil, nil)
				}
				return finish(ReasonError, fmt.Errorf("verify: %w", lastVerify.Err), nil, nil)
			}

			// BUILD BREAK. A test that ran and failed is information; a build that no
			// longer compiles is damage, it hides every other signal, and on kloo-bench
			// A05 the model never repaired it — it read sixteen more times and stopped.
			// Say so in plain terms, quote the error, and arm the force-edit rail NOW
			// rather than after another six read-only turns.
			if buildBreakGuard() && !lastVerify.Passed {
				if broken, detail := buildBreak(failingOutput(lastVerify)); broken {
					convo = append(convo, llm.Message{Role: llm.RoleUser, Content: "YOUR LAST EDIT BROKE THE BUILD. " +
						"This is not a failing test — the file no longer compiles, so nothing can run at all:\n" + detail +
						"\nFix THIS first, in the file you just edited, before anything else. If you added code that " +
						"already existed, remove the duplicate you introduced rather than adding more."})
					if forceEdit() {
						l.editOnlyLeft = editOnlyBudget
					}
				}
			}

			// A7 repeated-verify stop: count CONSECUTIVE identical verifier failures
			// (normalised, so volatile bits don't defeat the match). A pass or a
			// different failure resets the streak. When --stop-on repeated-verify=N is
			// set and the streak reaches N, halt early rather than churning to the step
			// budget. Independent of the churn rail (which stays the default backstop).
			if l.StopOn.RepeatedVerify > 0 {
				if lastVerify.Passed {
					verifyFailStreak, verifyFailKey = 0, ""
				} else if key := normalizeChurn(failingOutput(lastVerify)); key != "" && key == verifyFailKey {
					verifyFailStreak++
				} else {
					verifyFailStreak, verifyFailKey = 1, key
				}
				if verifyFailStreak >= l.StopOn.RepeatedVerify {
					safetyEv = &SafetyEvidence{
						Rule:    "repeated-verify",
						Class:   "repeated_verify_failure",
						Message: fmt.Sprintf("verify %q failed %d times in a row with no progress", lastVerify.Command, verifyFailStreak),
					}
					return finish(ReasonSafetyStop, nil, nil, nil)
				}
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
		//
		// A SKIPPED verify feeds nothing either, for the mirror-image reason: the
		// only output available is the PREVIOUS turn's, and re-feeding it would count
		// a failure the model was never re-shown, advancing the repeated-failure rail
		// on turns that produced no new evidence. VerifySkipped carries the turn
		// instead, and churn.Observe handles it as neutral — see types.Turn.
		verifyOut := ""
		if l.Verifier != nil && !verifySkipped {
			verifyOut = failingOutput(lastVerify)
		}
		// #2 malformation-aware churn: a CORRECTABLE edit failure (got a corrective
		// within the repair budget) is not "repeating a wrong approach" — don't feed
		// its signature to the churn rail, so repairLimit governs it instead of the
		// rail halting at N before the corrective takes. Off ⇒ stock (still churns).
		editSig := editSignature(call)
		if smartChurn() && correctableEdit {
			editSig = ""
		}
		l.Churn.Observe(Turn{
			VerifyOutput:  verifyOut,
			Edit:          editSig,
			Acted:         call.Name == tools.NameRunCommand && derr == nil && !readOnlyTurn,
			ReadOnly:      readOnlyTurn,
			VerifySkipped: verifySkipped,
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
		// track the call's normalised (name + args) signature. A distinct call resets
		// the streak, so a progressing run — which never fires the same call twice
		// running — is immune.
		//
		// What happens at the thresholds depends on whether the repeated call MUTATES:
		//
		//   - A MUTATING call (edit_file/write_file/run_command) is nudged once at
		//     repeatNudgeRounds and ENDS the run as churn at repeatAbortRounds. Firing
		//     the same mutation forever is not exploration, and there is nothing to
		//     recover toward.
		//   - A READ-ONLY call is nudged AGAIN at every multiple of repeatNudgeRounds
		//     and never ends the run here. Measurement (KLOO-VS-GROK.md, 2026-09-11)
		//     found 11 of the 16 solvable-but-lost runs died on this abort while
		//     re-reading their way toward a fix: it was cutting off recovery, not
		//     stopping a stuck model. A read spin that never recovers is still
		//     terminated — by the explore rail just below (consecutive read-only turns
		//     to DefaultExploreAbortRounds), the stall backstop and the budget — only
		//     later, and after more chances to break out.
		//
		// counters.RepeatedReadFile counts every repeat either way: it is the
		// benchmark's diagnostic signal, and it no longer zeroes the run.
		if key := repeatKey(call); key != "" {
			if key == repeatKeyLast {
				repeatStreak++
				if call.Name == tools.NameReadFile {
					counters.RepeatedReadFile++
				}
			} else {
				repeatKeyLast, repeatStreak, repeatNudged = key, 1, false
			}
			nudgeEvery := l.repeatNudgeRounds()
			switch {
			case isReadOnlyTool(call.Name):
				// Downgraded: no abort arm at all. Re-armed rather than one-shot, so the
				// model gets a fresh push every nudgeEvery rounds instead of silence all
				// the way to the explore ceiling.
				if repeatStreak >= nudgeEvery && repeatStreak%nudgeEvery == 0 {
					recordRail(RailRepeatedCall)
					convo = append(convo, l.repeatCorrective(call, repeatStreak))
				}
			case repeatStreak >= l.repeatAbortRounds():
				return finish(ReasonChurn, nil, nil, &ChurnEvidence{
					Kind:     ChurnRepeatedCall,
					Artifact: repeatArtifact(call, repeatStreak),
				})
			case repeatStreak >= nudgeEvery && !repeatNudged:
				repeatNudged = true
				recordRail(RailRepeatedCall)
				convo = append(convo, l.repeatCorrective(call, repeatStreak))
			}
		}
		if sig := editSignature(call); sig != "" {
			if sig == editSigLast {
				counters.RepeatedEdits++
			}
			editSigLast = sig
		}

		// Exploration rail: a weak model (e.g. a 2B ollama model) inspects file after
		// file, narrating analysis and asking the user questions, but never edits — and
		// because it reads a DIFFERENT file each turn (not repetition), with no verify
		// change (not stall) and no edit/failure (not churn), nothing else stops it; it
		// spins to the step ceiling.
		//
		// The streak counts read-only turns that reveal NOTHING NEW. Reading a file
		// this run has not read before is exploration doing its job: on a real repo a
		// competent model reads many files to locate a one-file change, and a raw
		// count of read-only turns cannot tell that from a 2B model narrating in a
		// loop. Measured on kloo-bench (22 real-commit cases, median ONE source file
		// edited per case): every v0.17.1 failure was this rail stopping a run at 16
		// read-only steps having written nothing.
		//
		// A repeat of something already seen still climbs the streak, so the weak-model
		// spin the rail was built for is still caught.
		if isEditTool(call.Name) && derr == nil {
			readsSinceEdit = 0
		}
		if readOnlyTurn {
			readsSinceEdit++
			exploreTotal++ // read-only turns since the last action, new ground or not
			sig := exploreSignature(call)
			if sig != "" && !seenTargets[sig] {
				seenTargets[sig] = true
				exploreStreak = 0 // new ground: this turn made progress
			} else {
				exploreStreak++
			}
		} else {
			exploreStreak, exploreTotal, exploreNudgedAt = 0, 0, 0
		}
		if l.DelegateAfterReads > 0 && l.canAutoDelegate() && handoffs < l.maxHandoffs() &&
			!edited && readsSinceEdit >= l.DelegateAfterReads {
			handoffs++
			msg, childSteps, ok := l.autoDelegate(ctx, task)
			counters.SubagentSteps += childSteps
			if ok {
				counters.AutoDelegations++
				convo = append(convo, msg)
				// Reset the read counters so a SECOND handoff needs another full N
				// read-only turns: the limit is a ceiling, not a schedule.
				readsSinceEdit = 0
				exploreStreak, exploreTotal, exploreNudgedAt = 0, 0, 0
				// The child edits the SAME tree, but its writes never pass through the
				// parent's dispatch, so mutated() never sees them and the verify gate
				// stays shut. The parent then cannot know the child's work is wrong.
				//
				// Measured on kloo-bench C07: the child edited the correct file, ended
				// in churn leaving 2 of 6 tests failing, and the parent — with
				// verify_attempts=1 for the whole 38-step run — read 15 more files and
				// was stopped by the explore rail. It never once ran the tests over the
				// child's edit. The rescue handoff already sets this; the read-threshold
				// handoff is the path production actually uses, and it did not.
				mutatedSinceVerify = true
			}
		}
		// RESCUE HANDOFF (KLOO_DELEGATE_ON_STOP). The explore rail is about to end this
		// run as a failure; hand the work to a subagent first.
		//
		// Measured on kloo-bench C66: glimmer made an early edit that did not fix the
		// case, then read 16 more files until the rail stopped it. The read-count
		// trigger never fired, because it requires "no edit yet" — yet qwen alone
		// PASSES C66. The rail is the last point at which a handoff can still help,
		// and firing only here cannot affect a run that would have succeeded.
		//
		// After the child, the tree may have changed under the parent, so it must
		// VERIFY. This is not optional: a run that has edited has a checkpoint, and
		// kloo rolls back to it on any non-success exit. Without the re-verify, a
		// child that fixed the case would be undone the moment the parent was stopped
		// again — the handoff would look like it fired and do nothing.
		if delegateOnStop() && l.canAutoDelegate() && handoffs < l.maxHandoffs() &&
			(exploreTotal >= l.exploreTotalCap() || exploreStreak >= l.exploreAbortRounds()) {
			handoffs++
			msg, childSteps, ok := l.autoDelegate(ctx, task)
			counters.SubagentSteps += childSteps
			if ok {
				counters.AutoDelegations++
				counters.RescueDelegations++
				convo = append(convo, msg)
				exploreStreak, exploreTotal, exploreNudgedAt = 0, 0, 0
				mutatedSinceVerify = true
				continue
			}
		}
		switch {
		// A CEILING on total consecutive read-only turns, independent of whether each
		// covers new ground. Measured on kloo-bench case C12: a model issued 42
		// DISTINCT searches and 9 other reads without a single edit, burning 1.2M
		// tokens to the step budget — every query was new ground, so the no-new-ground
		// streak alone never fired. Distinctness proves a turn is not a REPEAT; it does
		// not prove the run is converging on a change.
		case exploreTotal >= l.exploreTotalCap():
			return finish(ReasonExploreStop, nil, nil, nil)
		case exploreStreak >= l.exploreAbortRounds():
			// ReasonExploreStop, not ReasonAnswered: a run a RAIL killed with no edits
			// is not the model answering a question, and reporting it as "answered"
			// made a stopped run indistinguishable from a clean one in every report
			// and benchmark that reads the reason.
			return finish(ReasonExploreStop, nil, nil, nil)
		// The NUDGE keys on TOTAL read-only turns, not the no-new-ground streak, and
		// re-arms. It only appends a message, so firing it early and repeatedly is
		// cheap — and it is what converts reading into an edit attempt. Measured on
		// kloo-bench C30: with the nudge tied to the no-new-ground streak a model
		// reading distinct files was NEVER nudged and made 0 edits, where the same
		// case under the old rule was nudged and made 2. Nudge eagerly; abort
		// reluctantly. They are not the same decision and must not share a counter.
		case exploreTotal > 0 && exploreTotal%l.exploreNudgeRounds() == 0 && exploreNudgedAt != exploreTotal:
			exploreNudgedAt = exploreTotal
			recordRail(RailExplore)
			// AUTO-DELEGATION. On the FIRST explore nudge, kloo delegates the
			// investigation itself instead of asking the model to stop reading.
			//
			// The model is offered the task tool and does not use it: measured on
			// kloo-bench, glimmer lists `task` among its own tools and called it 0
			// times across 36 calls on a case it then lost to explore-stop. That
			// matches every other result on this model — it does not adopt a better
			// strategy when offered one, or when told to. So the harness drives the
			// decomposition rather than suggesting it.
			//
			// One-shot: a second investigation would be the same spin one level down.
			// The gate is !everActed by default. Measured on kloo-bench C66, that is
			// too strict: the model ran the failing test early (a healthy move), which
			// set everActed and disabled delegation for the rest of the run; it then
			// spun 35 steps. With KLOO_DELEGATE_UNTIL_EDIT the gate is "no edit yet",
			// so running a command no longer forfeits the handoff.
			if l.DelegateAfterReads == 0 && l.canAutoDelegate() && handoffs < l.maxHandoffs() && !delegationBlocked(everActed, edited, delegateUntilEdit()) {
				handoffs++
				msg, childSteps, ok := l.autoDelegate(ctx, task)
				counters.SubagentSteps += childSteps
				if ok {
					counters.AutoDelegations++
					convo = append(convo, msg)
					break
				}
			}
			if editRail() {
				// NOT gated on "the run has never edited". Measured on kloo-bench C66:
				// gating on that let a run make ONE early edit (2 failing tests -> 1),
				// which disarmed the rail for the rest of the run, and the model then
				// read 16 more times and was stopped with the case still red. Every
				// counter that drives this nudge resets on an edit, so a nudge firing
				// at all already means a long read-only streak with nothing changed —
				// which is the condition the corrective is for, first edit or tenth.
				//
				// Arm the vocabulary restriction only from the SECOND nudge of the
				// streak: the first is a fair warning, and a model that acts on it
				// should never see a narrowed tool list.
				// Armed from the run's SECOND no-edit nudge, counted across the whole
				// run rather than the current streak. Measured on kloo-bench C66: with
				// a per-streak counter a run was nudged, edited, then read 21 more
				// times in a fresh streak whose nudges restarted at one — so the rail
				// never armed and the run was stopped with the file broken. A model
				// that has already ignored one nudge this run does not get another
				// free pass.
				if forceEdit() && exploreNudges > 0 {
					l.editOnlyLeft = editOnlyBudget
				}
				exploreNudges++
				convo = append(convo, editCorrective(exploreStreak, edited,
					failingAssertions(failingOutput(lastVerify)), l.verifyTestImports()))
			} else {
				convo = append(convo, exploreCorrective(exploreStreak))
			}
		}

		// Stall backstop: a no-progress counter, ORTHOGONAL to MaxSteps. It engages
		// ONLY when verify is PASSING — a green check with no edit and no tree change
		// for stallLimit consecutive turns means the model is spinning on redundant
		// read-only commands (re-confirming a done state with echo/ls) instead of
		// calling finish. A FAILING verify is deliberately left to churn + budget, so
		// a legitimate read-heavy run toward a fix (read many files, THEN edit) is
		// never cut off. Resets to 0 on any progress, so it fires at a small N far
		// below the step budget — the two ceilings never overlap.
		//
		// Pre-action exploration is NOT a confirming-spin: a model that reads several
		// files BEFORE making its first edit is doing legitimate work, not stalling.
		// Only start counting once the model has executed a real action (everActed),
		// so initial exploration is handled by the explore rail (higher ceiling) rather
		// than the stall backstop (tighter ceiling designed for post-action spins).
		editedThisTurn := isEditTool(call.Name) && derr == nil
		fp := l.treeFingerprint()
		switch {
		case !stallSeeded:
			stallSeeded = true // first turn establishes the baseline; nothing to compare yet
		case !everActed:
			stall = 0 // pre-action exploration — let the explore rail govern, not stall
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
		// The map is budgeted from the CURATOR cap, not the window: capacity and
		// appetite are separate decisions. With no cap configured this resolves to
		// the usable window, i.e. exactly the pre-split arithmetic.
		mapBudget = mapBudgetTokens(EffectiveCuratorBudget(l.ContextTokens, l.CuratorTokens))
	}
	// Curate the map once, then place it: appended to the system prompt (legacy)
	// or emitted as a trailing message (default) so the prefix above it can cache.
	//
	// KLOO_NO_MAP=1 drops the map entirely (off by default). This is the one
	// ARCHITECTURAL difference between kloo and grok on this benchmark: grok has no
	// repo map at all — it explores with search/grep/list_dir and edits early —
	// while kloo injects ~18k tokens of map into every prompt at --ctx 131072
	// (0.30 of the compaction trigger). Measured on kloo-bench, kloo's glimmer
	// reads 12-44 files and NEVER edits on 7 of 10 runs, where grok's glimmer
	// passes the same five cases 5/5 at a median 377s. A map that enumerates the
	// repo may be inviting exactly that reading.
	mapSection := ""
	if !noRepoMap() {
		mapSection = repoMapSection(l.assembleContext(task, mapBudget))
	}
	sysContent := l.System
	var tailMsgs []llm.Message
	if mapSection != "" {
		if l.mapAtTail() {
			// USER, not system. Several open-weight chat templates hard-raise on a
			// system message that is not the leading one, and the tail map is
			// exactly that — a trailing system message:
			//
			//	[system, user]        -> 200 OK
			//	[user, system, user]  -> 500 Jinja: "System message must be at the
			//	                         beginning."
			//
			// Qwen3.8-Flash scored 0/22 on a 220-case benchmark for this reason, at a
			// 10s median: no case ever reached the model. grok scored 22/22 on the
			// identical seat. Anthropic rejects the same shape.
			//
			// The map is reference material, so the user role carries it correctly,
			// and keeping it at the TAIL preserves the stable cacheable prefix that
			// motivated the placement in the first place. Folding it back into the
			// lead system prompt would fix the ordering and lose the caching.
			tailMsgs = []llm.Message{{Role: llm.RoleUser, Content: mapSection}}
		} else {
			sysContent = l.System + "\n\n" + mapSection
		}
	}
	sys := llm.Message{Role: llm.RoleSystem, Content: sysContent}
	// The memory assembler budgets against everything that is NOT history, so the
	// map counts wherever it sits — placement must not change the token math.
	nonHistoryTokens := l.estimate(sysContent)
	for _, m := range tailMsgs {
		nonHistoryTokens += l.estimate(m.Content)
	}

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
			EditAnchor:   l.curEditAnchor,
			Exercises:    l.verifyTestImports(),
			WindowTokens: win,
			SystemTokens: nonHistoryTokens,
			MapBudget:    mapBudget,
			Estimate:     l.estimate,
		})
		if merr != nil {
			// ErrWindowTooSmall ⇒ a config error surfaced as a ReasonError stop.
			return tools.Call{}, nil, llm.Usage{}, llm.Message{}, merr
		}
		hist = h
	} else {
		hist = boundedHistory(convo, l.maxConv())
	}

	// Pin the task list, if one is in use: a list the model cannot see every turn
	// is not a task list. Cheap (a few lines) and regenerated per turn like the
	// other pins.
	if td := l.Registry.Todos(); td != nil {
		if r := td.Render(); r != "" {
			tailMsgs = append(tailMsgs, llm.Message{Role: llm.RoleUser, Content: r})
		}
	}
	msgs := append([]llm.Message{sys}, hist...)
	// Appended AFTER history, never spliced into it: a tool result must stay
	// adjacent to the assistant message that requested it.
	msgs = append(msgs, tailMsgs...)
	// Prompt-cache breakpoint: mark the LAST message of the stable prefix — the
	// final tail message, immediately above the per-turn pins. Everything below it
	// (pins, then the repo map) is rewritten every turn, so a breakpoint there
	// would cache nothing and burn the slot.
	markCacheBreakpoint(msgs, l.PromptCache, pinnedMessages(l.Memory))
	req := l.withThinkingControl(l.Adapter.BuildRequest(llm.ChatRequest{
		Model:       l.Model,
		Messages:    msgs,
		Temperature: l.Temperature,
	}, l.turnRegistry(lastVerify.Passed)))
	// Measure what we actually SEND: messages PLUS the tool schemas, which the
	// provider also counts in prompt_tokens. Counting message text alone made the
	// measured ratio collapse to the clamp floor on short conversations, where the
	// schemas dominate the prompt.
	l.lastPromptChars = messageChars(msgs) + l.toolSchemaChars(req.Tools)

	resp, err := l.complete(ctx, req)
	if err != nil {
		return tools.Call{}, nil, llm.Usage{}, llm.Message{}, err
	}
	msg := assistantMessage(resp)
	usage := estimateUsage(resp.Usage, msgs, msg)
	calls, perr := l.Adapter.ParseAll(msg)

	if perr != nil || len(calls) == 0 {
		if perr == nil {
			if terr := runawayThinkingError(msg); terr != nil {
				// The model burned its whole output budget and returned NOTHING. kloo's
				// own message names the remedy — "disable thinking" — and then never
				// applied it: it retried the identical request, got the identical
				// nothing, and ended the run. Measured on kloo-bench A06: five retries,
				// five empty turns, run over at step 7.
				//
				// Retrying blind cannot work here; the request is deterministic in the
				// way that matters. Apply the remedy instead, ONCE, then give up with
				// the original message if it still produces nothing.
				if !l.NoThink {
					noThink := req
					noThink.ReasoningEffort = "none"
					if resp2, err2 := l.complete(ctx, noThink); err2 == nil {
						msg2 := assistantMessage(resp2)
						if calls2, perr2 := l.Adapter.ParseAll(msg2); perr2 == nil && len(calls2) > 0 {
							usage2 := estimateUsage(resp2.Usage, msgs, msg2)
							return calls2[0], calls2[1:], usage2, msg2, nil
						} else if perr2 == nil && runawayThinkingError(msg2) == nil {
							// content but no tool call: fall through to the normal
							// corrective path with the better message in hand
							msg, usage, calls, perr = msg2, estimateUsage(resp2.Usage, msgs, msg2), calls2, perr2
						}
					}
				}
				if len(calls) == 0 && perr == nil {
					if terr2 := runawayThinkingError(msg); terr2 != nil {
						return tools.Call{}, nil, usage, msg, terr2
					}
				}
			}
		}
		// One corrective re-prompt (the anti-spiral rail, mirrored from P02).
		corrective := llm.Message{Role: llm.RoleUser, Content: l.Adapter.Corrective(perr)}
		retryMsgs := append(append([]llm.Message{}, msgs...), msg, corrective)
		resp2, err2 := l.complete(ctx, l.withThinkingControl(llm.ChatRequest{Model: l.Model, Messages: retryMsgs, Temperature: l.Temperature}))
		if err2 != nil {
			return tools.Call{}, nil, usage, msg, err2
		}
		msg2 := assistantMessage(resp2)
		usage2 := estimateUsage(resp2.Usage, retryMsgs, msg2)
		calls, perr = l.Adapter.ParseAll(msg2)
		if perr == nil && len(calls) == 0 {
			if err := runawayThinkingError(msg2); err != nil {
				return tools.Call{}, nil, addUsage(usage, usage2), msg2, err
			}
		}
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

// repoMapHeader labels the curated repo-map section, wherever it is placed.
const repoMapHeader = "Repository map (most relevant first):\n"

// pinnedMessages reports how many trailing history messages were per-turn pins in
// the assembly just performed (0 when no working memory is wired).
func pinnedMessages(m WorkingMemory) int {
	if m == nil {
		return 0
	}
	return m.Stats().PinnedMessages
}

// markCacheBreakpoint attaches the cache breakpoint to the last message of the
// stable prefix. msgs is [system, ...history, map?]; the history's trailing pins
// and the map are the volatile tail, so the breakpoint goes immediately above
// them. A no-op when caching is off, which is what keeps the request byte-identical
// for every endpoint that does not opt in.
func markCacheBreakpoint(msgs []llm.Message, enabled bool, pins int) {
	if !enabled || len(msgs) == 0 {
		return
	}
	// Walk back over the trailing map message (never marked — it changes every
	// turn) and the pins, to the last message that is stable across turns.
	end := len(msgs) - 1
	for end > 0 && isMapMessage(msgs[end]) {
		end--
	}
	end -= pins
	if end <= 0 { // nothing but the system message above: nothing worth caching
		return
	}
	msgs[end].CacheControl = llm.CacheControlEphemeral()
}

// isMapMessage reports whether a message is the trailing repo map.
func isMapMessage(m llm.Message) bool {
	return m.Role == llm.RoleUser && strings.HasPrefix(m.Content, repoMapHeader)
}

// repoMapSection renders the curated map as a standalone prompt section, or ""
// when there is no map for this turn.
func repoMapSection(mapText string) string {
	if mapText == "" {
		return ""
	}
	return repoMapHeader + mapText
}

// MapPositionTail places the freshly curated repo map in a trailing message,
// AFTER the conversation; MapPositionSystem appends it to the system prompt (the
// original layout).
//
// Tail is the default because providers cache a re-sent prompt PREFIX and bill
// the cached part at a steep discount. The map is re-curated every turn — kloo
// edits files, so it changes constantly — and cache invalidation is a clean cut
// from the first differing token. With the map at the FRONT, that cut lands
// above the conversation and every turn re-pays for history that never changed.
// Moving the volatile section below the append-only history makes the whole
// conversation a stable, cacheable prefix.
//
// Set MapPositionSystem if an endpoint rejects a non-leading system message.
const (
	MapPositionTail   = "tail"
	MapPositionSystem = "system"
)

// mapAtTail reports whether the curated map goes in a trailing message. Unset ⇒
// tail (the cache-friendly default).
func (l *Loop) mapAtTail() bool { return l.MapPosition != MapPositionSystem }

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

	ranked := repomap.Rank(repomap.RankInput{Files: files, Symbols: byFile, Task: task, Contents: contents,
		DeprioritiseTests: mapDeprioritiseTests()})
	budget := mapBudget
	if budget <= 0 {
		budget = 2000
	}
	ctxStr, _ := repomap.Assemble(ranked, budget, l.estimate)
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

func (l *Loop) currentFileContents(path string) (string, bool) {
	if path == "" || l.Root == "" {
		return "", false
	}
	ws, err := tools.NewWorkspace(l.Root)
	if err != nil {
		return "", false
	}
	content, err := tools.ReadFile(ws, path)
	if err != nil {
		return "", false
	}
	return content, true
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

// exploreSignature identifies WHAT a read-only turn looked at, so the explore rail
// can tell "read a file I have not seen" (progress) from "looked at the same thing
// again" (spinning). Empty when the call carries no identifiable target, which is
// treated as no-new-ground so an unrecognised read cannot defeat the rail.
func exploreSignature(call tools.Call) string {
	for _, k := range []string{"path", "dir", "query", "pattern", "command", "id"} {
		if v := str(call.Args[k]); v != "" {
			return call.Name + "\x00" + v
		}
	}
	return ""
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
func (l *Loop) repeatCorrective(call tools.Call, n int) llm.Message {
	target := str(call.Args["path"])
	var b strings.Builder
	fmt.Fprintf(&b, "STOP — you have called %s", call.Name)
	if target != "" {
		fmt.Fprintf(&b, " on %s", target)
	}
	fmt.Fprintf(&b, " %d times in a row with the SAME arguments, and nothing changed. Repeating it will not help.\n", n)
	b.WriteString("Take a DIFFERENT action now:\n")
	if call.Name == tools.NameReadFile {
		if target != "" && l.Root != "" {
			if content, ok := l.currentFileContents(target); ok {
				if strings.TrimSpace(content) == "" {
					fmt.Fprintf(&b, "- %s is empty. Create its contents with write_file — do NOT read it again.\n", target)
				} else {
					fmt.Fprintf(&b, "- %s has already been read and is unchanged. Use edit_file for a surgical change, or inspect a DIFFERENT path with search/list_dir only if the task needs more context.\n", target)
				}
			} else {
				fmt.Fprintf(&b, "- %s is missing or unreadable. Create it with write_file if the task requires it, or inspect a DIFFERENT path with search/list_dir.\n", target)
			}
		} else {
			b.WriteString("- If that file is empty or missing, create its contents with write_file — do NOT read it again.\n")
			b.WriteString("- If it has known content, use edit_file for a surgical change, or search/list_dir only to inspect a DIFFERENT target.\n")
		}
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
		"You have inspected %d files without making any change. You now have enough context — "+
			"STOP reading and take action immediately this turn. Pick ONE of: "+
			"(a) call edit_file or write_file to implement a required change, "+
			"(b) call run_command to run a necessary command (e.g. a report/test command), "+
			"(c) call finish if all tasks are genuinely complete. "+
			"Do NOT read another file. If you genuinely cannot proceed without clarification, "+
			"reply with ONE short question and no tool call.", n)}
}

// editCorrective is the KLOO_EDIT_RAIL form of the explore nudge, used when the
// run has made NO edit yet.
//
// The general corrective above offers three ways out, two of which (run a
// command, call finish) a reading model can take without ever changing the code —
// and on kloo-bench it does: the explore rail fires 4-6 times in a typical failing
// run and the run still ends with an untouched tree. When nothing has been edited,
// the only useful next call is an edit, so ask for exactly that and say what the
// task requires.
func editCorrective(n int, edited bool, failing, exercises []string) llm.Message {
	lead := fmt.Sprintf("You have inspected %d files without changing a single line. ", n)
	if edited {
		lead = fmt.Sprintf("You made an edit earlier, and you have now inspected %d more files "+
			"without changing anything further. The test is still failing, so that edit was not "+
			"the whole fix. ", n)
	}
	if len(failing) > 0 {
		lead += "Still failing:\n  - " + strings.Join(failing, "\n  - ") +
			"\nThe remaining fix may be in a DIFFERENT file from the one you already changed — a " +
			"single behaviour often spans the query, the resolver and the helper that both use. "
	}
	if len(exercises) > 0 {
		lead += "The failing test exercises these files directly:\n  - " + strings.Join(exercises, "\n  - ") +
			"\nIf you have been editing the same file without the test going green, the change probably " +
			"belongs in one of these instead. "
	}
	return llm.Message{Role: llm.RoleUser, Content: lead +
		"Reading more will not complete this task: the code must change for the failing test to pass. " +
		"This turn, make your best edit to the source file you believe is wrong. " +
		"Do not read, do not search, do not run a command, do not call finish. " +
		"If you are not certain the edit is right, make it anyway and let the test tell you — " +
		"an edit that turns out wrong is progress and can be revised; another read is not."}
}

// editRail reports whether the no-edit explore nudge demands an edit
// (KLOO_EDIT_RAIL=1). Off by default.
func editRail() bool { return envOnDefault("KLOO_EDIT_RAIL") }

// forceEdit reports whether a repeated no-edit explore nudge also WITHHOLDS the
// read tools for that turn (KLOO_FORCE_EDIT=1). Off by default.
func forceEdit() bool { return envOnDefault("KLOO_FORCE_EDIT") }

// turnRegistry is the vocabulary advertised for the turn being built: the full
// registry, or the edit-only view when the force-edit rail armed it. One turn
// only — act() clears the flag, so a model that edits (or refuses to) is back to
// the full vocabulary immediately.
func (l *Loop) turnRegistry(verifyPassed bool) *tools.Registry {
	if l.editOnlyLeft > 0 {
		return l.Registry.EditOnlyView(verifyPassed)
	}
	return l.Registry
}

// editOnlyBudget is how many consecutive turns the force-edit rail holds the
// vocabulary down. Three: enough that a model which merely ignored the narrowed
// list once still meets it again, few enough that a run which genuinely has
// nothing to edit is not held hostage.
const editOnlyBudget = 3

// envOnDefault reports whether a DEFAULT-ON behaviour is still enabled: the
// opt-OUT twin of envOnAgent, active unless the variable is explicitly falsy.
//
// Used by the seven convergence rails (v0.22.0). They ship ON: each one removes a
// way kloo lost work or refused to act, and shipping them off was shipping the
// defect. `KLOO_<NAME>=0` restores the previous behaviour for any of them.
func envOnDefault(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

func envOnAgent(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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

// confirmFinishCorrective nudges a run that has been EXECUTING real actions but tries to
// stop with a bare prose turn instead of calling finish. On a multi-step ops task (a
// deploy, a migration) the model often completes one step and narrates "done" while
// later steps remain — the run would otherwise accept that as a calm answered-stop. The
// nudge forces the binary choice: call finish (the explicit, verify-gated terminator)
// ONLY if every step is done, otherwise do the next step now. One-shot (the confirm-
// finish rail flips confirmFinishNudged), so a genuinely-complete run still stops calmly
// on its next bare turn — this costs at most one extra round.
func confirmFinishCorrective() llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: "You stopped with a prose message but did NOT call the finish tool, " +
		"and you have been running real actions this session — so I cannot tell whether the TASK is actually complete. " +
		"Re-read the original task and its definition of done. If EVERY step is genuinely complete, call the finish tool now " +
		"with a one-line summary (it runs the final verify). If ANY step remains, do the next one THIS turn with a tool call. " +
		"Do not end with a prose 'done' — either call finish or keep going."}
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
	"let me deploy", "let me set", "let me register", "let me login", "let me continue",
	"let me implement", "let me edit", "let me write", "let me modify", "let me apply",
	"let's run", "let's check", "let's try", "let's see", "let's start",
	"let's implement", "let's edit", "let's write", "let's modify",
	"i'll run", "i'll check", "i'll try", "i'll look", "i'll start", "i'll fix",
	"i'll build", "i'll deploy", "i'll set", "i'll create", "i'll also", "i'll now",
	"i'll implement", "i'll edit", "i'll modify", "i'll apply", "i'll code",
	"i will run", "i will check", "i'm going to", "i am going to",
	"i will implement", "i will edit", "i will modify", "i will write",
	"going to run", "going to check", "try running", "now let me", "now i'll",
	"going to implement", "going to edit", "going to modify",
	"next, let me", "next i'll", "next, i'll", "start by",
	"now implement", "now edit", "start implementing", "begin implementing",
	"need to implement", "proceed to implement", "proceed to edit",
	"will implement", "to implement the", "to implement task",
	"implementing the", "implementing task",
}

// promisesToAct reports whether a no-tool-call reply READS like the model announced a
// next action ("let me run X") rather than delivering a final answer — including a
// reply that contains a FENCED CODE BLOCK, which on a doer run almost always means the
// model WROTE a command/snippet in prose but forgot to actually call the tool (seen
// live: dsv4 wrote ```bash … lokal mp deploy …``` and stopped). Used by the
// promised-but-didn't-act rail to nudge the model to emit the call before the run
// accepts the calm answered-stop.
func promisesToAct(content string) bool {
	if strings.Contains(content, "```") { // wrote a command/code block but never called a tool
		return true
	}
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

// editAnchorOf extracts the text an edit call targeted, for the pin window to
// centre on. search_replace carries it plainly; edit_file's SEARCH/REPLACE block
// carries it between the markers. write_file has no target region, so it yields
// nothing and the pin falls back to the head of the file.
func editAnchorOf(call tools.Call) string {
	if s := str(call.Args["old_string"]); s != "" {
		return s
	}
	d := str(call.Args["diff"])
	if d == "" {
		return ""
	}
	if i := strings.Index(d, "<<<<<<< SEARCH"); i >= 0 {
		d = d[i+len("<<<<<<< SEARCH"):]
	}
	if j := strings.Index(d, "======="); j >= 0 {
		d = d[:j]
	}
	return strings.TrimSpace(d)
}

// subsetTestWarning returns the observation for a model that ran a NARROWER test
// command than the verify command, got a pass, and is about to believe the task is
// finished. Empty when that is not what happened.
//
// Measured on kloo-bench C62: the verify command names two test files; the model
// ran ONE of them four times, saw exit 0 every time, never ran the other, never
// edited anything, and the run ended "answered" with the case red. Refusing tools
// cannot fix this — the model is not stuck exploring, it believes it is done. The
// only thing that helps is telling it, at the moment it happens, that the green it
// is looking at is not the green it is graded on.
func (l *Loop) subsetTestWarning(call tools.Call, res tools.Result, derr error) string {
	if !envOnDefault("KLOO_VERIFY_AUTHORITY") || derr != nil || res.ExitCode != 0 {
		return ""
	}
	if call.Name != tools.NameRunCommand || strings.TrimSpace(l.VerifyCmd) == "" {
		return ""
	}
	ran := pathTokens(str(call.Args["command"]))
	want := pathTokens(l.VerifyCmd)
	if len(ran) == 0 || len(want) == 0 {
		return ""
	}
	var missing []string
	for _, w := range want {
		found := false
		for _, r := range ran {
			if r == w {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, w)
		}
	}
	// Every file the gate covers was run: the pass is a real pass, say nothing.
	if len(missing) == 0 {
		return ""
	}
	// The command must at least overlap the gate, or this is an unrelated command.
	if len(missing) == len(want) {
		return ""
	}
	return "That command passed, but it is NOT the check this task is graded on. It left out: " +
		strings.Join(missing, ", ") + ". The full verification command is:\n  " + l.VerifyCmd +
		"\nRun that command, not a narrower one. The task is not complete until IT passes."
}

// pathTokens picks the file-like arguments out of a command line: tokens that are
// not flags and contain a dot, compared on the file name so a path relative to the
// gate's working directory still matches one relative to the workspace root.
func pathTokens(cmd string) []string {
	var out []string
	for _, tok := range strings.Fields(cmd) {
		tok = strings.Trim(tok, "\"'")
		if tok == "" || strings.HasPrefix(tok, "-") || !strings.Contains(tok, ".") {
			continue
		}
		// Config files are arguments to the runner, not part of the covered set.
		if strings.Contains(tok, "config") {
			continue
		}
		base := tok
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		out = append(out, base)
	}
	return out
}

// shouldRollback decides whether a finished run's edits are discarded.
//
// kloo rolls back to its checkpoint on ANY non-success exit. Measured on
// kloo-bench C62, that is how kloo loses a case it has already solved: the verify
// command names two test files, one of which is a Playwright spec that cannot
// resolve node:os under vitest — unfixable, failing in BOTH arms. kloo made the
// correct fix to the other file (its own summary describes it accurately), verify
// stayed red because of the broken spec, the run ended "answered" rather than
// success, and the fix was ERASED. The independent gate then measured unfixed
// code and scored 1f. grok, which has no rollback, kept its edit and passed.
//
// A developer whose agent ran out of steps wants the diff, not an empty tree —
// discarding real work because a verify could not go green is the wrong default
// even away from this bench. So with KLOO_KEEP_WORK_ON_FAIL the rollback is
// narrowed to the outcomes where the tree itself is untrustworthy: an internal
// error, an interrupt, or a safety stop. A run that simply did not finish keeps
// what it wrote.
func (l *Loop) shouldRollback(reason Reason, last VerifyResult) bool {
	if reason == ReasonSuccess {
		return false
	}
	if !envOnDefault("KLOO_KEEP_WORK_ON_FAIL") {
		return true // stock behaviour: any non-success exit rolls back
	}
	switch reason {
	case ReasonError, ReasonInterrupted, ReasonSafetyStop:
		return true
	}
	// Keeping partial work is right; keeping a tree that no longer BUILDS is not.
	// Measured on kloo-bench A05: a duplicate-declaration edit left the file
	// untransformable and the gate collected zero tests, which is strictly worse
	// than the state the run started in. Hand back a tree that at least compiles.
	if buildBreakGuard() && !last.Passed {
		if broken, _ := buildBreak(failingOutput(last)); broken {
			return true
		}
	}
	return false
}

// buildBreakSignatures are the verify outputs that mean the code no longer
// COMPILES or COLLECTS, as opposed to tests that ran and failed. The distinction
// matters: a failing test is information, while a broken build is damage the agent
// itself just did, and it hides every other signal behind it.
var buildBreakSignatures = []string{
	"transform failed",
	"has already been declared",
	"syntaxerror",
	"parse error",
	"cannot find module",
	"failed to load",
	"unexpected token",
}

// buildBreak reports whether a verify output shows a broken build, and returns the
// offending lines to quote back at the model.
//
// Measured on kloo-bench A05: an edit introduced duplicate `restDay` / `isHoliday`
// declarations, vitest could no longer transform the file, and the gate collected
// ZERO tests. The model then read sixteen more times — six of them refused by the
// force-edit rail — and never repaired what it had broken.
func buildBreak(out string) (bool, string) {
	low := strings.ToLower(out)
	hit := false
	for _, sig := range buildBreakSignatures {
		if strings.Contains(low, sig) {
			hit = true
			break
		}
	}
	if !hit {
		return false, ""
	}
	var keep []string
	for _, line := range strings.Split(out, "\n") {
		ll := strings.ToLower(line)
		for _, sig := range buildBreakSignatures {
			if strings.Contains(ll, sig) {
				keep = append(keep, strings.TrimSpace(line))
				break
			}
		}
		if len(keep) >= 6 {
			break
		}
	}
	return true, strings.Join(keep, "\n")
}

// buildBreakGuard reports whether kloo treats a broken build as urgent
// (KLOO_BUILD_BREAK_GUARD=1): it demands an immediate repair edit, and it refuses
// to leave the tree unbuildable at the end of a failed run.
func buildBreakGuard() bool { return envOnDefault("KLOO_BUILD_BREAK_GUARD") }

// failingTestSource returns the content of the test files the verify command
// names, for a one-time injection at the start of a run (KLOO_SHOW_FAILING_TEST).
//
// The brief names the failing test file but never shows it, so the model has to
// infer the required behaviour from the source it is trying to fix — which is
// circular when the bug IS the behaviour. Measured on kloo-bench C17: kloo failed
// 3 of 4 runs with ZERO edits, searching 17-30 times without ever writing a line,
// and the real fix is a subtle VAT accounting rule (exempt-discount handling,
// tax-corroboration) that cannot be derived from the broken code. The test states
// the rule outright.
//
// This is the plan's §5.4 lead "feed the failing test's content", which was never
// tested. It is NOT the same as the step-0 verify experiment (net -2), which fed
// the test's OUTPUT rather than its SOURCE.
func (l *Loop) failingTestSource() string {
	if !envOnAgent("KLOO_SHOW_FAILING_TEST") || strings.TrimSpace(l.VerifyCmd) == "" || l.Root == "" {
		return ""
	}
	ws, err := tools.NewWorkspace(l.Root)
	if err != nil {
		return ""
	}
	var b strings.Builder
	shown := 0
	for _, tok := range strings.Fields(l.VerifyCmd) {
		tok = strings.Trim(tok, "\"'")
		if tok == "" || strings.HasPrefix(tok, "-") || !strings.Contains(tok, ".") ||
			strings.Contains(tok, "config") || !looksLikeTestFile(tok) {
			continue
		}
		src, rerr := tools.ReadFile(ws, tok)
		if rerr != nil || strings.TrimSpace(src) == "" {
			continue
		}
		if n := strings.Count(src, "\n"); n > showTestMaxLines {
			lines := strings.Split(src, "\n")[:showTestMaxLines]
			src = strings.Join(lines, "\n") + fmt.Sprintf("\n… (%d more lines — read_file this path for the rest)", n-showTestMaxLines)
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", tok, src)
		if shown++; shown >= 2 {
			break
		}
	}
	if shown == 0 {
		return ""
	}
	return "This is the test your work is graded against. It states the behaviour required; " +
		"the source you are fixing does not. Do NOT edit it — make the code satisfy it.\n" + b.String()
}

// showTestMaxLines bounds the injected test so a large suite cannot swallow the
// window; the model can read_file the rest.
const showTestMaxLines = 400

// verifyTestImports lists the source files the verify command's test files import.
//
// Measured on kloo-bench A33: kloo edited `thirteenth-month.ts` four times, twice
// identically, and churned out with one assertion red. grok passed by changing
// THREE files — the computation, the repository query, and the resolver that
// passes the group id. kloo never edited a resolver in any run. Telling it what
// the failing test actually pulls in is the difference between "edit something"
// and "edit the right thing".
func (l *Loop) verifyTestImports() []string {
	// Its OWN flag, not KLOO_VERIFY_AUTHORITY's. Measured: it helped A16 on one rep
	// of two and did nothing for A33, the case it was designed from. That is not a
	// result, and folding an unproven change into a proven flag would make both
	// unmeasurable later.
	if !envOnAgent("KLOO_SAME_FAILURE_REDIRECT") || strings.TrimSpace(l.VerifyCmd) == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, tok := range strings.Fields(l.VerifyCmd) {
		tok = strings.Trim(tok, "\"'")
		if tok == "" || strings.HasPrefix(tok, "-") || !strings.Contains(tok, ".") ||
			strings.Contains(tok, "config") {
			continue
		}
		for _, f := range l.importsOf(tok) {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
		if len(out) >= 10 {
			break
		}
	}
	return out
}

// importsOf returns the workspace files a test file imports, so a refused edit can
// point at the code instead of just saying no. Best-effort: anything it cannot
// resolve to a real file is dropped rather than guessed at.
func (l *Loop) importsOf(rel string) []string {
	if rel == "" || l.Root == "" {
		return nil
	}
	ws, err := tools.NewWorkspace(l.Root)
	if err != nil {
		return nil
	}
	src, err := tools.ReadFile(ws, rel)
	if err != nil {
		return nil
	}
	dir := path.Dir(rel)
	seen := map[string]bool{}
	var out []string
	for _, m := range importPattern.FindAllStringSubmatch(src, -1) {
		spec := m[1]
		if spec == "" || !strings.HasPrefix(spec, ".") {
			continue // a package, not a file in this repo
		}
		base := path.Clean(path.Join(dir, spec))
		for _, ext := range []string{"", ".ts", ".tsx", ".js", ".mjs", "/index.ts", "/index.js"} {
			cand := base + ext
			if seen[cand] {
				break
			}
			if _, rerr := tools.ReadFile(ws, cand); rerr == nil {
				seen[cand] = true
				out = append(out, cand)
				break
			}
		}
		if len(out) >= 8 {
			break
		}
	}
	return out
}

// importPattern matches `from '…'` and `require('…')` specifiers.
var importPattern = regexp.MustCompile(`(?:from|require\()\s*['"]([^'"]+)['"]`)

// failingAssertions pulls the names of the tests that are still failing out of a
// verify output, for the edit corrective to aim the model at.
//
// Measured on kloo-bench A33: kloo made ONE edit, then read sixteen more times
// against a rail that refused every one of them, and stopped with a single
// assertion still red. grok passed the same case with a THREE-file change. The
// rail was pushing as hard as it could; what it never did was say WHAT was still
// broken. "Make your best edit to the file you believe is wrong" is no help to a
// model that does not know which file that is.
func failingAssertions(out string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(stripANSI(line))
		// vitest marks failures with × / ✕ / "FAIL"; take the test name that follows.
		var name string
		switch {
		case strings.HasPrefix(l, "×"), strings.HasPrefix(l, "✕"):
			name = strings.TrimSpace(strings.TrimLeft(l, "×✕ "))
		case strings.HasPrefix(l, "FAIL"):
			name = strings.TrimSpace(strings.TrimPrefix(l, "FAIL"))
		default:
			continue
		}
		if i := strings.Index(name, " "); i > 0 && strings.Contains(name[:i], "/") {
			name = strings.TrimSpace(name[i:]) // drop a leading file path
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if names = append(names, name); len(names) >= 4 {
			break
		}
	}
	return names
}

// stripANSI removes the colour escapes vitest writes, so assertion names match.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// maxEmptyTurnRecoveries bounds how many exhausted-retry empty completions a run
// will absorb before the error becomes fatal. Small: a genuinely broken model
// must still stop, and each recovery costs one round-trip.
const maxEmptyTurnRecoveries = 3

// emptyTurnRecovery reports whether an exhausted empty completion is recoverable
// (KLOO_EMPTY_TURN_RECOVERY=1) rather than fatal.
func emptyTurnRecovery() bool { return envOnDefault("KLOO_EMPTY_TURN_RECOVERY") }

// errProtectedPath marks an edit aimed at a file the verify command names.
var errProtectedPath = errors.New("agent: file is part of the verify command")

// protectedByVerify reports whether path is one of the files the verify command
// names. Gated on KLOO_PROTECT_VERIFY_PATHS so the released behaviour is unchanged
// until measured; a task that legitimately asks for a change to a file its own
// verify command names would otherwise be blocked.
func (l *Loop) protectedByVerify(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" || !envOnDefault("KLOO_PROTECT_VERIFY_PATHS") {
		return false
	}
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	for _, tok := range strings.Fields(l.VerifyCmd) {
		tok = strings.Trim(tok, "\"'")
		if tok == "" || strings.HasPrefix(tok, "-") {
			continue
		}
		// Compare on the file name, because the verify command's paths are relative
		// to the gate's working directory and the edit's are relative to the
		// workspace root; on this bench those two differ by a leading component.
		tb := tok
		if i := strings.LastIndex(tb, "/"); i >= 0 {
			tb = tb[i+1:]
		}
		if tb == base && strings.Contains(tok, ".") && looksLikeTestFile(tok) {
			return true
		}
	}
	return false
}

// coversNewGround reports whether a read-only call targets something this run has
// not seen. The force-edit rail lets those through: refusing them would punish
// legitimate exploration, which is the failure mode that made kloo relax this rail
// in the first place.
func coversNewGround(call tools.Call, seen map[string]bool) bool {
	sig := exploreSignature(call)
	return sig != "" && !seen[sig]
}

// noOpFeedback reports whether an edit that changed nothing is surfaced to the
// model rather than silently counted (KLOO_NOOP_EDIT_FEEDBACK=1).
func noOpFeedback() bool { return envOnAgent("KLOO_NOOP_EDIT_FEEDBACK") }

// looksLikeTestFile reports whether a path named by the verify command is a TEST,
// as opposed to a file the task is supposed to produce.
//
// The protection exists to stop an agent editing the spec it is graded against. It
// must NOT stop it writing a deliverable that the verify command happens to
// mention: a verify of `grep -qx right answer.txt` names answer.txt, which is
// exactly the file the task must create. Measured by kloo's own suite —
// TestHeadlessWiresMCPNonFatal and four others churned to a halt with every
// write_file refused, because the guard matched the goal file.
func looksLikeTestFile(path string) bool {
	p := strings.ToLower(path)
	for _, marker := range []string{
		".test.", ".spec.", "_test.go", "_test.py", "test_",
		"/tests/", "/test/", "/__tests__/", "/spec/",
	} {
		if strings.Contains(p, marker) {
			return true
		}
	}
	return strings.HasPrefix(p, "tests/") || strings.HasPrefix(p, "test/") || strings.HasPrefix(p, "spec/")
}

// errEditOnlyTurn marks a non-edit call the force-edit rail refused to dispatch.
var errEditOnlyTurn = errors.New("agent: tool withheld on a forced-edit turn")

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
func (l *Loop) chatGate(ctx context.Context, task string) (reply string, conversational bool, usage llm.Usage, gateErr error) {
	msgs := []llm.Message{{Role: llm.RoleSystem, Content: l.ChatSystem}}
	msgs = append(msgs, l.SessionHistory...)
	msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: task})

	resp, err := l.Client.Complete(ctx, l.withThinkingControl(llm.ChatRequest{Model: l.Model, Messages: msgs, Temperature: l.Temperature}))
	if err != nil {
		return "", false, llm.Usage{}, nil // fail open: run the loop normally
	}
	msg := assistantMessage(resp)
	usage = estimateUsage(resp.Usage, msgs, msg)
	if err := runawayThinkingError(msg); err != nil {
		return "", false, usage, err
	}
	text := strings.TrimSpace(msg.Content)
	if text == "" || isTaskVerdict(text) {
		return "", false, usage, nil
	}
	return text, true, usage, nil
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

func (l *Loop) llmRetries() int {
	return l.LLMRetries
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
	attempts := max(1, l.llmRetries()+1)
	var (
		resp   llm.ChatResponse
		err    error
		shrunk bool
	)
	start := time.Now()
	for attempt := 1; ; attempt++ {
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
		// A context overflow is not a transient — retrying the SAME prompt cannot
		// help, and the server says so. But it hands us the real limit, so shrink to
		// it and rebuild rather than ending the run. Only once per call: if the
		// shrunk prompt still overflows, something else is wrong.
		if lim := contextOverflowLimit(err); lim != 0 && !shrunk && l.ctxShrinks < maxContextShrinks {
			shrunk = true
			l.ctxShrinks++
			before := l.ContextTokens
			// The shrink must STRICTLY REDUCE. Setting the window to the server's
			// stated limit looks right and can be a no-op: --ctx was already 131072
			// and the request still totalled 132449, because the prompt budget is not
			// the whole request — tool schemas and the completion reserve sit on top.
			// The window then never changed, the step was rebuilt, it overflowed
			// again, and because the rebuild refunds the step it never ran out of
			// budget: 1536 identical retries in one run, at zero backoff, hammering
			// the endpoint. A recovery that does not converge is worse than the
			// failure it replaces.
			next := before * 4 / 5 // always at least a 20% cut
			if lim > 0 && lim < next {
				next = lim
			}
			l.ContextTokens = next
			// Floor it. A server that keeps rejecting is a different problem, and a
			// window driven toward zero would turn one bad response into an unusable
			// agent rather than a clear failure.
			if min := 8000; l.ContextTokens < min {
				l.ContextTokens = min
			}
			if l.OnRetry != nil {
				l.OnRetry(attempt, attempts-1, fmt.Errorf(
					"prompt exceeded the server's context limit; shrinking %d -> %d and rebuilding",
					before, l.ContextTokens), 0)
			}
			return resp, errContextShrunk
		}
		// A cold start earns more attempts than the normal budget, bounded by wall
		// time — but only if retrying is enabled at all (LLMRetries 0 still means
		// "never retry").
		coldStart := attempts > 1 && isColdStart(err) && time.Since(start) < l.coldStartPatience()
		exhausted := attempt >= attempts && !coldStart
		if err == nil || ctx.Err() != nil || exhausted || emitted || !l.isRetryableLLMError(err) {
			return resp, l.modelCallError(err)
		}
		// Cap the shift: extended cold-start retries would otherwise overflow the
		// doubling (2s << 30 is not a wait, it is a bug).
		wait := l.retryBaseDelay() << min(attempt-1, 10) // 2s, 4s, …
		if l.RetryMaxDelay > 0 && wait > l.RetryMaxDelay {
			wait = l.RetryMaxDelay
		}
		if coldStart && wait > 30*time.Second {
			wait = 30 * time.Second
		}
		if l.OnRetry != nil {
			l.OnRetry(attempt, attempts-1, err, wait)
		}
		select {
		case <-ctx.Done():
			return resp, err
		case <-time.After(wait):
		}
	}
}

func (l *Loop) withThinkingControl(req llm.ChatRequest) llm.ChatRequest {
	if l.NoThink {
		req.ReasoningEffort = "none"
	}
	return req
}

func (l *Loop) modelCallError(err error) error {
	if err == nil {
		return nil
	}
	parts := []string{"agent: model call failed"}
	if strings.TrimSpace(l.Endpoint) != "" {
		parts = append(parts, "endpoint="+l.Endpoint)
	}
	if strings.TrimSpace(l.Model) != "" {
		parts = append(parts, "model="+l.Model)
	}
	return fmt.Errorf("%s: %w", strings.Join(parts, " "), err)
}

// isRetryableLLMError reports whether a failed model call is a TRANSIENT endpoint
// hiccup worth retrying — versus a deterministic error (4xx auth/bad-request,
// parse) that a retry would only repeat. The parent-ctx guard lives in the caller,
// so a context.DeadlineExceeded reaching here is the request's OWN timeout (slow
// prefill / cold load), which is retryable.
func isRetryableLLMError(err error) bool {
	return retryableLLMError(err, nil)
}

func (l *Loop) isRetryableLLMError(err error) bool {
	return retryableLLMError(err, l.RetryableStatusCodes)
}

// contextOverflowLimit reads the TRUE per-request limit out of a 400 that says the
// prompt is too long, e.g.
//
//	this request needs ~132449 tokens, above the glimmer-tp2-179 per-request limit
//	of 131072. Retrying will not help; shorten the prompt or lower max_tokens.
//
// The server tells us the number AND that retrying is pointless, and kloo ignored
// both and ended the run. Measured on kloo-bench at ctx 131072: two cases died
// this way. Returns 0 when the error is not a context overflow.
func contextOverflowLimit(err error) int {
	var apiErr *llm.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 400 {
		return 0
	}
	low := strings.ToLower(apiErr.Body)
	// An overflow says BOTH what is too big and that it is too big. Requiring a
	// size word alone would swallow ordinary 400s (a bad tool schema, a malformed
	// message) and shrink the window for a fault shrinking cannot fix.
	sized := strings.Contains(low, "token") || strings.Contains(low, "context") ||
		strings.Contains(low, "prompt")
	over := strings.Contains(low, "above") || strings.Contains(low, "exceed") ||
		strings.Contains(low, "too long") || strings.Contains(low, "too large") ||
		strings.Contains(low, "maximum context")
	if !sized || !over {
		return 0
	}
	// Prefer an explicitly stated limit ("limit of N", "maximum ... N").
	for _, re := range overflowLimitPatterns {
		if m := re.FindStringSubmatch(low); len(m) > 1 {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				return n
			}
		}
	}
	return -1 // an overflow whose limit we could not read: shrink by a fraction
}

var overflowLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`limit of (\d+)`),
	regexp.MustCompile(`maximum context length is (\d+)`),
	regexp.MustCompile(`maximum of (\d+) tokens`),
}

// isColdStart reports whether an error is the endpoint explicitly saying it is
// starting up and the request should be resubmitted. Deliberately narrow: only
// these explicit signals earn the longer patience.
func isColdStart(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	for _, s := range []string{"powering on", "scheduler_busy"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	return false
}

func (l *Loop) coldStartPatience() time.Duration {
	if l.ColdStartPatience > 0 {
		return l.ColdStartPatience
	}
	return DefaultColdStartPatience
}

func retryableLLMError(err error, retryCodes []int) bool {
	if err == nil {
		return false
	}
	// The classic local-endpoint hiccups: no token in time (cold load / slow
	// prefill) and a stream that ended before [DONE] (dropped connection).
	if errors.Is(err, llm.ErrStreamIdle) || errors.Is(err, llm.ErrStreamIncomplete) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// An empty turn is a hiccup, not a config fault — retry it. If the model does
	// it every time the retries exhaust and the message still names the remedy.
	if errors.Is(err, ErrNoUsableContent) {
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
	// "worker <failed|timed out> while generating the completion": the serving
	// worker died or stalled mid-request. The gateway uses more than one verb for
	// the same fault — the first fix matched only "failed", and a rerun then died
	// on "worker timed out while generating the completion" with no retry at all —
	// so the match is on the shared suffix. Measured on kloo-bench, this ended runs outright (A04 at step 8,
	// before any edit). Only retried when no tokens were emitted — the complete()
	// loop already refuses to retry after output, so a partial answer is never
	// duplicated.
	for _, s := range []string{"connection reset", "connection refused", "unexpected eof", "broken pipe", "while generating the completion"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	// HTTP/2 stream faults. The REQUEST already succeeded — status 200 — and the
	// stream then died mid-transfer, so there is no APIError to match on and none
	// of the substrings above appear. Measured on kloo-bench at 131072: two of
	// kloo's eight losses to grok were
	//   "llm: read stream: stream error: stream ID 11; INTERNAL_ERROR; received from peer"
	// with no retry attempted. That is ~10% of the bench thrown away on a fault
	// the very next request would have survived.
	//
	// Only the SERVER-SIDE codes are retried. PROTOCOL_ERROR and FRAME_SIZE_ERROR
	// indicate a malformed request and would repeat identically, so they are not
	// matched — retrying a request the peer considers invalid is a loop, not a
	// recovery.
	for _, s := range []string{"internal_error", "refused_stream", "enhance_your_calm", "goaway"} {
		if strings.Contains(low, s) {
			return true
		}
	}
	// Upstream 5xx / 429 / 408 are server-side transient; other 4xx are not.
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		if retryCodes == nil {
			retryCodes = []int{408, 429, 500, 502, 503, 504}
		}
		for _, code := range retryCodes {
			if apiErr.StatusCode == code {
				return true
			}
		}
		return false
	}
	return false
}

func assistantMessage(resp llm.ChatResponse) llm.Message {
	if len(resp.Choices) == 0 {
		return llm.Message{Role: llm.RoleAssistant}
	}
	msg := resp.Choices[0].Message
	if msg.FinishReason == "" {
		msg.FinishReason = resp.Choices[0].FinishReason
	}
	return msg
}

const runawayReasoningChars = 2000

// ErrNoUsableContent is a turn that produced no tool call, no content and no
// reasoning worth keeping. It is a MODEL HICCUP, not a configuration fault: the
// same request usually succeeds on the next attempt. Measured on kloo-bench at
// ctx 131072, one of kloo's eight losses to grok was a single such turn ending
// the whole run ("0 reasoning chars but no usable content", finish_reason=length).
var ErrNoUsableContent = errors.New("model produced no usable content")

// errContextShrunk signals that the prompt overflowed the server's context limit
// and the window has been reduced to the limit the SERVER reported. The step must
// be rebuilt and retried under the new budget — the old request cannot succeed.
var errContextShrunk = errors.New("context window shrunk to the server's limit; rebuilding")

func runawayThinkingError(msg llm.Message) error {
	if len(msg.ToolCalls) > 0 {
		return nil
	}
	rawReasoning := msg.RawReasoningContent
	if rawReasoning == "" {
		rawReasoning = msg.ReasoningContent
	}
	rawContent := msg.RawContent
	if rawContent == "" && rawReasoning == "" {
		rawContent = msg.Content
	}
	if strings.TrimSpace(rawContent) != "" {
		return nil
	}
	reasoningChars := len([]rune(rawReasoning))
	if reasoningChars >= runawayReasoningChars || msg.FinishReason == "length" {
		return fmt.Errorf("%w (%d reasoning chars); kloo already retried with thinking disabled — raise the output budget or try a different model",
			ErrNoUsableContent, reasoningChars)
	}
	return nil
}

// addUsage accumulates a turn's usage into the run total. The cached-prompt
// count is normalised through CachedPromptTokens before summing, so a run that
// mixes provider shapes still totals correctly; it is carried in the DeepSeek-style
// hit field because the run total is a sum, not any one provider's response.
// estimate sizes a string in tokens using the run's calibrated estimator when
// there is one. Every budgeting path goes through here so the loop and the
// memory assembler never mix a calibrated count with an uncalibrated one.
func (l *Loop) estimate(s string) int {
	if l.Tokens != nil {
		return l.Tokens.Estimate(s)
	}
	return repomap.ApproxTokens(s)
}

// observeUsage records a turn's token usage: the cumulative budget counter plus
// the run's prompt-cache accounting, so the report can state what fraction of the
// re-sent prompt the provider served from cache. A provider that reports nothing
// leaves cachedPromptTokens at 0, which reads as "no discount observed" rather
// than as a proven miss.
func (l *Loop) observeUsage(u llm.Usage) {
	l.Budget.AddTokens(u.TotalTokens)
	l.promptTokens += u.PromptTokens
	l.cachedPromptTokens += u.CachedPromptTokens()
	// Ground truth for the estimator: this many characters cost exactly this many
	// tokens. Cleared either way, so one turn's chars can never be credited twice.
	if l.Tokens != nil {
		l.Tokens.Observe(l.lastPromptChars, u.PromptTokens)
	}
	l.lastPromptChars = 0
}

// tokenRatio is the run's measured chars-per-token, or 0 when nothing was
// measured (no calibrator, or an endpoint that reports no usage).
func (l *Loop) tokenRatio() float64 {
	if l.Tokens == nil || !l.Tokens.Calibrated() {
		return 0
	}
	return l.Tokens.Ratio()
}

// toolSchemaChars is the character cost of the tool definitions attached to a
// request. They are constant for a run, so the marshalled size is computed once.
func (l *Loop) toolSchemaChars(tls []llm.Tool) int {
	if len(tls) == 0 {
		return 0
	}
	if l.toolCharsCache > 0 {
		return l.toolCharsCache
	}
	b, err := json.Marshal(tls)
	if err != nil {
		return 0
	}
	l.toolCharsCache = len([]rune(string(b)))
	return l.toolCharsCache
}

// messageChars counts the characters kloo actually sent as the prompt.
func messageChars(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += tokens.CountChars(m.Content)
		for _, tc := range m.ToolCalls {
			n += tokens.CountChars(tc.Function.Name, tc.Function.Arguments)
		}
	}
	return n
}

func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		PromptTokens:          a.PromptTokens + b.PromptTokens,
		CompletionTokens:      a.CompletionTokens + b.CompletionTokens,
		TotalTokens:           a.TotalTokens + b.TotalTokens,
		PromptCacheHitTokens:  a.CachedPromptTokens() + b.CachedPromptTokens(),
		PromptCacheMissTokens: a.PromptCacheMissTokens + b.PromptCacheMissTokens,
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
