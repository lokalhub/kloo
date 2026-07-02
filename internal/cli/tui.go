package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/session"
	"github.com/lokalhub/kloo/internal/tools"
	"github.com/lokalhub/kloo/internal/tui"
)

// defaultLaunchTUI composes the full stack — P00 client, P01/P02 tools + jail,
// P03 repo-map context, the P04 autonomous loop + safety rails — and runs it
// under the Bubble Tea TUI (P05). verifyCmd is the deprecated --verify override
// ("" ⇒ kloo auto-detects the project's build/test); when it resolves to "" the
// loop runs unverified (the model's finish stops calmly, but nothing is success).
func defaultLaunchTUI(cfg config.Config, baseFlags config.Flags, verifyCmd string, lint lintOpts, opt SessionOpts, profilePath string, getenv func(string) string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ws, err := tools.NewWorkspace(cwd)
	if err != nil {
		return err
	}
	// Attach the A1/A2 scope policy + A4 patch-only flag (see headless.go).
	ws, err = applyScope(cfg, ws, cwd, writerLogf(os.Stderr))
	if err != nil {
		return err
	}

	// Resolve the verify command: the deprecated --verify flag (when set) overrides
	// kloo's project-aware auto-detection; "" ⇒ unverified mode (no verifier built).
	verifyCmd = resolveVerifyCommand(verifyCmd, cwd, writerLogf(os.Stderr))
	// Resolve the fast advisory lint command (--lint/--no-lint + env, else auto-detect,
	// else "" = no lint step). Advisory only — it never gates the run's success.
	lintCmd, lintPerFile := resolveLintCommand(lint.Override, lint.Disabled, cwd, writerLogf(os.Stderr))

	// Resolve which session this launch uses (fresh by default, or --resume <id>),
	// and the banner shown in the transcript when resuming.
	store := session.NewStore(cwd)
	sess, banner, err := chooseSession(store, cfg, verifyCmd, lintCmd, opt, time.Now())
	if err != nil {
		return err
	}

	adapter, err := tools.SelectAdapter(cfg.ToolFormat, tools.EndpointCaps{SupportsTools: true})
	if err != nil {
		return err
	}

	// MCP: connect configured servers (non-fatal) + register their tools alongside
	// the builtins; the startup/trust lines go to stderr. The context lives for the
	// whole TUI session and is cancelled (and sessions closed) on exit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reg, mcpMgr, closeMCP := wireMCP(ctx, cfg, ws, writerLogf(os.Stderr))
	defer closeMCP()

	clientFactory := func(endpoint, model, apiKey string) llm.LLMClient {
		return llm.New(endpoint, model, llm.WithAPIKey(apiKey), llm.WithTimeout(cfg.LLMColdLoadTimeout), llm.WithStreamIdleTimeout(cfg.LLMStreamIdleTimeout))
	}
	client := llm.New(cfg.Endpoint, cfg.Model, llm.WithAPIKey(cfg.APIKey), llm.WithTimeout(cfg.LLMColdLoadTimeout), llm.WithStreamIdleTimeout(cfg.LLMStreamIdleTimeout))

	// /profile <path> (C6): re-resolve the runtime from a different profiles.json for
	// subsequent runs, preserving the launch CLI flags (flags > env > profile).
	reloadProfile := buildReloadProfile(baseFlags, getenv, clientFactory)

	loop := &agent.Loop{
		Client:        client,
		Adapter:       adapter,
		Registry:      reg,
		Verifier:      buildLayeredVerifier(ws, verifyCmd, cfg.Prechecks, cfg.Postchecks, writerLogf(os.Stderr)),
		Linter:        buildLinter(ws, lintCmd, lintPerFile),
		Budget:        agent.NewBudget(cfg, nil),
		Churn:         agent.NewChurnDetector(cfg.ChurnRounds),
		Checkpoint:    agent.NewGitCheckpointer(cwd),
		Root:          ws.Root(),
		ContextTokens: cfg.MaxContextTokens,
		Memory:        agent.NewWorkingMemory(), // working memory on by default (P00); maxContextTokens governs compaction
		System:        defaultSystemPrompt + scopeSystemPromptSuffix(ws) + agentsInstructions(cwd, cfg.AllowedImportDirs, cfg.MaxContextTokens, writerLogf(os.Stderr)),
		ChatSystem:    chatGateSystemPrompt, // interactive only: answer chit-chat without launching a run

		StopOn:               agentStopPolicy(cfg.StopOn),
		StallRounds:          cfg.ChurnRounds,
		Endpoint:             cfg.Endpoint,
		Model:                cfg.Model,
		Temperature:          cfg.Temperature,
		NoThink:              cfg.NoThink,
		LLMRetries:           cfg.LLMMaxRetries,
		RetryBaseDelay:       cfg.LLMRetryBaseDelay,
		RetryMaxDelay:        cfg.LLMRetryMaxDelay,
		RetryableStatusCodes: cfg.LLMRetryableStatusCodes,
	}

	runner := tui.NewLoopRunner(loop, ws, cfg.MaxTokens).WithSession(store, sess)
	runner.WithRunHooks(
		func(ctx context.Context, task string, runtime tui.RuntimeConfig) string {
			runCfg := tuiRuntimeConfig(cfg, runtime)
			return memoryRecallSystemSection(memoryRecall(ctx, runCfg, mcpMgr, cwd, task, writerLogf(os.Stderr)))
		},
		func(ctx context.Context, task string, runtime tui.RuntimeConfig, rep *agent.Report, elapsed time.Duration) {
			runCfg := tuiRuntimeConfig(cfg, runtime)
			summary := buildRunSummary(runCfg, verifyCmd, rep, elapsed, nil)
			memoryStore(ctx, runCfg, mcpMgr, cwd, task, summary, rep, writerLogf(os.Stderr))
		},
	)
	if cfg.StatusFile != "" {
		statusPath := cfg.StatusFile
		runner.WithStatusWriter(func(runtime tui.RuntimeConfig, rep *agent.Report, elapsed time.Duration) error {
			runCfg := tuiRuntimeConfig(cfg, runtime)
			summary := buildRunSummary(runCfg, verifyCmd, rep, elapsed, nil)
			withFilesChanged(&summary, ws.Root()) // B3: changed-file accounting for the status file
			return writeRunSummaryFile(statusPath, summary)
		})
	}
	runErr := tui.Run(tui.Config{
		Version:       Version(),
		Effort:        cfg.Effort,
		Model:         cfg.Model,
		MaxSteps:      cfg.MaxSteps,
		MaxTokens:     cfg.MaxTokens,
		Runner:        runner,
		Banner:        banner,
		ModelList:     client,
		Provider:      cfg.Provider,
		Endpoint:      cfg.Endpoint,
		APIKey:        cfg.APIKey,
		ContextTokens: cfg.MaxContextTokens,
		Temperature:   cfg.Temperature,
		ToolFormat:    cfg.ToolFormat,
		NoThink:       cfg.NoThink,
		NoThinkLocked: cfg.NoThinkExplicit,
		NewClient:     clientFactory,
		ProfilePath:   profilePath,
		Getenv:        getenv,
		ReloadProfile: reloadProfile,
		History:       sess.Transcript, // replay prior turns on resume (empty for a fresh session)
	})
	// Sessions are fresh by default, so on exit print the id (only once something
	// was saved) so the user can pick this conversation back up with --resume.
	if sess.Runs > 0 {
		fmt.Fprintf(os.Stderr, "\nsession %s saved · resume it with:  kloo --resume %s\n", sess.ID, sess.ID)
	}
	return runErr
}

// buildReloadProfile returns the /profile <path> reload closure (C6): it re-resolves
// the runtime from a DIFFERENT profiles.json preserving the launch CLI flags (flags >
// env > profile), maps it to a TUI RuntimeConfig reusing the client factory, and
// returns a SECRET-FREE one-line summary (provider/model/endpoint/ctx — never the API
// key). A resolution error is returned so the TUI keeps its current runtime intact.
func buildReloadProfile(baseFlags config.Flags, getenv func(string) string, clientFactory func(endpoint, model, apiKey string) llm.LLMClient) func(string) (tui.RuntimeConfig, string, error) {
	return func(path string) (tui.RuntimeConfig, string, error) {
		newCfg, err := config.Resolve(baseFlags, getenv, path)
		if err != nil {
			return tui.RuntimeConfig{}, "", err
		}
		rc := tui.RuntimeConfig{
			Provider:      newCfg.Provider,
			Endpoint:      newCfg.Endpoint,
			APIKey:        newCfg.APIKey,
			Model:         newCfg.Model,
			ContextTokens: newCfg.MaxContextTokens,
			Temperature:   newCfg.Temperature,
			ToolFormat:    newCfg.ToolFormat,
			NoThink:       newCfg.NoThink,
			NoThinkLocked: newCfg.NoThinkExplicit,
			NewClient:     clientFactory,
			UseNewClient:  true,
		}
		summary := fmt.Sprintf("provider=%s model=%s endpoint=%s ctx=%d",
			providerOrNone(newCfg.Provider), newCfg.Model, newCfg.Endpoint, newCfg.MaxContextTokens)
		return rc, summary, nil
	}
}

// providerOrNone renders a provider name for the redacted /profile summary, using
// "none" when no provider is selected (a bare endpoint profile).
func providerOrNone(p string) string {
	if strings.TrimSpace(p) == "" {
		return "none"
	}
	return p
}

func tuiRuntimeConfig(base config.Config, runtime tui.RuntimeConfig) config.Config {
	runCfg := base
	runCfg.Provider = runtime.Provider
	runCfg.Endpoint = runtime.Endpoint
	runCfg.APIKey = runtime.APIKey
	runCfg.Model = runtime.Model
	runCfg.MaxContextTokens = runtime.ContextTokens
	runCfg.Temperature = runtime.Temperature
	runCfg.ToolFormat = runtime.ToolFormat
	runCfg.NoThink = runtime.NoThink
	return runCfg
}

// chooseSession resolves which session a TUI launch uses. Every launch is a FRESH
// session unless --resume <id> names one to reopen. (Auto-resuming the workspace's
// last session was surprising — a bare `kloo` reloaded a stale, possibly polluted
// transcript that also re-primed the model. The id to resume is printed on exit.)
func chooseSession(store *session.Store, cfg config.Config, verifyCmd, lintCmd string, opt SessionOpts, now time.Time) (*session.Session, string, error) {
	if opt.ResumeID != "" {
		s, err := store.Load(opt.ResumeID)
		if err != nil {
			return nil, "", fmt.Errorf("resume session %q: %w", opt.ResumeID, err)
		}
		return s, resumeBanner(s), nil
	}
	// Default (and --new): a clean session. Lint is persisted beside Verify for
	// resume parity.
	return &session.Session{ID: session.NewID(now), Model: cfg.Model, Verify: verifyCmd, Lint: lintCmd, Created: now, Updated: now}, "", nil
}

func resumeBanner(s *session.Session) string {
	return fmt.Sprintf("resumed session · %s · %d run(s) · last active %s",
		titleOrSession(s), s.Runs, s.Updated.Format("Jan 2 15:04"))
}

func titleOrSession(s *session.Session) string {
	if strings.TrimSpace(s.Title) == "" {
		return s.ID
	}
	return s.Title
}
