package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// ─── Phase 02 Task 01: verify runs only after a mutating step ───────────────

// scriptTool is a registry Tool whose dispatch outcome the test dictates, so a
// timed-out command or an MCP-style tool name can be exercised without a shell.
type scriptTool struct {
	name string
	res  tools.Result
	err  error
}

func (t scriptTool) Name() string        { return t.name }
func (t scriptTool) Description() string { return "scripted" }
func (t scriptTool) Schema() tools.ParamSchema {
	return tools.ParamSchema{Properties: map[string]tools.Property{
		"path": {Type: "string"}, "command": {Type: "string"},
	}}
}
func (t scriptTool) Invoke(ctx context.Context, c tools.Call) (tools.Result, error) {
	return t.res, t.err
}

// countingVerifier records every Verify call and the results it handed back.
type countingVerifier struct {
	results []VerifyResult
	calls   int
}

func (v *countingVerifier) Verify(ctx context.Context) VerifyResult {
	r := v.results[min(v.calls, len(v.results)-1)]
	v.calls++
	return r
}

// recordingChurn captures every Turn the loop feeds the churn rail, so the
// VerifySkipped neutrality can be asserted on the real feed rather than inferred.
type recordingChurn struct{ turns []Turn }

func (c *recordingChurn) Observe(t Turn)           { c.turns = append(c.turns, t) }
func (c *recordingChurn) Check() (bool, ChurnKind) { return false, "" }
func (c *recordingChurn) Artifact() string         { return "" }
func (c *recordingChurn) Reset()                   {}

// gateLoop builds a loop wired to the mocked endpoint with the full read-only tool
// vocabulary registered, plus any scripted tools the test supplies.
func gateLoop(t *testing.T, srv *llmtest.Server, v Verifier, c ChurnDetector, extra ...tools.Tool) *Loop {
	t.Helper()
	loop, calls := newLoop(t, srv, v, &stubBudget{tripAt: 50}, c)
	for _, name := range []string{tools.NameListDir, tools.NameReadDir, tools.NameSearch, tools.NameCommandOutput} {
		loop.Registry.Register(recordTool{name: name, calls: calls})
	}
	// newLoop's run_command stub declares "path" as required, which would reject a
	// real run_command call (whose argument is "command") before dispatch and make
	// every shell turn look like a failed one. Replace it with a permissive stub.
	loop.Registry.Register(scriptTool{name: tools.NameRunCommand, res: tools.Result{Output: "ok"}})
	for _, tl := range extra {
		loop.Registry.Register(tl)
	}
	loop.RepeatNudgeRounds, loop.RepeatAbortRounds, loop.StallRounds = 50, 60, 60
	return loop
}

func mocksFor(t *testing.T, specs ...tcSpec) []llmtest.Mock {
	t.Helper()
	out := make([]llmtest.Mock, 0, len(specs))
	for _, s := range specs {
		out = append(out, llmtest.Mock{Body: toolResp(t, 5, s)})
	}
	return out
}

var finishSpec = tcSpec{"finish", map[string]any{"summary": "done"}}

// TestReadOnlyStepSkipsVerify: steps that only LOOK at the tree cannot have changed
// the verify result, so the suite is not re-run for them. The one attempt is the
// unconditional finish-path verify.
func TestReadOnlyStepSkipsVerify(t *testing.T) {
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"read_file", map[string]any{"path": "a.go"}},
		tcSpec{"list_dir", map[string]any{"path": "."}},
		tcSpec{"search", map[string]any{"query": "func"}},
		finishSpec,
	)...)
	v := &countingVerifier{results: []VerifyResult{passResult()}}
	loop := gateLoop(t, srv, v, &stubChurn{})

	rep, err := loop.Run(context.Background(), "inspect the tree")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 1 {
		t.Errorf("VerifyAttempts = %d, want exactly 1 (three reads skip; finish always verifies)",
			rep.ToolCounters.VerifyAttempts)
	}
	if rep.Steps != 4 {
		t.Errorf("steps = %d, want 4", rep.Steps)
	}
}

// TestEditTriggersVerify: an edit that APPLIES changed the tree, so the suite runs
// that turn. Behaviour already true at v0.16.7 and it must stay true.
func TestEditTriggersVerify(t *testing.T) {
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"edit_file", map[string]any{"path": "a.go"}},
		finishSpec,
	)...)
	v := &countingVerifier{results: []VerifyResult{failResult()}}
	loop := gateLoop(t, srv, v, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix a.go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One for the edit turn, one for finish.
	if rep.ToolCounters.VerifyAttempts != 2 {
		t.Errorf("VerifyAttempts = %d, want exactly 2 (the edit, then finish)", rep.ToolCounters.VerifyAttempts)
	}
}

// TestFailedEditDoesNotTriggerVerify: an edit that fails to apply changes no bytes,
// so there is nothing to re-check.
func TestFailedEditDoesNotTriggerVerify(t *testing.T) {
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"edit_file", map[string]any{"path": "a.go"}},
		finishSpec,
	)...)
	v := &countingVerifier{results: []VerifyResult{passResult()}}
	loop := gateLoop(t, srv, v, &stubChurn{},
		scriptTool{name: tools.NameEditFile, err: fmt.Errorf("edit: no match for SEARCH block")})

	rep, err := loop.Run(context.Background(), "fix a.go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 1 {
		t.Errorf("VerifyAttempts = %d, want exactly 1 (the failed edit mutated nothing; only finish verifies)",
			rep.ToolCounters.VerifyAttempts)
	}
}

// TestReadOnlyCommandSkipsVerify: run_command is classified by its COMMAND, reusing
// tools.IsReadOnlyCommand — `go test` and `git diff` inspect, so they skip.
func TestReadOnlyCommandSkipsVerify(t *testing.T) {
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"run_command", map[string]any{"command": "go test ./..."}},
		tcSpec{"run_command", map[string]any{"command": "git diff"}},
		finishSpec,
	)...)
	v := &countingVerifier{results: []VerifyResult{passResult()}}
	loop := gateLoop(t, srv, v, &stubChurn{})

	rep, err := loop.Run(context.Background(), "look around")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 1 {
		t.Errorf("VerifyAttempts = %d, want exactly 1 (both commands only inspect)", rep.ToolCounters.VerifyAttempts)
	}
}

// TestMutatingCommandTriggersVerify: an unrecognised shell command defaults to
// acting, so it triggers the re-check.
func TestMutatingCommandTriggersVerify(t *testing.T) {
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"run_command", map[string]any{"command": "rm -f x"}},
		finishSpec,
	)...)
	v := &countingVerifier{results: []VerifyResult{failResult()}}
	loop := gateLoop(t, srv, v, &stubChurn{})

	rep, err := loop.Run(context.Background(), "clean up")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 2 {
		t.Errorf("VerifyAttempts = %d, want exactly 2 (the command, then finish)", rep.ToolCounters.VerifyAttempts)
	}
}

// TestTimedOutCommandTriggersVerify: the killed-`npm install` case. run_command
// returns its PARTIAL result alongside ErrCommandTimeout after the process ran, so
// derr != nil there does not mean "nothing landed". Without the result.TimedOut
// term this run would skip the regression check.
func TestTimedOutCommandTriggersVerify(t *testing.T) {
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"run_command", map[string]any{"command": "npm install"}},
		finishSpec,
	)...)
	v := &countingVerifier{results: []VerifyResult{failResult()}}
	loop := gateLoop(t, srv, v, &stubChurn{}, scriptTool{
		name: tools.NameRunCommand,
		res:  tools.Result{Output: "added 120 packages", TimedOut: true, ExitCode: -1},
		err:  tools.ErrCommandTimeout,
	})

	rep, err := loop.Run(context.Background(), "install deps")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 2 {
		t.Errorf("VerifyAttempts = %d, want exactly 2 — a KILLED command still ran and may have "+
			"changed the tree, so it must not skip the verify", rep.ToolCounters.VerifyAttempts)
	}
}

// TestUnknownToolTriggersVerify: a tool registered under an arbitrary MCP-style
// name mutates for all the loop knows, so it triggers the re-check. This is the
// test that fails if the predicate is ever narrowed back to an isEditTool allowlist.
func TestUnknownToolTriggersVerify(t *testing.T) {
	const mcpName = "mcp__fs__write"
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{mcpName, map[string]any{"path": "a.go"}},
		finishSpec,
	)...)
	v := &countingVerifier{results: []VerifyResult{failResult()}}
	loop := gateLoop(t, srv, v, &stubChurn{},
		scriptTool{name: mcpName, res: tools.Result{Output: "wrote a.go"}})

	rep, err := loop.Run(context.Background(), "write through mcp")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 2 {
		t.Errorf("VerifyAttempts = %d, want exactly 2 — an unrecognised tool must default to "+
			"MUTATING, or a tree-changing MCP tool silently skips the regression check",
			rep.ToolCounters.VerifyAttempts)
	}
}

// TestVerifyRunsOncePerMutationBurst: edit, then three reads ⇒ one verify for the
// edit, not four.
func TestVerifyRunsOncePerMutationBurst(t *testing.T) {
	read := tcSpec{"read_file", map[string]any{"path": "a.go"}}
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"edit_file", map[string]any{"path": "a.go"}},
		read, read, read,
	)...)
	v := &countingVerifier{results: []VerifyResult{failResult()}}
	loop := gateLoop(t, srv, v, &stubChurn{})
	loop.RepeatAbortRounds = 3 // end the run deterministically on the repeated read

	rep, err := loop.Run(context.Background(), "fix then review")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.VerifyAttempts != 1 {
		t.Errorf("VerifyAttempts = %d, want exactly 1 (the edit; the three reads skip)",
			rep.ToolCounters.VerifyAttempts)
	}
}

// TestMutationStaysPendingAcrossReadOnlyTurns: the flag is STICKY. The verifier is
// absent on the mutating turn — so nothing consumes the flag — and installed at the
// start of the next turn. A pending mutation must still be owed a verify, so the
// following READ-ONLY turn runs it. With a bare per-turn assignment the read-only
// turn would clear the flag and the mutation would never be checked.
func TestMutationStaysPendingAcrossReadOnlyTurns(t *testing.T) {
	read := tcSpec{"read_file", map[string]any{"path": "a.go"}}
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"edit_file", map[string]any{"path": "a.go"}},
		read, read, finishSpec,
	)...)
	v := &countingVerifier{results: []VerifyResult{failResult()}}
	loop := gateLoop(t, srv, nil, &stubChurn{})
	loop.OnProgress = func(step, maxSteps, tokens, maxTokens int) {
		if step >= 2 { // the edit has already been dispatched with no verifier present
			loop.Verifier = v
		}
	}

	rep, err := loop.Run(context.Background(), "fix then review")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Turn 2 (read-only) consumes the mutation pending from turn 1; turn 3 skips;
	// finish verifies unconditionally.
	if rep.ToolCounters.VerifyAttempts != 2 {
		t.Errorf("VerifyAttempts = %d, want exactly 2 — the mutation from turn 1 must still be "+
			"owed a verify on turn 2, and the flag must not be cleared by a read-only turn",
			rep.ToolCounters.VerifyAttempts)
	}
}

// TestUnverifiedModeDoesNotSetVerifySkipped: with no verifier there is no verify to
// skip, so the unverified path feeds the churn rail exactly what it fed at v0.16.7.
func TestUnverifiedModeDoesNotSetVerifySkipped(t *testing.T) {
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"read_file", map[string]any{"path": "a.go"}},
		tcSpec{"edit_file", map[string]any{"path": "a.go"}},
		finishSpec,
	)...)
	rc := &recordingChurn{}
	loop := gateLoop(t, srv, nil, rc)

	if _, err := loop.Run(context.Background(), "no verifier configured"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rc.turns) == 0 {
		t.Fatal("churn rail saw no turns")
	}
	for i, turn := range rc.turns {
		if turn.VerifySkipped {
			t.Errorf("turn %d: VerifySkipped = true in UNVERIFIED mode; there is no verify to skip", i+1)
		}
		if turn.VerifyOutput != "" {
			t.Errorf("turn %d: VerifyOutput = %q, want empty in unverified mode", i+1, turn.VerifyOutput)
		}
	}
}

// TestSkippedVerifyIsChurnNeutral: after a RED verify, read-only turns whose verify
// is skipped must neither advance nor reset the repeated-failure counter.
//
// This is the trap the phase exists to avoid: a skipped turn carries
// VerifyOutput: "", and the rail's first arm treats a blank output as "verify
// passed" and zeroes failCount. Feeding VerifySkipped instead — handled first in the
// switch — keeps the turn neutral. This test fails if the loop passes
// VerifyOutput: "" for a skipped verify.
func TestSkippedVerifyIsChurnNeutral(t *testing.T) {
	read := tcSpec{"read_file", map[string]any{"path": "a.go"}}
	srv := llmtest.Sequence(t, mocksFor(t,
		tcSpec{"edit_file", map[string]any{"path": "a.go"}},
		read, read, read, finishSpec,
	)...)
	rc := &recordingChurn{}
	v := &countingVerifier{results: []VerifyResult{failResult()}}
	loop := gateLoop(t, srv, v, rc)

	if _, err := loop.Run(context.Background(), "edit then read"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The three read-only turns after the edit must be flagged skipped, and must
	// carry no verify output that the rail could mistake for progress.
	skipped := 0
	for i, turn := range rc.turns {
		if !turn.VerifySkipped {
			continue
		}
		skipped++
		if turn.VerifyOutput != "" {
			t.Errorf("turn %d: a skipped verify must carry no output, got %q", i+1, turn.VerifyOutput)
		}
	}
	if skipped != 3 {
		t.Fatalf("skipped turns = %d, want 3 (the three reads after the edit)", skipped)
	}

	// And the real rail must treat them as neutral. Neutrality is an EQUIVALENCE:
	// interleaving skipped turns between real failures must not change the verdict.
	fail := Turn{VerifyOutput: "FAIL: TestThing"}
	skip := Turn{VerifySkipped: true}
	seed := Turn{VerifyOutput: "FAIL: TestThing", Edit: "e1", Acted: true}

	plain := NewChurnDetector(3)
	plain.Observe(seed)
	plain.Observe(fail)
	plain.Observe(fail)
	plain.Observe(fail)

	withSkips := NewChurnDetector(3)
	withSkips.Observe(seed)
	withSkips.Observe(fail)
	withSkips.Observe(skip)
	withSkips.Observe(fail)
	withSkips.Observe(skip)
	withSkips.Observe(skip)
	withSkips.Observe(fail)

	wantTrip, wantKind := plain.Check()
	gotTrip, gotKind := withSkips.Check()
	if !wantTrip {
		t.Fatal("harness error: three identical failures should trip a ChurnRounds=3 rail")
	}
	if gotTrip != wantTrip || gotKind != wantKind {
		t.Errorf("skipped turns changed the churn verdict: with skips (%t, %v), without (%t, %v) — "+
			"a skipped verify must neither count nor reset", gotTrip, gotKind, wantTrip, wantKind)
	}

	// And the trap itself, demonstrated rather than described: a turn carrying
	// VerifyOutput: "" — what a naive implementation would feed for a skipped
	// verify — RESETS the run, so the rail never trips.
	blank := NewChurnDetector(3)
	blank.Observe(seed)
	blank.Observe(fail)
	blank.Observe(Turn{}) // the naive "skipped" turn
	blank.Observe(fail)
	blank.Observe(fail)
	if trip, _ := blank.Check(); trip {
		t.Error("harness error: a blank-output turn is supposed to reset the run — " +
			"if it no longer does, VerifySkipped's reason for existing has changed")
	}
}

// TestMutatedPredicate pins the predicate itself, including the two terms a
// restatement is most likely to drop.
func TestMutatedPredicate(t *testing.T) {
	boom := fmt.Errorf("dispatch failed")
	cases := []struct {
		name     string
		readOnly bool
		timedOut bool
		err      error
		want     bool
	}{
		{name: "read-only tool", readOnly: true, want: false},
		{name: "read-only tool that errored", readOnly: true, err: boom, want: false},
		{name: "read-only command that timed out", readOnly: true, timedOut: true, want: false},
		{name: "successful mutating call", want: true},
		{name: "failed mutating call changed nothing", err: boom, want: false},
		{name: "KILLED command still ran", timedOut: true, err: boom, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mutated(tc.readOnly, tc.timedOut, tc.err); got != tc.want {
				t.Errorf("mutated(readOnly=%t, timedOut=%t, err=%v) = %t, want %t",
					tc.readOnly, tc.timedOut, tc.err, got, tc.want)
			}
		})
	}
}
