package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// toolCallStream renders an SSE transcript for a single native tool call, the
// shape the headless loop streams (conventions/testing.md Pattern 2).
func toolCallStream(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	chunk := func(v any) string {
		b, _ := json.Marshal(v)
		return "data: " + string(b) + "\n\n"
	}
	call := chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
		"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
			"function": map[string]any{"name": name, "arguments": string(raw)}}},
	}, "finish_reason": nil}}})
	done := chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
		"usage": map[string]any{"total_tokens": 40}})
	return call + done + "data: [DONE]\n\n"
}

// readHeavyRun drives a headless run of three reads followed by one applied write,
// against a verify command that only passes once the write has landed. It returns
// the parsed KLOO_RESULT_JSON object and the full run output.
func readHeavyRun(t *testing.T) (map[string]any, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "answer.txt"), []byte("wrong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	read := llmtest.Mock{Body: toolCallStream(t, "read_file", map[string]any{"path": "answer.txt"}), SSE: true}
	srv := llmtest.Sequence(t,
		read, read, read,
		llmtest.Mock{Body: toolCallStream(t, "write_file", map[string]any{"path": "answer.txt", "content": "right\n"}), SSE: true},
	)
	cfg := config.Config{
		Endpoint: srv.URL + "/v1", Model: "test-model", ToolFormat: config.DefaultToolFormat,
		Effort: config.DefaultEffort, MaxSteps: 10, ChurnRounds: 10,
		MaxContextTokens: config.DefaultMaxContextTokens, JSONSummary: true,
	}
	var out strings.Builder
	if err := defaultRunHeadless(cfg, "make the check pass", "grep -qx right answer.txt", lintOpts{}, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}

	var line string
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "KLOO_RESULT_JSON ") {
			line = strings.TrimPrefix(l, "KLOO_RESULT_JSON ")
		}
	}
	if line == "" {
		t.Fatalf("no KLOO_RESULT_JSON line:\n%s", out.String())
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(line), &summary); err != nil {
		t.Fatalf("invalid summary JSON: %v", err)
	}
	return summary, out.String()
}

func intField(t *testing.T, m map[string]any, path ...string) int {
	t.Helper()
	cur := any(m)
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("%s: not an object", strings.Join(path, "."))
		}
		cur = obj[k]
	}
	f, ok := cur.(float64)
	if !ok {
		t.Fatalf("%s missing or not a number: %v", strings.Join(path, "."), cur)
	}
	return int(f)
}

// TestVerifyAttemptsLessThanStepsInSummary: the gate's observable effect, end to
// end through the headless JSON — a read-heavy run reports fewer verify attempts
// than steps. Before the gate these were equal on essentially every run
// (verify_attempts >= steps in 78 of 83 benchmark runs).
func TestVerifyAttemptsLessThanStepsInSummary(t *testing.T) {
	summary, out := readHeavyRun(t)
	steps := intField(t, summary, "steps")
	attempts := intField(t, summary, "tool_counters", "verify_attempts")

	if attempts >= steps {
		t.Errorf("verify_attempts = %d, steps = %d — want attempts < steps on a read-heavy run\n%s",
			attempts, steps, out)
	}
	if attempts != 1 {
		t.Errorf("verify_attempts = %d, want exactly 1 (three reads skip; the write triggers one)", attempts)
	}
	if steps != 4 {
		t.Errorf("steps = %d, want 4", steps)
	}
}

// TestHeadlessJSONVerifyAttemptsBelowSteps: the same run, asserted as the S4 e2e
// condition — fewer verifies than steps, a real success, and a green final verify.
func TestHeadlessJSONVerifyAttemptsBelowSteps(t *testing.T) {
	summary, out := readHeavyRun(t)

	if intField(t, summary, "tool_counters", "verify_attempts") >= intField(t, summary, "steps") {
		t.Errorf("want verify_attempts < steps:\n%s", out)
	}
	if summary["success"] != true {
		t.Errorf("success = %v, want true — the gate must not cost the run its success", summary["success"])
	}
	verify, ok := summary["verify"].(map[string]any)
	if !ok || verify["passed"] != true {
		t.Errorf("verify.passed = %v, want true", summary["verify"])
	}
}
