package tools

import (
	"context"
	"strings"
)

// NameFinish is the explicit completion tool. The model calls it to end the run
// with a final answer/summary instead of spinning on redundant read-only commands
// when there is nothing left to do — e.g. a question it has now answered, or a task
// whose verify already reflects the desired state. A small local model rarely emits
// a tool-free turn (the other way to end), so without this it tacks on echo/ls every
// turn and loops to the budget ceiling.
//
// The loop INTERCEPTS this call as a terminal stop (loop.go); Invoke is a harmless
// echo so the registry and every adapter advertise it like any other tool.
const NameFinish = "finish"

type finishTool struct{}

func (finishTool) Name() string { return NameFinish }
func (finishTool) Description() string {
	return "End the run. Call this when the task is complete and verify reflects it, OR when the user's request was a question you have now answered. The summary is shown to the user as your reply, so if the request was a question, put the ANSWER there. Do NOT keep running redundant read-only commands once there is nothing left to change."
}

func (finishTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"summary": {Type: "string", Description: "Your reply to the user — this text is what they see. If the request was a question, give the full answer itself (the findings, the list, the explanation), NOT a description of the steps you took. If the request was an edit, say what changed and what the state is."},
		},
		Required: []string{"summary"},
	}
}

func (finishTool) Invoke(ctx context.Context, c Call) (Result, error) {
	return Result{Output: FinishSummary(c.Args)}, nil
}

// finishSummaryKeys are the argument names a model actually uses for the finish
// text, in preference order. "summary" is the schema's name and wins; the rest are
// observed substitutes. Qwen3-Next emits
//
//	<tool_call><function=finish><parameter=message>…</parameter></function></tool_call>
//
// and a model that renames the only required argument is not making an error worth
// punishing — the run is over either way, and the sole question is whether the user
// sees the reply or an empty box. Reading only "summary" silently discarded it,
// which is the same user-visible failure as the dropped Report.Summary field.
var finishSummaryKeys = []string{
	"summary", "message", "answer", "text", "content", "result", "response", "final_answer",
}

// FinishSummary extracts the model's closing text from a finish call's arguments,
// tolerating the argument-name variants in finishSummaryKeys. Returns "" when none
// carries a non-empty string.
func FinishSummary(args map[string]any) string {
	for _, k := range finishSummaryKeys {
		if s, ok := argString(args, k); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
