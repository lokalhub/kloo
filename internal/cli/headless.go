package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/edit"
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
		return maybeBenchmarkSetupError(cfg, err)
	}
	ws, err := tools.NewWorkspace(cwd)
	if err != nil {
		return maybeBenchmarkSetupError(cfg, err)
	}
	// Attach the A1/A2 scope policy + A4 patch-only flag to the workspace, so the
	// model's edit_file/write_file are gated and (when scope/patch-only is active)
	// the model-facing run_command is withheld. The verifier/linter are unaffected.
	ws, err = applyScope(cfg, ws, cwd, writerLogf(out))
	if err != nil {
		return maybeBenchmarkSetupError(cfg, err)
	}
	// Resolve the verify command: deprecated --verify override, else auto-detect,
	// else "" (unverified — the run can only end in answered/unverified, not success).
	verifyCmd = resolveVerifyCommand(verifyCmd, cwd, writerLogf(out))
	// Resolve the fast advisory lint command (--lint/--no-lint + env, else auto-detect,
	// else "" = no lint step). Advisory only — it never gates the run's success.
	lintCmd, lintPerFile := resolveLintCommand(lint.Override, lint.Disabled, cwd, writerLogf(out))
	adapter, err := tools.SelectAdapter(cfg.ToolFormat, tools.EndpointCaps{SupportsTools: true})
	if err != nil {
		return maybeBenchmarkSetupError(cfg, err)
	}

	// MCP: connect configured servers (non-fatal) + register their tools alongside
	// the builtins; the startup/trust lines go to out. Closed on return.
	ctx := context.Background()
	reg, mcpMgr, closeMCP := wireMCP(ctx, cfg, ws, writerLogf(out))
	defer closeMCP()
	recall := memoryRecall(ctx, cfg, mcpMgr, cwd, task, writerLogf(out))
	systemPrompt := defaultSystemPrompt + scopeSystemPromptSuffix(ws) + agentsInstructions(cwd, cfg.AllowedImportDirs, cfg.MaxContextTokens, writerLogf(out))
	systemPrompt += memoryRecallSystemSection(recall)

	loop := &agent.Loop{
		Client:               llm.New(cfg.Endpoint, cfg.Model, llm.WithAPIKey(cfg.APIKey), llm.WithTimeout(cfg.LLMColdLoadTimeout), llm.WithStreamIdleTimeout(cfg.LLMStreamIdleTimeout)),
		Adapter:              adapter,
		Registry:             reg,
		Verifier:             buildLayeredVerifier(ws, verifyCmd, cfg.Prechecks, cfg.Postchecks, writerLogf(out), agent.WithVerifyTimeout(headlessVerifyTimeout)),
		Linter:               buildLinter(ws, lintCmd, lintPerFile),
		Budget:               agent.NewBudget(cfg, nil),
		Churn:                agent.NewChurnDetector(cfg.ChurnRounds),
		Checkpoint:           agent.NewGitCheckpointer(cwd),
		Root:                 ws.Root(),
		ContextTokens:        cfg.MaxContextTokens,
		Memory:               agent.NewWorkingMemory(), // working memory on by default (P00); maxContextTokens governs compaction
		System:               systemPrompt,
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

	fmt.Fprintf(out, "kloo task run — effort=%s  model=%s  steps=%d  churn=%d  verify=%q  lint=%q\n",
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
	summary := buildRunSummary(cfg, verifyCmd, rep, elapsed, runErr)
	memoryStore(ctx, cfg, mcpMgr, cwd, task, summary, rep, writerLogf(out))
	if cfg.JSONSummary || cfg.BenchmarkMode {
		withFilesChanged(&summary, ws.Root()) // B3: changed-file accounting for the JSON
		printRunSummaryJSON(out, summary)
	}
	if cfg.BenchmarkMode {
		code := benchmarkExitCode(summary)
		if code == 0 {
			return nil
		}
		return exitError{code: code, err: fmt.Errorf("benchmark run did not reach success (failure_code: %s)", summary.FailureCode)}
	}
	if runErr != nil {
		return runErr
	}
	// Exit non-zero unless the loop stopped because the verify passed (success),
	// so a script/CI run can branch on kloo's exit code.
	if rep == nil || rep.Reason != agent.ReasonSuccess {
		return fmt.Errorf("task run did not reach success (reason: %s)", reportReason(rep))
	}
	return nil
}

func maybeBenchmarkSetupError(cfg config.Config, err error) error {
	if cfg.BenchmarkMode {
		return exitError{code: benchmarkExitConfigError, err: err}
	}
	return err
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
	if len(rep.RailFires) > 0 {
		// Printed only when a soft rail fired, so a clean run's footer is unchanged.
		// Stable key order so the line is deterministic (tests, diffing across runs).
		names := make([]string, 0, len(rep.RailFires))
		for k := range rep.RailFires {
			names = append(names, k)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, k := range names {
			parts = append(parts, fmt.Sprintf("%s×%d", k, rep.RailFires[k]))
		}
		fmt.Fprintf(out, "  rails:   %s\n", strings.Join(parts, ", "))
	}
	if !toolCountersZero(rep.ToolCounters) {
		fmt.Fprintf(out, "  tool counters: %s\n", formatToolCounters(rep.ToolCounters))
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

type failureDetail struct {
	Source     string `json:"source,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Class      string `json:"class,omitempty"`
	Tool       string `json:"tool,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Message    string `json:"message,omitempty"`
}

type toolCountersSummary struct {
	InvalidToolCalls int `json:"invalid_tool_calls"`
	RepeatedReadFile int `json:"repeated_read_file"`
	RepeatedEdits    int `json:"repeated_edits"`
	FailedEdits      int `json:"failed_edits"`
	NoOpEdits        int `json:"no_op_edits"`
	VerifyAttempts   int `json:"verify_attempts"`
	ToolErrors       int `json:"tool_errors"`
	OffScopeEdits    int `json:"off_scope_edits"`
	ReadOnlyEdits    int `json:"read_only_edits"`
}

type runSummary struct {
	BenchmarkMode  bool           `json:"benchmark_mode,omitempty"`
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
	FailureCode    string         `json:"failure_code,omitempty"`
	FailureDetail  *failureDetail `json:"failure_detail,omitempty"`
	TranscriptTail string         `json:"transcript_tail,omitempty"`
	// RailFires tallies the soft recovery rails that fired (corrective injected, run
	// continued), keyed by rail name. Omitted when none fired, so a clean run's JSON is
	// unchanged. Lets a benchmark assert a run's self-corrections (e.g. confirm-finish=1).
	RailFires    map[string]int       `json:"rail_fires,omitempty"`
	ToolCounters *toolCountersSummary `json:"tool_counters,omitempty"`
	// B5 layered verifier hooks: the precheck/postcheck gates attempted for the
	// final verify (command/passed/exit_code). Omitted when no hooks are configured,
	// so an un-hooked run's JSON is byte-identical.
	Prechecks  []hookSummary `json:"prechecks,omitempty"`
	Postchecks []hookSummary `json:"postchecks,omitempty"`
	// B3 benchmark accounting deltas (missing accountability only; shipped metrics
	// above are NOT duplicated). FilesChanged is emitted in benchmark mode (or when
	// non-empty); OffScopeEdits mirrors the report counter; CorrectionCount is the sum
	// of rail_fires; FinalReason is the most specific terminal class.
	FilesChanged    *filesChangedSummary `json:"files_changed,omitempty"`
	OffScopeEdits   int                  `json:"off_scope_edits"`
	CorrectionCount int                  `json:"correction_count"`
	FinalReason     string               `json:"final_reason"`
}

type hookSummary struct {
	Command  string `json:"command"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
}

type filesChangedSummary struct {
	Count int      `json:"count"`
	Paths []string `json:"paths"`
}

func buildRunSummary(cfg config.Config, verifyCmd string, rep *agent.Report, elapsed time.Duration, runErr error) runSummary {
	round2 := func(f float64) float64 { return math.Round(f*100) / 100 }
	s := runSummary{BenchmarkMode: cfg.BenchmarkMode, Model: cfg.Model, Endpoint: cfg.Endpoint, Ctx: cfg.MaxContextTokens, ElapsedSeconds: round2(elapsed.Seconds())}
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
		s.RailFires = rep.RailFires
		if cfg.BenchmarkMode || !toolCountersZero(rep.ToolCounters) {
			tc := rep.ToolCounters
			s.ToolCounters = &toolCountersSummary{
				InvalidToolCalls: tc.InvalidToolCalls,
				RepeatedReadFile: tc.RepeatedReadFile,
				RepeatedEdits:    tc.RepeatedEdits,
				FailedEdits:      tc.FailedEdits,
				NoOpEdits:        tc.NoOpEdits,
				VerifyAttempts:   tc.VerifyAttempts,
				ToolErrors:       tc.ToolErrors,
				OffScopeEdits:    tc.OffScopeEdits,
				ReadOnlyEdits:    tc.ReadOnlyEdits,
			}
		}
		// B5: surface the precheck/postcheck gates attempted for the final verify.
		s.Prechecks = hookSummaries(rep.FinalVerify.Prechecks)
		s.Postchecks = hookSummaries(rep.FinalVerify.Postchecks)
		// B3 accounting deltas derived from the report (files_changed needs the git
		// root and is filled by the CLI entry point via withFilesChanged).
		s.OffScopeEdits = rep.ToolCounters.OffScopeEdits
		s.CorrectionCount = correctionCount(rep.RailFires)
	}
	if runErr != nil && s.Error == "" {
		s.Error = runErr.Error()
	}
	if verifyCmd != "" && s.Verify == nil {
		s.Verify = &verifySummary{Command: verifyCmd}
	}
	s.FailureCode, s.FailureDetail = classifyFailure(rep, runErr)
	// B3 final_reason: the most specific terminal class (failure_detail.class when a
	// failure was classified), else the report reason (e.g. "success"). Lets a harness
	// read one field instead of composing reason + failure_code + class.
	s.FinalReason = finalReason(s)
	return s
}

// hookSummaries converts agent HookResults to the JSON hook summary shape.
func hookSummaries(hooks []agent.HookResult) []hookSummary {
	if len(hooks) == 0 {
		return nil
	}
	out := make([]hookSummary, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, hookSummary{Command: h.Command, Passed: h.Passed, ExitCode: h.ExitCode})
	}
	return out
}

// correctionCount is the deterministic sum of all rail_fires values (B3): the soft
// model-facing corrections that fired this run. rail_fires itself is unchanged.
func correctionCount(railFires map[string]int) int {
	n := 0
	for _, v := range railFires {
		n += v
	}
	return n
}

// finalReason returns the most specific terminal class for the run.
func finalReason(s runSummary) string {
	if s.FailureDetail != nil && s.FailureDetail.Class != "" {
		return s.FailureDetail.Class
	}
	if s.FailureCode != "" {
		return s.FailureCode
	}
	return s.Reason
}

// withFilesChanged fills the B3 files_changed accounting from the git working tree
// at root (sorted, deterministic). It is a no-op when root is empty. Called by the
// CLI entry points (which know the workspace root); a non-git repo yields count:0.
func withFilesChanged(s *runSummary, root string) {
	if root == "" {
		return
	}
	paths := agent.ChangedFiles(context.Background(), root)
	s.FilesChanged = &filesChangedSummary{Count: len(paths), Paths: paths}
}

func classifyFailure(rep *agent.Report, runErr error) (string, *failureDetail) {
	if rep != nil && rep.Reason == agent.ReasonSuccess {
		return "", nil
	}
	detail := &failureDetail{Source: "internal"}
	if rep != nil {
		detail.Reason = string(rep.Reason)
	}
	err := runErr
	if rep != nil && rep.Err != nil {
		err = rep.Err
	}
	msg := boundedMessage(err)
	if rep == nil {
		if strings.Contains(msg, "config:") {
			detail.Source = "config"
			detail.Class = "config_error"
			detail.Message = msg
			return "config_error", detail
		}
		detail.Class = "nil_report"
		detail.Message = msg
		return "internal_error", detail
	}
	// B5: a precheck/postcheck hook failure classifies distinctly, regardless of the
	// terminal reason (answered/churn/error), and takes precedence over verify_failed
	// (the hook, not verify, is the salient failure). Verify's own failure keeps
	// FailedStage="" and falls through to the existing verify_failed path.
	if code, d := hookFailure(rep, detail); code != "" {
		return code, d
	}
	switch rep.Reason {
	case agent.ReasonBudgetExceeded:
		detail.Source = "budget"
		if rep.Budget != nil {
			detail.Class = string(rep.Budget.Kind)
			detail.Message = fmt.Sprintf("%s/%s", rep.Budget.Observed, rep.Budget.Limit)
		}
		return "budget_exceeded", detail
	case agent.ReasonInterrupted:
		detail.Source = "internal"
		detail.Class = "interrupted"
		detail.Message = msg
		return "interrupted", detail
	case agent.ReasonChurn:
		detail.Source = "rail"
		if rep.Churn != nil {
			detail.Class = string(rep.Churn.Kind)
			if rep.Churn.Class != "" {
				detail.Class = rep.Churn.Class
			}
			detail.Tool = rep.Churn.Tool
			detail.Message = boundedString(rep.Churn.Artifact, 240)
			if rep.Churn.Kind == agent.ChurnEditFailed {
				return "edit_failed", detail
			}
		}
		return "repetition_halt", detail
	case agent.ReasonSafetyStop:
		return safetyStopFailure(rep, detail)
	case agent.ReasonUnverified:
		if rep.FinalVerify.Command != "" && !rep.FinalVerify.Passed {
			return verifyFailure(rep, detail)
		}
		if code, d := scopeDenialFailure(rep, detail); code != "" {
			return code, d
		}
		if code, d := patchOnlyRejectFailure(rep, detail); code != "" {
			return code, d
		}
		detail.Source = "verify"
		detail.Class = "no_verify_command"
		detail.Message = "no verify command was available"
		return "unverified", detail
	case agent.ReasonAnswered:
		if rep.FinalVerify.Command != "" && !rep.FinalVerify.Passed {
			return verifyFailure(rep, detail)
		}
		if code, d := scopeDenialFailure(rep, detail); code != "" {
			return code, d
		}
		if code, d := patchOnlyRejectFailure(rep, detail); code != "" {
			return code, d
		}
		detail.Source = "internal"
		detail.Class = "answered"
		return "answered", detail
	case agent.ReasonError:
		return classifyErrorFailure(rep, err, detail)
	default:
		detail.Class = "unknown_reason"
		detail.Message = msg
		return "internal_error", detail
	}
}

func verifyFailure(rep *agent.Report, detail *failureDetail) (string, *failureDetail) {
	detail.Source = "verify"
	detail.Class = "verify_failed"
	line := firstNonEmptyLine(rep.FinalVerify.Stdout, rep.FinalVerify.Stderr)
	if line != "" {
		detail.Message = boundedString(line, 240)
	}
	return "verify_failed", detail
}

// hookFailure classifies a B5 precheck/postcheck gate failure. It returns
// ("", nil) when no hook failed (FailedStage is "") so the caller uses its normal
// classification (verify_failed etc.). A precheck failure → precheck_failed (verify
// never ran); a postcheck failure → postcheck_failed (verify passed but the gate did
// not, so success is still false).
func hookFailure(rep *agent.Report, detail *failureDetail) (string, *failureDetail) {
	stage := rep.FinalVerify.FailedStage
	if stage != "precheck" && stage != "postcheck" {
		return "", nil
	}
	detail.Source = stage
	detail.Class = stage + "_failed"
	line := firstNonEmptyLine(rep.FinalVerify.Stdout, rep.FinalVerify.Stderr)
	cmd := rep.FinalVerify.Command
	switch {
	case cmd != "" && line != "":
		detail.Message = boundedString(cmd+": "+line, 240)
	case cmd != "":
		detail.Message = boundedString(cmd, 240)
	default:
		detail.Message = boundedString(line, 240)
	}
	if stage == "precheck" {
		return "precheck_failed", detail
	}
	return "postcheck_failed", detail
}

// safetyStopFailure maps an A7 ReasonSafetyStop onto a stable failure code:
// off_scope_edit for a scope/read-only stop (spec preserves off_scope_edit as the
// shared scope-denial code), and repetition_halt (class repeated_verify_failure)
// for the repeated-verify stop — keeping the shipped verify_failed meaning as
// "final failed verify".
func safetyStopFailure(rep *agent.Report, detail *failureDetail) (string, *failureDetail) {
	s := rep.Safety
	if s == nil {
		detail.Source = "rail"
		detail.Class = "safety_stop"
		return "off_scope_edit", detail
	}
	if s.Rule == "repeated-verify" {
		detail.Source = "rail"
		detail.Class = "repeated_verify_failure"
		detail.Message = boundedString(s.Message, 240)
		return "repetition_halt", detail
	}
	detail.Source = "scope"
	detail.Class = s.Class
	detail.Tool = s.Tool
	detail.Message = boundedString(s.Message, 240)
	return "off_scope_edit", detail
}

// scopeDenialFailure classifies a non-stop run that ended calmly (answered/
// unverified) AFTER a scope denial as off_scope_edit — so the denial is visible in
// KLOO_RESULT_JSON even without a --stop-on rule. Returns ("", nil) when the run had
// no scope denial (the caller then uses its normal classification).
func scopeDenialFailure(rep *agent.Report, detail *failureDetail) (string, *failureDetail) {
	d := rep.LastScopeDenial
	if d == nil {
		return "", nil
	}
	detail.Source = "scope"
	detail.Class = d.Class
	detail.Tool = d.Tool
	detail.Message = boundedString(d.Message, 240)
	return "off_scope_edit", detail
}

// patchOnlyRejectFailure classifies a run that ended calmly (answered/unverified)
// AFTER a patch-only run_command rejection (A4) as tool_call_invalid with class
// patch_only_forbidden_tool — so the machine-readable classification the spec
// promises is present in KLOO_RESULT_JSON even without a terminal error. Returns
// ("", nil) when the run had no patch-only rejection.
func patchOnlyRejectFailure(rep *agent.Report, detail *failureDetail) (string, *failureDetail) {
	r := rep.PatchOnlyReject
	if r == nil {
		return "", nil
	}
	detail.Source = "tool"
	detail.Class = r.Class
	detail.Tool = r.Tool
	detail.Message = boundedString(r.Message, 240)
	return "tool_call_invalid", detail
}

func classifyErrorFailure(rep *agent.Report, err error, detail *failureDetail) (string, *failureDetail) {
	detail.Message = boundedMessage(err)
	switch {
	case errors.Is(err, agent.ErrWindowTooSmall) || strings.Contains(strings.ToLower(detail.Message), "context") && strings.Contains(strings.ToLower(detail.Message), "too small"):
		detail.Source = "config"
		detail.Class = "window_too_small"
		return "context_too_small", detail
	case strings.Contains(detail.Message, "valid JSON only"):
		detail.Source = "json"
		detail.Class = "json_decoder"
		return "json_invalid", detail
	case errors.Is(err, tools.ErrMalformedToolCall) || strings.Contains(detail.Message, "no usable tool call"):
		detail.Source = "tool"
		detail.Class = "malformed_tool_call"
		return "tool_call_invalid", detail
	case errors.Is(err, tools.ErrPatchOnlyForbidden):
		detail.Source = "tool"
		detail.Class = "patch_only_forbidden_tool"
		return "tool_call_invalid", detail
	case errors.Is(err, tools.ErrUnknownTool) || errors.Is(err, tools.ErrInvalidArgs):
		detail.Source = "tool"
		detail.Class = "invalid_tool"
		return "tool_call_invalid", detail
	case errors.Is(err, edit.ErrSearchNotFound) || errors.Is(err, edit.ErrAmbiguousMatch) || errors.Is(err, edit.ErrMalformedBlock):
		detail.Source = "edit"
		detail.Class = "edit_apply"
		return "edit_failed", detail
	case strings.HasPrefix(detail.Message, "verify:") || (rep.FinalVerify.Command != "" && rep.FinalVerify.Err != nil):
		return verifyFailure(rep, detail)
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		detail.Source = "llm"
		detail.Class = "api_error"
		detail.HTTPStatus = apiErr.StatusCode
		return "model_error", detail
	}
	if strings.Contains(detail.Message, "llm:") || strings.Contains(detail.Message, "model call failed") {
		detail.Source = "llm"
		detail.Class = "model_error"
		return "model_error", detail
	}
	if strings.Contains(detail.Message, "config:") {
		detail.Source = "config"
		detail.Class = "config_error"
		return "config_error", detail
	}
	detail.Source = "internal"
	detail.Class = "internal_error"
	return "internal_error", detail
}

func toolCountersZero(c agent.ToolCounters) bool {
	return c == agent.ToolCounters{}
}

func formatToolCounters(c agent.ToolCounters) string {
	parts := []string{}
	add := func(name string, n int) {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", name, n))
		}
	}
	add("invalid_tool_calls", c.InvalidToolCalls)
	add("repeated_read_file", c.RepeatedReadFile)
	add("repeated_edits", c.RepeatedEdits)
	add("failed_edits", c.FailedEdits)
	add("no_op_edits", c.NoOpEdits)
	add("verify_attempts", c.VerifyAttempts)
	add("tool_errors", c.ToolErrors)
	add("off_scope_edits", c.OffScopeEdits)
	add("read_only_edits", c.ReadOnlyEdits)
	return strings.Join(parts, " ")
}

func firstNonEmptyLine(values ...string) string {
	for _, v := range values {
		for _, line := range strings.Split(v, "\n") {
			if s := strings.TrimSpace(line); s != "" {
				return s
			}
		}
	}
	return ""
}

func boundedMessage(err error) string {
	if err == nil {
		return ""
	}
	return boundedString(err.Error(), 400)
}

func boundedString(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// printHeadlessJSON emits a compact, machine-readable result line (prefixed
// KLOO_RESULT_JSON) for benchmarking harnesses: model/endpoint/ctx, the terminal
// reason + success, steps/tokens/tokens-per-sec/elapsed, the final verify, any error,
// and a short transcript tail. A harness greps the prefix and parses the rest.
func printHeadlessJSON(out io.Writer, cfg config.Config, verifyCmd string, rep *agent.Report, elapsed time.Duration, runErr error) {
	printRunSummaryJSON(out, buildRunSummary(cfg, verifyCmd, rep, elapsed, runErr))
}

func printRunSummaryJSON(out io.Writer, summary runSummary) {
	b, err := json.Marshal(summary)
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
