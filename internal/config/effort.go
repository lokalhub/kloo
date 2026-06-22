package config

// EffortTier bundles the model + loop budgets + churn patience into a single
// named intensity level. Selecting a tier (--effort / KLOO_EFFORT) seeds all of
// these at once; a per-tier "efforts" override in the profile file, a per-model
// profile entry, env vars, and explicit flags still win on top (see Resolve).
type EffortTier struct {
	Model               string
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

// builtinEfforts are the shipped tiers, in increasing intensity:
//   - fast:   quick & decisive on the small model — bail early if stuck.
//   - medium: the balanced default (== legacy defaults).
//   - heavy:  patient & thorough on the strong model — for hard multi-file work.
//
// Any field is overridable per tier via the "efforts" section of profiles.json.
var builtinEfforts = map[string]EffortTier{
	"fast":   {Model: "snappy", MaxSteps: 20, ChurnRounds: 2, MaxTokens: 80_000, MaxWallClockSeconds: 300},
	"medium": {Model: "snappy", MaxSteps: 40, ChurnRounds: 3, MaxTokens: 200_000, MaxWallClockSeconds: 600},
	"heavy":  {Model: "smart", MaxSteps: 80, ChurnRounds: 10, MaxTokens: 500_000, MaxWallClockSeconds: 1800},
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
	Model               *string `json:"model,omitempty"`
	MaxSteps            *int    `json:"maxSteps,omitempty"`
	ChurnRounds         *int    `json:"churnRounds,omitempty"`
	MaxTokens           *int    `json:"maxTokens,omitempty"`
	MaxWallClockSeconds *int    `json:"maxWallClockSeconds,omitempty"`
}

// applyEffortOverride layers a config override onto a built-in tier in place.
func applyEffortOverride(t *EffortTier, ov *effortOverride) {
	if ov.Model != nil {
		t.Model = *ov.Model
	}
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
