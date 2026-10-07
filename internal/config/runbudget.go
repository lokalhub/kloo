package config

import "math"

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
	// defaultWorkingSetCap mirrors agent's defaultWorkingSetTokens, the absolute
	// bound on the compaction trigger. It is only the DEFAULT: the caller passes the
	// configured value, because the cap is overridable and a budget sized from a cap
	// the loop is not using would be wrong in exactly the case someone bothered to
	// tune it.
	//
	// It matters because this budget is denominated in turns: at ctx 131072 the
	// uncapped fraction gives 73399 a turn, and 40 turns of that lands on the 1.5M
	// ceiling — only ~20 real turns. Capped, a turn costs 32768 and the budget buys
	// the 40 turns it claims to.
	defaultWorkingSetCap = 32768
)

// ComputeRunTokenBudget returns the cumulative token ceiling for a run on a model
// whose context window is `window`, or 0 (unbounded) when the window is unknown.
//
// window <= 0 means the endpoint reported no context length and nothing supplied
// one, so any budget derived from it would be a guess about a guess. Previous
// behaviour stands rather than inventing a ceiling that could stop real work.
// workingSet is the configured absolute cap on the compaction trigger, in the
// same sign convention as everywhere else: 0 ⇒ the built-in default, negative ⇒
// disabled, in which case the per-turn cost is the fraction alone and the ceiling
// is what bounds a large window.
func ComputeRunTokenBudget(window int, usableFrac, triggerFrac float64, workingSet int) int {
	if window <= 0 {
		return 0
	}
	if usableFrac <= 0 || usableFrac > 1 {
		usableFrac = defaultUsableFrac
	}
	if triggerFrac <= 0 || triggerFrac > 1 {
		triggerFrac = defaultTriggerFrac
	}
	// The per-turn cost is bounded by the SAME cap the loop applies, and at the
	// default that cap follows the window (agent/workingset.go: the geometric mean
	// of the usable window and the base). Keeping a flat 32768 here would size the
	// budget from a per-turn cost the loop stopped using — too few turns on a large
	// window, which is the error this whole budget exists to avoid.
	usable := float64(window) * usableFrac
	if workingSet == 0 {
		workingSet = int(math.Sqrt(usable * float64(defaultWorkingSetCap)))
	}
	perTurn := int(usable * triggerFrac)
	if workingSet > 0 && perTurn > workingSet {
		perTurn = workingSet
	}
	budget := perTurn * runBudgetTurns
	if budget < runBudgetFloor {
		return runBudgetFloor
	}
	if budget > runBudgetCeiling {
		return runBudgetCeiling
	}
	return budget
}
