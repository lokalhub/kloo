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
	"time"
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
	// Ignored records tool calls dropped by the one-tool-per-turn rail.
	Ignored []string
}
