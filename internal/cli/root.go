// Package cli wires the cobra root command and flag parsing for kloo.
//
// For Phase 00 the root runs a single one-shot completion: parse the task
// argument + core flags, resolve config (internal/config), and stream the
// model's reply to stdout. The autonomous loop (Phase 04) and interactive TUI
// (Phase 05) later plug into this same seam.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/spf13/cobra"
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
	// NewClient builds the LLM client from resolved config. Defaults to a real
	// llm.Client; tests inject a fake.
	NewClient func(cfg config.Config) llm.LLMClient
	// LaunchTUI starts the interactive TUI session (the autonomous loop wired
	// under the Bubble Tea UI). sess carries the user's session choice (--new /
	// --resume); lint carries the fast-advisory-lint config (--lint/--no-lint +
	// env). Injected so tests stay offline.
	LaunchTUI func(cfg config.Config, verifyCmd string, lint lintOpts, sess SessionOpts) error
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
	if d.NewClient == nil {
		d.NewClient = func(cfg config.Config) llm.LLMClient {
			return llm.New(cfg.Endpoint, cfg.Model, llm.WithAPIKey(cfg.APIKey))
		}
	}
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
		flagHeadless    bool
		flagEffort      string
		flagNewSess     bool
		flagResume      string
		flagNoMCP       bool
		flagLint        string
		flagNoLint      bool
		flagCtx         int
		flagAllowedDirs []string
	)

	cmd := &cobra.Command{
		Use:   "kloo [task]",
		Short: "Autonomous coding CLI for small local LLMs",
		Long: "kloo drives any OpenAI-compatible endpoint (llama.cpp, Ollama, vLLM, " +
			"OpenAI, OpenRouter, …) to edit and verify code autonomously.\n\n" +
			"Launch with no task for the interactive TUI session:  kloo\n" +
			"Or pass a one-shot task to stream a reply non-interactively:  kloo \"say hi\"",
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

			cfg, err := config.Resolve(flags, deps.Getenv, flagProfile)
			if err != nil {
				return err
			}

			// Fast-advisory-lint knobs (--lint/--no-lint + KLOO_LINT/KLOO_NO_LINT),
			// threaded into the entry points alongside verifyCmd; resolved to a
			// command (or nil linter) downstream in defaultLaunchTUI/defaultRunHeadless.
			lopts := lintOptsFrom(fs.Changed("lint"), fs.Changed("no-lint"), flagLint, flagNoLint, deps.Getenv)

			if len(args) == 0 {
				if flagHeadless {
					return fmt.Errorf("--headless requires a task argument (e.g. kloo --headless --verify '…' \"do X\")")
				}
				// No task argument → launch the interactive TUI session (the
				// autonomous loop under the Bubble Tea UI).
				return deps.LaunchTUI(cfg, flagVerify, lopts, SessionOpts{New: flagNewSess, ResumeID: flagResume})
			}

			if flagHeadless {
				// A task argument + --headless → run the autonomous loop
				// non-interactively (no TTY), streaming progress to stdout. This is
				// the acceptance-benchmark / scripted-CI path.
				return deps.RunHeadless(cfg, args[0], flagVerify, lopts, deps.Out)
			}

			// A task argument → the non-interactive one-shot stream (scripting /
			// the Phase-00 live smoke). The full interactive session is the no-arg TUI.
			client := deps.NewClient(cfg)
			return runOneShot(cmd.Context(), client, cfg, args[0], deps.Out)
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
	f.StringVar(&flagVerify, "verify", "", "(deprecated) override kloo's auto-detected verify command; when unset, kloo infers the project's build/test")
	_ = f.MarkDeprecated("verify", "kloo now auto-detects the project's build/test command — pass --verify only to override the detected one")
	f.BoolVar(&flagHeadless, "headless", false, "run the autonomous loop non-interactively (no TTY), streaming progress to stdout; requires a task arg")
	f.BoolVar(&flagNewSess, "new", false, "start a fresh session (the default; sessions are no longer auto-resumed)")
	f.StringVar(&flagResume, "resume", "", "resume a specific saved session by id (printed on exit; see {workspace}/.kloo/sessions)")
	f.BoolVar(&flagNoMCP, "no-mcp", false, "disable all MCP servers for this run (overrides KLOO_MCP and the profile's mcpServers)")
	f.StringVar(&flagLint, "lint", "", "override kloo's auto-detected fast lint command (advisory; runs on edited files after each edit)")
	f.BoolVar(&flagNoLint, "no-lint", false, "disable the fast advisory lint step (lint is on by default when a linter is detected)")
	f.StringSliceVar(&flagAllowedDirs, "allowed-dirs", nil, "dirs OUTSIDE the workspace that AGENTS.md @import may read from (repeatable/comma-separated; read-only, load-time only)")

	cmd.SetVersionTemplate("kloo {{.Version}}\n")
	return cmd
}

// runOneShot streams a single completion for task to out.
func runOneShot(ctx context.Context, client llm.LLMClient, cfg config.Config, task string, out io.Writer) error {
	req := llm.ChatRequest{
		Model:       cfg.Model,
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: task}},
		Temperature: cfg.Temperature,
	}
	_, err := client.Stream(ctx, req, func(d llm.Delta) error {
		if d.Content != "" {
			fmt.Fprint(out, d.Content)
		}
		return nil
	})
	fmt.Fprintln(out) // terminate the streamed line
	return err
}

// Execute builds the root command with production dependencies and runs it,
// exiting non-zero on error.
func Execute() {
	cmd := NewRootCmd(Deps{})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "kloo:", err)
		os.Exit(1)
	}
}
