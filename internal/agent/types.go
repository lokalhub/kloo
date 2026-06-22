// Package agent is kloo's autonomous harness loop and its safety rails. The loop
// (loop.go) runs a deterministic act → apply → verify → decide state machine,
// one bounded tool call per turn, assembling per-step context from the repo map
// (internal/repomap) and stopping on a real success or a terminal signal.
//
// The rails that contain the small-model autonomy spiral are non-negotiable and
// each live in their own file: verify on REAL signals (verify.go), budgets
// (budget.go), churn detection (churn.go), and git checkpoint/rollback
// (checkpoint.go). Every termination produces a structured Report (report.go).
package agent

import (
	"context"
	"errors"
	"time"

	"github.com/lokalhub/kloo/internal/llm"
)

// State is a loop state. The machine cycles Act→Apply→Verify→Decide until Decide
// returns a terminal stop, reaching Stop.
type State int

const (
	StateAct State = iota
	StateApply
	StateVerify
	StateDecide
	StateStop
)

func (s State) String() string {
	switch s {
	case StateAct:
		return "act"
	case StateApply:
		return "apply"
	case StateVerify:
		return "verify"
	case StateDecide:
		return "decide"
	case StateStop:
		return "stop"
	default:
		return "unknown"
	}
}

// Reason is how a run ended (kebab-case, naming.md).
type Reason string

const (
	ReasonSuccess        Reason = "success"
	ReasonBudgetExceeded Reason = "budget-exceeded"
	ReasonChurn          Reason = "churn"
	ReasonError          Reason = "error"
	ReasonInterrupted    Reason = "interrupted"
	// ReasonAnswered: the model replied in prose with no tool call (a conversational
	// answer) rather than acting — a calm stop, not an error. The reply is already
	// shown; the loop just stops instead of churning/erroring on "no tool call".
	ReasonAnswered Reason = "answered"
)

// BudgetKind names which budget tripped.
type BudgetKind string

const (
	BudgetSteps     BudgetKind = "steps"
	BudgetTokens    BudgetKind = "tokens"
	BudgetWallClock BudgetKind = "wall-clock"
)

// ChurnKind names which churn signal fired.
type ChurnKind string

const (
	ChurnRepeatedFailure ChurnKind = "repeated-failure"
	ChurnRepeatedEdit    ChurnKind = "repeated-edit"
)

// VerifyResult is the REAL signal from running the configured verify command —
// the only thing the loop trusts to decide success (never the model's claim).
type VerifyResult struct {
	Command  string
	ExitCode int
	Stdout   string
	Stderr   string
	Passed   bool
	Duration time.Duration
	// Err is set when the verify command could not be run at all (non-runnable,
	// timeout, jail escape) — distinct from a command that ran and exited
	// non-zero. The loop turns a non-nil Err into an `error` outcome, never a
	// false pass.
	Err error
}

// Verifier runs the configured verify command and returns the real result.
// (Seam implemented in verify.go.)
type Verifier interface {
	Verify(ctx context.Context) VerifyResult
}

// BudgetStats are the current budget counters (for the report).
type BudgetStats struct {
	Steps     int
	MaxSteps  int
	Tokens    int
	MaxTokens int
	Elapsed   time.Duration
	MaxWall   time.Duration
}

// Budget bounds a run by steps, tokens, and wall-clock. The first to exceed wins.
// (Seam implemented in budget.go.)
type Budget interface {
	Observe(step int)
	AddTokens(n int)
	Check() (tripped bool, kind BudgetKind)
	Stats() BudgetStats
	// Reset clears the per-run counters (steps, tokens, wall-clock baseline) while
	// keeping the configured ceilings, so a reused Loop (e.g. the TUI submits many
	// tasks against one Loop) starts each run fresh instead of inheriting the prior
	// run's totals.
	Reset()
}

// Turn is what the churn detector observes each round: the (normalised) failing
// verify output and the (normalised) attempted edit.
type Turn struct {
	VerifyOutput string // empty when verify passed (progress)
	Edit         string // empty when the turn made no edit
}

// ChurnDetector halts a no-progress loop. (Seam implemented in churn.go.)
type ChurnDetector interface {
	Observe(t Turn)
	Check() (churned bool, kind ChurnKind)
	Artifact() string // the repeated artifact, for the report
	// Reset clears the accumulated repeat counters so a reused Loop starts each run
	// fresh — without this, a second task on the same Loop inherits the prior run's
	// failure/edit streak and can churn at step 1.
	Reset()
}

// WorkingMemory is kloo-core in-process working memory: it assembles the
// per-request transcript messages (the system prompt is built separately by the
// loop) so the pin-hot set + running summary + recent tail fit under the model's
// context window, compacting cold turns into the summary when projected tokens
// cross the trigger. It is a deterministic, in-process seam (same style as
// Budget / ChurnDetector / Checkpointer) — NOT the BYO recall port — because the
// working-set/compaction logic is the loop's brain and must stay fast and
// deterministic (overview §2). P00 does NO LLM call: the fold is structural.
type WorkingMemory interface {
	// Assemble returns the per-request messages (history only; the loop prepends
	// the system message), fitting the pin-hot set + running summary + recent
	// tail under budget. Pure given its inputs.
	//
	// It returns ErrWindowTooSmall when the window is below the irreducible floor
	// (the already-built system prompt + the task message convo[0]) — the one
	// case where the hard ceiling cannot be honored without dropping the task.
	// The loop turns this into a ReasonError stop, never an over-ceiling prompt
	// and never a dropped goal (overview §2.4).
	Assemble(in MemoryInput) ([]llm.Message, error)
	// Stats exposes the last assembly for the report / UI / DoD tests.
	Stats() MemoryStats
}

// ErrWindowTooSmall is returned by WorkingMemory.Assemble when WindowTokens is
// smaller than the irreducible prompt floor (the assembled system prompt + the
// task message). The loop surfaces it as a ReasonError config failure (raise
// maxContextTokens), never an over-ceiling prompt or a silently dropped task.
var ErrWindowTooSmall = errors.New("agent: maxContextTokens below the irreducible prompt floor (system prompt + task)")

// MemoryInput is everything WorkingMemory.Assemble needs for one turn. The loop
// fills it from its live state; the assembler is pure given these inputs.
type MemoryInput struct {
	Task         string        // convo[0], the goal — always pinned, never dropped
	Convo        []llm.Message // full running transcript (Convo[0] is the task)
	History      []llm.Message // prior-session turns (older than this run); seeded as the oldest tail so follow-ups have context. nil ⇒ standalone run.
	LastVerify   VerifyResult  // pinned: the last real verify signal
	EditPath     string        // file currently under edit ("" if none)
	FreshFile    string        // EditPath re-read from disk this turn (bounded)
	WindowTokens int           // = cfg.MaxContextTokens (the hard ceiling)
	SystemTokens int           // ApproxTokens(system prompt incl. repo map) already spent
	MapBudget    int           // the repo-map token budget the loop used this turn (for Stats/observability)
}

// MemoryStats is the last assembly's accounting (for the report / UI / DoD).
type MemoryStats struct {
	PromptTokens  int  // projected tokens for the assembled request (system + history)
	WindowTokens  int  // the hard ceiling this turn
	Compactions   int  // cumulative this run
	SummaryTokens int  // tokens in the running-summary slot
	DroppedTurns  int  // cold turns folded into the summary this run
	TrimmedTail   bool // tail shortened to fit after compaction
	MapBudget     int  // repo-map budget the loop used (echoed from MemoryInput)
	HotBudget     int  // pin-hot+tail token cap (hotBudgetFrac × window)
}

// Snapshot identifies a checkpointed working-tree state.
type Snapshot struct {
	Head     string // HEAD commit sha at checkpoint
	StashRef string // `git stash create` sha capturing the dirty tree ("" if clean)
	Taken    bool   // false ⇒ no usable snapshot (e.g. non-git workspace)
}

// Checkpointer snapshots the working tree before the first edit and restores it
// on abort. (Seam implemented in checkpoint.go.)
type Checkpointer interface {
	Checkpoint(ctx context.Context) (Snapshot, error)
	Rollback(ctx context.Context, s Snapshot) error
}

// BudgetEvidence is the report detail for a budget-exceeded stop.
type BudgetEvidence struct {
	Kind     BudgetKind
	Limit    string // the tripped limit (e.g. "10" or "2s")
	Observed string // the observed value
}

// ChurnEvidence is the report detail for a churn stop.
type ChurnEvidence struct {
	Kind     ChurnKind
	Artifact string // the repeated failure output / edit
}

// Report is the structured outcome of a run — the source of truth the CLI/TUI
// renders. The human rendering (report.go) is derived from this struct.
type Report struct {
	Reason      Reason
	Steps       int
	FinalVerify VerifyResult // the last real verify signal
	Budget      *BudgetEvidence
	Churn       *ChurnEvidence
	Err         error
	RolledBack  bool
	TokensUsed  int
	Elapsed     time.Duration
	// Compactions is how many times working memory folded cold turns into the
	// running summary this run (0 when memory is off or never triggered). The
	// report/UI print it only when > 0, so the no-compaction output is unchanged.
	Compactions int
	// Ignored records tool calls dropped by the one-tool-per-turn rail.
	Ignored []string
	// Transcript is this run's full conversation (task + every step). The TUI
	// accumulates it into the session so the next submission carries context
	// (seeded back via Loop.SessionHistory). Empty for callers that don't read it.
	Transcript []llm.Message
}
