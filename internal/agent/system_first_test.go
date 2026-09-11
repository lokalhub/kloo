package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// Several open-weight chat templates hard-raise on a system message that is not
// the leading one:
//
//	[system, user]        -> 200 OK
//	[user, system, user]  -> 500 Jinja: "System message must be at the beginning."
//
// kloo placed the repo map in a TRAILING system message, which is exactly that
// shape. On a 220-case benchmark Qwen3.8-Flash scored 0/22 at a 10s median --
// no case ever reached the model -- while grok scored 22/22 on the identical
// seat. Anthropic rejects the same shape.
//
// This asserts the wire format directly, because the failure is invisible from
// inside the loop: kloo believed it was sending a valid conversation.
func TestNoSystemMessageAfterTheFirst(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "a.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 100}, &stubChurn{})
	// A repo Root is what produces the tail map at all; without it this test
	// asserts nothing (the loop simply emits no trailing message).
	loop.Root = repoRootWithSource(t)
	loop.ContextTokens = 32000

	if _, err := loop.Run(context.Background(), "fix the thing"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	reqs := srv.ModelCalls()
	if len(reqs) == 0 {
		t.Fatal("no model calls captured; the assertion below would be vacuous")
	}
	// Guard the guard: if no request carries the map, this test proves nothing.
	var sawMap bool
	for _, raw := range reqs {
		if bytes.Contains(raw, []byte("repository map")) || bytes.Contains(raw, []byte("Repository map")) {
			sawMap = true
		}
	}
	if !sawMap {
		t.Fatal("no request carried a repo map; the system-first assertion would be vacuous")
	}
	for n, raw := range reqs {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("request %d: %v", n, err)
		}
		for i, m := range req.Messages {
			if m.Role == "system" && i != 0 {
				t.Errorf("request %d: system message at index %d of %d; templates that "+
					"require system-first reject this outright (content starts %.60q)",
					n, i, len(req.Messages), m.Content)
			}
		}
	}
}

// repoRootWithSource creates a tiny Go repo so the loop actually curates a map.
func repoRootWithSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := "package sample\n\nfunc Alpha() int { return 1 }\n\nfunc Beta(s string) string { return s }\n"
	if err := os.WriteFile(filepath.Join(dir, "sample.go"), []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}
