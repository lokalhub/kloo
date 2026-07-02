package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// --- B5 classifier tests -----------------------------------------------------

// TestClassifyPrecheckFailed: a run whose final verify short-circuited on a precheck
// classifies as precheck_failed with source=precheck, regardless of terminal reason.
func TestClassifyPrecheckFailed(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonAnswered,
		FinalVerify: agent.VerifyResult{
			Command: "scope-check", ExitCode: 1, Stdout: "off-scope edit detected",
			FailedStage: "precheck", Passed: false,
		},
	}
	code, detail := classifyFailure(rep, nil)
	if code != "precheck_failed" || detail.Source != "precheck" || detail.Class != "precheck_failed" {
		t.Fatalf("code=%q detail=%+v, want precheck_failed", code, detail)
	}
	if !strings.Contains(detail.Message, "scope-check") {
		t.Fatalf("message should name the failing precheck: %q", detail.Message)
	}
}

// TestClassifyPostcheckFailed: verify passed but a postcheck failed → postcheck_failed
// (success remains false), taking precedence over verify_failed.
func TestClassifyPostcheckFailed(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonAnswered,
		FinalVerify: agent.VerifyResult{
			Command: "e2e", ExitCode: 2, Stdout: "e2e broke",
			FailedStage: "postcheck", Passed: false, VerifyRan: true, VerifyPassed: true,
		},
	}
	code, detail := classifyFailure(rep, nil)
	if code != "postcheck_failed" || detail.Source != "postcheck" {
		t.Fatalf("code=%q detail=%+v, want postcheck_failed/postcheck", code, detail)
	}
}

// TestClassifyVerifyFailedUnaffectedByHooks: a plain verify failure (FailedStage "")
// still classifies as verify_failed even when hook arrays are present.
func TestClassifyVerifyFailedUnaffectedByHooks(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonAnswered,
		FinalVerify: agent.VerifyResult{
			Command: "npm test", ExitCode: 1, Stdout: "tests failed", Passed: false,
			Prechecks: []agent.HookResult{{Command: "lint", Stage: "precheck", Passed: true}},
		},
	}
	code, _ := classifyFailure(rep, nil)
	if code != "verify_failed" {
		t.Fatalf("code = %q, want verify_failed (hooks passed, verify decided)", code)
	}
}

func TestHookFailedBenchmarkExits(t *testing.T) {
	if benchmarkExitCode(runSummary{FailureCode: "precheck_failed"}) != benchmarkExitVerify {
		t.Fatal("precheck_failed should map to the verify-family exit")
	}
	if benchmarkExitCode(runSummary{FailureCode: "postcheck_failed"}) != benchmarkExitVerify {
		t.Fatal("postcheck_failed should map to the verify-family exit")
	}
}

// --- B3 accounting tests -----------------------------------------------------

// TestRunSummaryCorrectionCountAndOffScope: correction_count is the sum of rail_fires
// values and off_scope_edits mirrors the report counter.
func TestRunSummaryCorrectionCountAndOffScope(t *testing.T) {
	rep := &agent.Report{
		Reason:       agent.ReasonAnswered,
		RailFires:    map[string]int{"confirm-finish": 1, "promise-to-act": 2},
		ToolCounters: agent.ToolCounters{OffScopeEdits: 3},
	}
	s := buildRunSummary(config.Config{Model: "m"}, "", rep, time.Second, nil)
	if s.CorrectionCount != 3 {
		t.Fatalf("correction_count = %d, want 3 (sum of rail_fires)", s.CorrectionCount)
	}
	if s.OffScopeEdits != 3 {
		t.Fatalf("off_scope_edits = %d, want 3", s.OffScopeEdits)
	}
}

// TestRunSummaryFinalReason: final_reason is the most specific terminal class, and
// "success" on a verified success.
func TestRunSummaryFinalReason(t *testing.T) {
	fail := &agent.Report{Reason: agent.ReasonSafetyStop, Safety: &agent.SafetyEvidence{Rule: "off-scope-edit", Class: "deny", Tool: "edit_file", Message: "denied"}}
	if got := buildRunSummary(config.Config{Model: "m"}, "", fail, time.Second, nil).FinalReason; got != "deny" {
		t.Fatalf("final_reason = %q, want deny (most specific class)", got)
	}
	ok := &agent.Report{Reason: agent.ReasonSuccess, FinalVerify: agent.VerifyResult{Command: "t", Passed: true}}
	if got := buildRunSummary(config.Config{Model: "m"}, "", ok, time.Second, nil).FinalReason; got != "success" {
		t.Fatalf("final_reason = %q, want success", got)
	}
}

// TestWithFilesChangedGitRepo: files_changed reflects the git working tree, sorted,
// in a real repo.
func TestWithFilesChangedGitRepo(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	os.WriteFile(filepath.Join(root, "base.txt"), []byte("b\n"), 0o644)
	git("add", "-A")
	git("commit", "-m", "init")
	os.WriteFile(filepath.Join(root, "z.txt"), []byte("z\n"), 0o644)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644)

	var s runSummary
	withFilesChanged(&s, root)
	if s.FilesChanged == nil {
		t.Fatal("files_changed should be populated for a git repo")
	}
	if s.FilesChanged.Count != 2 || len(s.FilesChanged.Paths) != 2 || s.FilesChanged.Paths[0] != "a.txt" || s.FilesChanged.Paths[1] != "z.txt" {
		t.Fatalf("files_changed = %+v, want sorted [a.txt z.txt]", s.FilesChanged)
	}
}

// TestWithFilesChangedNonGitEmpty: a non-git dir yields count 0 and an empty list.
func TestWithFilesChangedNonGitEmpty(t *testing.T) {
	var s runSummary
	withFilesChanged(&s, t.TempDir())
	if s.FilesChanged == nil || s.FilesChanged.Count != 0 || len(s.FilesChanged.Paths) != 0 {
		t.Fatalf("non-git files_changed = %+v, want count 0 empty list", s.FilesChanged)
	}
	// The empty list must marshal as [] (not null) so a harness can rely on the shape.
	b, _ := json.Marshal(s.FilesChanged)
	if !strings.Contains(string(b), `"paths":[]`) {
		t.Fatalf("empty paths should marshal as []: %s", b)
	}
}

// --- B5 end-to-end (headless, real hook commands) ----------------------------

// TestHeadlessPostcheckFailure drives a full headless run where verify passes but a
// postcheck fails: the run is non-success and the JSON classifies postcheck_failed
// with the postcheck recorded.
func TestHeadlessPostcheckFailure(t *testing.T) {
	scopeWorkspace(t, map[string]string{"README.md": "x\n"})
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: sseToolCall(t, "finish", map[string]any{"summary": "done"}), SSE: true},
	)
	cfg := config.Config{
		Endpoint:    srv.URL + "/v1",
		Model:       "test-model",
		ToolFormat:  config.DefaultToolFormat,
		JSONSummary: true,
		Postchecks:  []string{"false"}, // always fails
		MaxSteps:    20,
		ChurnRounds: 3,
	}
	var out bytes.Buffer
	// verifyCmd "true" so verify passes; the postcheck then fails.
	_ = defaultRunHeadless(cfg, "do it", "true", lintOpts{Disabled: true}, &out)

	s := lastResultJSON(t, out.String())
	if s["success"] != false {
		t.Fatalf("success = %v, want false (postcheck failed)", s["success"])
	}
	if s["failure_code"] != "postcheck_failed" {
		t.Fatalf("failure_code = %v, want postcheck_failed\n%s", s["failure_code"], out.String())
	}
	posts, _ := s["postchecks"].([]any)
	if len(posts) != 1 {
		t.Fatalf("postchecks array = %v, want one entry", s["postchecks"])
	}
	t.Logf("postcheck-fail headless output:\n%s", out.String())
}

// TestHeadlessPrecheckFailureBlocksVerify drives a run where a precheck fails: verify
// never runs, the run is non-success, and the JSON classifies precheck_failed.
func TestHeadlessPrecheckFailureBlocksVerify(t *testing.T) {
	scopeWorkspace(t, map[string]string{"README.md": "x\n"})
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: sseToolCall(t, "finish", map[string]any{"summary": "done"}), SSE: true},
	)
	cfg := config.Config{
		Endpoint:    srv.URL + "/v1",
		Model:       "test-model",
		ToolFormat:  config.DefaultToolFormat,
		JSONSummary: true,
		Prechecks:   []string{"false"},
		MaxSteps:    20,
		ChurnRounds: 3,
	}
	var out bytes.Buffer
	_ = defaultRunHeadless(cfg, "do it", "true", lintOpts{Disabled: true}, &out)

	s := lastResultJSON(t, out.String())
	if s["failure_code"] != "precheck_failed" {
		t.Fatalf("failure_code = %v, want precheck_failed\n%s", s["failure_code"], out.String())
	}
	fd, _ := s["failure_detail"].(map[string]any)
	if fd == nil || fd["source"] != "precheck" {
		t.Fatalf("failure_detail = %v, want source=precheck", s["failure_detail"])
	}
}

// TestHeadlessNoHooksUnchanged: a run with no hooks emits no precheck/postcheck arrays
// (byte-identical shape), confirming the hook fields are omitted when unused.
func TestHeadlessNoHooksUnchanged(t *testing.T) {
	scopeWorkspace(t, map[string]string{"README.md": "x\n"})
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: sseToolCall(t, "finish", map[string]any{"summary": "done"}), SSE: true},
	)
	cfg := config.Config{
		Endpoint: srv.URL + "/v1", Model: "test-model", ToolFormat: config.DefaultToolFormat,
		JSONSummary: true, MaxSteps: 20, ChurnRounds: 3,
	}
	var out bytes.Buffer
	_ = defaultRunHeadless(cfg, "do it", "true", lintOpts{Disabled: true}, &out)
	s := lastResultJSON(t, out.String())
	if _, present := s["prechecks"]; present {
		t.Errorf("prechecks must be omitted with no hooks, got %v", s["prechecks"])
	}
	if _, present := s["postchecks"]; present {
		t.Errorf("postchecks must be omitted with no hooks, got %v", s["postchecks"])
	}
}
