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

	"github.com/lokal/kloo/internal/config"
	"github.com/lokal/kloo/internal/llm"
	"github.com/spf13/cobra"
)

// Deps are the injectable dependencies of the root command, so tests can run
// offline (fake client) and capture output.
type Deps struct {
	// NewClient builds the LLM client from resolved config. Defaults to a real
	// llm.Client; tests inject a fake.
	NewClient func(cfg config.Config) llm.LLMClient
	// LaunchTUI starts the interactive TUI session (the autonomous loop wired
	// under the Bubble Tea UI). Injected so tests stay offline.
	LaunchTUI func(cfg config.Config, verifyCmd string) error
	// RunHeadless runs the autonomous loop NON-interactively (no TTY), streaming
	// progress to out and returning the loop's terminal report. Used for the
	// Phase-06 acceptance benchmark and any scripted/CI autonomous run. Injected
	// so tests stay offline.
	RunHeadless func(cfg config.Config, task, verifyCmd string, out io.Writer) error
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
		flagModel    string
		flagEndpoint string
		flagMode     string
		flagProfile  string
		flagMaxSteps int
		flagTemp     float64
		flagVerify   string
		flagHeadless bool
		flagEffort   string
	)

	cmd := &cobra.Command{
		Use:   "kloo [task]",
		Short: "Autonomous coding CLI for small local LLMs",
		Long: "kloo drives a local llama-swap OpenAI-compatible endpoint to edit and " +
			"verify code autonomously.\n\n" +
			"Launch with no task for the interactive TUI session:  kloo\n" +
			"Or pass a one-shot task to stream a reply non-interactively:  kloo --model snappy \"say hi\"",
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
			if fs.Changed("endpoint") {
				flags.Endpoint = &flagEndpoint
			}
			if fs.Changed("mode") {
				flags.Mode = &flagMode
			}
			if fs.Changed("max-steps") {
				flags.MaxSteps = &flagMaxSteps
			}
			if fs.Changed("temperature") {
				flags.Temperature = &flagTemp
			}

			cfg, err := config.Resolve(flags, deps.Getenv, flagProfile)
			if err != nil {
				return err
			}

			if len(args) == 0 {
				if flagHeadless {
					return fmt.Errorf("--headless requires a task argument (e.g. kloo --headless --verify '…' \"do X\")")
				}
				// No task argument → launch the interactive TUI session (the
				// autonomous loop under the Bubble Tea UI).
				return deps.LaunchTUI(cfg, flagVerify)
			}

			if flagHeadless {
				// A task argument + --headless → run the autonomous loop
				// non-interactively (no TTY), streaming progress to stdout. This is
				// the acceptance-benchmark / scripted-CI path.
				return deps.RunHeadless(cfg, args[0], flagVerify, deps.Out)
			}

			// A task argument → the non-interactive one-shot stream (scripting /
			// the Phase-00 live smoke). The full interactive session is the no-arg TUI.
			client := deps.NewClient(cfg)
			return runOneShot(cmd.Context(), client, cfg, args[0], deps.Out)
		},
	}

	f := cmd.Flags()
	f.StringVar(&flagEffort, "effort", config.DefaultEffort, "effort tier (fast|medium|heavy) — seeds model + step/token budgets + churn patience")
	f.StringVar(&flagModel, "model", config.DefaultModel, "model name (e.g. snappy, smart); overrides the tier's model")
	f.StringVar(&flagEndpoint, "endpoint", config.DefaultEndpoint, "OpenAI-compatible base URL")
	f.StringVar(&flagMode, "mode", config.DefaultMode, "run mode (auto|manual)")
	f.StringVar(&flagProfile, "profile", "", "path to profiles.json (default ~/.config/kloo/profiles.json)")
	f.IntVar(&flagMaxSteps, "max-steps", config.DefaultMaxSteps, "max autonomous steps")
	f.Float64Var(&flagTemp, "temperature", config.DefaultTemperature, "sampling temperature")
	f.StringVar(&flagVerify, "verify", "go test ./...", "verify command the loop runs each step (the real success signal)")
	f.BoolVar(&flagHeadless, "headless", false, "run the autonomous loop non-interactively (no TTY), streaming progress to stdout; requires a task arg")

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
