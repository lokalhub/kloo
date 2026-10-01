package agent

import (
	"time"

	"github.com/lokalhub/kloo/internal/config"
)

// runBudget bounds a run by three independent ceilings — steps, cumulative
// tokens, and wall-clock — so the loop can never run forever. The FIRST ceiling
// to exceed wins and is named in the stop reason. A ceiling of 0 means
// "unbounded" for that dimension.
//
// The wall-clock uses an injectable clock so tests are deterministic (no sleeps).
type runBudget struct {
	maxSteps  int
	steps     int
	maxTokens int
	tokens    int
	// unproductive is tokens spent since the last real file change. It is the
	// counter the COMPUTED ceiling is checked against, because total spend is the
	// wrong thing to bound: a long refactor that keeps landing edits legitimately
	// costs millions, while a question task that reads in circles costs the same
	// and produces nothing. Only the second one should be stopped, and the
	// difference between them is whether the workspace is changing.
	//
	// An EXPLICIT --max-tokens still bounds total spend. If a ceiling was asked for
	// by name, it means what it says.
	unproductive    int
	computedCeiling bool
	maxWall         time.Duration
	start           time.Time
	now             func() time.Time
}

// NewBudget builds a budget from resolved config. now is the clock (pass nil for
// time.Now); the wall-clock baseline is taken when NewBudget is called.
func NewBudget(cfg config.Config, now func() time.Time) *runBudget {
	if now == nil {
		now = time.Now
	}
	return &runBudget{
		maxSteps:        cfg.MaxSteps,
		maxTokens:       cfg.MaxTokens,
		computedCeiling: cfg.MaxTokensComputed,
		maxWall:         time.Duration(cfg.MaxWallClockSeconds) * time.Second,
		start:           now(),
		now:             now,
	}
}

// Reset clears the per-run counters and re-bases the wall-clock to now, keeping
// the configured ceilings. Called at the start of each Run so a reused Loop does
// not carry the previous run's step/token/elapsed totals into the next task.
func (b *runBudget) Reset() {
	b.steps = 0
	b.tokens = 0
	b.unproductive = 0
	b.start = b.now()
}

// Observe records the current step number (the loop calls it each turn).
func (b *runBudget) Observe(step int) { b.steps = step }

// AddTokens adds the turn's reported token usage to both counters.
func (b *runBudget) AddTokens(n int) {
	b.tokens += n
	b.unproductive += n
}

// NoteProgress records that a real change landed in the workspace, clearing the
// unproductive counter. A run that keeps editing therefore keeps renewing its
// allowance and is never stopped by the computed ceiling — which is the point: the
// ceiling exists to end runs that are not getting anywhere, not to ration work.
func (b *runBudget) NoteProgress() { b.unproductive = 0 }

// Check returns whether any budget is exceeded, naming the first that tripped
// (steps, then tokens, then wall-clock).
func (b *runBudget) Check() (bool, BudgetKind) {
	if b.maxSteps > 0 && b.steps > b.maxSteps {
		return true, BudgetSteps
	}
	// Trip EARLY by the closing answer's allowance, so the reply fits inside the
	// ceiling the user set. Stopping at maxTokens and then spending more on a final
	// answer would make --max-tokens 600000 cost 600000 plus the reply — a budget
	// that quietly overshoots itself, which is the complaint this feature exists to
	// answer, one layer down. Never reserve more than half the budget, so a small
	// ceiling is not consumed entirely by its own reserve.
	if b.maxTokens > 0 {
		// A FRACTION of the ceiling, capped at the flat allowance. A fixed 2048
		// slab is most of a small budget: TestBudgetTokensTrip sets maxTokens=100
		// and 2048 (clamped to half) tripped it at 60 tokens — the reserve ate the
		// budget it was meant to protect.
		reserve := b.maxTokens / 20
		if reserve > FinalAnswerReserve {
			reserve = FinalAnswerReserve
		}
		// The computed ceiling bounds UNPRODUCTIVE spend; an explicit one bounds
		// total spend. See the field comment: a run that is changing files is doing
		// the job it was given, and the cost of that is the user's call, not a
		// number kloo inferred from the context window.
		spent := b.tokens
		if b.computedCeiling {
			spent = b.unproductive
		}
		if spent > b.maxTokens-reserve {
			return true, BudgetTokens
		}
	}
	if b.maxWall > 0 && b.now().Sub(b.start) > b.maxWall {
		return true, BudgetWallClock
	}
	return false, ""
}

// Stats returns the current counters (for the report).
func (b *runBudget) Stats() BudgetStats {
	return BudgetStats{
		Steps:              b.steps,
		MaxSteps:           b.maxSteps,
		Tokens:             b.tokens,
		UnproductiveTokens: b.unproductive,
		ComputedCeiling:    b.computedCeiling,
		MaxTokens:          b.maxTokens,
		Elapsed:            b.now().Sub(b.start),
		MaxWall:            b.maxWall,
	}
}
