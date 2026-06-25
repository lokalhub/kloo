package llm

import (
	"context"
	"testing"
	"time"
)

// TestNormalizeMessagesMergesSameRole: consecutive same-role messages merge so the
// request never ends with two assistant turns (the llama.cpp 400 case).
func TestNormalizeMessagesMergesSameRole(t *testing.T) {
	in := []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "task"},
		{Role: RoleAssistant, Content: "let me check"},
		{Role: RoleAssistant, Content: "and edit"}, // two trailing assistants → must merge
	}
	got := normalizeMessages(in)
	if len(got) != 3 {
		t.Fatalf("want 3 messages (merged), got %d: %+v", len(got), got)
	}
	if got[2].Role != RoleAssistant || got[2].Content != "let me check\nand edit" {
		t.Errorf("merged assistant = %+v", got[2])
	}
	// No two adjacent same-role messages remain.
	for i := 1; i < len(got); i++ {
		if got[i].Role == got[i-1].Role && got[i].Role != RoleSystem {
			t.Errorf("adjacent same-role at %d: %s", i, got[i].Role)
		}
	}
}

// stalledBody is a reader that blocks forever (a stalled stream: connected, silent).
type stalledBody struct{ ctx context.Context }

func (b stalledBody) Read(p []byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }

// TestParseSSEStallAbortsViaContext: when the context is cancelled (the idle
// watchdog's mechanism), parseSSE returns promptly rather than blocking forever.
func TestStreamIdleTimeoutAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { time.Sleep(60 * time.Millisecond); cancel() }() // simulate the idle watchdog firing
	start := time.Now()
	_, err := parseSSE(ctx, stalledBody{ctx: ctx}, nil)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("stalled stream did not abort promptly: %s", time.Since(start))
	}
	if err == nil {
		t.Error("expected an error from a cancelled/stalled stream")
	}
}
