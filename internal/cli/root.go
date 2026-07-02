// Package cli wires the cobra root command and flag parsing for kloo.
//
// The root parses the task argument + core flags, resolves config
// (internal/config), and routes to either the interactive TUI (no task) or the
// non-interactive autonomous loop (task).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// SessionOpts is the user's session choice for an interactive launch: --new forces
// a fresh session, --resume <id> resumes a specific one; both unset uses the
// default policy (resume the single session, prompt when several, new when none).
type SessionOpts struct {
	New      bool
	ResumeID string
}

// lintOpts is the user's fast-advisory-lint configuration for a run, built from the
// --lint/--no-lint flags with KLOO_LINT/KLOO_NO_LINT as env fallbacks (flag beats
// env). resolveLintCommand turns it into the effective lint command. It is threaded
// into the entry points in Phase 02; this phase only parses and resolves it.
type lintOpts struct {
	Override string // --lint / KLOO_LINT — explicit command, wins over detection
	Disabled bool   // --no-lint / KLOO_NO_LINT (or KLOO_LINT "0"/"false") — forces lint off
}

// lintOptsFrom builds lintOpts from the flag state and env, mirroring the verify/MCP
// flag-beats-env shape. lintChanged/noLintChanged are the cobra Changed() bits for
// --lint/--no-lint. A changed flag wins over env; otherwise KLOO_LINT supplies the
// override and KLOO_NO_LINT (truthy) or KLOO_LINT ("0"/"false") disables.
func lintOptsFrom(lintChanged, noLintChanged bool, flagLint string, flagNoLint bool, getenv func(string) string) lintOpts {
	var o lintOpts

	switch {
	case lintChanged:
		o.Override = flagLint // explicit --lint wins over env
	default:
		if v := getenv(config.EnvLint); v != "" && !envLintDisables(v) {
			o.Override = v
		}
	}

	switch {
	case noLintChanged:
		o.Disabled = flagNoLint // explicit --no-lint (true or false) wins over env
	default:
		o.Disabled = envTruthy(getenv(config.EnvNoLint)) || envLintDisables(getenv(config.EnvLint))
	}

	return o
}

// envLintDisables mirrors the EnvMCP convention: "0"/"false" (case-insensitive)
// means "off". Used for KLOO_LINT, where such a value disables lint rather than
// being treated as a command.
func envLintDisables(v string) bool {
	return v == "0" || strings.EqualFold(v, "false")
}

// envTruthy reports whether v is an affirmative env value ("1"/"true"), used for
// the KLOO_NO_LINT disable switch.
func envTruthy(v string) bool {
	return v == "1" || strings.EqualFold(v, "true")
}

// Deps are the injectable dependencies of the root command, so tests can run
// offline (fake client) and capture output.
type Deps struct {
	// LaunchTUI starts the interactive TUI session (the autonomous loop wired
	// under the Bubble Tea UI). sess carries the user's session choice (--new /
	// --resume); lint carries the fast-advisory-lint config (--lint/--no-lint +
	// env). Injected so tests stay offline.
	LaunchTUI func(cfg config.Config, baseFlags config.Flags, verifyCmd string, lint lintOpts, sess SessionOpts, profilePath string, getenv func(string) string) error
	// RunHeadless runs the autonomous loop NON-interactively (no TTY), streaming
	// progress to out and returning the loop's terminal report. Used for the
	// Phase-06 acceptance benchmark and any scripted/CI autonomous run. lint carries
	// the fast-advisory-lint config. Injected so tests stay offline.
	RunHeadless func(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error
	// Getenv looks up env vars (defaults to os.Getenv).
	Getenv func(string) string
	Out    io.Writer
	Err    io.Writer
}

func (d *Deps) withDefaults() {
	if d.LaunchTUI == nil {
		d.LaunchTUI = defaultLaunchTUI
	}
	if d.RunHeadless == nil {
		d.RunHeadless = defaultRunHeadless
	}
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	if d.Out == nil {
		d.Out = os.Stdout
	}
	if d.Err == nil {
		d.Err = os.Stderr
	}
}

// NewRootCmd builds the kloo root command with the given dependencies.
func NewRootCmd(deps Deps) *cobra.Command {
	deps.withDefaults()

	var (
		flagModel       string
		flagProvider    string
		flagEndpoint    string
		flagMode        string
		flagProfile     string
		flagMaxSteps    int
		flagTemp        float64
		flagVerify      string
		flagBenchmark   bool
		flagEffort      string
		flagNewSess     bool
		flagResume      string
		flagNoMCP       bool
		flagLint        string
		flagNoLint      bool
		flagCtx         int
		flagAllowedDirs []string
		flagAllowEnv    []string
		flagJSON        bool
		flagJSONOnly    bool
		flagStatusFile  string
		flagNoThink     bool
		flagAllow       []string
		flagDeny        []string
		flagReadOnly    []string
		flagPatchOnly   bool
		flagStopOn      []string
		flagPrecheck    []string
		flagPostcheck   []string
		flagRetryCodes  []int
		flagRetryBase   time.Duration
		flagRetryMax    time.Duration
		flagColdLoad    time.Duration
		flagStreamIdle  time.Duration
		flagMaxRetries  int
	)

	cmd := &cobra.Command{
		Use:   "kloo [task]",
		Short: "Autonomous coding CLI for small local LLMs",
		Long: "kloo drives any OpenAI-compatible endpoint (llama.cpp, Ollama, vLLM, " +
			"OpenAI, OpenRouter, …) to edit and verify code autonomously.\n\n" +
			"Launch with no task for the interactive TUI session:  kloo\n" +
			"Or pass a task to run the autonomous loop non-interactively:  kloo \"fix the test\"",
		Args:          cobra.MaximumNArgs(1),
		Version:       versionString(), // enables `kloo --version`; stamped by goreleaser ldflags
		SilenceErrors: true,            // Execute() prints errors; avoids double-printing
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Build the override set: only flags the user actually changed win
			// over env/profile/defaults (config.Flags fields stay nil otherwise).
			flags := config.Flags{}
			fs := cmd.Flags()
			if fs.Changed("effort") {
				if !config.IsEffort(flagEffort) {
					return fmt.Errorf("invalid --effort %q (want one of: %s)", flagEffort, strings.Join(config.EffortNames(), ", "))
				}
				flags.Effort = &flagEffort
			}
			if fs.Changed("model") {
				flags.Model = &flagModel
			}
			if fs.Changed("provider") {
				flags.Provider = &flagProvider
			}
			if fs.Changed("endpoint") {
				flags.Endpoint = &flagEndpoint
			}
			if fs.Changed("mode") {
				flags.Mode = &flagMode
			}
			if fs.Changed("max-steps") {
				flags.MaxSteps = &flagMaxSteps
			}
			if fs.Changed("ctx") {
				flags.MaxContextTokens = &flagCtx
			}
			if fs.Changed("temperature") {
				flags.Temperature = &flagTemp
			}
			if fs.Changed("no-mcp") {
				flags.NoMCP = &flagNoMCP // flag beats KLOO_MCP env and the profile
			}
			if fs.Changed("allowed-dirs") {
				flags.AllowedImportDirs = flagAllowedDirs
			}
			if fs.Changed("allow-env") {
				flags.AllowedEnv = flagAllowEnv
			}
			if fs.Changed("json") {
				flags.JSONSummary = &flagJSON
			}
			if fs.Changed("json-only") {
				flags.JSONOnly = &flagJSONOnly
			}
			if fs.Changed("status-file") {
				flags.StatusFile = &flagStatusFile
			}
			if fs.Changed("no-think") {
				flags.NoThink = &flagNoThink
			}
			if fs.Changed("allow") {
				flags.ScopeAllow = flagAllow
			}
			if fs.Changed("deny") {
				flags.ScopeDeny = flagDeny
			}
			if fs.Changed("read-only") {
				flags.ScopeReadOnly = flagReadOnly
			}
			if fs.Changed("patch-only") {
				flags.PatchOnly = &flagPatchOnly
			}
			if fs.Changed("stop-on") {
				flags.StopOn = flagStopOn
			}
			if fs.Changed("precheck") {
				flags.Prechecks = flagPrecheck
			}
			if fs.Changed("postcheck") {
				flags.Postchecks = flagPostcheck
			}
			if fs.Changed("benchmark") {
				flags.BenchmarkMode = &flagBenchmark
			}
			if fs.Changed("llm-max-retries") {
				flags.LLMMaxRetries = &flagMaxRetries
			}
			if fs.Changed("llm-retry-codes") {
				flags.LLMRetryableStatusCodes = flagRetryCodes
			}
			if fs.Changed("llm-retry-base-delay") {
				flags.LLMRetryBaseDelay = &flagRetryBase
			}
			if fs.Changed("llm-retry-max-delay") {
				flags.LLMRetryMaxDelay = &flagRetryMax
			}
			if fs.Changed("llm-cold-load-timeout") {
				flags.LLMColdLoadTimeout = &flagColdLoad
			}
			if fs.Changed("llm-stream-idle-timeout") {
				flags.LLMStreamIdleTimeout = &flagStreamIdle
			}

			cfg, err := config.Resolve(flags, deps.Getenv, flagProfile)
			if err != nil {
				if flagBenchmark {
					return exitError{code: benchmarkExitConfigError, err: err}
				}
				return err
			}

			// Fast-advisory-lint knobs (--lint/--no-lint + KLOO_LINT/KLOO_NO_LINT),
			// threaded into the entry points alongside verifyCmd; resolved to a
			// command (or nil linter) downstream in defaultLaunchTUI/defaultRunHeadless.
			lopts := lintOptsFrom(fs.Changed("lint"), fs.Changed("no-lint"), flagLint, flagNoLint, deps.Getenv)

			if len(args) == 0 {
				if cfg.BenchmarkMode {
					return exitError{code: benchmarkExitConfigError, err: fmt.Errorf("--benchmark requires a task argument")}
				}
				// No task argument → launch the interactive TUI session (the
				// autonomous loop under the Bubble Tea UI).
				return deps.LaunchTUI(cfg, flags, flagVerify, lopts, SessionOpts{New: flagNewSess, ResumeID: flagResume}, flagProfile, deps.Getenv)
			}

			return deps.RunHeadless(cfg, args[0], flagVerify, lopts, deps.Out)
		},
	}

	f := cmd.Flags()
	f.StringVar(&flagEffort, "effort", config.DefaultEffort, "effort tier (fast|medium|heavy) — seeds step/token budgets + churn patience")
	f.StringVar(&flagModel, "model", config.DefaultModel, "model your endpoint serves (e.g. qwen2.5-coder, gpt-4o); with --provider, a model alias from the profile")
	f.StringVar(&flagProvider, "provider", "", "named provider from the profile's \"providers\" block (sets endpoint+key; scopes --model alias lookup)")
	f.StringVar(&flagEndpoint, "endpoint", config.DefaultEndpoint, "OpenAI-compatible base URL")
	f.StringVar(&flagMode, "mode", config.DefaultMode, "run mode (auto|manual)")
	f.StringVar(&flagProfile, "profile", "", "path to profiles.json (default ~/.config/kloo/profiles.json)")
	f.IntVar(&flagMaxSteps, "max-steps", config.DefaultMaxSteps, "max autonomous steps")
	f.IntVar(&flagCtx, "ctx", config.DefaultMaxContextTokens, "per-step context window (match your server's -c; needed for a llama-swap/Ollama alias the bundled defaults can't size)")
	f.Float64Var(&flagTemp, "temperature", config.DefaultTemperature, "sampling temperature")
	f.StringVar(&flagVerify, "verify", "", "override kloo's auto-detected verify command; when unset, kloo infers the project's build/test")
	f.BoolVar(&flagBenchmark, "benchmark", false, "automation preset: run task loop with JSON summary and stable benchmark exit codes")
	f.BoolVar(&flagNewSess, "new", false, "start a fresh session (the default; sessions are no longer auto-resumed)")
	f.StringVar(&flagResume, "resume", "", "resume a specific saved session by id (printed on exit; see {workspace}/.kloo/sessions)")
	f.BoolVar(&flagNoMCP, "no-mcp", false, "disable all MCP servers for this run (overrides KLOO_MCP and the profile's mcpServers)")
	f.StringVar(&flagLint, "lint", "", "override kloo's auto-detected fast lint command (advisory; runs on edited files after each edit)")
	f.BoolVar(&flagNoLint, "no-lint", false, "disable the fast advisory lint step (lint is on by default when a linter is detected)")
	f.StringSliceVar(&flagAllowedDirs, "allowed-dirs", nil, "dirs OUTSIDE the workspace that AGENTS.md @import may read from (repeatable/comma-separated; read-only, load-time only)")
	f.StringSliceVar(&flagAllowEnv, "allow-env", nil, "env var NAMES to forward from kloo's env into run_command (repeatable/comma-separated) — the trusted-secret passthrough for a deploy/CI step; default exposes only PATH/HOME/…")
	f.BoolVar(&flagJSON, "json", false, "emit a compact machine-readable JSON result line at the end (model/reason/steps/tokens/tokens-per-sec/verify/error) for benchmarking")
	f.BoolVar(&flagJSONOnly, "json-only", false, "require the final assistant answer to be valid JSON only")
	f.StringVar(&flagStatusFile, "status-file", "", "write the run summary JSON to this path after a visible TUI run completes")
	f.BoolVar(&flagNoThink, "no-think", false, "ask compatible OpenAI-style backends to disable thinking/reasoning for chat requests")
	f.StringSliceVar(&flagAllow, "allow", nil, "glob(s) the model MAY edit (repeatable/comma-separated); empty ⇒ all in-jail files unless narrowed. Any scope flag disables model-facing run_command")
	f.StringSliceVar(&flagDeny, "deny", nil, "glob(s) the model may NOT edit (repeatable/comma-separated); deny wins over --allow")
	f.StringSliceVar(&flagReadOnly, "read-only", nil, "glob(s) the model may READ but not edit (repeatable/comma-separated); wins over --allow")
	f.BoolVar(&flagPatchOnly, "patch-only", false, "restrict model changes to edit_file/write_file exact edits; withhold model-facing run_command")
	f.StringSliceVar(&flagStopOn, "stop-on", nil, "hard-stop rule(s) (repeatable/comma-separated): off-scope-edit, read-only-edit, repeated-verify=N")
	f.StringArrayVar(&flagPrecheck, "precheck", nil, "harness command run BEFORE verify each turn (repeatable); a failure blocks verify/postcheck and is non-success. Verify stays the only success signal")
	f.StringArrayVar(&flagPostcheck, "postcheck", nil, "harness command run AFTER a passing verify (repeatable); a failure is non-success even though verify passed")
	f.IntVar(&flagMaxRetries, "llm-max-retries", config.DefaultLLMMaxRetries, "extra model-call retry attempts after the first")
	f.IntSliceVar(&flagRetryCodes, "llm-retry-codes", config.DefaultLLMRetryableStatusCodes, "HTTP status codes retryable for model calls")
	f.DurationVar(&flagRetryBase, "llm-retry-base-delay", config.DefaultLLMRetryBaseDelay, "first model-call retry backoff")
	f.DurationVar(&flagRetryMax, "llm-retry-max-delay", config.DefaultLLMRetryMaxDelay, "maximum model-call retry backoff")
	f.DurationVar(&flagColdLoad, "llm-cold-load-timeout", config.DefaultLLMColdLoadTimeout, "non-streaming model call timeout")
	f.DurationVar(&flagStreamIdle, "llm-stream-idle-timeout", config.DefaultLLMStreamIdle, "streaming no-token idle timeout")

	cmd.AddCommand(newDoctorCmd(&deps))
	cmd.AddCommand(newProbeCmd(&deps))
	cmd.SetVersionTemplate("kloo {{.Version}}\n")
	return cmd
}

func buildConfigFlagsFromCommand(cmd *cobra.Command, values configFlagValues) (config.Flags, error) {
	flags := config.Flags{}
	fs := cmd.Flags()
	if fs.Changed("effort") {
		if !config.IsEffort(values.Effort) {
			return flags, fmt.Errorf("invalid --effort %q (want one of: %s)", values.Effort, strings.Join(config.EffortNames(), ", "))
		}
		flags.Effort = &values.Effort
	}
	if fs.Changed("model") {
		flags.Model = &values.Model
	}
	if fs.Changed("provider") {
		flags.Provider = &values.Provider
	}
	if fs.Changed("endpoint") {
		flags.Endpoint = &values.Endpoint
	}
	if fs.Changed("mode") {
		flags.Mode = &values.Mode
	}
	if fs.Changed("max-steps") {
		flags.MaxSteps = &values.MaxSteps
	}
	if fs.Changed("ctx") {
		flags.MaxContextTokens = &values.Ctx
	}
	if fs.Changed("temperature") {
		flags.Temperature = &values.Temperature
	}
	if fs.Changed("no-mcp") {
		flags.NoMCP = &values.NoMCP
	}
	if fs.Changed("allowed-dirs") {
		flags.AllowedImportDirs = values.AllowedDirs
	}
	if fs.Changed("allow-env") {
		flags.AllowedEnv = values.AllowEnv
	}
	if fs.Changed("json") {
		flags.JSONSummary = &values.JSON
	}
	if fs.Changed("json-only") {
		flags.JSONOnly = &values.JSONOnly
	}
	if fs.Changed("status-file") {
		flags.StatusFile = &values.StatusFile
	}
	if fs.Changed("no-think") {
		flags.NoThink = &values.NoThink
	}
	if fs.Changed("allow") {
		flags.ScopeAllow = values.Allow
	}
	if fs.Changed("deny") {
		flags.ScopeDeny = values.Deny
	}
	if fs.Changed("read-only") {
		flags.ScopeReadOnly = values.ReadOnly
	}
	if fs.Changed("patch-only") {
		flags.PatchOnly = &values.PatchOnly
	}
	if fs.Changed("stop-on") {
		flags.StopOn = values.StopOn
	}
	if fs.Changed("precheck") {
		flags.Prechecks = values.Prechecks
	}
	if fs.Changed("postcheck") {
		flags.Postchecks = values.Postchecks
	}
	if fs.Changed("benchmark") {
		flags.BenchmarkMode = &values.Benchmark
	}
	if fs.Changed("llm-max-retries") {
		flags.LLMMaxRetries = &values.LLMMaxRetries
	}
	if fs.Changed("llm-retry-codes") {
		flags.LLMRetryableStatusCodes = values.LLMRetryCodes
	}
	if fs.Changed("llm-retry-base-delay") {
		flags.LLMRetryBaseDelay = &values.LLMRetryBaseDelay
	}
	if fs.Changed("llm-retry-max-delay") {
		flags.LLMRetryMaxDelay = &values.LLMRetryMaxDelay
	}
	if fs.Changed("llm-cold-load-timeout") {
		flags.LLMColdLoadTimeout = &values.LLMColdLoadTimeout
	}
	if fs.Changed("llm-stream-idle-timeout") {
		flags.LLMStreamIdleTimeout = &values.LLMStreamIdleTimeout
	}
	return flags, nil
}

type configFlagValues struct {
	Model                string
	Provider             string
	Endpoint             string
	Mode                 string
	Profile              string
	MaxSteps             int
	Temperature          float64
	Effort               string
	NoMCP                bool
	Ctx                  int
	AllowedDirs          []string
	AllowEnv             []string
	JSON                 bool
	JSONOnly             bool
	StatusFile           string
	NoThink              bool
	Allow                []string
	Deny                 []string
	ReadOnly             []string
	PatchOnly            bool
	StopOn               []string
	Prechecks            []string
	Postchecks           []string
	Benchmark            bool
	LLMMaxRetries        int
	LLMRetryCodes        []int
	LLMRetryBaseDelay    time.Duration
	LLMRetryMaxDelay     time.Duration
	LLMColdLoadTimeout   time.Duration
	LLMStreamIdleTimeout time.Duration
}

func addConfigFlags(f *pflag.FlagSet, v *configFlagValues) {
	f.StringVar(&v.Effort, "effort", config.DefaultEffort, "effort tier (fast|medium|heavy) — seeds step/token budgets + churn patience")
	f.StringVar(&v.Model, "model", config.DefaultModel, "model your endpoint serves (e.g. qwen2.5-coder, gpt-4o); with --provider, a model alias from the profile")
	f.StringVar(&v.Provider, "provider", "", "named provider from the profile's \"providers\" block (sets endpoint+key; scopes --model alias lookup)")
	f.StringVar(&v.Endpoint, "endpoint", config.DefaultEndpoint, "OpenAI-compatible base URL")
	f.StringVar(&v.Mode, "mode", config.DefaultMode, "run mode (auto|manual)")
	f.StringVar(&v.Profile, "profile", "", "path to profiles.json (default ~/.config/kloo/profiles.json)")
	f.IntVar(&v.MaxSteps, "max-steps", config.DefaultMaxSteps, "max autonomous steps")
	f.IntVar(&v.Ctx, "ctx", config.DefaultMaxContextTokens, "per-step context window (match your server's -c; needed for a llama-swap/Ollama alias the bundled defaults can't size)")
	f.Float64Var(&v.Temperature, "temperature", config.DefaultTemperature, "sampling temperature")
	f.BoolVar(&v.NoMCP, "no-mcp", false, "disable all MCP servers for this run (overrides KLOO_MCP and the profile's mcpServers)")
	f.StringSliceVar(&v.AllowedDirs, "allowed-dirs", nil, "dirs OUTSIDE the workspace that AGENTS.md @import may read from (repeatable/comma-separated; read-only, load-time only)")
	f.StringSliceVar(&v.AllowEnv, "allow-env", nil, "env var NAMES to forward from kloo's env into run_command (repeatable/comma-separated) — the trusted-secret passthrough for a deploy/CI step; default exposes only PATH/HOME/…")
	f.BoolVar(&v.JSON, "json", false, "emit JSON output")
	f.BoolVar(&v.JSONOnly, "json-only", false, "require the final assistant answer to be valid JSON only")
	f.StringVar(&v.StatusFile, "status-file", "", "write the run summary JSON to this path after a visible TUI run completes")
	f.BoolVar(&v.NoThink, "no-think", false, "ask compatible OpenAI-style backends to disable thinking/reasoning for chat requests")
	f.StringSliceVar(&v.Allow, "allow", nil, "glob(s) the model MAY edit (repeatable/comma-separated); any scope flag disables model-facing run_command")
	f.StringSliceVar(&v.Deny, "deny", nil, "glob(s) the model may NOT edit (repeatable/comma-separated); deny wins over --allow")
	f.StringSliceVar(&v.ReadOnly, "read-only", nil, "glob(s) the model may READ but not edit (repeatable/comma-separated); wins over --allow")
	f.BoolVar(&v.PatchOnly, "patch-only", false, "restrict model changes to edit_file/write_file exact edits; withhold model-facing run_command")
	f.StringSliceVar(&v.StopOn, "stop-on", nil, "hard-stop rule(s) (repeatable/comma-separated): off-scope-edit, read-only-edit, repeated-verify=N")
	f.StringArrayVar(&v.Prechecks, "precheck", nil, "harness command run BEFORE verify each turn (repeatable); a failure blocks verify/postcheck and is non-success")
	f.StringArrayVar(&v.Postchecks, "postcheck", nil, "harness command run AFTER a passing verify (repeatable); a failure is non-success even though verify passed")
	f.BoolVar(&v.Benchmark, "benchmark", false, "automation preset: run task loop with JSON summary and stable benchmark exit codes")
	f.IntVar(&v.LLMMaxRetries, "llm-max-retries", config.DefaultLLMMaxRetries, "extra model-call retry attempts after the first")
	f.IntSliceVar(&v.LLMRetryCodes, "llm-retry-codes", config.DefaultLLMRetryableStatusCodes, "HTTP status codes retryable for model calls")
	f.DurationVar(&v.LLMRetryBaseDelay, "llm-retry-base-delay", config.DefaultLLMRetryBaseDelay, "first model-call retry backoff")
	f.DurationVar(&v.LLMRetryMaxDelay, "llm-retry-max-delay", config.DefaultLLMRetryMaxDelay, "maximum model-call retry backoff")
	f.DurationVar(&v.LLMColdLoadTimeout, "llm-cold-load-timeout", config.DefaultLLMColdLoadTimeout, "non-streaming model call timeout")
	f.DurationVar(&v.LLMStreamIdleTimeout, "llm-stream-idle-timeout", config.DefaultLLMStreamIdle, "streaming no-token idle timeout")
}

// Execute builds the root command with production dependencies and runs it,
// exiting non-zero on error.
func Execute() {
	cmd := NewRootCmd(Deps{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		var ee exitError
		if errors.As(err, &ee) {
			fmt.Fprintln(os.Stderr, "kloo:", ee.err)
			os.Exit(ee.code)
		}
		fmt.Fprintln(os.Stderr, "kloo:", err)
		os.Exit(1)
	}
}
