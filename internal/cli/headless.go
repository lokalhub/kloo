package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

// headlessVerifyTimeout is the per-verify timeout for the headless loop's verify
// step (the acceptance benchmark's `npm run build` + structural harness). Kept at
// least as generous as the run_command default so a slow build is never the tighter
// ceiling.
const headlessVerifyTimeout = 300 // seconds (matches the run_command default)

// defaultRunHeadless composes the same full stack as defaultLaunchTUI (P00 client,
// P01/P02 tools+jail, P03 repo map, P04 loop + safety rails) but runs the loop
// NON-interactively: progress, tool calls, and streamed text are written to out as
// plain lines, and the terminal report is printed at the end. No Bubble Tea / TTY
// is involved, so it works under nohup, CI, or a captured pipe (the Phase-06
// acceptance benchmark, task 03).
func defaultRunHeadless(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ws, err := tools.NewWorkspace(cwd)
	if err != nil {
		return err
	}
	// Resolve the verify command: deprecated --verify override, else auto-detect,
	// else "" (unverified — the run can only end in answered/unverified, not success).
	verifyCmd = resolveVerifyCommand(verifyCmd, cwd, writerLogf(out))
	// Resolve the fast advisory lint command (--lint/--no-lint + env, else auto-detect,
	// else "" = no lint step). Advisory only — it never gates the run's success.
	lintCmd, lintPerFile := resolveLintCommand(lint.Override, lint.Disabled, cwd, writerLogf(out))
	adapter, err := tools.SelectAdapter(cfg.ToolFormat, tools.EndpointCaps{SupportsTools: true})
	if err != nil {
		return err
	}

	// MCP: connect configured servers (non-fatal) + register their tools alongside
	// the builtins; the startup/trust lines go to out. Closed on return.
	ctx := context.Background()
	reg, closeMCP := wireMCP(ctx, cfg, ws, writerLogf(out))
	defer closeMCP()

	loop := &agent.Loop{
		Client:        llm.New(cfg.Endpoint, cfg.Model, llm.WithAPIKey(cfg.APIKey)),
		Adapter:       adapter,
		Registry:      reg,
		Verifier:      buildVerifier(ws, verifyCmd, agent.WithVerifyTimeout(headlessVerifyTimeout)),
		Linter:        buildLinter(ws, lintCmd, lintPerFile),
		Budget:        agent.NewBudget(cfg, nil),
		Churn:         agent.NewChurnDetector(cfg.ChurnRounds),
		Checkpoint:    agent.NewGitCheckpointer(cwd),
		Root:          ws.Root(),
		ContextTokens: cfg.MaxContextTokens,
		Memory:        agent.NewWorkingMemory(), // working memory on by default (P00); maxContextTokens governs compaction
		System:        defaultSystemPrompt + agentsInstructions(cwd, cfg.AllowedImportDirs, cfg.MaxContextTokens, writerLogf(out)),
		StallRounds:   cfg.ChurnRounds,
		Endpoint:      cfg.Endpoint,
		Model:         cfg.Model,
		Temperature:   cfg.Temperature,
		NoThink:       cfg.NoThink,
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
	loop.OnRetry = func(attempt, max int, err error, wait time.Duration) {
		flush()
		fmt.Fprintf(out, "⟳ model call failed transiently — retrying %d/%d in %s\n", attempt, max, wait.Round(time.Second))
	}

	fmt.Fprintf(out, "kloo headless run — effort=%s  model=%s  steps=%d  churn=%d  verify=%q  lint=%q\n",
		cfg.Effort, cfg.Model, cfg.MaxSteps, cfg.ChurnRounds, verifyCmd, lintCmd)
	fmt.Fprintf(out, "task: %s\n\n", task)

	start := time.Now()
	rep, runErr := loop.Run(ctx, task)
	elapsed := time.Since(start)
	if cfg.JSONOnly {
		applyJSONOnlyValidation(rep)
	}
	flush()
	fmt.Fprintln(out)
	printHeadlessReport(out, rep, elapsed)
	if cfg.JSONSummary {
		printHeadlessJSON(out, cfg, verifyCmd, rep, elapsed, runErr)
	}
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

type verifySummary struct {
	Command  string `json:"command"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
}

type runSummary struct {
	Model          string         `json:"model"`
	Endpoint       string         `json:"endpoint"`
	Ctx            int            `json:"ctx"`
	Reason         string         `json:"reason"`
	Success        bool           `json:"success"`
	Steps          int            `json:"steps"`
	Tokens         int            `json:"tokens"`
	ElapsedSeconds float64        `json:"elapsed_seconds"`
	TokensPerSec   float64        `json:"tokens_per_sec"`
	Compactions    int            `json:"compactions"`
	Verify         *verifySummary `json:"verify,omitempty"`
	Error          string         `json:"error,omitempty"`
	TranscriptTail string         `json:"transcript_tail,omitempty"`
}

func buildRunSummary(cfg config.Config, verifyCmd string, rep *agent.Report, elapsed time.Duration, runErr error) runSummary {
	round2 := func(f float64) float64 { return math.Round(f*100) / 100 }
	s := runSummary{Model: cfg.Model, Endpoint: cfg.Endpoint, Ctx: cfg.MaxContextTokens, ElapsedSeconds: round2(elapsed.Seconds())}
	if rep != nil {
		s.Reason = string(rep.Reason)
		s.Success = rep.Reason == agent.ReasonSuccess
		s.Steps = rep.Steps
		s.Tokens = rep.TokensUsed
		s.Compactions = rep.Compactions
		if secs := elapsed.Seconds(); secs > 0 {
			s.TokensPerSec = round2(float64(rep.TokensUsed) / secs)
		}
		if rep.FinalVerify.Command != "" {
			s.Verify = &verifySummary{Command: rep.FinalVerify.Command, Passed: rep.FinalVerify.Passed, ExitCode: rep.FinalVerify.ExitCode}
		}
		if rep.Err != nil {
			s.Error = rep.Err.Error()
		}
		s.TranscriptTail = transcriptTail(rep.Transcript, 600)
	}
	if runErr != nil && s.Error == "" {
		s.Error = runErr.Error()
	}
	if verifyCmd != "" && s.Verify == nil {
		s.Verify = &verifySummary{Command: verifyCmd}
	}
	return s
}

// printHeadlessJSON emits a compact, machine-readable result line (prefixed
// KLOO_RESULT_JSON) for benchmarking harnesses: model/endpoint/ctx, the terminal
// reason + success, steps/tokens/tokens-per-sec/elapsed, the final verify, any error,
// and a short transcript tail. A harness greps the prefix and parses the rest.
func printHeadlessJSON(out io.Writer, cfg config.Config, verifyCmd string, rep *agent.Report, elapsed time.Duration, runErr error) {
	b, err := json.Marshal(buildRunSummary(cfg, verifyCmd, rep, elapsed, runErr))
	if err != nil {
		return
	}
	fmt.Fprintf(out, "KLOO_RESULT_JSON %s\n", b)
}

func writeRunSummaryFile(path string, summary runSummary) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	f, err := os.CreateTemp(dir, "."+base+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(summary); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func applyJSONOnlyValidation(rep *agent.Report) {
	if rep == nil {
		return
	}
	if rep.Err != nil {
		return
	}
	answer := finalAssistantAnswer(rep.Transcript)
	if err := validateJSONOnly(answer); err != nil {
		rep.Reason = agent.ReasonError
		rep.Err = fmt.Errorf("final assistant answer must be valid JSON only; remove prose/code fences and return one JSON value: %w", err)
	}
}

func finalAssistantAnswer(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant {
			return strings.TrimSpace(msgs[i].Content)
		}
	}
	return ""
}

func validateJSONOnly(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("empty answer")
	}
	dec := json.NewDecoder(bytes.NewBufferString(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("extra content after JSON value")
		}
		return err
	}
	return nil
}

// transcriptTail returns the last maxBytes of the transcript as "role: content"
// lines, for the JSON summary's failure-diagnosis tail.
func transcriptTail(msgs []llm.Message, maxBytes int) string {
	var b strings.Builder
	for _, m := range msgs {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	s := strings.TrimSpace(b.String())
	if len(s) > maxBytes {
		s = "…" + s[len(s)-maxBytes:]
	}
	return s
}
