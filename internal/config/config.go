// Package config resolves kloo's runtime config (endpoint, model, profile, and
// core knobs) from a precedence chain: flags > env (KLOO_*) > profile file >
// built-in defaults.
//
// The package is pure: it performs no network I/O and reads only the profile
// JSON file. Callers (internal/cli) build a Flags from parsed CLI flags, pass
// os.Getenv for env, and a profile path (or "" for the default location).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Default values, used when nothing higher in the precedence chain sets a field.
const (
	DefaultEndpoint    = "http://127.0.0.1:8080/v1"
	DefaultModel       = "snappy"
	DefaultTemperature = 0.1
	DefaultMaxSteps    = 40
	DefaultMode        = "auto"
	DefaultToolFormat  = "native" // native function-calling; XML is the fallback (Phase 02)
	// DefaultMaxContextTokens bounds the per-step repo-map/context window the
	// curator assembles (Phase 03). A conservative default for small local models.
	DefaultMaxContextTokens = 8000
	// Autonomous-loop safety budgets (Phase 04). Conservative, sized for a
	// small-model run.
	DefaultMaxTokens           = 200000 // cumulative prompt+completion tokens per run
	DefaultMaxWallClockSeconds = 600    // 10 minutes
	DefaultChurnRounds         = 3      // repeated failure/edit rounds before halting
)

// Env var names (KLOO_-prefixed, SCREAMING_SNAKE). Extendable.
const (
	EnvEndpoint = "KLOO_ENDPOINT"
	EnvModel    = "KLOO_MODEL"
	// EnvAPIKey is the bearer token for the endpoint (needed for hosted providers
	// like OpenRouter; ignored by a local llama-swap, which has no auth). Falls
	// back to the conventional OPENAI_API_KEY when KLOO_API_KEY is unset.
	EnvAPIKey       = "KLOO_API_KEY"
	EnvAPIKeyOpenAI = "OPENAI_API_KEY"
)

// ErrProfileParse wraps a malformed profile JSON file. A *missing* profile file
// is not an error (defaults are used); only an unreadable/unparseable one is.
var ErrProfileParse = errors.New("parse profile file")

// Config is kloo's fully resolved runtime configuration.
type Config struct {
	Endpoint    string
	Model       string
	APIKey      string // bearer token for the endpoint (hosted providers); "" for local
	Temperature float64
	MaxSteps    int
	Mode        string
	ToolFormat  string
	// Effort is the resolved intensity tier (fast|medium|heavy) that seeded the
	// model + budgets + churn below.
	Effort string
	// FewShotPath is an optional per-model few-shot prompt file (from the
	// profile); empty when none is configured.
	FewShotPath string
	// MaxContextTokens is the per-step context-window token budget the Phase-03
	// repo-map curator must stay under.
	MaxContextTokens int
	// Phase-04 autonomous-loop safety budgets.
	MaxTokens           int // cumulative tokens ceiling per run (0 ⇒ unbounded)
	MaxWallClockSeconds int // wall-clock ceiling per run in seconds (0 ⇒ unbounded)
	ChurnRounds         int // repeated failure/edit rounds before halting
}

// Flags carries the explicitly-set CLI overrides. A nil field means "the flag
// was not provided" (so it does not win over env/profile); the CLI layer builds
// this from cobra's Changed() checks so an unset flag stays nil.
type Flags struct {
	Endpoint    *string
	Model       *string
	Temperature *float64
	MaxSteps    *int
	Mode        *string
	Effort      *string
}

// profileEntry is the per-model override shape in the profile JSON file:
//
//	{ "snappy": {"toolFormat": "native", "temperature": 0.2, "fewShotPath": "..."} }
type profileEntry struct {
	ToolFormat          *string  `json:"toolFormat,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	FewShotPath         *string  `json:"fewShotPath,omitempty"`
	MaxContextTokens    *int     `json:"maxContextTokens,omitempty"`
	MaxTokens           *int     `json:"maxTokens,omitempty"`
	MaxWallClockSeconds *int     `json:"maxWallClockSeconds,omitempty"`
	ChurnRounds         *int     `json:"churnRounds,omitempty"`
}

// Resolve computes the effective Config from the precedence chain
// flags > env > profile-file > defaults.
//
// getenv looks up an environment variable (pass os.Getenv in production; a map
// closure in tests). profilePath points at the profile JSON; when empty the
// default (~/.config/kloo/profiles.json) is used. A missing profile file yields
// defaults with no error; a malformed one returns an error wrapping
// ErrProfileParse.
func Resolve(flags Flags, getenv func(string) string, profilePath string) (Config, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	cfg := Config{
		Endpoint:            DefaultEndpoint,
		Model:               DefaultModel,
		Temperature:         DefaultTemperature,
		MaxSteps:            DefaultMaxSteps,
		Mode:                DefaultMode,
		ToolFormat:          DefaultToolFormat,
		MaxContextTokens:    DefaultMaxContextTokens,
		MaxTokens:           DefaultMaxTokens,
		MaxWallClockSeconds: DefaultMaxWallClockSeconds,
		ChurnRounds:         DefaultChurnRounds,
	}

	// Effort tier (flag > env > default): seeds Model + budgets + churn from a
	// named intensity level, replacing the flat defaults. A per-tier "efforts"
	// override in the profile file adjusts the tier; env/flags/per-model profile
	// still win on top (below). medium == the legacy defaults, so an unset effort
	// changes nothing.
	effort := DefaultEffort
	if v := getenv(EnvEffort); IsEffort(v) {
		effort = v
	}
	if flags.Effort != nil {
		effort = *flags.Effort
	}
	tier := lookupEffort(effort)
	if ov, err := loadEffortOverride(profilePath, effort); err != nil {
		return Config{}, err
	} else if ov != nil {
		applyEffortOverride(&tier, ov)
	}
	cfg.Effort = effort
	cfg.Model = tier.Model
	cfg.MaxSteps = tier.MaxSteps
	cfg.ChurnRounds = tier.ChurnRounds
	cfg.MaxTokens = tier.MaxTokens
	cfg.MaxWallClockSeconds = tier.MaxWallClockSeconds

	// Resolve the selected model first (flag > env > default) because the
	// profile file is keyed by model name.
	if v := getenv(EnvModel); v != "" {
		cfg.Model = v
	}
	if flags.Model != nil {
		cfg.Model = *flags.Model
	}

	// Profile-file layer: per-model overrides sit above defaults, below env/flags.
	entry, err := loadProfileEntry(profilePath, cfg.Model)
	if err != nil {
		return Config{}, err
	}
	if entry != nil {
		if entry.ToolFormat != nil {
			cfg.ToolFormat = *entry.ToolFormat
		}
		if entry.Temperature != nil {
			cfg.Temperature = *entry.Temperature
		}
		if entry.FewShotPath != nil {
			cfg.FewShotPath = *entry.FewShotPath
		}
		if entry.MaxContextTokens != nil {
			cfg.MaxContextTokens = *entry.MaxContextTokens
		}
		if entry.MaxTokens != nil {
			cfg.MaxTokens = *entry.MaxTokens
		}
		if entry.MaxWallClockSeconds != nil {
			cfg.MaxWallClockSeconds = *entry.MaxWallClockSeconds
		}
		if entry.ChurnRounds != nil {
			cfg.ChurnRounds = *entry.ChurnRounds
		}
	}

	// Env layer (above profile).
	if v := getenv(EnvEndpoint); v != "" {
		cfg.Endpoint = v
	}
	if v := getenv(EnvAPIKey); v != "" {
		cfg.APIKey = v
	} else if v := getenv(EnvAPIKeyOpenAI); v != "" {
		cfg.APIKey = v
	}

	// Flag layer (wins over everything).
	if flags.Endpoint != nil {
		cfg.Endpoint = *flags.Endpoint
	}
	if flags.Temperature != nil {
		cfg.Temperature = *flags.Temperature
	}
	if flags.MaxSteps != nil {
		cfg.MaxSteps = *flags.MaxSteps
	}
	if flags.Mode != nil {
		cfg.Mode = *flags.Mode
	}

	return cfg, nil
}

// readProfileFile reads the profile JSON bytes, or returns (nil, path, nil) when
// the file is absent or no home dir is available (→ defaults, not an error). An
// unreadable existing file is an error.
func readProfileFile(profilePath string) ([]byte, string, error) {
	path := profilePath
	if path == "" {
		var err error
		path, err = defaultProfilePath()
		if err != nil {
			return nil, "", nil // no home dir → treat as "no profile"
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, path, nil // missing file → defaults
		}
		return nil, path, fmt.Errorf("config: read profile %s: %w", path, err)
	}
	return data, path, nil
}

// loadProfileEntry reads the profile JSON and returns the override entry for
// model, or nil when the file is absent or has no entry for that model. Returns
// an error (wrapping ErrProfileParse) only when the file exists but is invalid.
func loadProfileEntry(profilePath, model string) (*profileEntry, error) {
	data, path, err := readProfileFile(profilePath)
	if err != nil || data == nil {
		return nil, err
	}
	var profiles map[string]profileEntry
	if err := json.Unmarshal(data, &profiles); err != nil {
		return nil, fmt.Errorf("config: %w %s: %v", ErrProfileParse, path, err)
	}
	if entry, ok := profiles[model]; ok {
		return &entry, nil
	}
	return nil, nil
}

// loadEffortOverride reads the optional "efforts" section of the profile file and
// returns the override for the named tier, or nil when absent.
func loadEffortOverride(profilePath, effort string) (*effortOverride, error) {
	data, path, err := readProfileFile(profilePath)
	if err != nil || data == nil {
		return nil, err
	}
	var file struct {
		Efforts map[string]effortOverride `json:"efforts"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("config: %w %s: %v", ErrProfileParse, path, err)
	}
	if ov, ok := file.Efforts[effort]; ok {
		return &ov, nil
	}
	return nil, nil
}

// defaultProfilePath is ~/.config/kloo/profiles.json, honouring XDG_CONFIG_HOME.
func defaultProfilePath() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "kloo", "profiles.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "kloo", "profiles.json"), nil
}
