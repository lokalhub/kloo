package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// pollOutput reads a background id until its combined seen output contains `want`
// (or the deadline passes), returning everything seen. Incremental reads are
// accumulated so a line emitted between polls is not missed.
func pollOutput(m *BackgroundManager, id, want string, d time.Duration) string {
	var seen strings.Builder
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		out, _, found := m.Output(id)
		if !found {
			return seen.String()
		}
		seen.WriteString(out)
		if strings.Contains(seen.String(), want) {
			return seen.String()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return seen.String()
}

func statusOf(m *BackgroundManager, id string) string {
	_, status, _ := m.Output(id)
	return status
}

// TestBackgroundStartReadStop: a long-running command keeps running, its output is
// read incrementally while it runs, and Stop kills it.
func TestBackgroundStartReadStop(t *testing.T) {
	m := NewBackgroundManager()
	id, err := m.Start(t.TempDir(), controlledEnv(nil), "echo ready; for i in 1 2 3; do echo line$i; sleep 0.05; done; sleep 30")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id == "" {
		t.Fatal("expected a non-empty id")
	}

	seen := pollOutput(m, id, "ready", 3*time.Second)
	if !strings.Contains(seen, "ready") {
		t.Fatalf("expected to read 'ready' from the running command, got %q", seen)
	}
	// Still running (it has a 30s tail sleep).
	if s := statusOf(m, id); !strings.HasPrefix(s, "running") {
		t.Errorf("status = %q, want running", s)
	}

	if !m.Stop(id) {
		t.Fatal("Stop returned false for a known id")
	}
	// After the kill, it should report exited within a moment.
	exited := false
	for i := 0; i < 100; i++ {
		if strings.Contains(statusOf(m, id), "exited") {
			exited = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !exited {
		t.Error("background command should report exited after Stop")
	}
}

// TestBackgroundIncrementalReads: each Output returns only NEW bytes, not the whole
// log again.
func TestBackgroundIncrementalReads(t *testing.T) {
	m := NewBackgroundManager()
	id, err := m.Start(t.TempDir(), controlledEnv(nil), "echo first; sleep 0.3; echo second; sleep 30")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(id)

	if got := pollOutput(m, id, "first", 2*time.Second); !strings.Contains(got, "first") {
		t.Fatalf("want 'first', got %q", got)
	}
	// A read right after consuming 'first' must not repeat it.
	second := pollOutput(m, id, "second", 2*time.Second)
	if strings.Contains(second, "first") {
		t.Errorf("incremental read repeated already-seen output: %q", second)
	}
	if !strings.Contains(second, "second") {
		t.Errorf("want 'second' in the new output, got %q", second)
	}
}

// TestBackgroundStopAll: StopAll kills every tracked process (the loop's run-end
// cleanup so a server never leaks).
func TestBackgroundStopAll(t *testing.T) {
	m := NewBackgroundManager()
	id1, _ := m.Start(t.TempDir(), controlledEnv(nil), "sleep 60")
	id2, _ := m.Start(t.TempDir(), controlledEnv(nil), "sleep 60")
	m.StopAll()
	for _, id := range []string{id1, id2} {
		exited := false
		for i := 0; i < 100; i++ {
			if strings.Contains(statusOf(m, id), "exited") {
				exited = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !exited {
			t.Errorf("%s should be exited after StopAll", id)
		}
	}
}

// TestRunCommandBackgroundFlag: the full path through DefaultRegistry —
// run_command background=true returns an id; command_output reads then stops it;
// Registry.StopBackground is the run-end safety net.
func TestRunCommandBackgroundFlag(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := DefaultRegistry(ws)
	ctx := context.Background()

	res, err := reg.Dispatch(ctx, Call{Name: NameRunCommand, Args: map[string]any{
		"command":    "echo server-up; sleep 30",
		"background": true,
	}})
	if err != nil {
		t.Fatalf("background run_command: %v", err)
	}
	if !strings.Contains(res.Output, "started background command bg1") {
		t.Fatalf("expected a started-background message with an id, got %q", res.Output)
	}

	// command_output should eventually surface the server's line.
	var saw string
	for i := 0; i < 100; i++ {
		out, derr := reg.Dispatch(ctx, Call{Name: NameCommandOutput, Args: map[string]any{"id": "bg1"}})
		if derr != nil {
			t.Fatalf("command_output: %v", derr)
		}
		saw += out.Output
		if strings.Contains(saw, "server-up") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(saw, "server-up") {
		t.Fatalf("command_output never returned the server output; saw %q", saw)
	}

	stopRes, _ := reg.Dispatch(ctx, Call{Name: NameCommandOutput, Args: map[string]any{"id": "bg1", "stop": true}})
	if !strings.Contains(stopRes.Output, "stopped background command bg1") {
		t.Errorf("expected stop confirmation, got %q", stopRes.Output)
	}
	reg.StopBackground() // idempotent run-end cleanup
}

// TestRunCommandBackgroundDisabled: without a BackgroundManager, background=true is
// a clear error (not a silent foreground run).
func TestRunCommandBackgroundDisabled(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir())
	tool := NewRunCommandTool(ws) // no WithBackground
	_, err := tool.Invoke(context.Background(), Call{Name: NameRunCommand, Args: map[string]any{
		"command":    "sleep 30",
		"background": true,
	}})
	if err == nil {
		t.Fatal("expected an error when background mode is not enabled")
	}
}
