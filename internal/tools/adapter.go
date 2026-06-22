package tools

import "github.com/lokal/kloo/internal/llm"

// ToolAdapter is the model-agnostic seam between the loop and however a given
// model expresses tool calls. Both the native function-calling adapter and the
// XML-tag fallback implement it, so the loop (and the re-prompt rail) are
// adapter-agnostic: they call BuildRequest to prepare the turn and Parse to
// normalise the reply to a single dispatchable Call.
type ToolAdapter interface {
	// BuildRequest prepares the chat request for this adapter — the native
	// adapter attaches the OpenAI tools param; the XML adapter injects a
	// grammar-teaching system prompt and leaves tools unset.
	BuildRequest(base llm.ChatRequest, reg *Registry) llm.ChatRequest

	// Parse normalises the assistant message to exactly one dispatchable Call,
	// or returns one of the one-tool-per-turn / malformed sentinels for the
	// re-prompt rail to act on. (Parse == ExactlyOneCall(ParseAll(msg)).)
	Parse(msg llm.Message) (Call, error)

	// ParseAll returns every tool call in the message, in order (empty if none).
	// It errors only on a malformed call (e.g. invalid JSON / broken XML), NOT
	// on the count — so a caller that wants the one-tool-per-turn rail uses Parse
	// while a caller that reduces-to-first (the agent loop) uses ParseAll.
	ParseAll(msg llm.Message) ([]Call, error)

	// Corrective returns the body of the single corrective re-prompt to send
	// when Parse failed with parseErr (restating the expected format).
	Corrective(parseErr error) string
}
