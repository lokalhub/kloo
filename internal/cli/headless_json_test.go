package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

func TestPrintHeadlessJSON(t *testing.T) {
	cfg := config.Config{Model: "dsv4", Endpoint: "http://x/v1", MaxContextTokens: 900000}
	rep := &agent.Report{
		Reason:      agent.ReasonSuccess,
		Steps:       4,
		TokensUsed:  2000,
		FinalVerify: agent.VerifyResult{Command: "npm test", Passed: true, ExitCode: 0},
		Transcript:  []llm.Message{{Role: "assistant", Content: "did the thing"}},
	}
	var buf bytes.Buffer
	printHeadlessJSON(&buf, cfg, "npm test", rep, 10*time.Second, nil)

	line := strings.TrimSpace(buf.String())
	if !strings.HasPrefix(line, "KLOO_RESULT_JSON ") {
		t.Fatalf("missing KLOO_RESULT_JSON prefix: %q", line)
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "KLOO_RESULT_JSON ")), &s); err != nil {
		t.Fatalf("emitted JSON is invalid: %v", err)
	}
	if s["model"] != "dsv4" || s["reason"] != "success" || s["success"] != true {
		t.Errorf("unexpected summary fields: %v", s)
	}
	if s["tokens_per_sec"].(float64) != 200 { // 2000 tokens / 10s
		t.Errorf("tokens_per_sec = %v, want 200", s["tokens_per_sec"])
	}
	if v, _ := s["verify"].(map[string]any); v == nil || v["passed"] != true {
		t.Errorf("verify block missing/wrong: %v", s["verify"])
	}
	// A clean run fired no rails ⇒ the field is omitted entirely (no key in the JSON).
	if _, present := s["rail_fires"]; present {
		t.Errorf("rail_fires must be omitted when no rail fired, got %v", s["rail_fires"])
	}
}

// TestPrintHeadlessJSONRailFires: when soft rails fired, the JSON carries a rail_fires
// tally so a benchmark can ASSERT the run's self-corrections (e.g. that a multi-step
// run was rescued by exactly one confirm-finish nudge) instead of parsing transcripts.
func TestPrintHeadlessJSONRailFires(t *testing.T) {
	cfg := config.Config{Model: "dsv4", Endpoint: "http://x/v1", MaxContextTokens: 900000}
	rep := &agent.Report{
		Reason:     agent.ReasonAnswered,
		Steps:      3,
		TokensUsed: 900,
		RailFires:  map[string]int{string(agent.RailConfirmFinish): 1},
		Transcript: []llm.Message{{Role: "assistant", Content: "all done"}},
	}
	var buf bytes.Buffer
	printHeadlessJSON(&buf, cfg, "npm test", rep, 5*time.Second, nil)

	line := strings.TrimPrefix(strings.TrimSpace(buf.String()), "KLOO_RESULT_JSON ")
	var s map[string]any
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		t.Fatalf("emitted JSON is invalid: %v", err)
	}
	rf, ok := s["rail_fires"].(map[string]any)
	if !ok {
		t.Fatalf("rail_fires missing/wrong type: %v", s["rail_fires"])
	}
	if rf[string(agent.RailConfirmFinish)] != float64(1) {
		t.Errorf("rail_fires[%q] = %v, want 1", agent.RailConfirmFinish, rf[string(agent.RailConfirmFinish)])
	}
}

func TestPrintHeadlessJSON_Error(t *testing.T) {
	var buf bytes.Buffer
	printHeadlessJSON(&buf, config.Config{Model: "m"}, "", nil, time.Second, errors.New("boom"))
	if !strings.Contains(buf.String(), `"error":"boom"`) {
		t.Errorf("expected error surfaced in JSON, got %q", buf.String())
	}
}

func TestPrintHeadlessJSON_ReportError(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonError,
		Err:    errors.New("model produced 2400 reasoning chars but no usable content; disable thinking (--no-think) or raise the output budget"),
	}
	var buf bytes.Buffer
	printHeadlessJSON(&buf, config.Config{Model: "m"}, "", rep, time.Second, nil)
	if !strings.Contains(buf.String(), `"reason":"error"`) || !strings.Contains(buf.String(), `--no-think`) {
		t.Errorf("expected report error surfaced in JSON, got %q", buf.String())
	}
}

func TestHeadlessReportAndJSONIncludeEnrichedModelError(t *testing.T) {
	errText := `agent: model call failed endpoint=http://127.0.0.1:9/v1 model=bad-model: llm: api error 500 Internal Server Error: {"error":"model loading"}`
	rep := &agent.Report{
		Reason: agent.ReasonError,
		Err:    errors.New(errText),
	}

	var plain bytes.Buffer
	printHeadlessReport(&plain, rep, time.Second)
	for _, want := range []string{"endpoint=http://127.0.0.1:9/v1", "model=bad-model", "model loading"} {
		if !strings.Contains(plain.String(), want) {
			t.Fatalf("plain report missing %q:\n%s", want, plain.String())
		}
	}

	var js bytes.Buffer
	printHeadlessJSON(&js, config.Config{Model: "bad-model", Endpoint: "http://127.0.0.1:9/v1"}, "", rep, time.Second, nil)
	for _, want := range []string{"endpoint=http://127.0.0.1:9/v1", "model=bad-model", "model loading"} {
		if !strings.Contains(js.String(), want) {
			t.Fatalf("JSON summary missing %q:\n%s", want, js.String())
		}
	}
	if strings.Contains(plain.String(), "super-secret") || strings.Contains(js.String(), "super-secret") {
		t.Fatalf("reports leaked secret:\nplain=%s\njson=%s", plain.String(), js.String())
	}
}

func TestValidateJSONOnly(t *testing.T) {
	valid := []string{
		`{"ok":true}`,
		`[1,2,3]`,
		`"done"`,
		`  {"nested":{"n":1}}  `,
	}
	for _, s := range valid {
		if err := validateJSONOnly(s); err != nil {
			t.Errorf("validateJSONOnly(%q) returned %v", s, err)
		}
	}
	invalid := []string{
		"",
		"```json\n{\"ok\":true}\n```",
		"Here is the JSON: {\"ok\":true}",
		"{\"ok\":true}\nextra",
	}
	for _, s := range invalid {
		if err := validateJSONOnly(s); err == nil {
			t.Errorf("validateJSONOnly(%q) should fail", s)
		}
	}
}

func TestApplyJSONOnlyValidationMarksReportRecoverable(t *testing.T) {
	rep := &agent.Report{
		Reason:     agent.ReasonAnswered,
		Transcript: []llm.Message{{Role: llm.RoleAssistant, Content: "```json\n{\"ok\":true}\n```"}},
	}
	applyJSONOnlyValidation(rep)
	if rep.Reason != agent.ReasonError || rep.Err == nil {
		t.Fatalf("json-only validation should turn prose/fences into report error, got %s/%v", rep.Reason, rep.Err)
	}
	for _, want := range []string{"valid JSON only", "remove prose/code fences", "one JSON value"} {
		if !strings.Contains(rep.Err.Error(), want) {
			t.Errorf("json-only error missing %q: %s", want, rep.Err)
		}
	}
}

func TestApplyJSONOnlyValidationPreservesExistingRunError(t *testing.T) {
	upstreamErr := errors.New("agent: model call failed endpoint=http://127.0.0.1:9/v1 model=bad-model: connection refused")
	rep := &agent.Report{
		Reason: agent.ReasonError,
		Err:    upstreamErr,
	}
	applyJSONOnlyValidation(rep)
	if rep.Err != upstreamErr || rep.Reason != agent.ReasonError {
		t.Fatalf("json-only validation should preserve existing run error, got %s/%v", rep.Reason, rep.Err)
	}
}

func TestHeadlessJSONOnlyUnreachableEndpointPreservesConnectionError(t *testing.T) {
	t.Chdir(t.TempDir())
	dead := llmtest.DeadURL(t) + "/v1"
	cfg := config.Config{
		Endpoint:         dead,
		Model:            "bad-model",
		ToolFormat:       config.DefaultToolFormat,
		Effort:           config.DefaultEffort,
		MaxSteps:         1,
		MaxContextTokens: config.DefaultMaxContextTokens,
		ChurnRounds:      config.DefaultChurnRounds,
		JSONSummary:      true,
		JSONOnly:         true,
	}
	var out bytes.Buffer
	err := defaultRunHeadless(cfg, "return json", "", lintOpts{}, &out)
	if err == nil {
		t.Fatal("unreachable endpoint should fail")
	}
	for _, want := range []string{dead, "bad-model", "connection"} {
		if !strings.Contains(strings.ToLower(out.String()), strings.ToLower(want)) {
			t.Fatalf("headless output missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "valid JSON only") || strings.Contains(out.String(), "empty answer") {
		t.Fatalf("json-only validation overwrote upstream failure:\n%s", out.String())
	}
}

func TestHeadlessLLMMaxRetriesZeroDisablesRetry(t *testing.T) {
	t.Chdir(t.TempDir())
	srv := llmtest.Sequence(t,
		llmtest.Mock{Status: 503, Body: `{"error":"model is loading"}`},
		llmtest.Mock{Body: `{"choices":[{"message":{"role":"assistant","content":"unused"}}]}`},
	)
	cfg := config.Config{
		Endpoint:                srv.URL + "/v1",
		Model:                   "loading-model",
		ToolFormat:              config.DefaultToolFormat,
		Effort:                  config.DefaultEffort,
		MaxSteps:                1,
		MaxContextTokens:        config.DefaultMaxContextTokens,
		ChurnRounds:             config.DefaultChurnRounds,
		LLMMaxRetries:           0,
		LLMRetryableStatusCodes: append([]int(nil), config.DefaultLLMRetryableStatusCodes...),
		LLMRetryBaseDelay:       time.Millisecond,
		LLMRetryMaxDelay:        time.Millisecond,
		LLMColdLoadTimeout:      config.DefaultLLMColdLoadTimeout,
		LLMStreamIdleTimeout:    config.DefaultLLMStreamIdle,
	}
	var out bytes.Buffer
	err := defaultRunHeadless(cfg, "fix x", "true", lintOpts{}, &out)
	if err == nil {
		t.Fatal("retryable 503 should fail when retry count is explicitly zero")
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("requests = %d, want 1 with retry disabled\n%s", n, out.String())
	}
	if strings.Contains(out.String(), "retrying") {
		t.Fatalf("output shows retry even though LLMMaxRetries is zero:\n%s", out.String())
	}
}

func TestBuildRunSummaryAndStatusFile(t *testing.T) {
	rep := &agent.Report{
		Reason:      agent.ReasonSuccess,
		Steps:       2,
		TokensUsed:  100,
		FinalVerify: agent.VerifyResult{Command: "go test", Passed: true, ExitCode: 0},
		Transcript:  []llm.Message{{Role: llm.RoleAssistant, Content: `{"ok":true}`}},
	}
	s := buildRunSummary(config.Config{Model: "m", Endpoint: "http://e/v1", MaxContextTokens: 123}, "go test", rep, 2*time.Second, nil)
	if s.Model != "m" || s.Endpoint != "http://e/v1" || s.Ctx != 123 || s.TokensPerSec != 50 || s.Verify == nil || !s.Verify.Passed {
		t.Fatalf("summary fields wrong: %+v", s)
	}
	path := filepath.Join(t.TempDir(), "status.json")
	if err := writeRunSummaryFile(path, s); err != nil {
		t.Fatalf("writeRunSummaryFile: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got runSummary
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("status file invalid JSON: %v\n%s", err, string(raw))
	}
	if got.Model != "m" || got.Verify == nil || got.Verify.Command != "go test" {
		t.Fatalf("status file summary wrong: %+v", got)
	}
}

func TestBuildRunSummaryFailureCodes(t *testing.T) {
	cfg := config.Config{Model: "m", Endpoint: "http://e/v1", MaxContextTokens: 123}
	cases := []struct {
		name string
		rep  *agent.Report
		err  error
		want string
	}{
		{
			name: "verify failed",
			rep:  &agent.Report{Reason: agent.ReasonAnswered, FinalVerify: agent.VerifyResult{Command: "go test", ExitCode: 1, Stdout: "FAIL first line\nmore"}},
			want: "verify_failed",
		},
		{
			name: "unverified",
			rep:  &agent.Report{Reason: agent.ReasonUnverified},
			want: "unverified",
		},
		{
			name: "model api error",
			rep:  &agent.Report{Reason: agent.ReasonError, Err: &llm.APIError{StatusCode: 503, Body: "model loading"}},
			want: "model_error",
		},
		{
			name: "json invalid",
			rep:  &agent.Report{Reason: agent.ReasonError, Err: errors.New("final assistant answer must be valid JSON only: invalid character")},
			want: "json_invalid",
		},
		{
			name: "context too small",
			rep:  &agent.Report{Reason: agent.ReasonError, Err: agent.ErrWindowTooSmall},
			want: "context_too_small",
		},
		{
			name: "budget",
			rep:  &agent.Report{Reason: agent.ReasonBudgetExceeded, Budget: &agent.BudgetEvidence{Kind: agent.BudgetSteps, Limit: "3", Observed: "4"}},
			want: "budget_exceeded",
		},
		{
			name: "repetition",
			rep:  &agent.Report{Reason: agent.ReasonChurn, Churn: &agent.ChurnEvidence{Kind: agent.ChurnRepeatedCall, Artifact: "read_file repeated"}},
			want: "repetition_halt",
		},
		{
			name: "edit failed",
			rep:  &agent.Report{Reason: agent.ReasonChurn, Churn: &agent.ChurnEvidence{Kind: agent.ChurnEditFailed, Artifact: "edit_file kept failing"}},
			want: "edit_failed",
		},
		{
			name: "interrupted",
			rep:  &agent.Report{Reason: agent.ReasonInterrupted},
			want: "interrupted",
		},
		{
			name: "config error without report",
			err:  errors.New("config: parse profile bad.json: invalid character"),
			want: "config_error",
		},
		{
			name: "nil report internal",
			err:  errors.New("boom"),
			want: "internal_error",
		},
		{
			name: "answered",
			rep:  &agent.Report{Reason: agent.ReasonAnswered},
			want: "answered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := buildRunSummary(cfg, "go test", tc.rep, time.Second, tc.err)
			if s.FailureCode != tc.want {
				t.Fatalf("FailureCode = %q, want %q; detail=%+v", s.FailureCode, tc.want, s.FailureDetail)
			}
			if s.FailureDetail == nil {
				t.Fatalf("FailureDetail missing for %s", tc.want)
			}
			if strings.Contains(fmt.Sprint(s.FailureDetail), "test-token-value") {
				t.Fatalf("failure detail leaked secret: %+v", s.FailureDetail)
			}
		})
	}
}

func TestBuildRunSummaryRepeatedReadFailureDetail(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonChurn,
		Churn: &agent.ChurnEvidence{
			Kind:     agent.ChurnRepeatedCall,
			Class:    "repeated_read_file",
			Tool:     "read_file",
			Artifact: "read_file src/app.ts repeated 6 times with unchanged content",
		},
		ToolCounters: agent.ToolCounters{RepeatedReadFile: 5},
	}
	s := buildRunSummary(config.Config{Model: "m", Endpoint: "http://e/v1", MaxContextTokens: 123}, "", rep, time.Second, nil)
	if s.FailureCode != "repetition_halt" || s.FailureDetail == nil ||
		s.FailureDetail.Class != "repeated_read_file" || s.FailureDetail.Tool != "read_file" {
		t.Fatalf("failure detail wrong: code=%s detail=%+v", s.FailureCode, s.FailureDetail)
	}
	if s.ToolCounters == nil || s.ToolCounters.RepeatedReadFile != 5 {
		t.Fatalf("tool counters wrong: %+v", s.ToolCounters)
	}
}

func TestBuildRunSummarySuccessOmitsFailureCodeAndSerializesCounters(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonSuccess,
		ToolCounters: agent.ToolCounters{
			InvalidToolCalls: 1,
			RepeatedReadFile: 2,
			RepeatedEdits:    3,
			FailedEdits:      4,
			NoOpEdits:        5,
			VerifyAttempts:   6,
			ToolErrors:       7,
		},
	}
	s := buildRunSummary(config.Config{Model: "m"}, "", rep, time.Second, nil)
	if s.FailureCode != "" || s.FailureDetail != nil {
		t.Fatalf("success should omit failure fields, got code=%q detail=%+v", s.FailureCode, s.FailureDetail)
	}
	if s.ToolCounters == nil || s.ToolCounters.InvalidToolCalls != 1 || s.ToolCounters.VerifyAttempts != 6 {
		t.Fatalf("tool counters missing/wrong: %+v", s.ToolCounters)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"tool_counters"`, `"invalid_tool_calls":1`, `"repeated_read_file":2`, `"verify_attempts":6`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("summary JSON missing %s: %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "failure_code") {
		t.Fatalf("success JSON should omit failure_code: %s", raw)
	}
}

func TestBenchmarkSummaryIncludesModeAndZeroCounters(t *testing.T) {
	rep := &agent.Report{Reason: agent.ReasonAnswered}
	s := buildRunSummary(config.Config{Model: "m", BenchmarkMode: true}, "", rep, time.Second, nil)
	if !s.BenchmarkMode {
		t.Fatal("benchmark_mode should be true")
	}
	if s.ToolCounters == nil {
		t.Fatal("benchmark mode should include zero tool_counters")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"benchmark_mode":true`, `"tool_counters"`, `"invalid_tool_calls":0`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("benchmark summary missing %s: %s", want, raw)
		}
	}
}

func TestBenchmarkExitCodeMap(t *testing.T) {
	cases := map[string]int{
		"verify_failed":     benchmarkExitVerify,
		"unverified":        benchmarkExitVerify,
		"model_error":       benchmarkExitModel,
		"tool_call_invalid": benchmarkExitTool,
		"tool_error":        benchmarkExitTool,
		"context_too_small": benchmarkExitContext,
		"repetition_halt":   benchmarkExitRepetition,
		"edit_failed":       benchmarkExitRepetition,
		"json_invalid":      benchmarkExitJSON,
		"budget_exceeded":   benchmarkExitBudget,
		"config_error":      benchmarkExitConfigError,
		"interrupted":       benchmarkExitInterrupted,
		"internal_error":    benchmarkExitInternal,
		"answered":          benchmarkExitAnswered,
		"off_scope_edit":    benchmarkExitScope,
	}
	for code, want := range cases {
		t.Run(code, func(t *testing.T) {
			if got := benchmarkExitCode(runSummary{FailureCode: code}); got != want {
				t.Fatalf("exit = %d, want %d", got, want)
			}
		})
	}
	if got := benchmarkExitCode(runSummary{Success: true}); got != 0 {
		t.Fatalf("success exit = %d, want 0", got)
	}
}

// TestClassifyOffScopeSafetyStop: an A7 safety-stop on an off-scope edit classifies
// as off_scope_edit with source=scope and the denial class/tool/message.
func TestClassifyOffScopeSafetyStop(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonSafetyStop,
		Safety: &agent.SafetyEvidence{
			Rule:    "off-scope-edit",
			Class:   "outside_allow",
			Tool:    "write_file",
			Message: "write denied: README.md is outside the allowed edit scope",
		},
	}
	code, detail := classifyFailure(rep, nil)
	if code != "off_scope_edit" {
		t.Fatalf("code = %q, want off_scope_edit", code)
	}
	if detail.Source != "scope" || detail.Class != "outside_allow" || detail.Tool != "write_file" {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.Message == "" {
		t.Fatal("expected a bounded message naming the path/policy")
	}
}

// TestClassifyReadOnlySafetyStop: a read-only safety-stop carries class read_only.
func TestClassifyReadOnlySafetyStop(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonSafetyStop,
		Safety: &agent.SafetyEvidence{Rule: "read-only-edit", Class: "read_only", Tool: "edit_file", Message: "write denied: tests/x_test.go is read-only"},
	}
	code, detail := classifyFailure(rep, nil)
	if code != "off_scope_edit" || detail.Class != "read_only" {
		t.Fatalf("code=%q detail=%+v, want off_scope_edit/read_only", code, detail)
	}
}

// TestClassifyRepeatedVerifySafetyStop: the repeated-verify stop preserves
// verify_failed as "final failed verify" and maps to repetition_halt with class
// repeated_verify_failure.
func TestClassifyRepeatedVerifySafetyStop(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonSafetyStop,
		Safety: &agent.SafetyEvidence{Rule: "repeated-verify", Class: "repeated_verify_failure", Message: "verify failed 2 times"},
	}
	code, detail := classifyFailure(rep, nil)
	if code != "repetition_halt" || detail.Class != "repeated_verify_failure" {
		t.Fatalf("code=%q detail=%+v, want repetition_halt/repeated_verify_failure", code, detail)
	}
}

// TestClassifyScopeDenialWithoutStopRule: a run that was denied and then ended
// calmly (unverified) is still classified off_scope_edit via LastScopeDenial.
func TestClassifyScopeDenialWithoutStopRule(t *testing.T) {
	rep := &agent.Report{
		Reason:          agent.ReasonUnverified,
		LastScopeDenial: &agent.ScopeDenial{Class: "outside_allow", Tool: "write_file", Message: "write denied: README.md is outside the allowed edit scope"},
	}
	code, detail := classifyFailure(rep, nil)
	if code != "off_scope_edit" || detail.Source != "scope" || detail.Class != "outside_allow" {
		t.Fatalf("code=%q detail=%+v, want off_scope_edit/scope/outside_allow", code, detail)
	}
}

// TestOffScopeCountersInJSON: the off_scope/read_only counters surface in the
// KLOO_RESULT_JSON tool_counters block.
func TestOffScopeCountersInJSON(t *testing.T) {
	cfg := config.Config{Model: "m", Endpoint: "http://x/v1"}
	rep := &agent.Report{
		Reason:       agent.ReasonSafetyStop,
		Safety:       &agent.SafetyEvidence{Rule: "off-scope-edit", Class: "deny", Tool: "edit_file", Message: "denied"},
		ToolCounters: agent.ToolCounters{OffScopeEdits: 2, ReadOnlyEdits: 1},
	}
	var buf bytes.Buffer
	printHeadlessJSON(&buf, cfg, "", rep, time.Second, nil)
	line := strings.TrimPrefix(strings.TrimSpace(buf.String()), "KLOO_RESULT_JSON ")
	var s map[string]any
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if s["failure_code"] != "off_scope_edit" {
		t.Fatalf("failure_code = %v", s["failure_code"])
	}
	tc, _ := s["tool_counters"].(map[string]any)
	if tc == nil || tc["off_scope_edits"].(float64) != 2 || tc["read_only_edits"].(float64) != 1 {
		t.Fatalf("tool_counters = %v", s["tool_counters"])
	}
}

// TestClassifyPatchOnlyRejectCalmEnd: a run that ended calmly (unverified) after a
// patch-only run_command rejection classifies as tool_call_invalid with class
// patch_only_forbidden_tool (the A4 machine-readable contract, spec line 34).
func TestClassifyPatchOnlyRejectCalmEnd(t *testing.T) {
	rep := &agent.Report{
		Reason:          agent.ReasonUnverified,
		PatchOnlyReject: &agent.ToolReject{Tool: "run_command", Class: "patch_only_forbidden_tool", Message: "run_command is disabled in patch-only mode"},
	}
	code, detail := classifyFailure(rep, nil)
	if code != "tool_call_invalid" || detail.Class != "patch_only_forbidden_tool" || detail.Source != "tool" {
		t.Fatalf("code=%q detail=%+v, want tool_call_invalid/patch_only_forbidden_tool/tool", code, detail)
	}
	if detail.Tool != "run_command" {
		t.Fatalf("detail.Tool = %q, want run_command", detail.Tool)
	}
}

// TestClassifyPatchOnlyForbiddenTerminalError: when ErrPatchOnlyForbidden is the
// terminal run error, classifyErrorFailure maps it to tool_call_invalid /
// patch_only_forbidden_tool.
func TestClassifyPatchOnlyForbiddenTerminalError(t *testing.T) {
	err := fmt.Errorf("wrap: %w", tools.ErrPatchOnlyForbidden)
	rep := &agent.Report{Reason: agent.ReasonError, Err: err}
	code, detail := classifyFailure(rep, err)
	if code != "tool_call_invalid" || detail.Class != "patch_only_forbidden_tool" {
		t.Fatalf("code=%q detail=%+v, want tool_call_invalid/patch_only_forbidden_tool", code, detail)
	}
}
