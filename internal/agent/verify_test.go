package agent

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/tools"
)

func verifyWS(t *testing.T) tools.Workspace {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("verify tests use /bin/sh")
	}
	root := t.TempDir()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestVerifyExitZeroPasses(t *testing.T) {
	v := NewCommandVerifier(verifyWS(t), "echo all good; exit 0")
	r := v.Verify(context.Background())
	if !r.Passed || r.ExitCode != 0 {
		t.Errorf("exit 0 should pass: %+v", r)
	}
	if !strings.Contains(r.Stdout, "all good") {
		t.Errorf("stdout not captured: %q", r.Stdout)
	}
}

func TestVerifyNonZeroFails(t *testing.T) {
	v := NewCommandVerifier(verifyWS(t), "echo boom 1>&2; exit 1")
	r := v.Verify(context.Background())
	if r.Passed {
		t.Errorf("non-zero exit must not pass: %+v", r)
	}
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "boom") {
		t.Errorf("output/exit not captured: %+v", r)
	}
	if r.Err != nil {
		t.Errorf("a clean red is not an Err outcome: %v", r.Err)
	}
}

// TestVerifyIgnoresModelClaim is the decisive anti-spiral case: the verifier has
// no input for the model's claim, so a red command is Passed=false regardless of
// what the model said. (We assert the structural impossibility: only the real
// exit code drives Passed.)
func TestVerifyIgnoresModelClaim(t *testing.T) {
	// The "model" would claim success here, but the command exits non-zero.
	v := NewCommandVerifier(verifyWS(t), "echo 'I am done, tests pass!'; exit 7")
	r := v.Verify(context.Background())
	if r.Passed {
		t.Errorf("model claim must be ignored when the command is red: %+v", r)
	}
	if r.ExitCode != 7 {
		t.Errorf("real exit code should drive the decision, got %d", r.ExitCode)
	}
}

func TestVerifyStructuralCheckCanFailDespiteExitZero(t *testing.T) {
	v := NewCommandVerifier(verifyWS(t), "echo missing-marker; exit 0",
		WithStructuralCheck(func(r VerifyResult) bool {
			return strings.Contains(r.Stdout, "REQUIRED-MARKER")
		}))
	r := v.Verify(context.Background())
	if r.Passed {
		t.Errorf("structural gate should fail the verify despite exit 0: %+v", r)
	}
}

func TestVerifyNonRunnableIsErrorOutcome(t *testing.T) {
	// An empty command can't be run → typed error, NOT a false pass.
	v := NewCommandVerifier(verifyWS(t), "")
	r := v.Verify(context.Background())
	if r.Passed {
		t.Errorf("non-runnable command must not pass: %+v", r)
	}
	if r.Err == nil {
		t.Errorf("expected an Err outcome for a non-runnable command")
	}
}

func TestVerifyTimeoutIsErrorOutcome(t *testing.T) {
	v := NewCommandVerifier(verifyWS(t), "sleep 10", WithVerifyTimeout(0.3))
	start := time.Now()
	r := v.Verify(context.Background())
	if time.Since(start) > 3*time.Second {
		t.Errorf("verify timeout did not stop promptly")
	}
	if r.Passed || r.Err == nil {
		t.Errorf("timed-out verify should be an Err outcome, not a pass: %+v", r)
	}
}

// TestVerifyCtxCancelStopsPromptly: a cancelled context kills the verify command
// promptly rather than blocking for its full duration (the interrupt foundation
// the loop relies on at its top-of-turn ctx check).
func TestVerifyCtxCancelStopsPromptly(t *testing.T) {
	v := NewCommandVerifier(verifyWS(t), "sleep 10")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	r := v.Verify(ctx)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancelled verify did not stop promptly: %s", elapsed)
	}
	// A cancelled command was killed → it did not pass.
	if r.Passed {
		t.Errorf("a cancelled verify must not report Passed: %+v", r)
	}
}

func TestVerifyJailEscapeViaWorkspace(t *testing.T) {
	// The verify command runs jailed to the workspace; this just confirms the
	// command executes within it (a positive control for the run_command jail).
	ws := verifyWS(t)
	v := NewCommandVerifier(ws, "pwd")
	r := v.Verify(context.Background())
	if !r.Passed {
		t.Fatalf("pwd should pass: %+v", r)
	}
	canon, _ := filepath.EvalSymlinks(ws.Root())
	if strings.TrimSpace(r.Stdout) != canon && strings.TrimSpace(r.Stdout) != ws.Root() {
		t.Errorf("verify ran outside the workspace: pwd=%q root=%q", strings.TrimSpace(r.Stdout), ws.Root())
	}
}
