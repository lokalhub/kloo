package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/tools"
)

// ErrVerifyTampered reports that a file named by the verify command changed
// during the run. The gate itself is then not evidence about the code, so the
// verify is an ERROR outcome, never a pass - the same rule the exit-code path
// follows for a command that could not be run at all.
var ErrVerifyTampered = errors.New("agent: a file named by the verify command changed during the run")

// guardMaxFileBytes is the size ceiling for the tamper guard's snapshot. A
// verify command naming a huge file is not a gate script, and hashing megabytes
// on every verify would tax a path that runs on every mutation.
const guardMaxFileBytes = 1 << 20

// gateNameMarkers are the name fragments that mark a file as the verify
// command's GATE — the thing that decides pass/fail, and which the model can
// therefore rewrite to make a red run look green (measured: `edit_file:
// .cap2gate.sh`, the very script the run was judged by). A file whose name
// carries one of these, or a script extension, is a gate; anything else the
// command names is an ARGUMENT.
//
// That distinction is not cosmetic: `grep -qx right answer.txt` names the
// DELIVERABLE the run exists to write, and guarding it would turn every
// successful run into "verify DID NOT RUN". Only the gate is guarded.
var gateNameMarkers = []string{"test", "spec", "gate", "check", "lint", "verify", "makefile"}

// guardFileExts are the executable/script shapes a gate is written in.
var guardFileExts = map[string]bool{
	".sh": true, ".bash": true, ".zsh": true, ".ksh": true,
	".py": true, ".rb": true, ".pl": true, ".php": true,
	".js": true, ".mjs": true, ".cjs": true, ".ts": true,
	".yaml": true, ".yml": true, ".toml": true,
}

// isGateFile reports whether a file named by the verify command is a gate the
// guard must protect: a script/test-shaped name, or a dot-prefixed extensionless
// script like .cap2gate.sh — the shape that was actually attempted.
func isGateFile(name string) bool {
	lower := strings.ToLower(name)
	base := filepath.Base(lower)
	if guardFileExts[filepath.Ext(lower)] {
		return true
	}
	for _, m := range gateNameMarkers {
		if strings.Contains(base, m) {
			return true
		}
	}
	// A dot-prefixed extensionless token is a gate script by convention.
	return strings.HasPrefix(base, ".") && len(base) > 1 && filepath.Ext(base) == ""
}

// CommandVerifier is the loop's verify step: it runs the CONFIGURED verify
// command through the Phase-02 run_command tool and reports the REAL signal —
// the process exit code and output. It has NO input for the model's textual
// claim of success, so it is structurally impossible to fool: a model that says
// "tests pass" while the command exits non-zero yields Passed=false.
//
// Success = exit 0, optionally AND an additional structural check (used by the
// Phase-06 benchmark's structure gate). A command that cannot be run at all
// (empty/non-runnable, timeout, jail escape) sets VerifyResult.Err — an error
// outcome, never a false pass.
//
// A GATE-shaped file named by the verify command (a script or a test file) is
// hashed at construction and re-checked before every run, because a gate the
// model can rewrite proves nothing. Deliverables are deliberately NOT guarded:
// `grep -qx right answer.txt` names the file the run exists to write, and
// refusing to verify once the agent has produced it would be a worse bug than
// the one this closes.
type CommandVerifier struct {
	tool       tools.RunCommandTool
	command    string
	timeoutSec float64
	structural func(VerifyResult) bool
	now        func() time.Time
	// guarded maps each in-jail regular file named by the command to its sha256
	// at construction. Empty for a command naming no file (e.g. "go test ./...").
	guarded map[string]string
}

// VerifyOption configures a CommandVerifier.
type VerifyOption func(*CommandVerifier)

// WithVerifyTimeout sets a per-verify timeout (seconds) for the command. The CLI
// resolves it from --verify-timeout / KLOO_VERIFY_TIMEOUT / the profile's
// verifyTimeout; 0 or less leaves timeout_seconds unset, so the command inherits
// tools.DefaultCommandTimeout exactly as it did before the knob existed.
func WithVerifyTimeout(seconds float64) VerifyOption {
	return func(v *CommandVerifier) { v.timeoutSec = seconds }
}

// timeout is the ceiling this verify actually runs under — the configured value,
// or the run_command default that an unset verifier silently inherits. It exists
// for the TIMEOUT MESSAGE: a run killed at a ceiling it does not name leaves the
// user with no way to pick a bigger one, and the inherited case is precisely the
// one that bit us (the interactive path passed no option, so a naive report would
// have said "killed after 0s").
func (v *CommandVerifier) timeout() time.Duration {
	if v.timeoutSec > 0 {
		return time.Duration(v.timeoutSec * float64(time.Second))
	}
	return tools.DefaultCommandTimeout
}

// WithStructuralCheck adds a structural gate applied only when exit==0; when it
// returns false the verify is NOT passed (e.g. a greppable assertion).
func WithStructuralCheck(check func(VerifyResult) bool) VerifyOption {
	return func(v *CommandVerifier) { v.structural = check }
}

// NewCommandVerifier builds a verifier that runs command jailed to ws. command
// is resolved from config by the caller (flag → env → profile → default).
func NewCommandVerifier(ws tools.Workspace, command string, opts ...VerifyOption) *CommandVerifier {
	v := &CommandVerifier{
		tool:    tools.NewRunCommandTool(ws),
		command: command,
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(v)
	}
	v.guarded = snapshotGuardedFiles(ws, command)
	return v
}

// snapshotGuardedFiles hashes every in-jail regular file named by command, so
// Verify can detect a gate rewritten mid-run. Tokens are the whitespace-separated
// words of the command with a leading "./" stripped; anything that is not a file
// in the jail (`go`, `test`, `./...`, `-run`, a flag, a jail escape) is simply not
// recorded — the guard must never turn an ordinary verify into an error. Files
// over guardMaxFileBytes are skipped rather than hashed, to keep the snapshot
// cheap on a path that runs after every mutation.
func snapshotGuardedFiles(ws tools.Workspace, command string) map[string]string {
	guarded := map[string]string{}
	for _, tok := range strings.Fields(command) {
		tok = strings.TrimPrefix(tok, "./")
		if tok == "" {
			continue
		}
		p, err := ws.Resolve(tok)
		if err != nil {
			continue // outside the jail, or unresolvable: not ours to guard
		}
		// Only the GATE is guarded. A token naming the run's deliverable
		// (`grep -qx right answer.txt`) is an argument, and the agent is
		// supposed to write it — guarding it would report "verify DID NOT RUN"
		// on every successful run.
		if !isGateFile(tok) {
			continue
		}
		sum, ok := hashFile(p)
		if !ok {
			continue // not a file, unreadable, or too large
		}
		guarded[p] = sum
	}
	if len(guarded) == 0 {
		return nil
	}
	return guarded
}

// hashFile returns the hex sha256 of the file at abs, or ok=false when abs is
// not a regular file, cannot be read, or exceeds guardMaxFileBytes.
func hashFile(abs string) (string, bool) {
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > guardMaxFileBytes {
		return "", false
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), true
}

// checkTampered re-hashes every guarded path and reports the first that changed
// or vanished. It runs BEFORE the command: a gate that has been rewritten is not
// evidence about the code, so there is nothing to gain from running it.
func (v *CommandVerifier) checkTampered() error {
	// Sorted so the reported path is deterministic when several changed.
	paths := make([]string, 0, len(v.guarded))
	for p := range v.guarded {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		want := v.guarded[p]
		got, ok := hashFile(p)
		if !ok {
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("%w: %s was deleted during the run", ErrVerifyTampered, p)
			}
			continue // became unreadable/oversize: not provably tampered
		}
		if got != want {
			return fmt.Errorf("%w: %s was changed during the run", ErrVerifyTampered, p)
		}
	}
	return nil
}

// Verify runs the configured command and returns the real result. The
// continue/stop decision the loop makes consumes ONLY this struct.
func (v *CommandVerifier) Verify(ctx context.Context) VerifyResult {
	start := v.now()
	if len(v.guarded) > 0 {
		if err := v.checkTampered(); err != nil {
			// The gate is no longer the gate. Report it as an error outcome and do
			// NOT run the (possibly rewritten) command — running a script the model
			// just rewrote is the exact thing the guard exists to prevent.
			vr := VerifyResult{Command: v.command, Duration: v.now().Sub(start)}
			vr.Err = err
			vr.Passed = false
			return vr
		}
	}
	args := map[string]any{"command": v.command}
	if v.timeoutSec > 0 {
		args["timeout_seconds"] = v.timeoutSec
	}

	res, err := v.tool.Invoke(ctx, tools.Call{Name: tools.NameRunCommand, Args: args})

	vr := VerifyResult{
		Command:  v.command,
		ExitCode: res.ExitCode,
		Stdout:   res.Output,
		Stderr:   res.Stderr,
		Duration: v.now().Sub(start),
	}
	if err != nil {
		// Non-runnable / timeout / jail escape — an error outcome, not a pass.
		//
		// A TIMEOUT is re-worded here, and only here. run_command's own message
		// ("re-run it with a larger timeout_seconds") is addressed to the MODEL,
		// which can set that tool argument. On the verify path nobody can: the
		// harness owns the call. A 13m16s run once discarded a correct nine-file
		// fix because its gate needed a cold Rust build of seven test binaries and
		// crossed the 5-minute ceiling — and the only remedy it printed was one the
		// product did not offer. Name the knob the reader actually has, and the
		// ceiling it was killed at, so the next value is obvious.
		if errors.Is(err, tools.ErrCommandTimeout) {
			err = fmt.Errorf("the verify command was killed after %s — if the gate just needs longer (a cold build, a slow install), raise the ceiling with --verify-timeout (e.g. --verify-timeout %s): %w",
				v.timeout(), 3*v.timeout(), tools.ErrCommandTimeout)
		}
		vr.Err = err
		vr.Passed = false
		return vr
	}

	vr.Passed = res.ExitCode == 0
	if vr.Passed && v.structural != nil && !v.structural(vr) {
		vr.Passed = false // exit 0 but the structural gate failed
	}
	// A plain verify IS the verify stage: record its own outcome so a LayeredVerifier
	// (and the JSON verify block) can distinguish the verify command from hooks.
	vr.VerifyRan = true
	vr.VerifyPassed = vr.Passed
	return vr
}

// LayeredVerifier (B5) wraps an inner verify command with optional precheck and
// postcheck command gates, running them in the fixed order:
//
//	precheck(s) -> verify -> postcheck(s)
//
// It preserves verify as the ONLY positive success signal: success requires the
// verify command to pass AND every configured hook to pass. Hooks add failure
// evidence; they never make a run succeed on their own. Ordering rules:
//
//   - A failing precheck short-circuits: verify and postchecks do NOT run.
//   - Verify runs next; a failing verify short-circuits postchecks (existing
//     verify_failed behaviour is unchanged — FailedStage stays "").
//   - Postchecks run only after verify exits 0. A failing postcheck makes the run
//     non-success even though verify passed, reported distinctly (FailedStage).
//
// Every hook is itself a CommandVerifier, so hooks reuse the same jailed,
// timeout-bounded run_command execution as verify (no forked runner).
type LayeredVerifier struct {
	prechecks  []Verifier
	inner      Verifier
	postchecks []Verifier
}

// NewLayeredVerifier wraps inner with the given precheck/postcheck verifiers (each a
// Verifier — the CLI builds CommandVerifiers, tests may inject stubs). When both hook
// slices are empty it still delegates to inner unchanged.
func NewLayeredVerifier(prechecks []Verifier, inner Verifier, postchecks []Verifier) *LayeredVerifier {
	return &LayeredVerifier{prechecks: prechecks, inner: inner, postchecks: postchecks}
}

// Verify runs precheck(s) -> verify -> postcheck(s) with the short-circuiting above
// and returns a single VerifyResult whose Passed is the overall gate.
func (l *LayeredVerifier) Verify(ctx context.Context) VerifyResult {
	var pres []HookResult
	for _, pc := range l.prechecks {
		r := pc.Verify(ctx)
		pres = append(pres, HookResult{Command: r.Command, Stage: "precheck", Passed: r.Passed, ExitCode: r.ExitCode})
		if r.Err != nil || !r.Passed {
			// Precheck decided the outcome; verify/postcheck do not run.
			r.Passed = false
			r.FailedStage = "precheck"
			r.VerifyRan = false
			r.VerifyPassed = false
			r.Prechecks = pres
			return r
		}
	}

	vr := l.inner.Verify(ctx)
	vr.Prechecks = pres
	if vr.Err != nil || !vr.Passed {
		// Verify failed → existing verify_failed path (FailedStage stays ""), no postcheck.
		return vr
	}

	var posts []HookResult
	for _, pc := range l.postchecks {
		r := pc.Verify(ctx)
		posts = append(posts, HookResult{Command: r.Command, Stage: "postcheck", Passed: r.Passed, ExitCode: r.ExitCode})
		if r.Err != nil || !r.Passed {
			// Verify passed but a postcheck failed ⇒ overall non-success, reported as
			// the postcheck stage. Keep the verify command's own outcome available.
			r.Passed = false
			r.FailedStage = "postcheck"
			r.VerifyRan = true
			r.VerifyPassed = true
			r.Prechecks = pres
			r.Postchecks = posts
			return r
		}
	}
	vr.Postchecks = posts
	return vr // all gates green: Passed=true, FailedStage=""
}
