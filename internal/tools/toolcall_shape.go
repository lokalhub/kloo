package tools

import "github.com/lokalhub/kloo/internal/llm"

// LooksLikeToolCall reports whether an assistant message is an attempted tool
// call in ANY dialect kloo knows — native tool_calls, text JSON, the
// <function=…>/<parameter=…> dialect, the DSML invoke/parameter dialect, the
// <invoke_name>/<parameters> dialect — or merely LOOKS like one (a marker is
// present but nothing parsed).
//
// It exists for callers outside the normal tool-parsing path that must not
// mistake an action for prose. The conversational gate (internal/agent chatGate)
// is the first: it runs one NO-TOOLS call and treats any reply that is not the
// TASK sentinel as the user's answer. A model told "answer, or say TASK" often
// answers by WRITING THE CALL anyway, and the gate printed that payload to the
// user and stopped the run at step 0 — kloo announcing work it never did.
//
// It is deliberately generous: a false positive only sends the turn into the
// agent loop, which is where actionable work belongs anyway.
func LooksLikeToolCall(msg llm.Message) bool {
	if len(msg.ToolCalls) > 0 {
		return true
	}
	if looksLikeToolCall(msg.Content) {
		return true
	}
	return len(extractJSONToolCalls(msg.Content)) > 0 ||
		len(extractFunctionCalls(msg.Content)) > 0 ||
		len(extractInvokeToolCalls(msg.Content)) > 0 ||
		len(extractInvokeNameToolCalls(msg.Content)) > 0
}
