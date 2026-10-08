package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/spf13/cobra"
)

type secretState struct {
	Set      bool `json:"set"`
	Redacted bool `json:"redacted"`
}

type profileDiagnostic struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// Source records HOW Path was chosen: "flag" when --profile was given, else
	// "search" (the first existing candidate) or "search-miss" (nothing exists
	// anywhere). Searched is the candidate list, in precedence order, and is set
	// only when --profile was NOT given — the one case where the user benefits from
	// seeing where kloo looked.
	Source   string   `json:"source"`
	Searched []string `json:"searched,omitempty"`
}

type commandDiagnostic struct {
	Command  string `json:"command"`
	Source   string `json:"source"`
	Advisory bool   `json:"advisory,omitempty"`
}

type mcpDiagnostic struct {
	Disabled          bool     `json:"disabled"`
	ConfiguredServers int      `json:"configured_servers"`
	EnabledServers    []string `json:"enabled_servers"`
	MaxExposedTools   int      `json:"max_exposed_tools"`
}

type retryDiagnostic struct {
	LLMMaxRetries        int    `json:"llm_max_retries"`
	LLMRetryCodes        []int  `json:"llm_retry_codes"`
	LLMRetryBaseDelay    string `json:"llm_retry_base_delay"`
	LLMRetryMaxDelay     string `json:"llm_retry_max_delay"`
	LLMColdLoadTimeout   string `json:"llm_cold_load_timeout"`
	LLMStreamIdleTimeout string `json:"llm_stream_idle_timeout"`
}

type memoryDiagnostic struct {
	Enabled        bool   `json:"enabled"`
	Server         string `json:"server,omitempty"`
	RecallTool     string `json:"recall_tool,omitempty"`
	StoreTool      string `json:"store_tool,omitempty"`
	MaxRecallBytes int    `json:"max_recall_bytes,omitempty"`
	StoreOnFailure bool   `json:"store_on_failure"`
}

type resolvedConfigDiagnostic struct {
	Profile        profileDiagnostic `json:"profile"`
	Provider       string            `json:"provider"`
	ProviderSource string            `json:"provider_source,omitempty"`
	Model          string            `json:"model"`
	Endpoint       string            `json:"endpoint"`
	APIKey         secretState       `json:"api_key"`
	Ctx            int               `json:"ctx"`
	Effort         string            `json:"effort"`
	MaxSteps       int               `json:"max_steps"`
	// Named max_RUN_tokens deliberately. This is the whole run's cumulative
	// ceiling, and printing it as "max_tokens" gave it the exact name of the
	// OpenAI per-request parameter — in the one command whose job is to remove
	// confusion about the resolved configuration. The per-request cap is printed
	// beside it as max_output_tokens.
	MaxRunTokens           int               `json:"max_run_tokens"`
	MaxRunTokensComputed   bool              `json:"max_run_tokens_computed"`
	MaxOutputTokens        int               `json:"max_output_tokens"`
	MaxWallClockSeconds    int               `json:"max_wall_clock_seconds"`
	ChurnRounds            int               `json:"churn_rounds"`
	RepeatNudgeRounds      int               `json:"repeat_nudge_rounds"`
	ExploreNudgeRounds     int               `json:"explore_nudge_rounds"`
	UsableWindowFrac       float64           `json:"usable_window_frac"`
	CompactTriggerFrac     float64           `json:"compact_trigger_frac"`
	UsablePromptTokens     int               `json:"usable_prompt_tokens"`
	CompactAtTokens        int               `json:"compact_at_tokens"`
	WorkingSetTokens       int               `json:"working_set_tokens"`
	SummaryBudgetFrac      float64           `json:"summary_budget_frac"`
	ExploreAbortRounds     int               `json:"explore_abort_rounds"`
	RepeatAbortRounds      int               `json:"repeat_abort_rounds"`
	Temperature            float64           `json:"temperature"`
	NoThink                bool              `json:"no_think"`
	ToolFormat             string            `json:"tool_format"`
	PromptCache            promptCacheDiag   `json:"prompt_cache"`
	Verify                 commandDiagnostic `json:"verify"`
	Lint                   commandDiagnostic `json:"lint"`
	MCP                    mcpDiagnostic     `json:"mcp"`
	Retry                  retryDiagnostic   `json:"retry"`
	Memory                 memoryDiagnostic  `json:"memory"`
	// MemoryGuard is the process-memory ceiling in force and the resident set right
	// now (agent/memguard.go). In doctor because a guard that can stop a run must be
	// inspectable before the run — and because kloo's previous OOM, at 44 GB, left no
	// record of anything whatsoever.
	MemoryGuard            memoryGuardDiagnostic `json:"memory_guard"`
	AllowedImportDirsCount int                   `json:"allowed_import_dirs_count"`
	AllowedEnvNames        []string          `json:"allowed_env_names"`
	PatchOnly              bool              `json:"patch_only"`
	Scope                  scopeDiagnostic   `json:"scope"`
	StopOn                 stopOnDiagnostic  `json:"stop_on"`
}

// promptCacheDiag reports the configured mode AND what it resolved to. The
// resolved half is the point: it tells a user why they are getting no cache hits
// without making them read the allowlist in source.
type promptCacheDiag struct {
	Mode     string `json:"mode"`
	Resolved bool   `json:"resolved"`
}

// memoryGuardDiagnostic reports the resident-memory ceiling. CeilingSource is here
// because a number whose provenance is invisible is precisely how this command came
// to print a working-set cap that held the prompt nowhere.
type memoryGuardDiagnostic struct {
	CeilingBytes  int64  `json:"ceiling_bytes"`
	CeilingSource string `json:"ceiling_source"` // "default" | "env" | "disabled"
	RSSBytes      uint64 `json:"rss_bytes"`
	// MapContentBudgetBytes is the aggregate cap on the repo-map content load
	// (agent/repomap_budget.go) — the bound that stops the pipeline that reproduces
	// the 44 GB kill. 0 means it has been disabled.
	MapContentBudgetBytes int64 `json:"map_content_budget_bytes"`
}

type scopeDiagnostic struct {
	Active   bool     `json:"active"`
	Allow    []string `json:"allow,omitempty"`
	Deny     []string `json:"deny,omitempty"`
	ReadOnly []string `json:"read_only,omitempty"`
}

type stopOnDiagnostic struct {
	OffScopeEdit   bool `json:"off_scope_edit"`
	ReadOnlyEdit   bool `json:"read_only_edit"`
	RepeatedVerify int  `json:"repeated_verify"`
}

func newDoctorCmd(deps *Deps) *cobra.Command {
	values := configFlagValues{}
	var flagVerify string
	var flagLint string
	var flagNoLint bool

	cmd := &cobra.Command{
		Use:           "doctor",
		Short:         "Print the resolved kloo configuration without starting a run",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			flags, err := buildConfigFlagsFromCommand(cmd, values)
			if err != nil {
				return err
			}
			cfg, err := config.Resolve(flags, deps.Getenv, values.Profile)
			if err != nil {
				return err
			}
			lopts := lintOptsFrom(cmd.Flags().Changed("lint"), cmd.Flags().Changed("no-lint"), flagLint, flagNoLint, deps.Getenv)
			diag := buildResolvedConfigDiagnostic(cfg, values.Profile, flagVerify, lopts)
			if values.JSON {
				return writeDoctorJSON(deps.Out, diag)
			}
			writeDoctorHuman(deps.Out, diag)
			return nil
		},
	}
	f := cmd.Flags()
	addConfigFlags(f, &values)
	f.StringVar(&flagVerify, "verify", "", "override kloo's auto-detected verify command")
	f.StringVar(&flagLint, "lint", "", "override kloo's auto-detected fast lint command")
	f.BoolVar(&flagNoLint, "no-lint", false, "disable the fast advisory lint step")
	return cmd
}

// doctorUsableFrac / doctorTriggerFrac report the fractions IN FORCE. Reporting
// the configured value would print 0 for an unset knob, which tells the reader
// nothing about the run — the same trap the repeat-rounds knobs had.
func doctorUsableFrac(cfg config.Config) float64 {
	u, _ := agent.ContextFractions()
	if cfg.UsableWindowFrac > 0 && cfg.UsableWindowFrac <= 1 {
		return cfg.UsableWindowFrac
	}
	return u
}

func doctorTriggerFrac(cfg config.Config) float64 {
	_, t := agent.ContextFractions()
	if cfg.CompactTriggerFrac > 0 && cfg.CompactTriggerFrac <= 1 {
		return cfg.CompactTriggerFrac
	}
	return t
}

// effectiveRepeatRounds resolves the repetition-rail knobs the way the loop does.
// Unlike ChurnRounds these have no config-level default: 0 means "use the agent
// package default" (the seam that keeps an unset config building an untuned Loop),
// so doctor must resolve the zero here or report a 0 that no run ever uses.
func effectiveRepeatRounds(configured, fallback int) int {
	if configured > 0 {
		return configured
	}
	return fallback
}

// onOff renders a resolved boolean as the on/off word the doctor line uses.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func buildResolvedConfigDiagnostic(cfg config.Config, profilePath, verifyOverride string, lint lintOpts) resolvedConfigDiagnostic {
	// Apply the configured fractions first so the numbers below describe the run
	// the user would actually get, not the built-in defaults.
	agent.SetContextFractions(cfg.UsableWindowFrac, cfg.CompactTriggerFrac)
	agent.SetWorkingSetTokens(cfg.WorkingSetTokens)
	if cfg.SummaryBudgetFrac != 0 {
		agent.SetSummaryBudgetFrac(cfg.SummaryBudgetFrac)
	}
	cwd, _ := os.Getwd()
	path := profilePath
	source := "flag"
	var searched []string
	if path == "" {
		var found string
		found, searched = config.FindProfile(cwd)
		if found != "" {
			path, source = found, "search"
		} else {
			source = "search-miss"
			if p, err := config.DefaultProfilePathForDiagnostics(); err == nil {
				path = p
			}
		}
	}
	exists := false
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			exists = true
		}
	}
	verify := commandDiagnostic{Command: strings.TrimSpace(verifyOverride), Source: "override"}
	if verify.Command == "" {
		if cmd := detectVerify(cwd); cmd != "" {
			verify = commandDiagnostic{Command: cmd, Source: "auto-detect"}
		} else {
			verify = commandDiagnostic{Source: "none"}
		}
	}
	lintDiag := commandDiagnostic{Advisory: true, Source: "none"}
	switch {
	case lint.Disabled:
		lintDiag.Source = "disabled"
	case strings.TrimSpace(lint.Override) != "":
		lintDiag.Command = lint.Override
		lintDiag.Source = "override"
	default:
		if lc := detectLint(cwd); lc.Command != "" {
			lintDiag.Command = lc.Command
			lintDiag.Source = "auto-detect"
		}
	}
	enabled := make([]string, 0, len(cfg.MCPServers))
	for name, srv := range cfg.MCPServers {
		if !srv.Disabled {
			enabled = append(enabled, name)
		}
	}
	sort.Strings(enabled)
	allowedEnv := append([]string(nil), cfg.AllowedEnv...)
	sort.Strings(allowedEnv)
	// Resolve the effective scope (CLI flags overlaid on .kloo/scope.yaml under cwd),
	// so doctor reflects exactly what a run would enforce. A manifest error degrades
	// to "no scope" rather than failing the diagnostic.
	scopeDiag := scopeDiagnostic{}
	if sc, err := config.ResolveScope(config.ScopeFlags{Allow: cfg.ScopeAllow, Deny: cfg.ScopeDeny, ReadOnly: cfg.ScopeReadOnly}, cwd); err == nil {
		scopeDiag = scopeDiagnostic{Active: sc.Active(), Allow: sc.Allow, Deny: sc.Deny, ReadOnly: sc.ReadOnly}
	}
	return resolvedConfigDiagnostic{
		Profile:              profileDiagnostic{Path: path, Exists: exists, Source: source, Searched: searched},
		Provider:             cfg.Provider,
		ProviderSource:       cfg.ProviderSource,
		Model:                cfg.Model,
		Endpoint:             cfg.Endpoint,
		APIKey:               secretState{Set: cfg.APIKey != "", Redacted: cfg.APIKey != ""},
		Ctx:                  cfg.MaxContextTokens,
		Effort:               cfg.Effort,
		MaxSteps:             cfg.MaxSteps,
		MaxRunTokens:         cfg.MaxTokens,
		MaxRunTokensComputed: cfg.MaxTokensComputed,
		MaxOutputTokens:      cfg.MaxOutputTokens,
		MaxWallClockSeconds:  cfg.MaxWallClockSeconds,
		ChurnRounds:          cfg.ChurnRounds,
		RepeatNudgeRounds:    effectiveRepeatRounds(cfg.RepeatNudgeRounds, agent.DefaultRepeatNudgeRounds),
		ExploreNudgeRounds:   effectiveRepeatRounds(cfg.ExploreNudgeRounds, agent.DefaultExploreNudgeRounds),
		UsableWindowFrac:     doctorUsableFrac(cfg),
		CompactTriggerFrac:   doctorTriggerFrac(cfg),
		UsablePromptTokens:   agent.UsableWindow(cfg.MaxContextTokens),
		CompactAtTokens:      agent.CompactTriggerTokens(agent.UsableWindow(cfg.MaxContextTokens)),
		// The EFFECTIVE cap at this window, not the base constant: at the default the
		// cap follows the window, so reporting the base told the reader a number that
		// is not the one holding their prompt.
		WorkingSetTokens:   agent.WorkingSetTokensFor(agent.UsableWindow(cfg.MaxContextTokens)),
		SummaryBudgetFrac:  agent.SummaryBudgetFrac(),
		ExploreAbortRounds: effectiveRepeatRounds(cfg.ExploreAbortRounds, agent.DefaultExploreAbortRounds),
		RepeatAbortRounds:  effectiveRepeatRounds(cfg.RepeatAbortRounds, agent.DefaultRepeatAbortRounds),
		Temperature:        cfg.Temperature,
		NoThink:            cfg.NoThink,
		ToolFormat:         cfg.ToolFormat,
		PromptCache:        promptCacheDiag{Mode: cfg.PromptCache, Resolved: cfg.PromptCacheEnabled()},
		Verify:             verify,
		Lint:               lintDiag,
		MCP:                mcpDiagnostic{Disabled: cfg.MCPDisabled, ConfiguredServers: len(cfg.MCPServers), EnabledServers: enabled, MaxExposedTools: cfg.MCPMaxExposedTools},
		Retry: retryDiagnostic{
			LLMMaxRetries:        cfg.LLMMaxRetries,
			LLMRetryCodes:        append([]int(nil), cfg.LLMRetryableStatusCodes...),
			LLMRetryBaseDelay:    formatDoctorDuration(cfg.LLMRetryBaseDelay),
			LLMRetryMaxDelay:     formatDoctorDuration(cfg.LLMRetryMaxDelay),
			LLMColdLoadTimeout:   formatDoctorDuration(cfg.LLMColdLoadTimeout),
			LLMStreamIdleTimeout: formatDoctorDuration(cfg.LLMStreamIdleTimeout),
		},
		Memory: memoryDiagnostic{
			Enabled:        cfg.Memory.Enabled,
			Server:         cfg.Memory.Server,
			RecallTool:     cfg.Memory.RecallTool,
			StoreTool:      cfg.Memory.StoreTool,
			MaxRecallBytes: cfg.Memory.MaxRecallBytes,
			StoreOnFailure: cfg.Memory.StoreOnFailure,
		},
		MemoryGuard: memoryGuardDiagnostic{
			CeilingBytes:          agent.MemCeilingBytes(),
			CeilingSource:         agent.MemCeilingSource(),
			RSSBytes:              agent.ProcessRSSBytes(),
			MapContentBudgetBytes: agent.RepoMapContentBudgetBytes(),
		},
		AllowedImportDirsCount: len(cfg.AllowedImportDirs),
		AllowedEnvNames:        allowedEnv,
		PatchOnly:              cfg.PatchOnly,
		Scope:                  scopeDiag,
		StopOn: stopOnDiagnostic{
			OffScopeEdit:   cfg.StopOn.OffScopeEdit,
			ReadOnlyEdit:   cfg.StopOn.ReadOnlyEdit,
			RepeatedVerify: cfg.StopOn.RepeatedVerify,
		},
	}
}

func formatDoctorDuration(d time.Duration) string {
	s := d.String()
	for strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	for strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	if s == "" {
		return "0s"
	}
	return s
}

func writeDoctorJSON(out io.Writer, diag resolvedConfigDiagnostic) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	return enc.Encode(diag)
}

func writeDoctorHuman(out io.Writer, diag resolvedConfigDiagnostic) {
	fmt.Fprintln(out, "kloo doctor")
	fmt.Fprintf(out, "profile: %s (exists=%t, source=%s)\n", diag.Profile.Path, diag.Profile.Exists, diag.Profile.Source)
	// Only on a total miss is the candidate list worth the screen space: that is
	// when the user has to decide WHERE to put a profile, and guessing is the thing
	// this output exists to prevent.
	if diag.Profile.Source == "search-miss" {
		fmt.Fprintf(out, "profile searched (none found, in order):\n")
		for _, p := range diag.Profile.Searched {
			fmt.Fprintf(out, "  %s\n", p)
		}
	}
	if diag.Provider != "" && diag.ProviderSource != "" {
		fmt.Fprintf(out, "provider: %s (source=%s)\n", diag.Provider, diag.ProviderSource)
	} else {
		fmt.Fprintf(out, "provider: %s\n", diag.Provider)
	}
	fmt.Fprintf(out, "model: %s\n", diag.Model)
	fmt.Fprintf(out, "endpoint: %s\n", diag.Endpoint)
	if diag.APIKey.Set {
		fmt.Fprintln(out, "api_key: set (redacted)")
	} else {
		fmt.Fprintln(out, "api_key: unset")
	}
	fmt.Fprintf(out, "ctx: %d\n", diag.Ctx)
	fmt.Fprintf(out, "effort: %s\n", diag.Effort)
	fmt.Fprintf(out, "max_steps: %d\n", diag.MaxSteps)
	switch {
	case diag.MaxRunTokens <= 0:
		fmt.Fprintf(out, "max_run_tokens: unbounded\n")
	case diag.MaxRunTokensComputed:
		fmt.Fprintf(out, "max_run_tokens: %d since the last file change (computed; a run that keeps editing is never stopped by it)\n", diag.MaxRunTokens)
	default:
		fmt.Fprintf(out, "max_run_tokens: %d cumulative for the whole run (explicit)\n", diag.MaxRunTokens)
	}
	switch {
	case diag.MaxOutputTokens > 0:
		fmt.Fprintf(out, "max_output_tokens: %d (per-request, fixed)\n", diag.MaxOutputTokens)
	case diag.MaxOutputTokens < 0:
		fmt.Fprintf(out, "max_output_tokens: computed per request (window - estimated prompt - slack)\n")
	default:
		fmt.Fprintf(out, "max_output_tokens: not sent (default)\n")
	}
	fmt.Fprintf(out, "max_wall_clock_seconds: %d\n", diag.MaxWallClockSeconds)
	fmt.Fprintf(out, "churn_rounds: %d\n", diag.ChurnRounds)
	fmt.Fprintf(out, "repeat_rounds: nudge=%d abort=%d\n", diag.RepeatNudgeRounds, diag.RepeatAbortRounds)
	fmt.Fprintf(out, "explore_rounds: nudge=%d abort=%d\n", diag.ExploreNudgeRounds, diag.ExploreAbortRounds)
	// Spell the multiplication out: a declared --ctx is NOT what kloo works in, and
	// that was previously invisible — which made every cross-driver context
	// comparison wrong without anyone being able to see why.
	fmt.Fprintf(out, "context: declared=%d usable=%d (%.2f) compact_at=%d (%.2f) => %.0f%% of declared\n",
		diag.Ctx, diag.UsablePromptTokens, diag.UsableWindowFrac,
		diag.CompactAtTokens, diag.CompactTriggerFrac,
		100*float64(diag.CompactAtTokens)/float64(max(diag.Ctx, 1)))
	// compact_at now has two possible sources, and which one is binding changes the
	// behaviour completely — so name it rather than leaving the reader to multiply
	// the fractions and wonder why the product does not match.
	switch {
	case diag.WorkingSetTokens <= 0:
		fmt.Fprintf(out, "working_set: disabled (compact_at is the fraction alone)\n")
	case diag.CompactAtTokens >= diag.WorkingSetTokens:
		fmt.Fprintf(out, "working_set: %d tokens (BINDING — holds the prompt here instead of %d)\n",
			diag.WorkingSetTokens, int(diag.CompactTriggerFrac*float64(diag.UsablePromptTokens)))
	default:
		fmt.Fprintf(out, "working_set: %d tokens (not binding — the fraction is tighter)\n", diag.WorkingSetTokens)
	}
	if f := diag.SummaryBudgetFrac; f <= 0 {
		fmt.Fprintf(out, "summary_budget: disabled (the running summary is unbounded)\n")
	} else {
		fmt.Fprintf(out, "summary_budget: %.2f of compact_at = %d tokens\n", f, int(f*float64(diag.CompactAtTokens)))
	}
	fmt.Fprintf(out, "temperature: %g\n", diag.Temperature)
	fmt.Fprintf(out, "no_think: %t\n", diag.NoThink)
	fmt.Fprintf(out, "tool_format: %s\n", diag.ToolFormat)
	fmt.Fprintf(out, "prompt_cache: %s (resolved=%s)\n", noneDash(diag.PromptCache.Mode), onOff(diag.PromptCache.Resolved))
	fmt.Fprintf(out, "verify: %s (source=%s)\n", noneDash(diag.Verify.Command), diag.Verify.Source)
	fmt.Fprintf(out, "lint: %s (source=%s, advisory=true)\n", noneDash(diag.Lint.Command), diag.Lint.Source)
	fmt.Fprintf(out, "mcp: enabled=%t servers=%d enabled_names=%q max_exposed_tools=%d\n",
		!diag.MCP.Disabled, diag.MCP.ConfiguredServers, diag.MCP.EnabledServers, diag.MCP.MaxExposedTools)
	fmt.Fprintf(out, "retry: max=%d codes=%v base=%s max_delay=%s cold_load=%s stream_idle=%s\n",
		diag.Retry.LLMMaxRetries, diag.Retry.LLMRetryCodes, diag.Retry.LLMRetryBaseDelay,
		diag.Retry.LLMRetryMaxDelay, diag.Retry.LLMColdLoadTimeout, diag.Retry.LLMStreamIdleTimeout)
	fmt.Fprintf(out, "memory: enabled=%t server=%s recall=%s store=%s max_recall_bytes=%d store_on_failure=%t\n",
		diag.Memory.Enabled, noneDash(diag.Memory.Server), noneDash(diag.Memory.RecallTool),
		noneDash(diag.Memory.StoreTool), diag.Memory.MaxRecallBytes, diag.Memory.StoreOnFailure)
	// The real default, stated as such. This file has a history of claiming "off by
	// default" for rails that shipped on, so the line names the SOURCE of the number
	// rather than describing the feature.
	switch mg := diag.MemoryGuard; {
	case mg.CeilingBytes <= 0:
		fmt.Fprintf(out, "memory_guard: DISABLED (rss %s now; nothing stops kloo before the kernel's OOM killer)\n",
			humanBytesCLI(mg.RSSBytes))
	default:
		fmt.Fprintf(out, "memory_guard: ceiling %s (source=%s), rss %s now — kloo stops itself above the ceiling\n",
			humanBytesCLI(uint64(mg.CeilingBytes)), mg.CeilingSource, humanBytesCLI(mg.RSSBytes))
	}
	if b := diag.MemoryGuard.MapContentBudgetBytes; b <= 0 {
		fmt.Fprintf(out, "map_content_budget: disabled (the repo-map content load is unbounded — the pre-v0.26 behaviour)\n")
	} else {
		fmt.Fprintf(out, "map_content_budget: %s of workspace source per map assembly\n", humanBytesCLI(uint64(b)))
	}
	fmt.Fprintf(out, "allowed_import_dirs: %d\n", diag.AllowedImportDirsCount)
	fmt.Fprintf(out, "allowed_env: %d names=%q\n", len(diag.AllowedEnvNames), diag.AllowedEnvNames)
	fmt.Fprintf(out, "patch_only: %t\n", diag.PatchOnly)
	fmt.Fprintf(out, "scope: active=%t allow=%v deny=%v read_only=%v\n",
		diag.Scope.Active, diag.Scope.Allow, diag.Scope.Deny, diag.Scope.ReadOnly)
	fmt.Fprintf(out, "stop_on: off_scope_edit=%t read_only_edit=%t repeated_verify=%d\n",
		diag.StopOn.OffScopeEdit, diag.StopOn.ReadOnlyEdit, diag.StopOn.RepeatedVerify)
}

func noneDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}
