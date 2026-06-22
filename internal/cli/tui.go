package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
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

	// Resolve which session this launch uses (new / resume / pick), and the banner
	// shown in the transcript when resuming.
	store := session.NewStore(cwd)
	sess, banner, err := chooseSession(store, cfg, verifyCmd, opt, os.Stdin, os.Stderr, time.Now())
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

	runner := tui.NewLoopRunner(loop, ws, cfg.MaxTokens).WithSession(store, sess)
	return tui.Run(tui.Config{
		Version:   Version(),
		Effort:    cfg.Effort,
		Model:     cfg.Model,
		MaxSteps:  cfg.MaxSteps,
		MaxTokens: cfg.MaxTokens,
		Runner:    runner,
		Banner:    banner,
	})
}

// chooseSession resolves which session a TUI launch uses. Policy when neither
// --new nor --resume is given: resume the workspace's single session, prompt when
// there are several, start fresh when there are none. in/out are injectable so the
// picker is testable.
func chooseSession(store *session.Store, cfg config.Config, verifyCmd string, opt SessionOpts, in io.Reader, out io.Writer, now time.Time) (*session.Session, string, error) {
	fresh := func() *session.Session {
		return &session.Session{ID: session.NewID(now), Model: cfg.Model, Verify: verifyCmd, Created: now, Updated: now}
	}
	if opt.New {
		return fresh(), "", nil
	}
	if opt.ResumeID != "" {
		s, err := store.Load(opt.ResumeID)
		if err != nil {
			return nil, "", fmt.Errorf("resume session %q: %w", opt.ResumeID, err)
		}
		return s, resumeBanner(s), nil
	}
	metas, err := store.List()
	if err != nil {
		return nil, "", err
	}
	switch len(metas) {
	case 0:
		return fresh(), "", nil
	case 1:
		if s, err := store.Load(metas[0].ID); err == nil {
			return s, resumeBanner(s), nil
		}
		return fresh(), "", nil // corrupt single session ⇒ start clean
	default:
		id := promptPick(metas, in, out)
		if id == "" {
			return fresh(), "", nil
		}
		if s, err := store.Load(id); err == nil {
			return s, resumeBanner(s), nil
		}
		return fresh(), "", nil
	}
}

// promptPick shows the saved sessions and reads a choice; returns the chosen id,
// or "" for a new session (also the default on empty/invalid input).
func promptPick(metas []session.Meta, in io.Reader, out io.Writer) string {
	fmt.Fprintln(out, "Multiple kloo sessions in this workspace:")
	for i, m := range metas {
		fmt.Fprintf(out, "  %d) %s  · %d run(s) · last active %s\n", i+1, titleOr(m), m.Runs, m.Updated.Format("Jan 2 15:04"))
	}
	fmt.Fprintf(out, "  n) new session\nResume which? [1-%d / n]: ", len(metas))
	var choice string
	fmt.Fscanln(in, &choice)
	switch strings.TrimSpace(choice) {
	case "", "n", "N":
		return ""
	}
	if idx, err := strconv.Atoi(strings.TrimSpace(choice)); err == nil && idx >= 1 && idx <= len(metas) {
		return metas[idx-1].ID
	}
	return "" // invalid ⇒ new session
}

func resumeBanner(s *session.Session) string {
	return fmt.Sprintf("resumed session · %s · %d run(s) · last active %s",
		titleOrSession(s), s.Runs, s.Updated.Format("Jan 2 15:04"))
}

func titleOr(m session.Meta) string {
	if strings.TrimSpace(m.Title) == "" {
		return m.ID
	}
	return m.Title
}

func titleOrSession(s *session.Session) string {
	if strings.TrimSpace(s.Title) == "" {
		return s.ID
	}
	return s.Title
}
