package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

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
	Profile                profileDiagnostic `json:"profile"`
	Provider               string            `json:"provider"`
	Model                  string            `json:"model"`
	Endpoint               string            `json:"endpoint"`
	APIKey                 secretState       `json:"api_key"`
	Ctx                    int               `json:"ctx"`
	Effort                 string            `json:"effort"`
	MaxSteps               int               `json:"max_steps"`
	MaxTokens              int               `json:"max_tokens"`
	MaxWallClockSeconds    int               `json:"max_wall_clock_seconds"`
	ChurnRounds            int               `json:"churn_rounds"`
	Temperature            float64           `json:"temperature"`
	NoThink                bool              `json:"no_think"`
	ToolFormat             string            `json:"tool_format"`
	Verify                 commandDiagnostic `json:"verify"`
	Lint                   commandDiagnostic `json:"lint"`
	MCP                    mcpDiagnostic     `json:"mcp"`
	Retry                  retryDiagnostic   `json:"retry"`
	Memory                 memoryDiagnostic  `json:"memory"`
	AllowedImportDirsCount int               `json:"allowed_import_dirs_count"`
	AllowedEnvNames        []string          `json:"allowed_env_names"`
	PatchOnly              bool              `json:"patch_only"`
	Scope                  scopeDiagnostic   `json:"scope"`
	StopOn                 stopOnDiagnostic  `json:"stop_on"`
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

func buildResolvedConfigDiagnostic(cfg config.Config, profilePath, verifyOverride string, lint lintOpts) resolvedConfigDiagnostic {
	path := profilePath
	if path == "" {
		if p, err := config.DefaultProfilePathForDiagnostics(); err == nil {
			path = p
		}
	}
	exists := false
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			exists = true
		}
	}
	cwd, _ := os.Getwd()
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
		Profile:             profileDiagnostic{Path: path, Exists: exists},
		Provider:            cfg.Provider,
		Model:               cfg.Model,
		Endpoint:            cfg.Endpoint,
		APIKey:              secretState{Set: cfg.APIKey != "", Redacted: cfg.APIKey != ""},
		Ctx:                 cfg.MaxContextTokens,
		Effort:              cfg.Effort,
		MaxSteps:            cfg.MaxSteps,
		MaxTokens:           cfg.MaxTokens,
		MaxWallClockSeconds: cfg.MaxWallClockSeconds,
		ChurnRounds:         cfg.ChurnRounds,
		Temperature:         cfg.Temperature,
		NoThink:             cfg.NoThink,
		ToolFormat:          cfg.ToolFormat,
		Verify:              verify,
		Lint:                lintDiag,
		MCP:                 mcpDiagnostic{Disabled: cfg.MCPDisabled, ConfiguredServers: len(cfg.MCPServers), EnabledServers: enabled, MaxExposedTools: cfg.MCPMaxExposedTools},
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
	fmt.Fprintf(out, "profile: %s (exists=%t)\n", diag.Profile.Path, diag.Profile.Exists)
	fmt.Fprintf(out, "provider: %s\n", diag.Provider)
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
	fmt.Fprintf(out, "max_tokens: %d\n", diag.MaxTokens)
	fmt.Fprintf(out, "max_wall_clock_seconds: %d\n", diag.MaxWallClockSeconds)
	fmt.Fprintf(out, "churn_rounds: %d\n", diag.ChurnRounds)
	fmt.Fprintf(out, "temperature: %g\n", diag.Temperature)
	fmt.Fprintf(out, "no_think: %t\n", diag.NoThink)
	fmt.Fprintf(out, "tool_format: %s\n", diag.ToolFormat)
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
