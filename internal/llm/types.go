// Package llm is the hand-rolled OpenAI-compatible LLM client: request/response
// types, a non-streaming Complete, and an SSE Stream over the stdlib (no SSE
// dependency). The JSON tags mirror the OpenAI /v1/chat/completions wire schema
// (snake_case) so requests/responses interoperate with llama.cpp and any other
// OpenAI-compatible endpoint.
package llm

import "strings"

// Role constants for chat messages.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ChatRequest is the body of POST /v1/chat/completions.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float64   `json:"temperature,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
	// StreamOptions opts a streaming request into a final usage chunk. A nil
	// pointer is omitted entirely, so non-streaming / opted-out requests
	// serialize byte-identically to a request without this field. Stream sets it
	// to {IncludeUsage:true} when the caller left it nil; Complete never sets it.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
	Tools         []Tool         `json:"tools,omitempty"`
	// ToolChoice is "auto"/"none"/"required" or a structured object; left as any
	// to match the OpenAI schema's union without over-modelling it for v1.
	ToolChoice any `json:"tool_choice,omitempty"`
	// ResponseFormat carries an optional OpenAI-style response_format (e.g. a
	// json_schema constraint). Set by the optional constrained-decoding layer
	// (internal/tools) only when the endpoint advertises support; omitted
	// otherwise so unconstrained endpoints behave identically.
	ResponseFormat any `json:"response_format,omitempty"`
	// Grammar carries an optional llama.cpp GBNF grammar (a non-standard field
	// llama.cpp accepts). Also set only by the constrained-decoding
	// layer when supported; omitted otherwise.
	Grammar string `json:"grammar,omitempty"`
}

// StreamOptions opts a streaming request into a final usage chunk
// (OpenAI/llama.cpp emit usage on a stream only when asked).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// Message is one chat message (request or response).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// ReasoningContent carries a thinking model's chain-of-thought when the backend
	// exposes it as a separate field (GLM-4.5 / DeepSeek-R1 / Qwen-thinking via
	// llama.cpp/vLLM commonly put the actual output HERE while content is empty).
	// FinalizeReasoning folds it into Content so the loop never treats such a turn as
	// blank. Sent back to the endpoint omitted (it's a response-only field).
	ReasoningContent string `json:"reasoning_content,omitempty"`
	// Name optionally identifies the author (tool name for tool messages, etc.).
	Name string `json:"name,omitempty"`
	// ToolCalls is set on an assistant message that calls tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a tool-result message back to the assistant's call.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// FinalizeReasoning applies the reasoning_content fallback: when Content is blank but
// ReasoningContent is not (a thinking model that emitted its output as reasoning, no
// tool call in content), promote the reasoning to Content so kloo's text-fallback
// parsers see the model's actual output instead of an empty turn. No-op when Content
// already has text.
func (m *Message) FinalizeReasoning() {
	if strings.TrimSpace(m.Content) == "" && strings.TrimSpace(m.ReasoningContent) != "" {
		m.Content = m.ReasoningContent
	}
}

// Tool describes a callable function offered to the model (request side).
type Tool struct {
	Type     string       `json:"type"` // always "function" for v1
	Function ToolFunction `json:"function"`
}

// ToolFunction is the schema of a function tool.
type ToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters is a JSON Schema object; kept as any to avoid modelling the
	// full JSON-Schema surface in v1.
	Parameters any `json:"parameters,omitempty"`
}

// ToolCall is a function call emitted by the model (response side).
type ToolCall struct {
	// Index is present on streaming tool-call deltas (which call is being built).
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"` // "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name + JSON-encoded arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// ChatResponse is the non-streaming response body.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Choice is one completion choice. Message holds the full assistant message for
// non-streaming responses; Delta holds the incremental chunk for streaming
// (populated by the SSE path in task 05).
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message,omitempty"`
	Delta        *Delta  `json:"delta,omitempty"`
	FinishReason string  `json:"finish_reason,omitempty"`
}

// Delta is the incremental content/tool-call fragment in a streaming chunk.
type Delta struct {
	Role             string     `json:"role,omitempty"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"` // thinking models stream CoT here
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
}

// Usage is the token accounting block.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
