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
func defaultLaunchTUI(cfg config.Config, verifyCmd string, lint lintOpts, opt SessionOpts) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ws, err := tools.NewWorkspace(cwd)
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
	reg, closeMCP := wireMCP(ctx, cfg, ws, writerLogf(os.Stderr))
	defer closeMCP()

	loop := &agent.Loop{
		Client:        llm.New(cfg.Endpoint, cfg.Model, llm.WithAPIKey(cfg.APIKey)),
		Adapter:       adapter,
		Registry:      reg,
		Verifier:      buildVerifier(ws, verifyCmd),
		Linter:        buildLinter(ws, lintCmd, lintPerFile),
		Budget:        agent.NewBudget(cfg, nil),
		Churn:         agent.NewChurnDetector(cfg.ChurnRounds),
		Checkpoint:    agent.NewGitCheckpointer(cwd),
		Root:          ws.Root(),
		ContextTokens: cfg.MaxContextTokens,
		Memory:        agent.NewWorkingMemory(), // working memory on by default (P00); maxContextTokens governs compaction
		System:        defaultSystemPrompt + agentsInstructions(cwd, cfg.AllowedImportDirs, cfg.MaxContextTokens, writerLogf(os.Stderr)),
		ChatSystem:    chatGateSystemPrompt, // interactive only: answer chit-chat without launching a run

		StallRounds: cfg.ChurnRounds,
		Model:       cfg.Model,
		Temperature: cfg.Temperature,
	}

	runner := tui.NewLoopRunner(loop, ws, cfg.MaxTokens).WithSession(store, sess)
	runErr := tui.Run(tui.Config{
		Version:   Version(),
		Effort:    cfg.Effort,
		Model:     cfg.Model,
		MaxSteps:  cfg.MaxSteps,
		MaxTokens: cfg.MaxTokens,
		Runner:    runner,
		Banner:    banner,
		History:   sess.Transcript, // replay prior turns on resume (empty for a fresh session)
	})
	// Sessions are fresh by default, so on exit print the id (only once something
	// was saved) so the user can pick this conversation back up with --resume.
	if sess.Runs > 0 {
		fmt.Fprintf(os.Stderr, "\nsession %s saved · resume it with:  kloo --resume %s\n", sess.ID, sess.ID)
	}
	return runErr
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
