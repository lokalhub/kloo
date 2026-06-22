package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// writeAnswerStream renders an SSE transcript for a native write_file tool call
// (the headless loop streams, so the mock must reply in event-stream form).
func writeAnswerStream(t *testing.T, content string) string {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"path": "answer.txt", "content": content})
	chunk := func(v any) string {
		b, _ := json.Marshal(v)
		return "data: " + string(b) + "\n\n"
	}
	call := chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
		"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
			"function": map[string]any{"name": "write_file", "arguments": string(args)}}},
	}, "finish_reason": nil}}})
	done := chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
		"usage": map[string]any{"total_tokens": 40}})
	return call + done + "data: [DONE]\n\n"
}

// C1 (full headless run): the per-step `tokens N/M` lines (the §9 curve source)
// still appear, working memory is wired (loop.Memory set), and a short run that
// never compacts omits the `compactions` line — byte-identical to pre-P00.
func TestHeadlessRunSurfacesTokensAndOmitsCompactionsWhenZero(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "answer.txt"), []byte("wrong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// defaultRunHeadless resolves the workspace from the cwd (go1.22: no t.Chdir).
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeAnswerStream(t, "right\n"), SSE: true})
	cfg := config.Config{
		Endpoint: srv.URL + "/v1", Model: "test-model", ToolFormat: config.DefaultToolFormat,
		MaxSteps: 10, ChurnRounds: 10, MaxContextTokens: 8000, MaxTokens: 200000, MaxWallClockSeconds: 600,
	}

	var out strings.Builder
	if err := defaultRunHeadless(cfg, "make the check pass", "grep -qx right answer.txt", &out); err != nil {
		t.Fatalf("headless run did not succeed: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "tokens ") {
		t.Errorf("per-step token lines (the §9 curve) must still appear:\n%s", got)
	}
	if !strings.Contains(got, "SUCCESS") {
		t.Errorf("the mocked fix should drive the run to success:\n%s", got)
	}
	if strings.Contains(got, "compactions") {
		t.Errorf("a short run that never compacts must omit the compactions line:\n%s", got)
	}
}

// C1 (report rendering): the `compactions: N` line is printed only when N > 0,
// mirroring the optional budget/churn lines so the zero case is unchanged.
func TestHeadlessReportCompactionsLineOnlyWhenPositive(t *testing.T) {
	base := &agent.Report{Reason: agent.ReasonSuccess, Steps: 3, TokensUsed: 100,
		FinalVerify: agent.VerifyResult{Command: "go test", Passed: true}}

	var zero strings.Builder
	printHeadlessReport(&zero, base, time.Second)
	if strings.Contains(zero.String(), "compactions") {
		t.Errorf("N=0 must omit the compactions line:\n%s", zero.String())
	}

	withN := *base
	withN.Compactions = 3
	var three strings.Builder
	printHeadlessReport(&three, &withN, time.Second)
	if !strings.Contains(three.String(), "compactions: 3") {
		t.Errorf("N>0 must print `compactions: 3`:\n%s", three.String())
	}
}
