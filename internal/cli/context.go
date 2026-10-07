package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/tools"
	"github.com/spf13/cobra"
)

// `kloo context` answers the question `kloo tokens` could not: how much of the
// window is in use, and WHAT is using it.
//
// It exists because `kloo tokens` reports `headroom = usable - approx(task)` — the
// task string and nothing else. No system prompt, no AGENTS.md, no repo map, no
// history. On kloo's own tree that omits the single largest consumer of the first
// turn. And the run display `tokens 1553679/480000` is cumulative SPEND against the
// run budget, which exceeds the window by 10x routinely because the whole prompt is
// counted every turn. Neither is a gauge.
//
// HOW IT GETS ITS NUMBERS. It builds a real agent.Loop with the same system prompt,
// the same workspace jail, the same scope policy, the same repo-map position and
// budget, the same tool registry and the same calibrated estimator a run would use,
// then calls the loop's OWN (*Loop).buildPrompt — the single prompt-assembly path,
// the one act() uses — and measures the []llm.Message it returns.
//
// It does not reimplement any of that, and that is the whole design. A gauge that
// recomputed occupancy from mapBudgetFrac/hotBudgetFrac would reproduce the fiction
// `kloo doctor` tells: doctor derives its working-set line from the same constants
// the assembler does, and for hotBudgetTokens the derivation was WRONG for three
// releases while doctor printed "BINDING — holds the prompt here" over a run that
// shed nothing (agent/memory.go:134). The instrument has to be downstream of the
// behaviour or it is just the same guess, twice.
//
// WHAT IT DOES NOT DO: no model call, no MCP connection, no /models fetch, no TUI,
// no verify, no task loop, and no write of any kind — the same read-only contract as
// `kloo tokens` and `kloo doctor`. Two consequences are stated in the output rather
// than hidden: MCP tool schemas are absent (so the schema figure is the builtins
// only), and the window is whatever --ctx resolves to locally rather than what a
// /models probe would discover.

type contextResult struct {
	Task   string             `json:"task"`
	Source string             `json:"source"`
	Model  string             `json:"model"`
	Gauge  agent.ContextGauge `json:"gauge"`
	// Notes are the caveats that apply to THIS invocation (no MCP, uncalibrated
	// estimate, a window that no /models probe confirmed). Carried in the JSON as
	// well as the human output: a number consumed by a script needs its provenance
	// as much as one read by a person.
	Notes []string `json:"notes,omitempty"`
}

func newContextCmd(deps *Deps) *cobra.Command {
	values := configFlagValues{}
	var file string
	cmd := &cobra.Command{
		Use:   "context [task]",
		Short: "Show what is occupying the context window, measured from the assembled prompt",
		Long: "Assemble the prompt kloo would send for this task and report window\n" +
			"occupancy by section: system+schema, repo map, pinned/hot state and\n" +
			"history. Measured from the assembled messages, not from the budget\n" +
			"constants. Never starts a model, MCP, TUI, task loop or verify command,\n" +
			"and writes nothing.",
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
			res, err := measureContext(cfg, text, source)
			if err != nil {
				return err
			}
			if values.JSON {
				enc := json.NewEncoder(deps.Out)
				enc.SetIndent("", "  ")
				return enc.Encode(res)
			}
			writeContextHuman(deps.Out, res)
			return nil
		},
	}
	addConfigFlags(cmd.Flags(), &values)
	cmd.Flags().StringVar(&file, "file", "", "read the task text from this file instead of an argument")
	return cmd
}

// measureContext assembles one real prompt and measures it.
func measureContext(cfg config.Config, task, source string) (contextResult, error) {
	// Apply the configured fractions FIRST, exactly as defaultRunHeadless does, so
	// every budget below describes the run the user would actually get rather than
	// the built-in defaults.
	agent.SetContextFractions(cfg.UsableWindowFrac, cfg.CompactTriggerFrac)
	agent.SetWorkingSetTokens(cfg.WorkingSetTokens)
	if cfg.SummaryBudgetFrac != 0 {
		agent.SetSummaryBudgetFrac(cfg.SummaryBudgetFrac)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return contextResult{}, err
	}
	ws, err := tools.NewWorkspace(cwd)
	if err != nil {
		return contextResult{}, err
	}
	quiet := func(string, ...any) {} // a diagnostic must not narrate over its own output
	ws, err = applyScope(cfg, ws, cwd, quiet)
	if err != nil {
		return contextResult{}, err
	}
	adapter, err := tools.SelectAdapter(cfg.ToolFormat, tools.EndpointCaps{SupportsTools: true})
	if err != nil {
		return contextResult{}, err
	}
	// The same system prompt a run builds, minus the memory-recall section, which
	// requires an MCP round trip. Noted in the output rather than silently omitted.
	systemPrompt := SystemPrompt() + scopeSystemPromptSuffix(ws) +
		agentsInstructions(cwd, cfg.AllowedImportDirs, cfg.MaxContextTokens, quiet)

	loop := &agent.Loop{
		Adapter:       adapter,
		Registry:      tools.DefaultRegistry(ws, tools.WithAllowedEnv(cfg.AllowedEnv)),
		Root:          ws.Root(),
		ContextTokens: cfg.MaxContextTokens,
		CuratorTokens: cfg.CuratorBudgetTokens,
		MapPosition:   cfg.MapPosition,
		// The ratio measured for this model on a previous run in this workspace, so
		// the numbers are on the same scale the loop budgets against. Cold start falls
		// back to chars/4 and the output says so.
		Tokens:      tokenCalibrator(cwd, cfg.Model),
		Memory:      agent.NewWorkingMemory(),
		System:      systemPrompt,
		Model:       cfg.Model,
		Temperature: cfg.Temperature,
		NoThink:     cfg.NoThink,
		PromptCache: cfg.PromptCacheEnabled(),
	}
	// Turn one: the task, no history, no verify signal, no file under edit. That is
	// the honest thing to report for a task that has not run — and it is also the
	// turn that matters, because the system prompt, AGENTS.md and the repo map are
	// the part that is paid on every turn thereafter.
	if _, _, err := loop.BuildPromptForMeasurement(context.Background(), task); err != nil {
		return contextResult{}, fmt.Errorf("assemble prompt: %w", err)
	}
	g := loop.ContextGauge()

	res := contextResult{Task: task, Source: source, Model: cfg.Model, Gauge: g}
	// Report the EFFECTIVE rate this prompt was charged at, derived from it, not the
	// base constant. The base alone said "4.00 chars/token" while the estimator
	// actually charged 3.82-3.88, because it bills high-entropy words at ~1.8 and a
	// repo map is almost entirely paths and identifiers. A number announcing a
	// precision it does not have is this feature's own failure mode.
	basis := "uncalibrated — no previous run in this workspace measured this model"
	if g.Calibrated {
		basis = "calibrated against reported usage on a previous run in this workspace"
	}
	res.Notes = append(res.Notes, fmt.Sprintf(
		"token counts are ESTIMATED: %s chars at an effective %.2f chars/token (base %.2f, %s); "+
			"the estimator bills high-entropy text nearer 1.8, so the effective rate sits below the base",
		commas(g.Chars), g.EffectiveRatio, g.Ratio, basis))
	res.Notes = append(res.Notes,
		"schema figure covers the BUILT-IN tools only — this command connects no MCP server, and MCP tools add to every request")
	res.Notes = append(res.Notes,
		"the section split is kloo's own structure: the transport merges adjacent same-role messages, so on the wire the task and a pinned repo map arrive as one message (the totals are unaffected)")
	if g.Window <= 0 {
		res.Notes = append(res.Notes,
			"--ctx resolved to 0 or less, so there is no window to measure occupancy against; percentages are omitted")
	}
	return res, nil
}

// writeContextHuman renders the gauge. The shape is deliberately the shape of the
// question: one line per consumer, with its share, and the budget it is over when it
// is over one.
func writeContextHuman(out io.Writer, res contextResult) {
	g := res.Gauge
	fmt.Fprintln(out, "kloo context")
	fmt.Fprintf(out, "model %s   task from %s\n", res.Model, res.Source)
	fmt.Fprintf(out, "window %s  usable %s  compaction at %s (%s binding)\n",
		commas(g.Window), commas(g.Usable), commas(g.Trigger), g.Binding)

	// Percentages are of USABLE, and only when there is one to divide by: a --ctx of
	// 0 produced `usable_window: 0, headroom: -1` in `kloo tokens`, which is nonsense
	// dressed as data. Here it simply prints the counts.
	pct := func(n int) string {
		if g.Usable <= 0 {
			return ""
		}
		return fmt.Sprintf("%4.0f%%", 100*float64(n)/float64(g.Usable))
	}
	row := func(label string, n int, extra string) {
		fmt.Fprintf(out, "  %-14s %9s  %s  %s\n", label, commas(n), pct(n), extra)
	}
	row("system+schema", g.SystemTokens+g.SchemaTokens,
		fmt.Sprintf("(system %s + tool schemas %s)", commas(g.SystemTokens), commas(g.SchemaTokens)))
	mapNote := fmt.Sprintf("(%s)", g.MapPlacement)
	if g.MapBudget > 0 {
		mapNote = fmt.Sprintf("(%s; budget %s", g.MapPlacement, commas(g.MapBudget))
		// Printed whenever it is over, however slightly. The overshoot was computed and
		// then never rendered, which is the measure-then-hide failure this whole
		// feature exists to correct. A sub-1% excess is two estimators disagreeing
		// (see ContextGauge.MapOverBudget) — saying "+104, 0.5%" is informative; saying
		// nothing is not.
		if g.MapOverBudget > 0 {
			mapNote += fmt.Sprintf(" — over by %s (%.2f%%)", commas(g.MapOverBudget), g.MapOverBudgetPct)
		}
		mapNote += ")"
	}
	row("repo map", g.RepoMapTokens, mapNote)
	row("pinned/hot", g.PinnedHotTokens, "(task + pins + todos — re-sent every turn)")
	row("history", g.HistoryTokens, "(summary + recent tail — what compaction sheds)")
	if g.Unattributed != 0 {
		// Printed whenever it is nonzero. A few tokens is estimator rounding; a large
		// value means the prompt layout moved and this accounting did not keep up, and
		// the user should see that rather than a tidy total that silently absorbed it.
		row("unattributed", g.Unattributed, "(estimator rounding; a large value here is a bug)")
	}
	// With no window configured there is nothing to be free OF. `kloo tokens` prints
	// `usable_window: 0, headroom: -1, fits: false` in that case, which is arithmetic
	// on a missing number presented as a finding; this says so instead.
	if g.Usable <= 0 {
		row("used", g.Used, fmt.Sprintf("in %d messages", g.Messages))
		fmt.Fprintf(out, "  %-14s %9s  (no window configured — set --ctx to measure occupancy)\n", "free", "n/a")
	} else {
		row("used", g.Used, fmt.Sprintf("of usable, in %d messages", g.Messages))
		row("free", g.Free, fmt.Sprintf("— %s before compaction", commas(g.FreeToCompaction)))
	}
	if g.Usable > 0 && g.FreeToCompaction < 0 {
		fmt.Fprintln(out, "note: already above the compaction trigger — the loop compacts on step one")
	}
	if g.Used > g.Usable {
		fmt.Fprintln(out, "note: the first turn alone exceeds the usable window — the assembler will shed to fit")
	}
	// The one thing the gauge structurally cannot see, named as such.
	fmt.Fprintf(out, "process rss %s (NOT window occupancy; memory ceiling %s)\n",
		humanBytesCLI(g.RSSBytes), ceilingLabel(g.CeilingBytes))
	for _, n := range res.Notes {
		fmt.Fprintf(out, "note: %s\n", n)
	}
}

func ceilingLabel(n int64) string {
	if n <= 0 {
		return "disabled"
	}
	return humanBytesCLI(uint64(n))
}

func humanBytesCLI(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%d MiB", n>>20)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// commas renders a token count with thousands separators, so a five-figure number is
// legible at a glance — the whole output is a comparison of magnitudes.
func commas(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

