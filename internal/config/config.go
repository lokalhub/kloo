// Package config resolves kloo's runtime config (endpoint, model, profile, and
// core knobs) from a precedence chain: flags > env (KLOO_*) > user profile file >
// bundled per-model defaults > built-in defaults. The bundled layer
// (model_defaults.go) is the lowest meaningful layer — it only ever fills the
// flat built-in constants for a known model, never anything the user set.
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
	"strings"
)

// Default values, used when nothing higher in the precedence chain sets a field.
const (
	DefaultEndpoint = "http://127.0.0.1:8080/v1"
	// DefaultModel is a neutral placeholder, not a real model. A single-model
	// llama.cpp server ignores the model field (serves whatever's loaded), so
	// this runs out-of-the-box there; for Ollama / vLLM / OpenAI / OpenRouter,
	// set your actual model via --model or KLOO_MODEL.
	DefaultModel       = "local"
	DefaultTemperature = 0.1
	DefaultMaxSteps    = 500
	DefaultMode        = "auto"
	DefaultToolFormat  = "native" // native function-calling; XML is the fallback (Phase 02)
	// DefaultMaxContextTokens bounds the per-step repo-map/context window the
	// curator assembles (Phase 03). A conservative default for small local models.
	DefaultMaxContextTokens = 8000
	// Autonomous-loop safety budgets (Phase 04). CHURN is the primary "stop when
	// stuck" guard; these are loose backstops (see internal/config/effort.go).
	// MaxTokens 0 ⇒ UNBOUNDED — cost is the endpoint/service's domain, and the
	// working-memory feature is meant to let small models run long; steps/wall-clock
	// are generous so a slow local model isn't cut off mid-progress.
	DefaultMaxTokens           = 0    // 0 ⇒ unbounded cumulative tokens (churn/steps/wall-clock guard)
	DefaultMaxWallClockSeconds = 3600 // 1 hour (final net for a churn-evading loop)
	DefaultChurnRounds         = 3    // repeated failure/edit rounds before halting (the primary guard)
	// DefaultMCPMaxExposedTools caps the total number of first-class MCP tools
	// exposed across all servers (overflow forces servers to lazy mode — Phase 02).
	// 0/absent in the profile ⇒ this default; small enough to protect a small
	// model's tool-selection quality (master plan §5).
	DefaultMCPMaxExposedTools = 16
)

// Env var names (KLOO_-prefixed, SCREAMING_SNAKE). Extendable.
const (
	EnvEndpoint = "KLOO_ENDPOINT"
	EnvModel    = "KLOO_MODEL"
	// EnvProvider selects a named provider from the profile's "providers" block
	// (below the --provider flag). A provider bundles the endpoint + bearer key
	// (+ its own model aliases) so provider and model are independent axes.
	EnvProvider = "KLOO_PROVIDER"
	// EnvAPIKey is the bearer token for the endpoint (needed for hosted providers
	// like OpenRouter; not needed for a local llama.cpp/Ollama server, which has no
	// auth). Falls back to the conventional OPENAI_API_KEY when KLOO_API_KEY is unset.
	EnvAPIKey       = "KLOO_API_KEY"
	EnvAPIKeyOpenAI = "OPENAI_API_KEY"
	// EnvMCP globally enables/disables MCP. "0"/"false" (case-insensitive) ⇒
	// disabled; unset or anything else ⇒ enabled. The --no-mcp flag (Phase 03)
	// overrides this; per-server "disabled" is the profile-level switch.
	EnvMCP = "KLOO_MCP"
	// EnvLint overrides the auto-detected fast advisory lint command (mirrors the
	// --lint flag). A value of "0"/"false" (case-insensitive) instead disables lint
	// — mirroring the EnvMCP truthiness convention — and is NOT treated as a command.
	EnvLint = "KLOO_LINT"
	// EnvNoLint disables the fast advisory lint step when "1"/"true" (case-insensitive),
	// mirroring the --no-lint flag. The --lint/--no-lint flags override both env vars.
	EnvNoLint = "KLOO_NO_LINT"
)

// ErrProfileParse wraps a malformed profile JSON file. A *missing* profile file
// is not an error (defaults are used); only an unreadable/unparseable one is.
var ErrProfileParse = errors.New("parse profile file")

// Config is kloo's fully resolved runtime configuration.
type Config struct {
	Endpoint string
	Model    string
	// Provider is the resolved provider name (--provider / KLOO_PROVIDER), or ""
	// when none was selected. It seeds Endpoint/APIKey and scopes the model-alias
	// lookup; it is not sent to the endpoint (informational/debug).
	Provider    string
	APIKey      string // bearer token for the endpoint (hosted providers); "" for local
	Temperature float64
	MaxSteps    int
	Mode        string
	ToolFormat  string
	// Effort is the resolved intensity tier (fast|medium|heavy) that seeded the
	// budgets + churn below (the model is a separate axis).
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
	// MCPServers is the parsed mcpServers block (empty map when none configured).
	// internal/mcp consumes these to dial servers; internal/config never imports
	// the SDK. Path/env values in command/args/env are already expanded.
	MCPServers map[string]MCPServerEntry
	// MCPMaxExposedTools caps total first-class MCP tools across all servers
	// (DefaultMCPMaxExposedTools when unset).
	MCPMaxExposedTools int
	// MCPDisabled globally disables MCP (env KLOO_MCP / --no-mcp). When true the
	// cli wiring skips connecting any server.
	MCPDisabled bool
}

// MCPServerEntry is one entry of the profile's reserved "mcpServers" block. It is
// decoded by loadMCPServers and carried on Config; internal/mcp turns it into a
// connection. Exactly one of Command (stdio) or URL (HTTP) is valid — that
// invariant is enforced in internal/mcp (non-fatally), not here. Leading "~"/"~/"
// and "$VAR"/"${VAR}" in Command, each Args element, each Env value, and each
// Headers value are expanded by the loader so internal/mcp receives ready-to-use
// values. Header names are protocol keys and are left literal.
type MCPServerEntry struct {
	Command        string            `json:"command,omitempty"`        // stdio: executable
	Args           []string          `json:"args,omitempty"`           // stdio: args
	Env            map[string]string `json:"env,omitempty"`            // stdio: extra env (merged over os.Environ in internal/mcp)
	URL            string            `json:"url,omitempty"`            // HTTP: endpoint (mutually exclusive with Command)
	Headers        map[string]string `json:"headers,omitempty"`        // HTTP: static request headers; values are expanded
	ExposeMode     string            `json:"exposeMode,omitempty"`     // curated | lazy | all ("" ⇒ curated if Expose set else lazy)
	Expose         []string          `json:"expose,omitempty"`         // curated allowlist (original MCP tool names)
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"` // per-call CallTool timeout (0 ⇒ default)
	Disabled       bool              `json:"disabled,omitempty"`       // per-server kill-switch
}

// Flags carries the explicitly-set CLI overrides. A nil field means "the flag
// was not provided" (so it does not win over env/profile); the CLI layer builds
// this from cobra's Changed() checks so an unset flag stays nil.
type Flags struct {
	Endpoint    *string
	Model       *string
	Provider    *string
	Temperature *float64
	MaxSteps    *int
	Mode        *string
	Effort      *string
	// NoMCP, when non-nil, forces MCP on/off above env+profile (true ⇒ disabled).
	// The cobra --no-mcp flag is wired in Phase 03; this field is the resolve seam.
	NoMCP *bool
}

// profileEntry is the per-model override shape in the profile JSON file:
//
//	{ "qwen2.5-coder": {"toolFormat": "native", "temperature": 0.2, "fewShotPath": "..."} }
type profileEntry struct {
	ToolFormat          *string  `json:"toolFormat,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	FewShotPath         *string  `json:"fewShotPath,omitempty"`
	MaxContextTokens    *int     `json:"maxContextTokens,omitempty"`
	MaxTokens           *int     `json:"maxTokens,omitempty"`
	MaxWallClockSeconds *int     `json:"maxWallClockSeconds,omitempty"`
	ChurnRounds         *int     `json:"churnRounds,omitempty"`
}

// providerEntry is one entry of the reserved "providers" profile block, selected
// by name via --provider / KLOO_PROVIDER. It bundles the endpoint and bearer key
// for a service (OpenRouter, Together, a local server …) plus that service's own
// model aliases — so the same model offered by several providers is just one
// alias per provider, each pointing at that provider's real model id. APIKey is
// expandValue'd; prefer a "${ENV_VAR}" reference over an inline secret, since the
// profile file is a trust root (same guidance as mcpServers headers).
type providerEntry struct {
	Endpoint string                `json:"endpoint,omitempty"`
	APIKey   string                `json:"apiKey,omitempty"`
	Models   map[string]modelEntry `json:"models,omitempty"`
}

// modelEntry is one alias inside a provider's "models" map. Model is the real
// model id sent to the endpoint (the map key is the short alias you pass to
// --model, e.g. "dsv4"); the embedded profileEntry carries the same per-model
// tuning knobs as the legacy top-level entries (toolFormat, temperature, …).
type modelEntry struct {
	Model        string `json:"model,omitempty"`
	profileEntry        // promoted: toolFormat, temperature, fewShotPath, context/budget knobs
}

// loadProviders reads the reserved "providers" block from the profile file. Like
// loadMCPServers/loadEffortOverride: a missing file or absent block ⇒ nil map and
// no error; a malformed file ⇒ an error wrapping ErrProfileParse. The block is an
// object, so its presence never disturbs the legacy top-level
// map[string]profileEntry decode in loadProfileEntry.
func loadProviders(profilePath string) (map[string]providerEntry, error) {
	data, path, err := readProfileFile(profilePath)
	if err != nil || data == nil {
		return nil, err
	}
	var file struct {
		Providers map[string]providerEntry `json:"providers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("config: %w %s: %v", ErrProfileParse, path, err)
	}
	return file.Providers, nil
}

// applyModelTuning layers a per-model entry's non-nil tuning fields onto cfg
// (toolFormat, temperature, few-shot, context/budget knobs). The model id and the
// endpoint/key are resolved on separate axes; this applies tuning only, so it is
// shared by the provider-alias path and the legacy top-level path.
func applyModelTuning(cfg *Config, e profileEntry) {
	if e.ToolFormat != nil {
		cfg.ToolFormat = *e.ToolFormat
	}
	if e.Temperature != nil {
		cfg.Temperature = *e.Temperature
	}
	if e.FewShotPath != nil {
		cfg.FewShotPath = *e.FewShotPath
	}
	if e.MaxContextTokens != nil {
		cfg.MaxContextTokens = *e.MaxContextTokens
	}
	if e.MaxTokens != nil {
		cfg.MaxTokens = *e.MaxTokens
	}
	if e.MaxWallClockSeconds != nil {
		cfg.MaxWallClockSeconds = *e.MaxWallClockSeconds
	}
	if e.ChurnRounds != nil {
		cfg.ChurnRounds = *e.ChurnRounds
	}
}

// Resolve computes the effective Config from the precedence chain
// flags > env > user profile-file > bundled per-model defaults > built-in
// defaults. The bundled layer (applyBundledDefaults) runs after the model id is
// resolved (incl. provider alias) and before the user's per-model tuning, so a
// known model "just works" while the user profile, env, and flags still win.
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

	// Effort tier (flag > env > default): seeds the loop budgets + churn from a
	// named intensity level, replacing the flat defaults. It does NOT set the
	// model — that's a separate axis (--model / KLOO_MODEL / profile). A per-tier
	// "efforts" override in the profile file adjusts the tier; env/flags/per-model
	// profile still win on top (below). medium == the legacy defaults, so an unset
	// effort changes nothing.
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
	cfg.MaxSteps = tier.MaxSteps
	cfg.ChurnRounds = tier.ChurnRounds
	cfg.MaxTokens = tier.MaxTokens
	cfg.MaxWallClockSeconds = tier.MaxWallClockSeconds

	// Provider axis (flag > env). A provider bundles an endpoint + bearer key
	// (+ its own model aliases) under a short name, so `--provider or --model dsv4`
	// fully describes where to send which model — decoupling the provider from the
	// model (the same model is served by many providers). The endpoint/key land at
	// the PROFILE layer here, so KLOO_ENDPOINT/KLOO_API_KEY and --endpoint still win
	// in the env/flag layers below.
	provider := getenv(EnvProvider)
	if flags.Provider != nil {
		provider = *flags.Provider
	}
	cfg.Provider = provider

	var providerModels map[string]modelEntry
	if provider != "" {
		providers, err := loadProviders(profilePath)
		if err != nil {
			return Config{}, err
		}
		p, ok := providers[provider]
		if !ok {
			return Config{}, fmt.Errorf("config: unknown --provider %q (define it under \"providers\" in the profile)", provider)
		}
		if p.Endpoint != "" {
			cfg.Endpoint = p.Endpoint
		}
		if p.APIKey != "" {
			cfg.APIKey = expandValue(p.APIKey)
		}
		providerModels = p.Models
	}

	// Resolve the model selector (flag > env > default). With a provider it is an
	// ALIAS looked up in that provider's "models" map (→ its real model id + tuning);
	// otherwise it is the model name itself, keying the legacy top-level per-model
	// entries. An unmatched selector is used verbatim as the model id, so
	// `--provider or --model gpt-4o` works even without an alias entry.
	modelSel := DefaultModel
	if v := getenv(EnvModel); v != "" {
		modelSel = v
	}
	if flags.Model != nil {
		modelSel = *flags.Model
	}
	cfg.Model = modelSel

	// Capture the user's per-model tuning entry (from EITHER the provider-alias
	// path OR the legacy top-level path) instead of applying it inline, so the
	// bundled-defaults layer can run between model-id resolution and user tuning.
	var userTuning *profileEntry
	if me, ok := providerModels[modelSel]; ok {
		if me.Model != "" {
			cfg.Model = me.Model // alias → real model id sent to the endpoint
		}
		e := me.profileEntry
		userTuning = &e
	} else {
		// Legacy / no-provider path: top-level per-model entry keyed by model name.
		entry, err := loadProfileEntry(profilePath, cfg.Model)
		if err != nil {
			return Config{}, err
		}
		userTuning = entry // may be nil
	}

	// BUNDLED defaults layer: below the user profile, above the flat built-ins.
	// Keyed by the *resolved* model id (post provider-alias), it overwrites only
	// the flat built-in fields (ToolFormat/Temperature/MaxContextTokens). An
	// unknown model gets the generic fallback (== built-ins ⇒ no change).
	applyBundledDefaults(&cfg, cfg.Model)

	// User profile tuning ALWAYS wins over bundled (non-nil fields overwrite).
	if userTuning != nil {
		applyModelTuning(&cfg, *userTuning)
	}

	// MCP servers + cap (reserved profile keys; never collide with model entries —
	// see loadMCPServers). The global enable/disable switch is applied in the
	// env/flag layers below so it honours flags > env > profile.
	servers, maxExposed, err := loadMCPServers(profilePath)
	if err != nil {
		return Config{}, err
	}
	cfg.MCPServers = servers
	if maxExposed > 0 {
		cfg.MCPMaxExposedTools = maxExposed
	} else {
		cfg.MCPMaxExposedTools = DefaultMCPMaxExposedTools
	}

	// Env layer (above profile).
	if v := getenv(EnvEndpoint); v != "" {
		cfg.Endpoint = v
	}
	if v := getenv(EnvMCP); v == "0" || strings.EqualFold(v, "false") {
		cfg.MCPDisabled = true
	}
	if v := getenv(EnvAPIKey); v != "" {
		cfg.APIKey = v // explicit env override beats a provider-supplied key
	} else if cfg.APIKey == "" {
		// Conventional fallback, only when nothing higher (provider/KLOO_API_KEY)
		// already set a key — so OPENAI_API_KEY in the shell can't silently clobber
		// an explicit provider key.
		if v := getenv(EnvAPIKeyOpenAI); v != "" {
			cfg.APIKey = v
		}
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
	if flags.NoMCP != nil { // flag wins over env+profile
		cfg.MCPDisabled = *flags.NoMCP
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

// loadMCPServers reads the reserved "mcpServers" block and the cap
// "mcp":{"maxExposedTools":N} from the profile file. It mirrors
// loadEffortOverride: a missing file/block ⇒ an empty map and 0, no error; a
// malformed file ⇒ an error wrapping ErrProfileParse. Both keys are objects, so
// they never break loadProfileEntry's map[string]profileEntry decode (a top-level
// *number* key would — which is exactly why the cap is nested under "mcp").
//
// Leading "~"/"~/" and "$VAR"/"${VAR}" in command/args/env/header values are
// expanded here (kloo runs stdio servers via a shell-less exec.Command, so the
// shell would otherwise never expand stdio values; HTTP header secrets need the
// same no-shell expansion). Header names are not expanded.
func loadMCPServers(profilePath string) (map[string]MCPServerEntry, int, error) {
	data, path, err := readProfileFile(profilePath)
	if err != nil || data == nil {
		return map[string]MCPServerEntry{}, 0, err
	}
	var file struct {
		MCPServers map[string]MCPServerEntry `json:"mcpServers"`
		MCP        struct {
			MaxExposedTools int `json:"maxExposedTools"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, 0, fmt.Errorf("config: %w %s: %v", ErrProfileParse, path, err)
	}
	servers := file.MCPServers
	if servers == nil {
		servers = map[string]MCPServerEntry{}
	}
	for name, e := range servers {
		e.Command = expandValue(e.Command)
		for i, a := range e.Args {
			e.Args[i] = expandValue(a)
		}
		for k, v := range e.Env {
			e.Env[k] = expandValue(v)
		}
		for k, v := range e.Headers {
			e.Headers[k] = expandValue(v)
		}
		servers[name] = e
	}
	return servers, file.MCP.MaxExposedTools, nil
}

// expandValue expands a config string the way a user expects from a shell, but
// without a shell: a *leading* "~" or "~/" becomes the user's home dir, then
// os.ExpandEnv resolves "$VAR"/"${VAR}". No globbing and no word-splitting — the
// result is forwarded literally to exec.Command. A non-leading "~" (e.g. "a~b")
// is left untouched. If the home dir can't be resolved, the "~" is left as-is.
func expandValue(s string) string {
	if s == "~" || strings.HasPrefix(s, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			s = home + s[1:]
		}
	}
	return os.ExpandEnv(s)
}

// defaultProfilePath resolves profiles.json from kloo's global home. As of the
// session feature that home is ~/.kloo (matching the {workspace}/.kloo scheme);
// the older XDG / ~/.config/kloo path is kept as a fallback for back-compat so
// existing installs keep working.
func defaultProfilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	// Preferred: ~/.kloo/profiles.json — use it when present.
	preferred := filepath.Join(home, ".kloo", "profiles.json")
	if _, err := os.Stat(preferred); err == nil {
		return preferred, nil
	}
	// Fallback: XDG, else legacy ~/.config/kloo (used only when it actually exists).
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "kloo", "profiles.json"), nil
	}
	legacy := filepath.Join(home, ".config", "kloo", "profiles.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	}
	// Neither exists: default to the preferred path (a missing profile is not an
	// error upstream — Resolve treats absent profiles as "use defaults").
	return preferred, nil
}
