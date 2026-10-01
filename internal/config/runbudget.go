package config

// The run's cumulative token budget.
//
// Every effort tier shipped MaxTokens: 0 ("unbounded"), and no flag reached
// runBudget's ceiling, so in practice a run had no token limit at all. A question
// task could spend millions and only the step ceiling or the wall clock would end
// it — reported directly, at ~2M tokens.
//
// The budget is now COMPUTED from the context window by default, because the right
// number is not a constant: what a run costs is dominated by how big each turn's
// prompt is, and that is set by the window. A flat default would be absurdly tight
// at ctx 131072 and absurdly loose at ctx 8000, which is the same unit error the
// turn-denominated explore cap had.
const (
	// runBudgetTurns is how many FULL turns the budget is sized for. A turn's cost
	// is approximated by the compaction trigger, since compaction is what holds a
	// prompt near that size for most of a run.
	runBudgetTurns = 40
	// The clamps matter more than the multiplier.
	//
	// Floor: a small window needs MORE turns, not fewer — a 2B model at ctx 8000
	// working a real task takes many cheap steps, and 40 x 4480 = 179k would stop it
	// mid-task. 250k is roughly 55 turns there.
	//
	// Ceiling: a large window needs FEWER turns, and the multiplier runs away — at
	// ctx 131072, 40 x 73399 is 2.9M, which is above the figure that prompted this
	// work. 1.5M is ~20 full turns at that window, past where a run that is going to
	// succeed has usually finished.
	runBudgetFloor   = 250_000
	runBudgetCeiling = 1_500_000
	// Mirrors agent's usableWindowFrac / compactTriggerFrac. Duplicated rather than
	// imported because internal/agent imports THIS package, so the dependency cannot
	// run the other way. TestRunBudgetFractionsMatchAgent pins them together.
	defaultUsableFrac  = 0.80
	defaultTriggerFrac = 0.70
)

// ComputeRunTokenBudget returns the cumulative token ceiling for a run on a model
// whose context window is `window`, or 0 (unbounded) when the window is unknown.
//
// window <= 0 means the endpoint reported no context length and nothing supplied
// one, so any budget derived from it would be a guess about a guess. Previous
// behaviour stands rather than inventing a ceiling that could stop real work.
func ComputeRunTokenBudget(window int, usableFrac, triggerFrac float64) int {
	if window <= 0 {
		return 0
	}
	if usableFrac <= 0 || usableFrac > 1 {
		usableFrac = defaultUsableFrac
	}
	if triggerFrac <= 0 || triggerFrac > 1 {
		triggerFrac = defaultTriggerFrac
	}
	perTurn := int(float64(window) * usableFrac * triggerFrac)
	budget := perTurn * runBudgetTurns
	if budget < runBudgetFloor {
		return runBudgetFloor
	}
	if budget > runBudgetCeiling {
		return runBudgetCeiling
	}
	return budget
}
