package agent

import (
	"context"
	"testing"
)

// recordingVerifier is a stub Verifier that records how many times it ran and
// returns a fixed result — so ordering/short-circuiting can be asserted precisely.
type recordingVerifier struct {
	name   string
	called int
	result VerifyResult
	log    *[]string
}

func (r *recordingVerifier) Verify(_ context.Context) VerifyResult {
	r.called++
	if r.log != nil {
		*r.log = append(*r.log, r.name)
	}
	return r.result
}

func pass(cmd string) VerifyResult {
	return VerifyResult{Command: cmd, ExitCode: 0, Passed: true, VerifyRan: true, VerifyPassed: true}
}
func fail(cmd string) VerifyResult {
	return VerifyResult{Command: cmd, ExitCode: 1, Passed: false, Stdout: "FAIL", VerifyRan: true, VerifyPassed: false}
}

// TestLayeredNoHooksDelegates: with no hooks, the layered verifier returns the inner
// result unchanged (byte-identical behaviour) and records no hook arrays.
func TestLayeredNoHooksDelegates(t *testing.T) {
	inner := &recordingVerifier{result: pass("verify")}
	lv := NewLayeredVerifier(nil, inner, nil)
	got := lv.Verify(context.Background())
	if !got.Passed || got.FailedStage != "" || len(got.Prechecks) != 0 || len(got.Postchecks) != 0 {
		t.Fatalf("no-hooks delegate wrong: %+v", got)
	}
	if inner.called != 1 {
		t.Fatalf("inner called %d times, want 1", inner.called)
	}
}

// TestLayeredOrderAllPass: precheck → verify → postcheck all run in order and the
// overall result passes with FailedStage "".
func TestLayeredOrderAllPass(t *testing.T) {
	var order []string
	pre := &recordingVerifier{name: "pre", result: pass("pre"), log: &order}
	inner := &recordingVerifier{name: "verify", result: pass("verify"), log: &order}
	post := &recordingVerifier{name: "post", result: pass("post"), log: &order}
	lv := NewLayeredVerifier([]Verifier{pre}, inner, []Verifier{post})

	got := lv.Verify(context.Background())
	if !got.Passed || got.FailedStage != "" {
		t.Fatalf("all-pass wrong: %+v", got)
	}
	if len(order) != 3 || order[0] != "pre" || order[1] != "verify" || order[2] != "post" {
		t.Fatalf("run order = %v, want [pre verify post]", order)
	}
	if len(got.Prechecks) != 1 || !got.Prechecks[0].Passed || got.Prechecks[0].Stage != "precheck" {
		t.Fatalf("prechecks = %+v", got.Prechecks)
	}
	if len(got.Postchecks) != 1 || !got.Postchecks[0].Passed || got.Postchecks[0].Stage != "postcheck" {
		t.Fatalf("postchecks = %+v", got.Postchecks)
	}
}

// TestLayeredPrecheckShortCircuits: a failing precheck blocks verify AND postcheck,
// yields non-success with FailedStage precheck, and records the failing precheck.
func TestLayeredPrecheckShortCircuits(t *testing.T) {
	pre := &recordingVerifier{name: "pre", result: fail("scope-check")}
	inner := &recordingVerifier{name: "verify", result: pass("verify")}
	post := &recordingVerifier{name: "post", result: pass("post")}
	lv := NewLayeredVerifier([]Verifier{pre}, inner, []Verifier{post})

	got := lv.Verify(context.Background())
	if got.Passed {
		t.Fatal("precheck failure must be non-success")
	}
	if got.FailedStage != "precheck" {
		t.Fatalf("FailedStage = %q, want precheck", got.FailedStage)
	}
	if got.VerifyRan {
		t.Fatal("verify must not run after a precheck failure")
	}
	if inner.called != 0 || post.called != 0 {
		t.Fatalf("verify called %d, postcheck called %d — both should be 0", inner.called, post.called)
	}
	if len(got.Prechecks) != 1 || got.Prechecks[0].Passed {
		t.Fatalf("prechecks = %+v, want one failed", got.Prechecks)
	}
}

// TestLayeredVerifyFailSkipsPostcheck: a failing verify keeps the existing
// verify_failed shape (FailedStage "") and does not run postchecks.
func TestLayeredVerifyFailSkipsPostcheck(t *testing.T) {
	pre := &recordingVerifier{name: "pre", result: pass("pre")}
	inner := &recordingVerifier{name: "verify", result: fail("npm test")}
	post := &recordingVerifier{name: "post", result: pass("post")}
	lv := NewLayeredVerifier([]Verifier{pre}, inner, []Verifier{post})

	got := lv.Verify(context.Background())
	if got.Passed {
		t.Fatal("verify failure must be non-success")
	}
	if got.FailedStage != "" {
		t.Fatalf("FailedStage = %q, want \"\" (verify decided, existing verify_failed path)", got.FailedStage)
	}
	if post.called != 0 {
		t.Fatalf("postcheck ran %d times after a failing verify, want 0", post.called)
	}
}

// TestLayeredPostcheckFailIsNonSuccess: verify passes but a postcheck fails ⇒ overall
// non-success with FailedStage postcheck, and the verify command's own pass is
// preserved.
func TestLayeredPostcheckFailIsNonSuccess(t *testing.T) {
	inner := &recordingVerifier{name: "verify", result: pass("npm test")}
	post := &recordingVerifier{name: "post", result: fail("e2e")}
	lv := NewLayeredVerifier(nil, inner, []Verifier{post})

	got := lv.Verify(context.Background())
	if got.Passed {
		t.Fatal("postcheck failure must make the overall gate non-success even though verify passed")
	}
	if got.FailedStage != "postcheck" {
		t.Fatalf("FailedStage = %q, want postcheck", got.FailedStage)
	}
	if !got.VerifyRan || !got.VerifyPassed {
		t.Fatalf("verify's own pass should be preserved: VerifyRan=%v VerifyPassed=%v", got.VerifyRan, got.VerifyPassed)
	}
	if len(got.Postchecks) != 1 || got.Postchecks[0].Passed {
		t.Fatalf("postchecks = %+v, want one failed", got.Postchecks)
	}
}

// TestLayeredMultiplePrechecksStopAtFirstFail: with two prechecks, the second does
// not run once the first fails.
func TestLayeredMultiplePrechecksStopAtFirstFail(t *testing.T) {
	p1 := &recordingVerifier{name: "p1", result: fail("p1")}
	p2 := &recordingVerifier{name: "p2", result: pass("p2")}
	inner := &recordingVerifier{name: "verify", result: pass("verify")}
	lv := NewLayeredVerifier([]Verifier{p1, p2}, inner, nil)

	got := lv.Verify(context.Background())
	if got.Passed || got.FailedStage != "precheck" {
		t.Fatalf("wrong result: %+v", got)
	}
	if p2.called != 0 {
		t.Fatalf("second precheck ran %d times after the first failed, want 0", p2.called)
	}
	if len(got.Prechecks) != 1 {
		t.Fatalf("prechecks recorded = %d, want 1 (stopped at first fail)", len(got.Prechecks))
	}
}
