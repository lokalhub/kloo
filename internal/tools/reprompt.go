package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/lokal/kloo/internal/llm"
)

// ErrToolCallUnrecoverable is returned when both the first reply and the single
// corrective re-prompt fail to parse into a valid tool call. It wraps both
// attempts' diagnostics. There is never a third attempt (design doc §8
// anti-spiral).
var ErrToolCallUnrecoverable = errors.New("tools: tool call unrecoverable after one re-prompt")

// ParseWithRetry asks the model for a tool call and is forgiving exactly once:
// if the first reply fails to parse, it appends a structured corrective message
// (restating the expected format) and tries ONE more time. A second failure
// surfaces ErrToolCallUnrecoverable — no third request, no loop. The re-prompt
// count is per turn (this call), not cumulative across the run; run-level
// budgets/churn are Phase 04's concern.
//
// A transport error from the client (not a parse error) is returned as-is.
func ParseWithRetry(ctx context.Context, client llm.LLMClient, adapter ToolAdapter, req llm.ChatRequest) (Call, error) {
	resp, err := client.Complete(ctx, req)
	if err != nil {
		return Call{}, err
	}
	msg := assistantMessage(resp)

	call, parseErr := adapter.Parse(msg)
	if parseErr == nil {
		return call, nil
	}

	// One corrective re-prompt: show the model its bad reply, then the correction.
	req.Messages = append(req.Messages,
		msg,
		llm.Message{Role: llm.RoleUser, Content: adapter.Corrective(parseErr)},
	)

	resp2, err := client.Complete(ctx, req)
	if err != nil {
		return Call{}, err
	}
	call2, parseErr2 := adapter.Parse(assistantMessage(resp2))
	if parseErr2 == nil {
		return call2, nil
	}

	return Call{}, fmt.Errorf("tools: first attempt: %v; after re-prompt: %v: %w", parseErr, parseErr2, ErrToolCallUnrecoverable)
}

// assistantMessage extracts the assistant message from a chat response (empty
// message if there are no choices).
func assistantMessage(resp llm.ChatResponse) llm.Message {
	if len(resp.Choices) == 0 {
		return llm.Message{Role: llm.RoleAssistant}
	}
	return resp.Choices[0].Message
}
