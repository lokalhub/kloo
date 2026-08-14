package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/repomap"
	"github.com/spf13/cobra"
)

// tokensResult answers one question before a run starts: does this task text fit
// the window kloo is configured to use. The count is kloo's own estimate
// (repomap.ApproxTokens, ~4 chars/token) and the budgets come from the loop's
// own helpers, so "fits" means "fits by the rule the loop applies" — not a
// second opinion that can disagree with it.
type tokensResult struct {
	Source          string `json:"source"`
	Chars           int    `json:"chars"`
	ApproxTokens    int    `json:"approx_tokens"`
	Ctx             int    `json:"ctx"`
	UsableWindow    int    `json:"usable_window"`
	CompactTrigger  int    `json:"compact_trigger"`
	Headroom        int    `json:"headroom"`
	Fits            bool   `json:"fits"`
	CompactsAtStart bool   `json:"compacts_at_start"`
	Estimate        string `json:"estimate"`
}

func newTokensCmd(deps *Deps) *cobra.Command {
	values := configFlagValues{}
	var file string
	cmd := &cobra.Command{
		Use:   "tokens [task]",
		Short: "Estimate whether a task prompt fits the configured context window",
		Long: "Estimate a task prompt's size against the resolved context window.\n" +
			"Reads the task from the argument, or from --file. Never starts a model,\n" +
			"MCP, TUI, task loop or verify command.",
		Args:          cobra.MaximumNArgs(1),
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
			text, source, err := readTokensInput(args, file)
			if err != nil {
				return err
			}
			res := measureTokens(text, source, cfg.MaxContextTokens)
			if values.JSON {
				return writeTokensJSON(deps.Out, res)
			}
			writeTokensHuman(deps.Out, res)
			return nil
		},
	}
	addConfigFlags(cmd.Flags(), &values)
	cmd.Flags().StringVar(&file, "file", "", "read the task text from this file instead of an argument")
	return cmd
}

// readTokensInput takes the task from the positional argument or --file, but not
// both — a silent precedence rule here would mean measuring text the caller did
// not think they passed.
func readTokensInput(args []string, file string) (string, string, error) {
	switch {
	case file != "" && len(args) > 0:
		return "", "", fmt.Errorf("pass a task argument or --file, not both")
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", "", fmt.Errorf("read %s: %w", file, err)
		}
		return string(raw), file, nil
	case len(args) > 0:
		return args[0], "arg", nil
	default:
		return "", "", fmt.Errorf("provide a task argument or --file")
	}
}

func measureTokens(text, source string, ctxTokens int) tokensResult {
	approx := repomap.ApproxTokens(text)
	usable := agent.UsableWindow(ctxTokens)
	trigger := agent.CompactTriggerTokens(ctxTokens)
	return tokensResult{
		Source:          source,
		Chars:           len(text),
		ApproxTokens:    approx,
		Ctx:             ctxTokens,
		UsableWindow:    usable,
		CompactTrigger:  trigger,
		Headroom:        usable - approx,
		Fits:            approx <= usable,
		CompactsAtStart: approx > trigger,
		Estimate:        "approx-4-chars-per-token",
	}
}

func writeTokensJSON(out io.Writer, res tokensResult) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

func writeTokensHuman(out io.Writer, res tokensResult) {
	fmt.Fprintln(out, "kloo tokens")
	fmt.Fprintf(out, "source: %s\n", res.Source)
	fmt.Fprintf(out, "chars: %d\n", res.Chars)
	fmt.Fprintf(out, "approx_tokens: %d (%s)\n", res.ApproxTokens, res.Estimate)
	fmt.Fprintf(out, "ctx: %d\n", res.Ctx)
	fmt.Fprintf(out, "usable_window: %d\n", res.UsableWindow)
	fmt.Fprintf(out, "compact_trigger: %d\n", res.CompactTrigger)
	fmt.Fprintf(out, "headroom: %d\n", res.Headroom)
	if res.Fits {
		fmt.Fprintln(out, "fits: PASS")
	} else {
		fmt.Fprintln(out, "fits: FAIL")
	}
	if res.CompactsAtStart {
		fmt.Fprintln(out, "note: above the compaction trigger — the loop compacts on step one")
	}
}
