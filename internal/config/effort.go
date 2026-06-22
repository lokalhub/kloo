package config

// EffortTier bundles the loop budgets + churn patience into a single named
// intensity level. Selecting a tier (--effort / KLOO_EFFORT) seeds all of these
// at once; a per-tier "efforts" override in the profile file, env vars, and
// explicit flags still win on top (see Resolve).
//
// The model is a SEPARATE axis (--model / KLOO_MODEL / profile) — a tier carries
// no model, so the same tier means the same intensity whether you point kloo at a
// local 8B or a frontier model.
type EffortTier struct {
	MaxSteps            int
	ChurnRounds         int
	MaxTokens           int
	MaxWallClockSeconds int
}

// DefaultEffort is used when neither --effort nor KLOO_EFFORT is set. medium is
// tuned to exactly match kloo's historical flat defaults, so the default run is
// unchanged by the introduction of tiers.
const DefaultEffort = "medium"

// EnvEffort selects the tier from the environment (below the flag).
const EnvEffort = "KLOO_EFFORT"

// builtinEfforts are the shipped tiers, in increasing intensity. A tier seeds
// only the loop budgets (steps/tokens/churn/wall-clock):
//   - fast:   quick & decisive — bail early if stuck.
//   - medium: the balanced default (== legacy defaults).
//   - heavy:  patient & thorough — for hard multi-file work.
//
// Any field is overridable per tier via the "efforts" section of profiles.json.
var builtinEfforts = map[string]EffortTier{
	"fast":   {MaxSteps: 20, ChurnRounds: 2, MaxTokens: 80_000, MaxWallClockSeconds: 300},
	"medium": {MaxSteps: 40, ChurnRounds: 3, MaxTokens: 200_000, MaxWallClockSeconds: 600},
	"heavy":  {MaxSteps: 80, ChurnRounds: 10, MaxTokens: 500_000, MaxWallClockSeconds: 1800},
}

// EffortNames lists the built-in tiers in increasing intensity (help/UX).
func EffortNames() []string { return []string{"fast", "medium", "heavy"} }

// IsEffort reports whether name is a known built-in tier.
func IsEffort(name string) bool { _, ok := builtinEfforts[name]; return ok }

// lookupEffort returns the built-in tier for name, falling back to the default.
func lookupEffort(name string) EffortTier {
	if e, ok := builtinEfforts[name]; ok {
		return e
	}
	return builtinEfforts[DefaultEffort]
}

// effortOverride is the per-tier override shape under the "efforts" key of the
// profile file: {"efforts": {"heavy": {"churnRounds": 15, "maxTokens": 800000}}}.
type effortOverride struct {
	MaxSteps            *int `json:"maxSteps,omitempty"`
	ChurnRounds         *int `json:"churnRounds,omitempty"`
	MaxTokens           *int `json:"maxTokens,omitempty"`
	MaxWallClockSeconds *int `json:"maxWallClockSeconds,omitempty"`
}

// applyEffortOverride layers a config override onto a built-in tier in place.
func applyEffortOverride(t *EffortTier, ov *effortOverride) {
	if ov.MaxSteps != nil {
		t.MaxSteps = *ov.MaxSteps
	}
	if ov.ChurnRounds != nil {
		t.ChurnRounds = *ov.ChurnRounds
	}
	if ov.MaxTokens != nil {
		t.MaxTokens = *ov.MaxTokens
	}
	if ov.MaxWallClockSeconds != nil {
		t.MaxWallClockSeconds = *ov.MaxWallClockSeconds
	}
}
