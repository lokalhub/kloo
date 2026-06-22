package agent

import (
	"context"
	"time"

	"github.com/lokal/kloo/internal/tools"
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
	return vr
}
