package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// sseToolCall renders an SSE transcript for one native tool call (the headless
// loop streams, so mocks must be event-stream form).
func sseToolCall(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	ab, _ := json.Marshal(args)
	chunk := func(v any) string {
		b, _ := json.Marshal(v)
		return "data: " + string(b) + "\n\n"
	}
	call := chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
		"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function",
			"function": map[string]any{"name": name, "arguments": string(ab)}}},
	}, "finish_reason": nil}}})
	done := chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
		"usage": map[string]any{"total_tokens": 20}})
	return call + done + "data: [DONE]\n\n"
}

// sseText renders an SSE transcript for a plain prose reply (no tool call).
func sseText(t *testing.T, text string) string {
	t.Helper()
	chunk := func(v any) string {
		b, _ := json.Marshal(v)
		return "data: " + string(b) + "\n\n"
	}
	msg := chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
		"role": "assistant", "content": text}, "finish_reason": nil}}})
	done := chunk(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		"usage": map[string]any{"total_tokens": 10}})
	return msg + done + "data: [DONE]\n\n"
}

// scopeWorkspace makes a temp dir with the given files and chdirs into it for the
// duration of the test, so defaultRunHeadless (which uses os.Getwd) runs against it.
func scopeWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	return dir
}

// lastResultJSON extracts and parses the final KLOO_RESULT_JSON line from output.
func lastResultJSON(t *testing.T, out string) map[string]any {
	t.Helper()
	const prefix = "KLOO_RESULT_JSON "
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			line = strings.TrimPrefix(l, prefix)
		}
	}
	if line == "" {
		t.Fatalf("no KLOO_RESULT_JSON line in output:\n%s", out)
	}
	var s map[string]any
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		t.Fatalf("invalid KLOO_RESULT_JSON: %v\n%s", err, line)
	}
	return s
}

// TestHeadlessScopeDeniedRun is the "Scope-denied headless run" screens-to-verify
// flow: `kloo --json --allow src/**` with a mocked model that tries to write
// README.md. The write fails immediately, the file is byte-identical, and the final
// JSON classifies failure_code=off_scope_edit with a bounded detail naming the path.
func TestHeadlessScopeDeniedRun(t *testing.T) {
	dir := scopeWorkspace(t, map[string]string{"README.md": "keep me\n"})
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: sseToolCall(t, "write_file", map[string]any{"path": "README.md", "content": "hacked"}), SSE: true},
		llmtest.Mock{Body: sseText(t, "I could not edit README."), SSE: true},
	)
	cfg := config.Config{
		Endpoint:    srv.URL + "/v1",
		Model:       "test-model",
		ToolFormat:  config.DefaultToolFormat,
		JSONSummary: true,
		ScopeAllow:  []string{"src/**"},
		MaxSteps:    50,
		ChurnRounds: 3,
	}
	var out bytes.Buffer
	_ = defaultRunHeadless(cfg, "edit README", "", lintOpts{Disabled: true}, &out)

	if got, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(got) != "keep me\n" {
		t.Fatalf("README.md mutated by a denied write: %q", string(got))
	}
	s := lastResultJSON(t, out.String())
	if s["failure_code"] != "off_scope_edit" {
		t.Fatalf("failure_code = %v, want off_scope_edit\n%s", s["failure_code"], out.String())
	}
	fd, _ := s["failure_detail"].(map[string]any)
	if fd == nil || fd["source"] != "scope" {
		t.Fatalf("failure_detail = %v, want source=scope", s["failure_detail"])
	}
	if msg, _ := fd["message"].(string); !strings.Contains(msg, "README.md") {
		t.Fatalf("failure_detail.message should name the path, got %q", msg)
	}
	t.Logf("scope-denied headless output:\n%s", out.String())
}

// TestHeadlessScopedShellBypassAttempt is the "Scoped run shell bypass attempt"
// flow: `kloo --json --allow src/**` with a mocked model emitting a run_command
// (sed -i) to mutate README. The model-facing run_command is unavailable/rejected
// before execution and README stays byte-identical.
func TestHeadlessScopedShellBypassAttempt(t *testing.T) {
	dir := scopeWorkspace(t, map[string]string{"README.md": "keep me\n"})
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: sseToolCall(t, "run_command", map[string]any{"command": "sed -i s/keep/gone/ README.md"}), SSE: true},
		llmtest.Mock{Body: sseText(t, "shell is unavailable."), SSE: true},
	)
	cfg := config.Config{
		Endpoint:    srv.URL + "/v1",
		Model:       "test-model",
		ToolFormat:  config.DefaultToolFormat,
		JSONSummary: true,
		ScopeAllow:  []string{"src/**"},
		StopOn:      config.StopPolicy{OffScopeEdit: true},
		MaxSteps:    50,
		ChurnRounds: 3,
	}
	var out bytes.Buffer
	_ = defaultRunHeadless(cfg, "mutate README with shell", "", lintOpts{Disabled: true}, &out)

	if got, _ := os.ReadFile(filepath.Join(dir, "README.md")); string(got) != "keep me\n" {
		t.Fatalf("README.md mutated via shell: %q", string(got))
	}
	s := lastResultJSON(t, out.String())
	if s["failure_code"] != "off_scope_edit" {
		t.Fatalf("failure_code = %v, want off_scope_edit\n%s", s["failure_code"], out.String())
	}
	fd, _ := s["failure_detail"].(map[string]any)
	if fd == nil || fd["class"] != "run_command_disabled_for_scope" {
		t.Fatalf("failure_detail = %v, want class=run_command_disabled_for_scope", s["failure_detail"])
	}
	t.Logf("scoped-shell-bypass headless output:\n%s", out.String())
}

// TestHeadlessPatchOnlyWithholdsShell: --patch-only (no scope) withholds the
// model-facing run_command; a fallback call is rejected as a patch-only error AND
// the final KLOO_RESULT_JSON carries the machine-readable classification the spec
// promises (failure_code:"tool_call_invalid", class:"patch_only_forbidden_tool").
func TestHeadlessPatchOnlyWithholdsShell(t *testing.T) {
	scopeWorkspace(t, map[string]string{"README.md": "keep\n"})
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: sseToolCall(t, "run_command", map[string]any{"command": "echo hi"}), SSE: true},
		llmtest.Mock{Body: sseToolCall(t, "finish", map[string]any{"summary": "cannot run shell"}), SSE: true},
	)
	cfg := config.Config{
		Endpoint:    srv.URL + "/v1",
		Model:       "test-model",
		ToolFormat:  config.DefaultToolFormat,
		JSONSummary: true,
		PatchOnly:   true,
		MaxSteps:    50,
		ChurnRounds: 3,
	}
	var out bytes.Buffer
	_ = defaultRunHeadless(cfg, "run a command", "", lintOpts{Disabled: true}, &out)
	if !strings.Contains(out.String(), "patch-only") {
		t.Fatalf("expected a patch-only rejection in output:\n%s", out.String())
	}
	s := lastResultJSON(t, out.String())
	if s["failure_code"] != "tool_call_invalid" {
		t.Fatalf("failure_code = %v, want tool_call_invalid\n%s", s["failure_code"], out.String())
	}
	fd, _ := s["failure_detail"].(map[string]any)
	if fd == nil || fd["class"] != "patch_only_forbidden_tool" {
		t.Fatalf("failure_detail = %v, want class=patch_only_forbidden_tool", s["failure_detail"])
	}
	t.Logf("patch-only headless output:\n%s", out.String())
}
