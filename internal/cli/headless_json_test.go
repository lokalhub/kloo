package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
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
