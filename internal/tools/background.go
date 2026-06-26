package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
)

// NameCommandOutput is the command_output tool name (naming.md).
const NameCommandOutput = "command_output"

// maxBackgroundOutput caps the per-process captured output buffer. A long-running
// server's logs are read incrementally (only new bytes each poll), so this only
// bounds how much UNREAD output is retained before the oldest is dropped.
const maxBackgroundOutput = 64 * 1024

// BackgroundManager owns the long-running ("background") commands started via
// run_command background=true: a dev server, worker sim, or watcher the agent must
// keep RUNNING while it does other work (build, e2e), then stop. The synchronous
// run_command can't do this — it blocks until the process exits — so a UI/e2e task
// could never bring up its own stack. Every started process is tracked so the loop
// can StopAll() on run termination; otherwise a server would leak past the run.
//
// It is safe for concurrent use: the process writes output from exec's I/O
// goroutine while the agent reads it from the loop goroutine.
type BackgroundManager struct {
	mu    sync.Mutex
	procs map[string]*bgProc
	seq   int
}

// bgProc is one tracked background process and its captured output.
type bgProc struct {
	id      string
	command string
	cmd     *exec.Cmd
	out     *lockedBuffer

	mu       sync.Mutex // guards readN/done/exitCode
	readN    int        // bytes of output already returned to the model
	done     bool
	exitCode int
}

// NewBackgroundManager returns an empty manager.
func NewBackgroundManager() *BackgroundManager {
	return &BackgroundManager{procs: make(map[string]*bgProc)}
}

// Start launches command detached in cwd with env, capturing combined stdout+stderr,
// and returns the assigned id (e.g. "bg1"). It does NOT wait for exit — the process
// keeps running until Stop/StopAll or it exits on its own.
func (m *BackgroundManager) Start(cwd string, env []string, command string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = nil   // os.DevNull — never block on a prompt (no TTY)
	setProcGroup(cmd) // own process group so the whole tree can be killed together

	buf := &lockedBuffer{}
	buf.b.cap = maxBackgroundOutput
	cmd.Stdout = buf
	cmd.Stderr = buf

	if err := cmd.Start(); err != nil {
		return "", err
	}

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("bg%d", m.seq)
	p := &bgProc{id: id, command: command, cmd: cmd, out: buf}
	m.procs[id] = p
	m.mu.Unlock()

	// Reap on exit so the agent sees the process finished (and any leaked goroutine
	// ends when the process is killed).
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.done = true
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			p.exitCode = ee.ExitCode()
		} else if err != nil {
			p.exitCode = -1
		}
		p.mu.Unlock()
	}()
	return id, nil
}

// Output returns the NEW output since the last read for id, plus a human status
// ("running" / "exited with code N"), and whether the id is known.
func (m *BackgroundManager) Output(id string) (newOutput, status string, found bool) {
	p := m.lookup(id)
	if p == nil {
		return "", "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out, total, truncated := p.out.read(p.readN)
	p.readN = total
	status = "running"
	if p.done {
		status = fmt.Sprintf("exited with code %d", p.exitCode)
	}
	if truncated {
		status += " · output buffer full (older output dropped)"
	}
	return out, status, true
}

// Stop kills the process group for id. Returns false if the id is unknown.
func (m *BackgroundManager) Stop(id string) bool {
	p := m.lookup(id)
	if p == nil {
		return false
	}
	killProcGroup(p.cmd)
	return true
}

// StopAll kills every tracked background process — the loop calls it on run
// termination so a started server never leaks past the run (critical when many runs
// share a machine, e.g. kloo as J1's T1).
func (m *BackgroundManager) StopAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.procs {
		killProcGroup(p.cmd)
	}
}

func (m *BackgroundManager) lookup(id string) *bgProc {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.procs[id]
}

// lockedBuffer is a concurrency-safe boundedBuffer: the process writes to it while
// the agent reads from it. read(from) returns the captured bytes after offset
// `from`, the new total length, and whether output was dropped at the cap.
type lockedBuffer struct {
	mu sync.Mutex
	b  boundedBuffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) read(from int) (string, int, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.b.buf.String() // raw captured content (no truncation marker)
	if from < 0 || from > len(s) {
		from = len(s)
	}
	return s[from:], len(s), l.b.truncated
}

// commandOutputTool reads/stops a background command started with run_command
// background=true. Keeping read+stop in one tool keeps the small-model vocabulary
// lean (one new verb, not two).
type commandOutputTool struct{ bg *BackgroundManager }

func (commandOutputTool) Name() string { return NameCommandOutput }
func (commandOutputTool) Description() string {
	return "Check on or stop a background command started with run_command background=true. Returns its new output and whether it is still running. Use it to wait until a server is ready, then to stop the server when done."
}
func (commandOutputTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"id":   {Type: "string", Description: "The background command id (e.g. bg1) returned by run_command background=true."},
			"stop": {Type: "boolean", Description: "Set true to stop (kill) the background command."},
		},
		Required: []string{"id"},
	}
}
func (t commandOutputTool) Invoke(_ context.Context, c Call) (Result, error) {
	if t.bg == nil {
		return Result{}, errors.New("tools: background commands are not enabled")
	}
	id, _ := argString(c.Args, "id")
	if id == "" {
		return Result{}, fmt.Errorf("tools: command_output requires an id: %w", ErrInvalidArgs)
	}
	if argBool(c.Args, "stop") {
		if !t.bg.Stop(id) {
			return Result{Output: "no such background command: " + id}, nil
		}
		return Result{Output: "stopped background command " + id}, nil
	}
	out, status, found := t.bg.Output(id)
	if !found {
		return Result{Output: "no such background command: " + id}, nil
	}
	body := fmt.Sprintf("background command %s [%s]", id, status)
	if out == "" {
		return Result{Output: body + " · no new output"}, nil
	}
	return Result{Output: body + ":\n" + out}, nil
}

// argBool reads a boolean argument that may arrive as a JSON bool (native FC) or a
// string ("true"/"1") from a text adapter.
func argBool(args map[string]any, key string) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	default:
		return false
	}
}
