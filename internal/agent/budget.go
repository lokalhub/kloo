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
	maxWall   time.Duration
	start     time.Time
	now       func() time.Time
}

// NewBudget builds a budget from resolved config. now is the clock (pass nil for
// time.Now); the wall-clock baseline is taken when NewBudget is called.
func NewBudget(cfg config.Config, now func() time.Time) *runBudget {
	if now == nil {
		now = time.Now
	}
	return &runBudget{
		maxSteps:  cfg.MaxSteps,
		maxTokens: cfg.MaxTokens,
		maxWall:   time.Duration(cfg.MaxWallClockSeconds) * time.Second,
		start:     now(),
		now:       now,
	}
}

// Reset clears the per-run counters and re-bases the wall-clock to now, keeping
// the configured ceilings. Called at the start of each Run so a reused Loop does
// not carry the previous run's step/token/elapsed totals into the next task.
func (b *runBudget) Reset() {
	b.steps = 0
	b.tokens = 0
	b.start = b.now()
}

// Observe records the current step number (the loop calls it each turn).
func (b *runBudget) Observe(step int) { b.steps = step }

// AddTokens adds the turn's reported token usage to the cumulative counter.
func (b *runBudget) AddTokens(n int) { b.tokens += n }

// Check returns whether any budget is exceeded, naming the first that tripped
// (steps, then tokens, then wall-clock).
func (b *runBudget) Check() (bool, BudgetKind) {
	if b.maxSteps > 0 && b.steps > b.maxSteps {
		return true, BudgetSteps
	}
	if b.maxTokens > 0 && b.tokens > b.maxTokens {
		return true, BudgetTokens
	}
	if b.maxWall > 0 && b.now().Sub(b.start) > b.maxWall {
		return true, BudgetWallClock
	}
	return false, ""
}

// Stats returns the current counters (for the report).
func (b *runBudget) Stats() BudgetStats {
	return BudgetStats{
		Steps:     b.steps,
		MaxSteps:  b.maxSteps,
		Tokens:    b.tokens,
		MaxTokens: b.maxTokens,
		Elapsed:   b.now().Sub(b.start),
		MaxWall:   b.maxWall,
	}
}
