package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// Three chat completions (one per check) plus the /v1/models listing the
	// context report reads — the listing is last and carries no body.
	reqs := srv.Requests()
	if n := len(reqs); n != 4 {
		t.Fatalf("requests = %d, want 4 (3 checks + models listing)", n)
	}
	for i, body := range reqs[:3] {
		if len(body) == 0 {
			t.Fatalf("check request %d had an empty body", i)
		}
	}
	if len(reqs[3]) != 0 {
		t.Fatalf("last request should be the GET models listing, got body %q", reqs[3])
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

// fakeModelLister stands in for GET /v1/models so the context report is testable
// without an endpoint.
type fakeModelLister struct {
	models []llm.ModelInfo
	err    error
}

func (f fakeModelLister) Models(context.Context) ([]llm.ModelInfo, error) {
	return f.models, f.err
}

// The case the report exists for: the endpoint serves a much larger window than
// kloo is configured to use, which is otherwise invisible until a run
// over-compacts.
func TestProbeContextReportsEndpointWindowLargerThanConfigured(t *testing.T) {
	cfg := config.Config{Model: "dsv4", MaxContextTokens: 8000}
	lister := fakeModelLister{models: []llm.ModelInfo{
		{ID: "other", ContextLength: 4096},
		{ID: "dsv4", ContextLength: 32768},
	}}

	got := probeContextWindow(context.Background(), cfg, lister)

	if got.Source != "endpoint" {
		t.Fatalf("source = %q, want endpoint", got.Source)
	}
	if got.Advertised != 32768 || got.Configured != 8000 {
		t.Fatalf("advertised/configured = %d/%d, want 32768/8000", got.Advertised, got.Configured)
	}
	if !strings.Contains(got.Message, "raise --ctx") {
		t.Fatalf("message should tell the user what to do, got %q", got.Message)
	}
}

// A listing that carries no context length is the silent-8k-fallback case.
func TestProbeContextReportsMissingContextLength(t *testing.T) {
	cfg := config.Config{Model: "local", MaxContextTokens: 8000}
	lister := fakeModelLister{models: []llm.ModelInfo{{ID: "local"}}}

	got := probeContextWindow(context.Background(), cfg, lister)

	if got.Source != "not-advertised" {
		t.Fatalf("source = %q, want not-advertised", got.Source)
	}
	if got.Advertised != 0 {
		t.Fatalf("advertised = %d, want 0", got.Advertised)
	}
	if got.Message == "" {
		t.Fatal("a silent fallback needs an explicit message")
	}
}

func TestProbeContextReportsModelNotListed(t *testing.T) {
	cfg := config.Config{Model: "dsv4", MaxContextTokens: 8000}
	lister := fakeModelLister{models: []llm.ModelInfo{{ID: "something-else", ContextLength: 4096}}}

	got := probeContextWindow(context.Background(), cfg, lister)

	if got.Source != "not-listed" {
		t.Fatalf("source = %q, want not-listed", got.Source)
	}
}

// An endpoint without /v1/models is common and fine — the check reports, never
// gates.
func TestProbeContextTolerantWhenListingUnavailable(t *testing.T) {
	cfg := config.Config{Model: "local", MaxContextTokens: 8000}
	lister := fakeModelLister{err: errors.New("connection refused")}

	got := probeContextWindow(context.Background(), cfg, lister)

	if got.Source != "unavailable" {
		t.Fatalf("source = %q, want unavailable", got.Source)
	}
	if got.Configured != 8000 {
		t.Fatalf("configured = %d, want 8000 even when the listing fails", got.Configured)
	}
	if !strings.Contains(got.Message, "connection refused") {
		t.Fatalf("message should carry the cause, got %q", got.Message)
	}
}

// Matching the advertised window exactly must not produce a "raise --ctx" nudge.
func TestProbeContextSilentWhenConfiguredMatchesAdvertised(t *testing.T) {
	cfg := config.Config{Model: "dsv4", MaxContextTokens: 32768}
	lister := fakeModelLister{models: []llm.ModelInfo{{ID: "dsv4", ContextLength: 32768}}}

	got := probeContextWindow(context.Background(), cfg, lister)

	if got.Message != "" {
		t.Fatalf("no advice expected when the window matches, got %q", got.Message)
	}
}
