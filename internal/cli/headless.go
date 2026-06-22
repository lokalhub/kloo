package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

// headlessVerifyTimeout is the per-verify timeout for the headless loop. The
// acceptance benchmark's verify (`npm run build` + the structural harness) is far
// slower than the run_command 30s default, so it gets a generous ceiling.
const headlessVerifyTimeout = 240 // seconds

// defaultRunHeadless composes the same full stack as defaultLaunchTUI (P00 client,
// P01/P02 tools+jail, P03 repo map, P04 loop + safety rails) but runs the loop
// NON-interactively: progress, tool calls, and streamed text are written to out as
// plain lines, and the terminal report is printed at the end. No Bubble Tea / TTY
// is involved, so it works under nohup, CI, or a captured pipe (the Phase-06
// acceptance benchmark, task 03).
func defaultRunHeadless(cfg config.Config, task, verifyCmd string, out io.Writer) error {
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
		Verifier:      agent.NewCommandVerifier(ws, verifyCmd, agent.WithVerifyTimeout(headlessVerifyTimeout)),
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

	// Stream progress/tools to out. Deltas are buffered and flushed on the next
	// tool call or at the end, so streamed reasoning stays readable in a log.
	var streamed strings.Builder
	flush := func() {
		if streamed.Len() > 0 {
			fmt.Fprintf(out, "  │ %s\n", strings.TrimSpace(streamed.String()))
			streamed.Reset()
		}
	}
	loop.OnDelta = func(content string) { streamed.WriteString(content) }
	loop.OnProgress = func(step, maxSteps, tokens, maxTokens int) {
		flush()
		if maxTokens > 0 {
			fmt.Fprintf(out, "── step %d/%d  tokens %d/%d\n", step, maxSteps, tokens, maxTokens)
		} else { // maxTokens 0 ⇒ unbounded: plain counter
			fmt.Fprintf(out, "── step %d/%d  tokens %d\n", step, maxSteps, tokens)
		}
	}
	loop.OnTool = func(call tools.Call, res tools.Result, err error) {
		flush()
		fmt.Fprint(out, headlessToolLine(call, res, err))
	}

	fmt.Fprintf(out, "kloo headless run — effort=%s  model=%s  steps=%d  churn=%d  verify=%q\n",
		cfg.Effort, cfg.Model, cfg.MaxSteps, cfg.ChurnRounds, verifyCmd)
	fmt.Fprintf(out, "task: %s\n\n", task)

	start := time.Now()
	rep, runErr := loop.Run(context.Background(), task)
	flush()
	fmt.Fprintln(out)
	printHeadlessReport(out, rep, time.Since(start))
	if runErr != nil {
		return runErr
	}
	// Exit non-zero unless the loop stopped because the verify passed (success),
	// so a script/CI run can branch on kloo's exit code.
	if rep == nil || rep.Reason != agent.ReasonSuccess {
		return fmt.Errorf("headless run did not reach success (reason: %s)", reportReason(rep))
	}
	return nil
}

// headlessToolLine renders one dispatched tool call as a compact log line.
func headlessToolLine(call tools.Call, res tools.Result, err error) string {
	switch call.Name {
	case tools.NameRunCommand:
		status := fmt.Sprintf("exit %d", res.ExitCode)
		if err != nil {
			status = "error: " + err.Error()
		} else if res.ExitCode == 0 {
			status += " ✓"
		} else {
			status += " ✗"
		}
		return fmt.Sprintf("  → run_command: %s  [%s]\n", str(call.Args["command"]), status)
	case tools.NameEditFile, tools.NameWriteFile:
		if err != nil {
			return fmt.Sprintf("  → %s: %s  [error: %v]\n", call.Name, str(call.Args["path"]), err)
		}
		return fmt.Sprintf("  → %s: %s\n", call.Name, str(call.Args["path"]))
	default:
		if err != nil {
			return fmt.Sprintf("  → %s  [error: %v]\n", call.Name, err)
		}
		return fmt.Sprintf("  → %s\n", call.Name)
	}
}

// printHeadlessReport writes the loop's terminal report as a plain block.
func printHeadlessReport(out io.Writer, rep *agent.Report, elapsed time.Duration) {
	if rep == nil {
		fmt.Fprintf(out, "run stopped — no report (elapsed %s)\n", elapsed.Round(time.Second))
		return
	}
	fmt.Fprintf(out, "run stopped — %s\n", strings.ToUpper(string(rep.Reason)))
	fmt.Fprintf(out, "  steps:   %d\n", rep.Steps)
	fmt.Fprintf(out, "  tokens:  %d\n", rep.TokensUsed)
	fmt.Fprintf(out, "  elapsed: %s\n", rep.Elapsed.Round(time.Second))
	if rep.FinalVerify.Command != "" {
		fmt.Fprintf(out, "  verify:  %q → exit %d (passed=%t)\n",
			rep.FinalVerify.Command, rep.FinalVerify.ExitCode, rep.FinalVerify.Passed)
	}
	if rep.Compactions > 0 {
		// Printed only when memory actually compacted, so a short run's report is
		// byte-identical to pre-P00 (mirrors the optional budget/churn lines).
		fmt.Fprintf(out, "  compactions: %d\n", rep.Compactions)
	}
	if rep.Budget != nil {
		fmt.Fprintf(out, "  budget:  %s (%s/%s)\n", rep.Budget.Kind, rep.Budget.Observed, rep.Budget.Limit)
	}
	if rep.Churn != nil {
		fmt.Fprintf(out, "  churn:   %s\n", rep.Churn.Kind)
	}
	if rep.RolledBack {
		fmt.Fprintln(out, "  rolled back to checkpoint")
	}
	if rep.Err != nil {
		fmt.Fprintf(out, "  error:   %v\n", rep.Err)
	}
}

// str extracts a string tool-arg value (empty when absent or non-string).
func str(v any) string {
	s, _ := v.(string)
	return s
}

func reportReason(rep *agent.Report) string {
	if rep == nil {
		return "none"
	}
	return string(rep.Reason)
}
