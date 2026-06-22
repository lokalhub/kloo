package tools

import (
	"encoding/json"
	"fmt"

	"github.com/lokal/kloo/internal/llm"
)

// NativeFCAdapter is the primary tool-call path: it serialises the registry into
// the OpenAI tools param and parses a native tool_calls reply. This is the path
// snappy/smart use when run with --jinja (design doc §2). It implements
// ToolAdapter so the loop is adapter-agnostic.
type NativeFCAdapter struct{}

// BuildRequest attaches the registry's tools (as OpenAI function specs) and asks
// the model to make a tool call. Temperature is left to the caller (the loop
// keeps it low); one-tool-per-turn is enforced at Parse, not here.
func (NativeFCAdapter) BuildRequest(base llm.ChatRequest, reg *Registry) llm.ChatRequest {
	for _, t := range reg.Tools() {
		base.Tools = append(base.Tools, llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  t.Schema().JSONSchema(),
			},
		})
	}
	if base.ToolChoice == nil {
		base.ToolChoice = "auto"
	}
	return base
}

// ParseAll reads msg.ToolCalls and JSON-unmarshals each function.arguments into
// a Call. Invalid JSON args → ErrMalformedToolCall. The count is not enforced
// here (zero/many are returned as-is).
func (NativeFCAdapter) ParseAll(msg llm.Message) ([]Call, error) {
	calls := make([]Call, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		args := map[string]any{}
		if a := tc.Function.Arguments; a != "" {
			if err := json.Unmarshal([]byte(a), &args); err != nil {
				return nil, fmt.Errorf("native: tool_call %q has invalid JSON args: %w", tc.Function.Name, ErrMalformedToolCall)
			}
		}
		calls = append(calls, Call{Name: tc.Function.Name, Args: args})
	}
	// Fallback for models without native function-calling in their template
	// (e.g. Qwen2.5-Coder): they emit the call as JSON in the text content. Only
	// consult the text when no native tool_calls were present, so well-behaved
	// models are unaffected.
	if len(calls) == 0 && msg.Content != "" {
		calls = append(calls, extractJSONToolCalls(msg.Content)...)
	}
	return calls, nil
}

// Parse enforces exactly one call (the one-tool-per-turn rail). Zero/many →
// the one-tool-per-turn sentinels (which the re-prompt rail turns into a single
// corrective re-prompt).
func (a NativeFCAdapter) Parse(msg llm.Message) (Call, error) {
	calls, err := a.ParseAll(msg)
	if err != nil {
		return Call{}, err
	}
	return ExactlyOneCall(calls)
}

// Corrective restates the native-FC contract for the one corrective re-prompt.
func (NativeFCAdapter) Corrective(parseErr error) string {
	return "Your previous reply did not contain a single valid tool call. " +
		"Respond with exactly ONE tool_call, choosing one of the available tools, " +
		"with valid JSON arguments matching that tool's parameters. Do not emit zero " +
		"or multiple tool calls, and do not put the call in prose."
}
