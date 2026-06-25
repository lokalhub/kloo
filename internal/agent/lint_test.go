package agent

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

func lintWS(t *testing.T) tools.Workspace {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("lint tests use /bin/sh")
	}
	ws, err := tools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// TestCommandLinter_PerFileAppendsPath: with perFile, the edited path is appended
// to the command (and shell-quoted), so the linter sees it; non-perFile runs the
// command verbatim and ignores paths.
func TestCommandLinter_PerFileAppendsPath(t *testing.T) {
	// perFile=true: `echo` echoes the appended path back, proving it was passed.
	l := NewCommandLinter(lintWS(t), "echo", true)
	r := l.Lint(context.Background(), []string{"sub dir/foo.go"})
	if r.Err != nil {
		t.Fatalf("unexpected Err: %v", r.Err)
	}
	if !strings.Contains(r.Command, `"sub dir/foo.go"`) {
		t.Errorf("command should carry the quoted path, got %q", r.Command)
	}
	if !strings.Contains(r.Stdout, "sub dir/foo.go") {
		t.Errorf("linter should have seen the path, stdout=%q", r.Stdout)
	}

	// perFile=false: command runs verbatim, paths ignored.
	lv := NewCommandLinter(lintWS(t), "echo verbatim", false)
	rv := lv.Lint(context.Background(), []string{"foo.go"})
	if rv.Command != "echo verbatim" {
		t.Errorf("non-perFile command must run verbatim, got %q", rv.Command)
	}
	if strings.Contains(rv.Stdout, "foo.go") {
		t.Errorf("non-perFile must ignore paths, stdout=%q", rv.Stdout)
	}
}

// TestCommandLinter_NonZeroExitIsAdvisoryNotErr: a linter that RAN and exited
// non-zero (issues found) is normal advisory output — ExitCode captured, Err nil —
// and lintObservation surfaces it.
func TestCommandLinter_NonZeroExitIsAdvisoryNotErr(t *testing.T) {
	l := NewCommandLinter(lintWS(t), "echo style problem; exit 1", false)
	r := l.Lint(context.Background(), nil)
	if r.Err != nil {
		t.Errorf("a non-zero EXIT is advisory output, not an Err: %v", r.Err)
	}
	if r.ExitCode != 1 {
		t.Errorf("exit code should be captured, got %d", r.ExitCode)
	}
	if _, ok := lintObservation(r); !ok {
		t.Errorf("non-empty advisory output should yield an observation")
	}
}

// TestCommandLinter_NonRunnable: a missing binary (sh exit 127) is non-runnable —
// Err is set and lintObservation is silent (constraint §3: missing binary ⇒ silent,
// never the shell's "not found" noise).
func TestCommandLinter_NonRunnable(t *testing.T) {
	l := NewCommandLinter(lintWS(t), "definitely-not-a-real-binary-xyzzy", true)
	r := l.Lint(context.Background(), []string{"foo.go"})
	if r.Err == nil {
		t.Errorf("a non-runnable linter should set Err, got %+v", r)
	}
	if _, ok := lintObservation(r); ok {
		t.Errorf("a non-runnable linter must be silent (no observation)")
	}
}

// TestCommandLinter_Timeout: a slow linter is killed at the short timeout, sets
// Err, and stays advisory (no panic, no loop error).
func TestCommandLinter_Timeout(t *testing.T) {
	l := NewCommandLinter(lintWS(t), "sleep 10", false, WithLintTimeout(0.3))
	start := time.Now()
	r := l.Lint(context.Background(), nil)
	if time.Since(start) > 3*time.Second {
		t.Errorf("lint timeout did not stop promptly")
	}
	if r.Err == nil {
		t.Errorf("a timed-out lint should set Err: %+v", r)
	}
	if _, ok := lintObservation(r); ok {
		t.Errorf("a timed-out (non-runnable) lint must be silent")
	}
}

// TestCommandLinter_EmptyCommand: an empty command is a no-op — zero result, no
// Invoke, and lintObservation is silent.
func TestCommandLinter_EmptyCommand(t *testing.T) {
	l := NewCommandLinter(lintWS(t), "   ", true)
	r := l.Lint(context.Background(), []string{"foo.go"})
	if r.Command != "" || r.Err != nil || r.Stdout != "" {
		t.Errorf("empty command should be a zero no-op result, got %+v", r)
	}
	if _, ok := lintObservation(r); ok {
		t.Errorf("an empty-command result must be silent")
	}
}

// TestLintObservation: clean output ⇒ silent; content ⇒ a RoleUser message labeled
// advisory and carrying the command + output.
func TestLintObservation(t *testing.T) {
	// Clean: exit 0, no output ⇒ silent.
	if _, ok := lintObservation(LintResult{Command: "gofmt -l x.go"}); ok {
		t.Errorf("clean lint (no output) must be silent")
	}
	// Non-runnable ⇒ silent.
	if _, ok := lintObservation(LintResult{Command: "x", Err: errLintNotRunnable}); ok {
		t.Errorf("non-runnable lint must be silent")
	}
	// Content ⇒ labeled advisory message.
	msg, ok := lintObservation(LintResult{Command: "gofmt -l loop.go", Stdout: "loop.go\n"})
	if !ok {
		t.Fatalf("non-empty output should yield an observation")
	}
	if msg.Role != llm.RoleUser {
		t.Errorf("lint observation should be a RoleUser message, got %q", msg.Role)
	}
	if !strings.Contains(msg.Content, "advisory — does NOT decide success") {
		t.Errorf("observation must carry the advisory label, got %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "$ gofmt -l loop.go") || !strings.Contains(msg.Content, "loop.go") {
		t.Errorf("observation must show the command + output, got %q", msg.Content)
	}
}
