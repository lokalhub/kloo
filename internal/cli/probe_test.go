package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

func probeToolResp(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	ab, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	resp := llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{
		Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{
			Name: name, Arguments: string(ab),
		}}},
	}}}}
	b, _ := json.Marshal(resp)
	return string(b)
}

func probeTextResp(t *testing.T, text string) string {
	t.Helper()
	resp := llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, Content: text}}}}
	b, _ := json.Marshal(resp)
	return string(b)
}

func probeMalformedToolResp(t *testing.T, name string) string {
	t.Helper()
	resp := llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{
		Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{
			Name: name, Arguments: "{not-json",
		}}},
	}}}}
	b, _ := json.Marshal(resp)
	return string(b)
}

func TestProbePassesAndCleansTempWorkspace(t *testing.T) {
	diff := "<<<<<<< SEARCH\nbefore\n=======\nafter\n>>>>>>> REPLACE\n"
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: probeToolResp(t, "list_dir", map[string]any{"path": "."})},
		llmtest.Mock{Body: probeToolResp(t, "edit_file", map[string]any{"path": "probe.txt", "diff": diff})},
		llmtest.Mock{Body: probeTextResp(t, `{"ok":true}`)},
	)
	res := runProbe(t.Context(), config.Config{
		Endpoint:   srv.URL + "/v1",
		Model:      "probe-model",
		ToolFormat: config.DefaultToolFormat,
	})
	if !res.OK || !res.Checks.ToolCall.OK || !res.Checks.FileEdit.OK || !res.Checks.JSONOnly.OK {
		t.Fatalf("probe should pass, got %+v", res)
	}
	if !res.TempWorkspaceClean {
		t.Fatalf("temp workspace should be removed: %+v", res)
	}
	if n := len(srv.Requests()); n != 3 {
		t.Fatalf("requests = %d, want 3", n)
	}
	var out bytes.Buffer
	writeProbeHuman(&out, res)
	for _, want := range []string{"kloo probe", "model: probe-model", "tool_call PASS", "file_edit PASS", "json_only PASS", "overall: PASS"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("human output missing %q:\n%s", want, out.String())
		}
	}
}

func TestProbeClassifiesFailures(t *testing.T) {
	t.Run("malformed tool call", func(t *testing.T) {
		diff := "<<<<<<< SEARCH\nbefore\n=======\nafter\n>>>>>>> REPLACE\n"
		srv := llmtest.Sequence(t,
			llmtest.Mock{Body: probeMalformedToolResp(t, "list_dir")},
			llmtest.Mock{Body: probeToolResp(t, "edit_file", map[string]any{"path": "probe.txt", "diff": diff})},
			llmtest.Mock{Body: probeTextResp(t, `{"ok":true}`)},
		)
		res := runProbe(t.Context(), config.Config{Endpoint: srv.URL + "/v1", Model: "m", ToolFormat: config.DefaultToolFormat})
		if res.OK || res.Checks.ToolCall.FailureCode != "tool_call_invalid" {
			t.Fatalf("malformed tool call not classified: %+v", res)
		}
	})
	t.Run("prose only response", func(t *testing.T) {
		diff := "<<<<<<< SEARCH\nbefore\n=======\nafter\n>>>>>>> REPLACE\n"
		srv := llmtest.Sequence(t,
			llmtest.Mock{Body: probeTextResp(t, "I can list files, but I will not call a tool.")},
			llmtest.Mock{Body: probeToolResp(t, "edit_file", map[string]any{"path": "probe.txt", "diff": diff})},
			llmtest.Mock{Body: probeTextResp(t, `{"ok":true}`)},
		)
		res := runProbe(t.Context(), config.Config{Endpoint: srv.URL + "/v1", Model: "m", ToolFormat: config.DefaultToolFormat})
		if res.OK || res.Checks.ToolCall.FailureCode != "tool_call_invalid" {
			t.Fatalf("prose-only response not classified: %+v", res)
		}
	})
	t.Run("failed edit", func(t *testing.T) {
		srv := llmtest.Sequence(t,
			llmtest.Mock{Body: probeToolResp(t, "list_dir", map[string]any{"path": "."})},
			llmtest.Mock{Body: probeToolResp(t, "write_file", map[string]any{"path": "probe.txt", "content": "before\n"})},
			llmtest.Mock{Body: probeTextResp(t, `{"ok":true}`)},
		)
		res := runProbe(t.Context(), config.Config{Endpoint: srv.URL + "/v1", Model: "m", ToolFormat: config.DefaultToolFormat})
		if res.OK || res.Checks.FileEdit.FailureCode != "edit_failed" {
			t.Fatalf("failed edit not classified: %+v", res)
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		diff := "<<<<<<< SEARCH\nbefore\n=======\nafter\n>>>>>>> REPLACE\n"
		srv := llmtest.Sequence(t,
			llmtest.Mock{Body: probeToolResp(t, "list_dir", map[string]any{"path": "."})},
			llmtest.Mock{Body: probeToolResp(t, "edit_file", map[string]any{"path": "probe.txt", "diff": diff})},
			llmtest.Mock{Body: probeTextResp(t, "not json")},
		)
		res := runProbe(t.Context(), config.Config{Endpoint: srv.URL + "/v1", Model: "m", ToolFormat: config.DefaultToolFormat})
		if res.OK || res.Checks.JSONOnly.FailureCode != "json_invalid" {
			t.Fatalf("json failure not classified: %+v", res)
		}
	})
	t.Run("model error", func(t *testing.T) {
		srv := llmtest.Status(t, 503, `{"error":"loading"}`)
		res := runProbe(t.Context(), config.Config{Endpoint: srv.URL + "/v1", Model: "m", ToolFormat: config.DefaultToolFormat})
		if res.OK || res.Checks.ToolCall.FailureCode != "model_error" {
			t.Fatalf("model failure not classified: %+v", res)
		}
	})
}
