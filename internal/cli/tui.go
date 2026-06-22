package cli

import (
	"os"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
	"github.com/lokalhub/kloo/internal/tui"
)

// defaultLaunchTUI composes the full stack — P00 client, P01/P02 tools + jail,
// P03 repo-map context, the P04 autonomous loop + safety rails — and runs it
// under the Bubble Tea TUI (P05). The verify command (verifyCmd) is the real
// success signal the loop trusts each step.
func defaultLaunchTUI(cfg config.Config, verifyCmd string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ws, err := tools.NewWorkspace(cwd)
	if err != nil {
		return err
	}

	adapter, err := tools.SelectAdapter(cfg.ToolFormat, tools.EndpointCaps{SupportsTools: true})
	if err != nil {
		return err
	}

	loop := &agent.Loop{
		Client:        llm.New(cfg.Endpoint, cfg.Model, llm.WithAPIKey(cfg.APIKey)),
		Adapter:       adapter,
		Registry:      tools.DefaultRegistry(ws),
		Verifier:      agent.NewCommandVerifier(ws, verifyCmd),
		Budget:        agent.NewBudget(cfg, nil),
		Churn:         agent.NewChurnDetector(cfg.ChurnRounds),
		Checkpoint:    agent.NewGitCheckpointer(cwd),
		Root:          ws.Root(),
		ContextTokens: cfg.MaxContextTokens,
		Memory:        agent.NewWorkingMemory(), // working memory on by default (P00); maxContextTokens governs compaction
		System: "You are kloo, an autonomous coding assistant. Each turn, make exactly one " +
			"tool call to read, edit, or run a command, working toward the user's task until " +
			"the verify command passes. Use SEARCH/REPLACE edits; never rewrite whole files.",
		Model:       cfg.Model,
		Temperature: cfg.Temperature,
	}

	runner := tui.NewLoopRunner(loop, ws, cfg.MaxTokens)
	return tui.Run(tui.Config{
		Effort:    cfg.Effort,
		Model:     cfg.Model,
		MaxSteps:  cfg.MaxSteps,
		MaxTokens: cfg.MaxTokens,
		Runner:    runner,
	})
}
