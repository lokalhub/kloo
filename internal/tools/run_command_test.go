package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func skipIfNoSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("run_command tests use /bin/sh; not on windows")
	}
}

func runCmd(t *testing.T, tool RunCommandTool, args map[string]any) (Result, error) {
	t.Helper()
	return tool.Invoke(context.Background(), Call{Name: NameRunCommand, Args: args})
}

func TestRunCommandCapturesStdoutAndExit(t *testing.T) {
	skipIfNoSh(t)
	ws, _ := wsAt(t)
	tool := NewRunCommandTool(ws)

	res, err := runCmd(t, tool, map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("stdout = %q", res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
}

func TestRunCommandStderrAndNonZeroExit(t *testing.T) {
	skipIfNoSh(t)
	ws, _ := wsAt(t)
	tool := NewRunCommandTool(ws)

	// Non-zero exit is captured, NOT a Go error (it is the verify signal).
	res, err := runCmd(t, tool, map[string]any{"command": "echo oops 1>&2; exit 3"})
	if err != nil {
		t.Fatalf("non-zero exit should not be a Go error, got %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "oops") {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestRunCommandTimeoutKills(t *testing.T) {
	skipIfNoSh(t)
	ws, _ := wsAt(t)
	tool := NewRunCommandTool(ws)

	start := time.Now()
	res, err := runCmd(t, tool, map[string]any{"command": "sleep 10", "timeout_seconds": 0.3})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrCommandTimeout) {
		t.Fatalf("want ErrCommandTimeout, got %v", err)
	}
	if !res.TimedOut {
		t.Errorf("result should be marked TimedOut")
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout did not kill promptly: %s", elapsed)
	}
}

func TestRunCommandCwdJail(t *testing.T) {
	skipIfNoSh(t)
	ws, root := wsAt(t)
	tool := NewRunCommandTool(ws)

	t.Run("traversal rejected", func(t *testing.T) {
		if _, err := runCmd(t, tool, map[string]any{"command": "pwd", "cwd": "../outside"}); !errors.Is(err, ErrPathEscape) {
			t.Errorf("want ErrPathEscape, got %v", err)
		}
	})
	t.Run("absolute escape rejected", func(t *testing.T) {
		if _, err := runCmd(t, tool, map[string]any{"command": "pwd", "cwd": "/etc"}); !errors.Is(err, ErrPathEscape) {
			t.Errorf("want ErrPathEscape, got %v", err)
		}
	})
	t.Run("symlink escape rejected", func(t *testing.T) {
		outside := t.TempDir()
		link := filepath.Join(root, "escape")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, tool, map[string]any{"command": "pwd", "cwd": "escape"}); !errors.Is(err, ErrPathEscape) {
			t.Errorf("want ErrPathEscape via symlink, got %v", err)
		}
	})
	t.Run("default cwd is the workspace root", func(t *testing.T) {
		res, err := runCmd(t, tool, map[string]any{"command": "pwd"})
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(res.Output) != root {
			t.Errorf("pwd = %q, want workspace root %q", strings.TrimSpace(res.Output), root)
		}
	})
}

func TestRunCommandOutputBounded(t *testing.T) {
	skipIfNoSh(t)
	ws, _ := wsAt(t)
	tool := NewRunCommandTool(ws, WithMaxOutput(64))

	// Emit far more than the 64-byte cap.
	res, err := runCmd(t, tool, map[string]any{"command": "for i in $(seq 1 500); do echo XXXXXXXX; done"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Truncated {
		t.Errorf("expected Truncated=true for over-cap output")
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Errorf("expected a truncation marker, got %q", res.Output)
	}
	// Captured payload (excluding the marker) must respect the cap.
	if len(res.Output) > 64+128 {
		t.Errorf("output not bounded: %d bytes", len(res.Output))
	}
}

func TestRunCommandEmptyCommand(t *testing.T) {
	ws, _ := wsAt(t)
	tool := NewRunCommandTool(ws)
	if _, err := runCmd(t, tool, map[string]any{"command": ""}); !errors.Is(err, ErrEmptyCommand) {
		t.Errorf("want ErrEmptyCommand, got %v", err)
	}
}
