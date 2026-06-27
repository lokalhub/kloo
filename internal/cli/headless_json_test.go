package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
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
