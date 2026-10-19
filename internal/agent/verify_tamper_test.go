package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/tools"
)

// tamperWS builds a workspace holding an executable gate script that exits with
// body, and returns the workspace plus the script's absolute path.
func tamperWS(t *testing.T, body string) (tools.Workspace, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tamper guard tests use /bin/sh")
	}
	root := t.TempDir()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "gate.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws, script
}

// The guard must be invisible to a verify whose named file is untouched.
func TestVerifyTamperUnchangedScriptStillRuns(t *testing.T) {
	ws, _ := tamperWS(t, "echo gate ran; exit 0")
	v := NewCommandVerifier(ws, "./gate.sh")
	r := v.Verify(context.Background())

	if r.Err != nil {
		t.Fatalf("untouched gate must not be an error outcome: %v", r.Err)
	}
	if !r.Passed || r.ExitCode != 0 {
		t.Errorf("untouched gate should pass: %+v", r)
	}
	if !strings.Contains(r.Stdout, "gate ran") {
		t.Errorf("the command did not run: stdout=%q", r.Stdout)
	}
}

// Rewriting the gate between construction and Verify is an error outcome naming
// the file — never a pass, and never a silent re-hash.
func TestVerifyTamperChangedScriptIsAnError(t *testing.T) {
	ws, script := tamperWS(t, "exit 0")
	v := NewCommandVerifier(ws, "./gate.sh")

	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0 # rewritten\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := v.Verify(context.Background())
	if !errors.Is(r.Err, ErrVerifyTampered) {
		t.Fatalf("rewritten gate must wrap ErrVerifyTampered, got %v", r.Err)
	}
	if r.Passed {
		t.Errorf("a tampered gate can never pass: %+v", r)
	}
	if !strings.Contains(r.Err.Error(), "gate.sh") {
		t.Errorf("the error must name the file: %v", r.Err)
	}
	if r.Command != "./gate.sh" {
		t.Errorf("the report must still say which command was meant: %q", r.Command)
	}
}

// The report saying "tampered" is not enough: the rewritten script must never
// have been executed, because running it is the whole point of rewriting it.
func TestVerifyTamperChangedScriptDoesNotRunTheCommand(t *testing.T) {
	ws, script := tamperWS(t, "exit 0")
	v := NewCommandVerifier(ws, "./gate.sh")

	marker := filepath.Join(ws.Root(), "RAN")
	// The rewritten gate passes AND leaves a marker, so its absence proves the
	// command was never invoked (rather than merely that the result says so).
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	r := v.Verify(context.Background())
	if !errors.Is(r.Err, ErrVerifyTampered) {
		t.Fatalf("expected ErrVerifyTampered, got %v", r.Err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("the rewritten gate was executed (marker %s exists, stat err=%v)", marker, err)
	}
}

// A command naming no existing file must behave exactly as before — `go`,
// `test`, `./...` and `-run` are not files and must not become errors.
func TestVerifyTamperCommandNamingNoFileStillRuns(t *testing.T) {
	ws, _ := tamperWS(t, "exit 0")
	// gate.sh exists in this workspace but its name is not gate-shaped, and no
	// other token is a file at all: `go`, `test`, `./...`, `-run` must never
	// become errors.
	v := NewCommandVerifier(ws, "go test ./... -run TestNothing")
	if len(v.guarded) != 0 {
		t.Fatalf("no token of that command is a gate file, got %v", v.guarded)
	}

	r := v.Verify(context.Background())
	if errors.Is(r.Err, ErrVerifyTampered) {
		t.Fatalf("a command naming no gate file must never report tampering: %v", r.Err)
	}
	if r.Err != nil {
		t.Fatalf("a non-file token must never turn the verify into an error outcome: %v", r.Err)
	}
	// The command really ran: it reported its own exit code (red here, because a
	// temp dir has no go module — which is the point: kloo passed it through).
	if !r.VerifyRan {
		t.Errorf("the command did not run: %+v", r)
	}
	if r.ExitCode == 0 && r.Passed != true {
		t.Errorf("exit code and verdict disagree: %+v", r)
	}
}

// A guarded file that disappears is tampering too, and is reported as such.
func TestVerifyTamperDeletedScriptIsAnError(t *testing.T) {
	ws, script := tamperWS(t, "exit 0")
	v := NewCommandVerifier(ws, "./gate.sh")

	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}

	r := v.Verify(context.Background())
	if !errors.Is(r.Err, ErrVerifyTampered) {
		t.Fatalf("deleted gate must wrap ErrVerifyTampered, got %v", r.Err)
	}
	if !strings.Contains(r.Err.Error(), "deleted") {
		t.Errorf("the error must say what happened: %v", r.Err)
	}
	if r.Passed {
		t.Errorf("a deleted gate can never pass: %+v", r)
	}
}
