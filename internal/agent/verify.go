package agent

import (
	"context"
	"time"

	"github.com/lokalhub/kloo/internal/tools"
)

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
type CommandVerifier struct {
	tool       tools.RunCommandTool
	command    string
	timeoutSec float64
	structural func(VerifyResult) bool
	now        func() time.Time
}

// VerifyOption configures a CommandVerifier.
type VerifyOption func(*CommandVerifier)

// WithVerifyTimeout sets a per-verify timeout (seconds) for the command.
func WithVerifyTimeout(seconds float64) VerifyOption {
	return func(v *CommandVerifier) { v.timeoutSec = seconds }
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
	return v
}

// Verify runs the configured command and returns the real result. The
// continue/stop decision the loop makes consumes ONLY this struct.
func (v *CommandVerifier) Verify(ctx context.Context) VerifyResult {
	start := v.now()
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
