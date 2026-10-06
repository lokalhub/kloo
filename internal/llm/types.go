// Package llm is the hand-rolled OpenAI-compatible LLM client: request/response
// types, a non-streaming Complete, and an SSE Stream over the stdlib (no SSE
// dependency). The JSON tags mirror the OpenAI /v1/chat/completions wire schema
// (snake_case) so requests/responses interoperate with llama.cpp and any other
// OpenAI-compatible endpoint.
package llm

import (
	"encoding/json"
	"strings"
)

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
	// ReasoningEffort is the OpenAI-compatible thinking control. kloo sets
	// "none" only when --no-think/profile noThink is configured.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// MaxTokens bounds the COMPLETION for this one request (the OpenAI max_tokens).
	// Omitted when 0, which was kloo's behaviour for its whole history: it reserved
	// 20% of the window for the reply (memory.go usableWindowFrac) and then never
	// told the server about that reserve. The server filled in its own default —
	// usually the rest of the window — so "prompt + max_tokens" could exceed the
	// per-request limit before a single token was generated, which is the 400 that
	// killed two kloo-bench cases at ctx 131072.
	MaxTokens int `json:"max_tokens,omitempty"`
}

// StreamOptions opts a streaming request into a final usage chunk
// (OpenAI/llama.cpp emit usage on a stream only when asked).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ModelInfo is one model advertised by GET /v1/models.
type ModelInfo struct {
	ID            string
	ContextLength int
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
	// RawContent/RawReasoningContent/FinishReason preserve the original response
	// shape before FinalizeReasoning promotes reasoning into Content. They are
	// response metadata for the agent and are never sent back to endpoints.
	RawContent          string `json:"-"`
	RawReasoningContent string `json:"-"`
	FinishReason        string `json:"-"`
	// Name optionally identifies the author (tool name for tool messages, etc.).
	Name string `json:"name,omitempty"`
	// ToolCalls is set on an assistant message that calls tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a tool-result message back to the assistant's call.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// CacheControl, when set, asks the provider to cache the prompt UP TO AND
	// INCLUDING this message (a "breakpoint"). It is tagged "-" because the field
	// never serializes as a message key: MarshalJSON instead rewrites this one
	// message's content into the OpenAI-compatible content-parts form that carries
	// cache_control. nil ⇒ the message serializes exactly as it always has, which
	// is what keeps every endpoint that has never heard of this field unaffected.
	CacheControl *CacheControl `json:"-"`
	// Images carries image attachments as data URLs ("data:image/png;base64,…") or
	// http(s) URLs. Tagged "-" for the same reason as CacheControl: it never
	// serializes as a message key. MarshalJSON instead rewrites the message into the
	// OpenAI-compatible content-parts form, one {"type":"image_url"} part per image
	// after the text part, so a message with no images is byte-identical to what it
	// has always been.
	//
	// It is a separate field rather than text because an image inlined into Content
	// is a megabyte of base64 the model reads as gibberish, counts as tokens, and
	// re-sends every single turn — which is the opposite of everything the working
	// set and prompt-cache work is for.
	Images []string `json:"-"`
}

// CacheControl is the prompt-cache breakpoint marker. "ephemeral" is the only
// type the OpenAI-compatible providers define.
type CacheControl struct {
	Type string `json:"type"`
}

// CacheControlEphemeral is the marker to attach to a breakpoint message.
func CacheControlEphemeral() *CacheControl { return &CacheControl{Type: "ephemeral"} }

// contentPart is one element of the content-parts array form. Only a marked
// message is ever serialized this way.
type contentPart struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	ImageURL     *imageURL     `json:"image_url,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// imageURL is the OpenAI vision image part's payload. "detail" is left off: the
// providers that honour it all default to "auto", and the ones that don't reject
// unknown keys.
type imageURL struct {
	URL string `json:"url"`
}

// markedMessage mirrors Message's serializable fields in the SAME key order, with
// content as a parts array. Declared separately rather than reusing Message so the
// unmarked path stays byte-for-byte what it was.
type markedMessage struct {
	Role             string        `json:"role"`
	Content          []contentPart `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	Name             string        `json:"name,omitempty"`
	ToolCalls        []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
}

// MarshalJSON emits the plain OpenAI message shape unless this message carries a
// cache breakpoint, in which case its content becomes a single-element parts array
// carrying cache_control. An UNMARKED message is byte-identical to the pre-cache
// serialization — that identity is the whole "never break other providers"
// guarantee, and TestRequestBodyUnchangedWhenCacheOff pins it against a golden
// captured from v0.16.7.
func (m Message) MarshalJSON() ([]byte, error) {
	// plain has no methods, so marshalling it cannot recurse back into this one.
	type plain Message
	if m.CacheControl == nil && len(m.Images) == 0 {
		return json.Marshal(plain(m))
	}
	// Text first, then one image part each. The cache breakpoint rides the TEXT part
	// when there is one, so an image attachment cannot move where the prefix cache
	// is cut.
	parts := []contentPart{{Type: "text", Text: m.Content, CacheControl: m.CacheControl}}
	for _, img := range m.Images {
		if img == "" {
			continue
		}
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: img}})
	}
	return json.Marshal(markedMessage{
		Role:             m.Role,
		Content:          parts,
		ReasoningContent: m.ReasoningContent,
		Name:             m.Name,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
	})
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
//
// The cache fields matter for hosted providers, which bill a re-sent prompt
// PREFIX at a steep discount when it is byte-identical to a previous request.
// kloo used to drop them on the floor, so it could not tell whether its prompt
// layout was cache-friendly or was paying full price every turn. Providers do
// not agree on a shape, so both known ones are decoded and normalised by
// CachedPromptTokens.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// DeepSeek-style: an explicit hit/miss split of the prompt.
	PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens int `json:"prompt_cache_miss_tokens,omitempty"`
	// OpenAI-style: a nested details block. Pointer so an absent block stays
	// distinguishable from a reported zero.
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetails is the OpenAI-style nested prompt breakdown.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// CachedPromptTokens is how many prompt tokens the provider served from its
// cache this request, normalised across the shapes above. 0 when the provider
// reports nothing — which is indistinguishable from a genuine miss, so callers
// should treat it as "no discount observed" rather than proof of a miss.
func (u Usage) CachedPromptTokens() int {
	if u.PromptCacheHitTokens > 0 {
		return u.PromptCacheHitTokens
	}
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.CachedTokens
	}
	return 0
}
