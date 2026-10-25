package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/tools"
)

// TestVerifyTimeoutErrorNamesASettableKnob is an incident report.
//
// A run ended like this after 13m16s and 940,515 tokens:
//
//	verify:  "./.gate-argv.sh" → exit 0 (passed=false)
//	rolled back to checkpoint
//	error:   verify: tools: command killed after 5m0s — if it just needs longer
//	         (e.g. a slow install/build), re-run it with a larger timeout_seconds
//
// The fix the model had produced was essentially correct across nine files. It was
// discarded because the project's gate had to build seven Rust test binaries into a
// cold CARGO_TARGET_DIR on its first complete run and crossed the 5-minute
// run_command ceiling.
//
// The remedy the message prescribed — "a larger timeout_seconds" — is a run_command
// TOOL ARGUMENT, addressed to the model. The person reading that line owned no way
// to set it. So the product named exactly one remedy, and it was the one remedy the
// product did not offer. A timed-out VERIFY must name the knob a user has.
func TestVerifyTimeoutErrorNamesASettableKnob(t *testing.T) {
	v := NewCommandVerifier(verifyWS(t), "sleep 10", WithVerifyTimeout(0.3))
	r := v.Verify(context.Background())
	if r.Passed || r.Err == nil {
		t.Fatalf("a killed verify must be an Err outcome, not a pass: %+v", r)
	}
	if !errors.Is(r.Err, tools.ErrCommandTimeout) {
		t.Fatalf("the timeout identity must survive the rewrite: %v", r.Err)
	}
	msg := r.Err.Error()
	if !strings.Contains(msg, "--verify-timeout") {
		t.Errorf("a timed-out verify must name the flag that raises the ceiling, got %q", msg)
	}
	if strings.Contains(msg, "timeout_seconds") {
		t.Errorf("the verify error must not prescribe timeout_seconds — a tool argument no user can set: %q", msg)
	}
}

// TestVerifyTimeoutErrorReportsTheCeilingItHit: the message is useless for picking a
// new value unless it says what the old one was. Both rungs are checked, because the
// interactive path passes NO option and inherits the run_command default — the exact
// configuration the incident ran under, and the one a naive implementation reports as
// "killed after 0s".
func TestVerifyTimeoutErrorReportsTheCeilingItHit(t *testing.T) {
	ws := verifyWS(t)
	if got := NewCommandVerifier(ws, "true", WithVerifyTimeout(42)).timeout(); got != 42*time.Second {
		t.Errorf("explicit ceiling = %s, want 42s", got)
	}
	if got := NewCommandVerifier(ws, "true").timeout(); got != tools.DefaultCommandTimeout {
		t.Errorf("unset ceiling = %s, want the run_command default %s", got, tools.DefaultCommandTimeout)
	}

	v := NewCommandVerifier(ws, "sleep 10", WithVerifyTimeout(0.3))
	r := v.Verify(context.Background())
	if r.Err == nil || !strings.Contains(r.Err.Error(), "300ms") {
		t.Errorf("the error must state the ceiling it hit, got %v", r.Err)
	}
}

// TestVerifyUnsetTimeoutStillOmitsTheToolArgument proves the default path BOTH ways:
// with no option the verifier must not synthesise a timeout_seconds of its own, so a
// run that passed nothing keeps inheriting the run_command default exactly as before.
func TestVerifyUnsetTimeoutStillOmitsTheToolArgument(t *testing.T) {
	v := NewCommandVerifier(verifyWS(t), "true")
	if v.timeoutSec != 0 {
		t.Errorf("an unconfigured verifier must leave timeoutSec at 0 (⇒ no timeout_seconds arg), got %v", v.timeoutSec)
	}
}
