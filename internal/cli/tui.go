package cli

import (
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
// under the Bubble Tea TUI (P05). The verify command (verifyCmd) is the real
// success signal the loop trusts each step.
func defaultLaunchTUI(cfg config.Config, verifyCmd string, opt SessionOpts) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ws, err := tools.NewWorkspace(cwd)
	if err != nil {
		return err
	}

	// Resolve which session this launch uses (fresh by default, or --resume <id>),
	// and the banner shown in the transcript when resuming.
	store := session.NewStore(cwd)
	sess, banner, err := chooseSession(store, cfg, verifyCmd, opt, time.Now())
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
		System:        defaultSystemPrompt,
		StallRounds:   cfg.ChurnRounds,
		Model:         cfg.Model,
		Temperature:   cfg.Temperature,
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
func chooseSession(store *session.Store, cfg config.Config, verifyCmd string, opt SessionOpts, now time.Time) (*session.Session, string, error) {
	if opt.ResumeID != "" {
		s, err := store.Load(opt.ResumeID)
		if err != nil {
			return nil, "", fmt.Errorf("resume session %q: %w", opt.ResumeID, err)
		}
		return s, resumeBanner(s), nil
	}
	// Default (and --new): a clean session.
	return &session.Session{ID: session.NewID(now), Model: cfg.Model, Verify: verifyCmd, Created: now, Updated: now}, "", nil
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
