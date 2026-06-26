package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// NameRunCommand is the run_command tool name (naming.md).
const NameRunCommand = "run_command"

// run_command defaults.
const (
	// defaultCommandTimeout is generous because the agent runs real build/test/
	// install steps (e.g. `npm install`, `ionic start`, `cargo build`) that take
	// minutes — a tight default silently killed them mid-run, and a small model
	// rarely thinks to raise timeout_seconds. The per-call timeout_seconds arg
	// still overrides this in either direction.
	defaultCommandTimeout = 5 * time.Minute
	defaultMaxOutput      = 64 * 1024 // 64 KiB per stream
)

// ErrCommandTimeout is returned when a command is killed for exceeding its
// timeout. The partial output captured before the kill is still returned.
var ErrCommandTimeout = errors.New("tools: command timed out")

// ErrEmptyCommand is returned when run_command is called with no command.
var ErrEmptyCommand = errors.New("tools: empty command")

// envAllowlist is the controlled environment passed to executed commands. We do
// NOT inherit the full os.Environ() — that would leak unrelated secrets (API
// tokens etc.) into model-proposed commands (Lead-lens least-privilege). The
// loop (Phase 04) may widen this for build/test toolchains as needed.
var envAllowlist = []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "TERM"}

// RunCommandTool is the run_command tool: it executes a command under a timeout
// and a workspace cwd-jail, capturing bounded stdout/stderr and the exit code.
// The exit code is the verify signal Phase 04 trusts (never the model's
// self-report).
type RunCommandTool struct {
	ws        Workspace
	timeout   time.Duration
	maxOutput int
	bg        *BackgroundManager // nil ⇒ background mode disabled
}

// RunCommandOption configures a RunCommandTool.
type RunCommandOption func(*RunCommandTool)

// WithCommandTimeout sets the default per-command timeout.
func WithCommandTimeout(d time.Duration) RunCommandOption {
	return func(t *RunCommandTool) { t.timeout = d }
}

// WithMaxOutput sets the per-stream captured-output byte cap.
func WithMaxOutput(n int) RunCommandOption {
	return func(t *RunCommandTool) { t.maxOutput = n }
}

// WithBackground enables run_command background=true, routing detached commands to
// the shared BackgroundManager (a server the agent keeps running). Without it,
// background=true returns an error.
func WithBackground(bg *BackgroundManager) RunCommandOption {
	return func(t *RunCommandTool) { t.bg = bg }
}

// NewRunCommandTool builds the run_command tool jailed to ws.
func NewRunCommandTool(ws Workspace, opts ...RunCommandOption) RunCommandTool {
	t := RunCommandTool{ws: ws, timeout: defaultCommandTimeout, maxOutput: defaultMaxOutput}
	for _, opt := range opts {
		opt(&t)
	}
	return t
}

func (t RunCommandTool) Name() string { return NameRunCommand }
func (t RunCommandTool) Description() string {
	return "Run a shell command in the workspace and capture stdout, stderr, and the exit code. Use this to build, test, or inspect the project. The exit code is the source of truth. For a LONG-RUNNING process you must keep running (a dev server, worker sim, watcher), set background=true: it returns immediately with an id; then use command_output to check it is ready and to stop it."
}
func (t RunCommandTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"command":         {Type: "string", Description: "The shell command to run."},
			"timeout_seconds": {Type: "number", Description: "Optional timeout in seconds (default applies otherwise). Ignored when background=true."},
			"cwd":             {Type: "string", Description: "Optional workspace-relative working directory (defaults to the workspace root)."},
			"background":      {Type: "boolean", Description: "Run the command DETACHED and return immediately — for a long-running server/watcher. Read its output and stop it with command_output. Auto-stopped when the run ends."},
		},
		Required: []string{"command"},
	}
}

// Invoke runs the command. A non-zero exit is NOT a Go error — it is captured in
// Result.ExitCode (the verify signal). Errors are reserved for a cwd-jail escape,
// a timeout (ErrCommandTimeout), an empty command, or a failure to start.
func (t RunCommandTool) Invoke(ctx context.Context, c Call) (Result, error) {
	command, _ := argString(c.Args, "command")
	if command == "" {
		return Result{}, ErrEmptyCommand
	}

	// cwd is jailed to the workspace via the Phase-01 Resolve chokepoint.
	cwd := t.ws.Root()
	if rel, ok := argString(c.Args, "cwd"); ok && rel != "" {
		resolved, err := t.ws.Resolve(rel)
		if err != nil {
			return Result{}, err // ErrPathEscape surfaces unchanged
		}
		cwd = resolved
	}

	// Background mode: launch detached and return immediately, leaving the process
	// running so the agent can build/test against it (a server it must keep up).
	if argBool(c.Args, "background") {
		if t.bg == nil {
			return Result{}, errors.New("tools: background mode is not enabled for run_command")
		}
		id, err := t.bg.Start(cwd, controlledEnv(), command)
		if err != nil {
			return Result{}, fmt.Errorf("tools: failed to start background command: %w", err)
		}
		return Result{Output: fmt.Sprintf(
			"started background command %s: %s\nIt is running detached. Use command_output with id=%q to read its output (e.g. to check a server is ready), and command_output id=%q stop=true to stop it. It is auto-stopped when this run ends.",
			id, command, id, id)}, nil
	}

	timeout := t.timeout
	if secs, ok := argSeconds(c.Args, "timeout_seconds"); ok && secs > 0 {
		timeout = time.Duration(secs * float64(time.Second))
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
	cmd.Dir = cwd
	cmd.Env = controlledEnv()
	setProcGroup(cmd) // unix: own process group so we can kill the child tree

	// Override the default context-kill to kill the whole process GROUP, not just
	// the parent shell. Otherwise a surviving child (e.g. `sleep`) keeps the
	// stdout pipe open and Run() blocks until it exits, defeating the timeout.
	// WaitDelay forces Run() to return even if a child lingers on the pipe.
	cmd.Cancel = func() error { killProcGroup(cmd); return nil }
	cmd.WaitDelay = 2 * time.Second

	stdout := &boundedBuffer{cap: t.maxOutput}
	stderr := &boundedBuffer{cap: t.maxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	runErr := cmd.Run()

	res := Result{
		Output:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.truncated || stderr.truncated,
	}

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		killProcGroup(cmd) // ensure any surviving children are reaped
		res.TimedOut = true
		// The message is surfaced verbatim to the model (observation()), so it carries
		// the fix: re-run with a larger timeout_seconds. A small model otherwise just
		// retries the same command and times out identically.
		return res, fmt.Errorf("tools: command killed after %s — if it just needs longer (e.g. a slow install/build), re-run it with a larger timeout_seconds: %w", timeout, ErrCommandTimeout)
	}

	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode() // non-zero exit: captured, not an error
			return res, nil
		}
		return res, fmt.Errorf("tools: run_command failed to execute: %w", runErr)
	}

	return res, nil
}

// nonInteractiveEnv signals automated/non-interactive mode to common toolchains so
// a build/test command never blocks on a consent or credential PROMPT (kloo has no
// TTY, so a prompt would hang until the timeout — e.g. Angular CLI's analytics
// "(y/N)", or git asking for credentials). CI=true is the near-universal switch
// (npm/ng/yarn/many tools disable prompts+analytics under it); the rest cover the
// usual stragglers. stdin is already /dev/null, so these are belt-and-suspenders.
var nonInteractiveEnv = []string{
	"CI=true",
	"GIT_TERMINAL_PROMPT=0",  // git never prompts for credentials
	"NG_CLI_ANALYTICS=false", // Angular CLI: no analytics prompt
	"DEBIAN_FRONTEND=noninteractive",
	"NPM_CONFIG_FUND=false",
	"NPM_CONFIG_AUDIT=false",
}

// controlledEnv builds the allowlisted environment for executed commands, plus the
// non-interactive signals so a prompting command can't hang the loop.
func controlledEnv() []string {
	env := make([]string, 0, len(envAllowlist)+len(nonInteractiveEnv))
	for _, k := range envAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, nonInteractiveEnv...)
}

// argSeconds reads a numeric "seconds" argument that may arrive as a JSON number
// (float64) from native FC or as a string from the XML adapter.
func argSeconds(args map[string]any, key string) (float64, bool) {
	switch v := args[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// boundedBuffer is an io.Writer that caps how much it retains, marking overflow
// rather than buffering unbounded output. It always reports a full write so the
// running command is never killed by a short-write error.
type boundedBuffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.cap > 0 {
		room := b.cap - b.buf.Len()
		if room <= 0 {
			b.truncated = true
			return len(p), nil
		}
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
			return len(p), nil
		}
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	s := b.buf.String()
	if b.truncated {
		s += fmt.Sprintf("\n…[truncated, output capped at %d bytes]", b.cap)
	}
	return s
}
