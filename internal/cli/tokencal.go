package cli

import (
	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/tokens"
)

// tokenCalibrator builds the run's token estimator, seeded with whatever ratio
// was measured for this model in an earlier run. A cold start (new model, new
// workspace) seeds the package default and calibrates itself after one turn.
func tokenCalibrator(workspace, model string) *tokens.Calibrator {
	return tokens.NewCalibrator(tokens.NewStore(workspace).Ratio(model))
}

// saveTokenCalibration persists what a finished run measured, so the NEXT run —
// and `kloo tokens`, which makes no network call and therefore has no other way
// to know — starts from a measurement instead of a guess. Best-effort: a failure
// to record an optimisation must never fail a run that otherwise succeeded.
func saveTokenCalibration(workspace, model string, rep *agent.Report) {
	if rep == nil || rep.TokenRatio <= 0 {
		return
	}
	_ = tokens.NewStore(workspace).Save(model, rep.TokenRatio)
}
