package agent

import (
	"context"
	"testing"

	"github.com/lokal/kloo/internal/llm"
	"github.com/lokal/kloo/internal/llm/llmtest"
	"github.com/lokal/kloo/internal/repomap"
)

// TestEstimateUsageFillsZero: when a turn reports zero usage, estimateUsage
// substitutes the ApproxTokens estimate of the turn's prompt + completion; when
// the server reports usage, it is returned verbatim (server wins).
func TestEstimateUsageFillsZero(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: "you are kloo"},
		{Role: llm.RoleUser, Content: "do the thing"},
	}
	msg := llm.Message{
		Role:      llm.RoleAssistant,
		Content:   "ok",
		ToolCalls: []llm.ToolCall{{Function: llm.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}},
	}

	wantPrompt := repomap.ApproxTokens("you are kloo") + repomap.ApproxTokens("do the thing")
	wantCompletion := repomap.ApproxTokens("ok") + repomap.ApproxTokens("read_file") + repomap.ApproxTokens(`{"path":"a.go"}`)

	got := estimateUsage(llm.Usage{}, msgs, msg)
	if got.TotalTokens == 0 {
		t.Fatal("estimate must never leave TotalTokens at zero")
	}
	if got.TotalTokens != wantPrompt+wantCompletion {
		t.Errorf("estimate total = %d, want %d", got.TotalTokens, wantPrompt+wantCompletion)
	}
	if got.PromptTokens != wantPrompt || got.CompletionTokens != wantCompletion {
		t.Errorf("estimate split = %+v, want prompt %d / completion %d", got, wantPrompt, wantCompletion)
	}

	// Server-reported usage is authoritative — returned unchanged.
	server := llm.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}
	if got := estimateUsage(server, msgs, msg); got != server {
		t.Errorf("server usage must pass through unchanged, got %+v", got)
	}
}

// TestLoopEstimatesWhenServerOmitsUsage: a run whose model response reports zero
// usage still produces a non-zero TokensUsed via the estimate fallback.
func TestLoopEstimatesWhenServerOmitsUsage(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 0, tcSpec{"read_file", map[string]any{"path": "a.go"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.TokensUsed <= 0 {
		t.Errorf("TokensUsed = %d, want > 0 (estimate should fill a zero-usage turn)", rep.TokensUsed)
	}
}

// TestLoopUsesServerUsage: when the server reports usage, the run's TokensUsed is
// exactly that value — the estimate is not applied.
func TestLoopUsesServerUsage(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 1410, tcSpec{"read_file", map[string]any{"path": "a.go"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.TokensUsed != 1410 {
		t.Errorf("TokensUsed = %d, want 1410 (server value, not an estimate)", rep.TokensUsed)
	}
}

// TestLoopTokensMonotonic: across a multi-turn run whose server omits usage, the
// per-turn progress token total is monotonically non-decreasing and ends > 0.
func TestLoopTokensMonotonic(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 0, tcSpec{"read_file", map[string]any{"path": "a.go"}})})
	// fail, fail, pass → three turns of token accrual.
	loop, _ := newLoop(t, srv,
		&stubVerifier{results: []VerifyResult{failResult(), failResult(), passResult()}},
		&stubBudget{}, &stubChurn{})

	var seen []int
	loop.OnProgress = func(step, maxSteps, tokens, maxTokens int) { seen = append(seen, tokens) }

	rep, err := loop.Run(context.Background(), "fix it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess || rep.Steps != 3 {
		t.Fatalf("want 3-step success, got %s in %d steps", rep.Reason, rep.Steps)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] < seen[i-1] {
			t.Errorf("token total decreased: %v", seen)
		}
	}
	if rep.TokensUsed <= 0 {
		t.Errorf("final TokensUsed = %d, want > 0", rep.TokensUsed)
	}
}

// TestLoopEstimatesOnStreamingPath: the estimate fallback also fires on the
// streaming transport (OnDelta set ⇒ Stream), where a usage-less transcript
// yields zero server usage.
func TestLoopEstimatesOnStreamingPath(t *testing.T) {
	srv := llmtest.SSE(t, llmtest.ReadTranscript(t, "..", "llm", "testdata", "sse", "tool-call.stream"))
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{}, &stubChurn{})
	loop.OnDelta = func(string) {} // force the streaming path

	rep, err := loop.Run(context.Background(), "read main.go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.TokensUsed <= 0 {
		t.Errorf("streaming run TokensUsed = %d, want > 0 (estimate fallback)", rep.TokensUsed)
	}
}
