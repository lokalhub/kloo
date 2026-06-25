package agent

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

// errLintNotRunnable marks a lint whose command could not actually execute — most
// often a missing/not-executable linter binary, which the sh -c wrapper surfaces
// as exit 127/126 rather than an Invoke error. It is mapped to LintResult.Err so
// the loop degrades it SILENTLY (constraint §3: a non-runnable linter is never an
// observation and never a stop), exactly like a timeout or jail escape.
var errLintNotRunnable = errors.New("agent: lint command not runnable")

// lintTimeout is the default per-lint timeout (seconds). It is deliberately SHORT
// — far below the 5-min run_command default and the 300s headless verify timeout —
// because lint is a FAST advisory signal on the edited file(s): a slow or hanging
// linter is killed and degrades to silence (LintResult.Err), never blocking or
// failing the run.
const lintTimeout = 20.0

// Linter runs the configured fast lint on the edited file(s) and returns the REAL,
// ADVISORY result. It NEVER gates success and is NEVER fed to the churn rail — the
// loop appends its output to the conversation as a model-visible observation only.
// (Concrete runner: CommandLinter.)
type Linter interface {
	Lint(ctx context.Context, paths []string) LintResult
}

// LintResult is advisory output only — the structural twin of VerifyResult, with
// the opposite authority. Err is set when the linter could not be run at all
// (missing binary, timeout, jail escape); the loop degrades that SILENTLY (no
// observation, no stop). A command that RAN and exited non-zero is normal advisory
// output, NOT an Err.
type LintResult struct {
	Command  string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	Err      error
}

// CommandLinter runs the resolved lint command jailed to ws via the same
// run_command tool CommandVerifier uses, with a short timeout. When perFile, the
// edited paths are appended to the command (gofmt/eslint/ruff/flake8); otherwise
// the command runs verbatim (tsc --noEmit is whole-project). It mirrors
// CommandVerifier's structure, but is advisory: it never returns an error to the
// loop and never decides success.
type CommandLinter struct {
	tool       tools.RunCommandTool
	command    string
	perFile    bool
	timeoutSec float64
	now        func() time.Time
}

// LintOption configures a CommandLinter.
type LintOption func(*CommandLinter)

// WithLintTimeout sets a per-lint timeout (seconds) for the command.
func WithLintTimeout(seconds float64) LintOption {
	return func(l *CommandLinter) { l.timeoutSec = seconds }
}

// NewCommandLinter builds a linter that runs command jailed to ws. command +
// perFile are resolved from config by the caller (Phase 02). An empty command
// yields a no-op Linter (Lint returns the zero LintResult).
func NewCommandLinter(ws tools.Workspace, command string, perFile bool, opts ...LintOption) *CommandLinter {
	l := &CommandLinter{
		tool:       tools.NewRunCommandTool(ws),
		command:    command,
		perFile:    perFile,
		timeoutSec: lintTimeout,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Lint runs the configured command on the edited paths and returns the real,
// advisory result. A non-runnable command / timeout / jail escape is captured in
// LintResult.Err (degraded silently by the loop), never returned as a loop error;
// a non-zero exit is captured in ExitCode as normal advisory output.
func (l *CommandLinter) Lint(ctx context.Context, paths []string) LintResult {
	command := l.command
	if strings.TrimSpace(command) == "" {
		return LintResult{} // no-op: nothing configured
	}
	if l.perFile {
		if q := quotePaths(paths); len(q) > 0 {
			command = command + " " + strings.Join(q, " ")
		}
	}

	start := l.now()
	args := map[string]any{"command": command}
	if l.timeoutSec > 0 {
		args["timeout_seconds"] = l.timeoutSec
	}
	res, err := l.tool.Invoke(ctx, tools.Call{Name: tools.NameRunCommand, Args: args})

	lr := LintResult{
		Command:  command,
		ExitCode: res.ExitCode,
		Stdout:   res.Output,
		Stderr:   res.Stderr,
		Duration: l.now().Sub(start),
	}
	switch {
	case err != nil:
		// Non-runnable / timeout / jail escape — advisory degrade, NOT a loop error.
		lr.Err = err
	case res.ExitCode == 127 || res.ExitCode == 126:
		// sh ran but the LINTER itself never executed (127 = command not found,
		// 126 = found but not executable). No real linter exits this way, so treat
		// it as non-runnable and degrade silently rather than feeding the shell's
		// "not found" noise back as advisory output.
		lr.Err = fmt.Errorf("%w (exit %d)", errLintNotRunnable, res.ExitCode)
	}
	return lr
}

// quotePaths shell-quotes each non-empty path so a path with spaces is passed as a
// single argument when appended to a per-file lint command.
func quotePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, strconv.Quote(p))
	}
	return out
}

// lintObservation turns a LintResult into a labeled, model-visible advisory
// message. It returns ok=false (append NOTHING) when the lint is non-runnable
// (Err set — silent degrade) OR clean (no combined output) — both byte-identical
// to a run with no linter. When there is real content it returns a RoleUser
// message explicitly labeled advisory, so a small model treats it as a hint, not a
// gate. The body is already bounded by run_command's output cap.
func lintObservation(lr LintResult) (llm.Message, bool) {
	if lr.Err != nil {
		return llm.Message{}, false // non-runnable: silent
	}
	body := strings.TrimRight(lr.Stdout, "\n")
	if s := strings.TrimRight(lr.Stderr, "\n"); s != "" {
		if body != "" {
			body += "\n"
		}
		body += s
	}
	if strings.TrimSpace(body) == "" {
		return llm.Message{}, false // clean: silent
	}

	var b strings.Builder
	b.WriteString("lint (advisory — does NOT decide success; only the verify command does):\n")
	b.WriteString("$ ")
	b.WriteString(lr.Command)
	b.WriteString("\n")
	b.WriteString(body)
	return llm.Message{Role: llm.RoleUser, Content: b.String()}, true
}
