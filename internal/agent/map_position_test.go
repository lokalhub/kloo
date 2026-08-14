package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// sentMessages returns the role/content pairs of the first captured request.
func sentMessages(t *testing.T, srv *llmtest.Server) []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
} {
	t.Helper()
	reqs := srv.Requests()
	if len(reqs) == 0 {
		t.Fatal("no request captured")
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqs[0], &body); err != nil {
		t.Fatalf("request not JSON: %v", err)
	}
	return body.Messages
}

// TestMapPositionTailKeepsSystemStable is the caching property: with the map at
// the tail, the SYSTEM message is exactly the static system prompt. That message
// is the prompt's prefix, so keeping the volatile map out of it is what lets a
// provider serve the prefix from cache instead of re-charging for it every turn.
func TestMapPositionTailKeepsSystemStable(t *testing.T) {
	root := genRepo(t, 14, 12)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "file00.go"}})})
	loop := memLoop(t, srv, root, 2000, NewWorkingMemory()) // MapPosition unset ⇒ tail
	loop.System = "you are kloo"

	if _, err := loop.Run(context.Background(), "look at File3Func2DoesSomething"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := sentMessages(t, srv)

	if msgs[0].Role != llm.RoleSystem {
		t.Fatalf("first message role = %q, want system", msgs[0].Role)
	}
	if msgs[0].Content != "you are kloo" {
		t.Errorf("system message carries more than the static prompt — the cacheable prefix is polluted:\n%q", msgs[0].Content)
	}
	last := msgs[len(msgs)-1]
	if !strings.Contains(last.Content, repoMapHeader) {
		t.Errorf("last message is not the repo map: %q", truncate(last.Content))
	}
	if last.Role != llm.RoleSystem {
		t.Errorf("trailing map role = %q, want system", last.Role)
	}
}

// TestMapPositionSystemIsTheLegacyLayout: the escape hatch restores the original
// shape for an endpoint that rejects a non-leading system message.
func TestMapPositionSystemIsTheLegacyLayout(t *testing.T) {
	root := genRepo(t, 14, 12)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "file00.go"}})})
	loop := memLoop(t, srv, root, 2000, NewWorkingMemory())
	loop.System = "you are kloo"
	loop.MapPosition = MapPositionSystem

	if _, err := loop.Run(context.Background(), "look at File3Func2DoesSomething"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	msgs := sentMessages(t, srv)

	if !strings.Contains(msgs[0].Content, repoMapHeader) {
		t.Error("legacy layout should carry the map inside the system prompt")
	}
	for _, m := range msgs[1:] {
		if strings.Contains(m.Content, repoMapHeader) {
			t.Error("legacy layout must not also emit a trailing map message")
		}
	}
}

// TestMapPlacementDoesNotChangeTokenBudget: placement is a caching concern only.
// The memory assembler must charge for the map wherever it sits, or moving it
// would silently hand the history a bigger budget than the window allows.
func TestMapPlacementDoesNotChangeTokenBudget(t *testing.T) {
	root := genRepo(t, 14, 12)
	const ctxTokens = 2000
	task := "look at File3Func2DoesSomething"

	budgets := map[string]int{}
	for _, pos := range []string{MapPositionTail, MapPositionSystem} {
		wm := NewWorkingMemory()
		srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "file00.go"}})})
		loop := memLoop(t, srv, root, ctxTokens, wm)
		loop.System = "you are kloo"
		loop.MapPosition = pos
		if _, err := loop.Run(context.Background(), task); err != nil {
			t.Fatalf("Run(%s): %v", pos, err)
		}
		budgets[pos] = wm.Stats().PromptTokens
	}
	// Not exact equality: ApproxTokens is chars/4, so summing two separately
	// rounded sections differs from rounding one concatenated string (plus the
	// "\n\n" join) by a token or two. What must hold is that the map is CHARGED
	// for either way — a real placement leak would be the map's full size.
	delta := budgets[MapPositionTail] - budgets[MapPositionSystem]
	if delta < 0 {
		delta = -delta
	}
	if delta > 4 {
		t.Errorf("projected prompt differs by placement: tail=%d system=%d (delta %d) — the map must be charged for either way",
			budgets[MapPositionTail], budgets[MapPositionSystem], delta)
	}
}

func truncate(s string) string {
	if len(s) > 120 {
		return s[:120] + "…"
	}
	return s
}
