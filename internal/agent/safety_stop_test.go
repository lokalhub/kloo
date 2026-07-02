package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// newScopedLoop builds a loop over a REAL scoped workspace seeded with the given
// files, so scope enforcement (and the hidden/rejected run_command) is exercised
// end-to-end. seed maps workspace-relative path → contents.
func newScopedLoop(t *testing.T, srv *llmtest.Server, seed map[string]string, allow, deny, readOnly []string, patchOnly bool, v Verifier) (*Loop, string) {
	t.Helper()
	root := t.TempDir()
	for rel, content := range seed {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := tools.NewWorkspace(canon)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := tools.NewScopePolicy(allow, deny, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	ws = ws.WithScope(policy).WithPatchOnly(patchOnly)
	return &Loop{
		Client:   llm.New(srv.URL+"/v1", "test-model"),
		Adapter:  tools.NativeFCAdapter{},
		Registry: tools.DefaultRegistry(ws),
		Verifier: v,
		Budget:   &stubBudget{tripAt: 50},
		Churn:    &stubChurn{},
		Root:     canon,
		Endpoint: srv.URL + "/v1",
		Model:    "test-model",
		System:   "you are kloo",
	}, canon
}

// TestStopOnOffScopeEditWrite: --stop-on off-scope-edit halts on the first denied
// write, emits ReasonSafetyStop with the off-scope-edit rule + outside_allow class,
// counts the off-scope edit, and leaves the denied file untouched.
func TestStopOnOffScopeEditWrite(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "README.md", "content": "hacked"}})},
	)
	loop, root := newScopedLoop(t, srv, map[string]string{"README.md": "keep\n"}, []string{"src/**"}, nil, nil, false, nil)
	loop.StopOn = StopPolicy{OffScopeEdit: true}

	rep, err := loop.Run(context.Background(), "edit README")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSafetyStop {
		t.Fatalf("Reason = %s, want safety-stop", rep.Reason)
	}
	if rep.Safety == nil || rep.Safety.Rule != "off-scope-edit" || rep.Safety.Class != tools.ScopeClassOutsideAllow {
		t.Fatalf("Safety = %+v, want rule off-scope-edit class outside_allow", rep.Safety)
	}
	if rep.ToolCounters.OffScopeEdits != 1 {
		t.Fatalf("OffScopeEdits = %d, want 1", rep.ToolCounters.OffScopeEdits)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "README.md")); string(got) != "keep\n" {
		t.Fatalf("denied file mutated: %q", string(got))
	}
}

// TestStopOnReadOnlyEdit: --stop-on read-only-edit halts on the first read-only
// write attempt, class read_only, and counts it as a read-only edit.
func TestStopOnReadOnlyEdit(t *testing.T) {
	diff := "<<<<<<< SEARCH\nfoo\n=======\nbar\n>>>>>>> REPLACE\n"
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "tests/x_test.go", "diff": diff}})},
	)
	loop, root := newScopedLoop(t, srv, map[string]string{"tests/x_test.go": "foo\n"}, nil, nil, []string{"tests/**"}, false, nil)
	loop.StopOn = StopPolicy{ReadOnlyEdit: true}

	rep, err := loop.Run(context.Background(), "weaken the test")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSafetyStop {
		t.Fatalf("Reason = %s, want safety-stop", rep.Reason)
	}
	if rep.Safety == nil || rep.Safety.Rule != "read-only-edit" || rep.Safety.Class != tools.ScopeClassReadOnly {
		t.Fatalf("Safety = %+v, want rule read-only-edit class read_only", rep.Safety)
	}
	if rep.ToolCounters.ReadOnlyEdits != 1 || rep.ToolCounters.OffScopeEdits != 1 {
		t.Fatalf("counters = %+v, want off_scope=1 read_only=1", rep.ToolCounters)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "tests/x_test.go")); string(got) != "foo\n" {
		t.Fatalf("read-only file mutated: %q", string(got))
	}
}

// TestStopOnOffScopeShellRejection: --stop-on off-scope-edit also halts when a
// scoped run_command is rejected (no process runs, target byte-identical).
func TestStopOnOffScopeShellRejection(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"run_command", map[string]any{"command": "sed -i s/keep/gone/ README.md"}})},
	)
	loop, root := newScopedLoop(t, srv, map[string]string{"README.md": "keep\n"}, []string{"src/**"}, nil, nil, false, nil)
	loop.StopOn = StopPolicy{OffScopeEdit: true}

	rep, err := loop.Run(context.Background(), "mutate README via shell")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSafetyStop || rep.Safety == nil || rep.Safety.Class != tools.ScopeClassRunCommandDisabled {
		t.Fatalf("Safety = %+v (reason %s), want run_command_disabled_for_scope", rep.Safety, rep.Reason)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "README.md")); string(got) != "keep\n" {
		t.Fatalf("file mutated by rejected run_command: %q", string(got))
	}
}

// TestStopOnRepeatedVerify: --stop-on repeated-verify=2 halts after two repeated
// verifier failures with no progress, well before the step budget.
func TestStopOnRepeatedVerify(t *testing.T) {
	edit := editFileCall(t, "a.txt", "foo\n", "bar\n", 5)
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: edit},
		llmtest.Mock{Body: edit},
		llmtest.Mock{Body: edit}, // should never be reached
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult(), failResult(), failResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.StallRounds = 20
	loop.EditFailLimit = 20
	loop.StopOn = StopPolicy{RepeatedVerify: 2}

	rep, err := loop.Run(context.Background(), "keep failing")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSafetyStop {
		t.Fatalf("Reason = %s, want safety-stop", rep.Reason)
	}
	if rep.Safety == nil || rep.Safety.Rule != "repeated-verify" || rep.Safety.Class != "repeated_verify_failure" {
		t.Fatalf("Safety = %+v, want rule repeated-verify class repeated_verify_failure", rep.Safety)
	}
	if rep.Steps != 2 {
		t.Fatalf("Steps = %d, want 2 (stopped early)", rep.Steps)
	}
}

// TestPatchOnlyRejectRecorded: a patch-only run_command rejection is recorded on the
// Report (for tool_call_invalid / patch_only_forbidden_tool classification) and the
// run continues rather than hard-stopping.
func TestPatchOnlyRejectRecorded(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"run_command", map[string]any{"command": "echo hi"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "cannot"}})},
	)
	loop, _ := newScopedLoop(t, srv, map[string]string{"a.txt": "x\n"}, nil, nil, nil, true, nil)
	rep, err := loop.Run(context.Background(), "run a command")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonSafetyStop {
		t.Fatal("patch-only rejection must NOT hard-stop the run")
	}
	if rep.PatchOnlyReject == nil || rep.PatchOnlyReject.Class != "patch_only_forbidden_tool" || rep.PatchOnlyReject.Tool != "run_command" {
		t.Fatalf("PatchOnlyReject = %+v, want run_command/patch_only_forbidden_tool", rep.PatchOnlyReject)
	}
	if rep.ToolCounters.InvalidToolCalls != 1 {
		t.Fatalf("InvalidToolCalls = %d, want 1", rep.ToolCounters.InvalidToolCalls)
	}
}

// TestNoStopOnRecordsDenialButContinues: without a stop rule, an off-scope denial
// is recorded (counter + LastScopeDenial) but the run continues (existing behaviour
// unchanged) and can end calmly.
func TestNoStopOnRecordsDenialButContinues(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "README.md", "content": "x"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "gave up"}})},
	)
	loop, _ := newScopedLoop(t, srv, map[string]string{"README.md": "keep\n"}, []string{"src/**"}, nil, nil, false, nil)
	// No StopOn. Verifier nil ⇒ finish → unverified.
	rep, err := loop.Run(context.Background(), "edit README")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonSafetyStop {
		t.Fatal("must NOT safety-stop without a --stop-on rule")
	}
	if rep.ToolCounters.OffScopeEdits != 1 {
		t.Fatalf("OffScopeEdits = %d, want 1", rep.ToolCounters.OffScopeEdits)
	}
	if rep.LastScopeDenial == nil || rep.LastScopeDenial.Class != tools.ScopeClassOutsideAllow {
		t.Fatalf("LastScopeDenial = %+v, want outside_allow", rep.LastScopeDenial)
	}
}
