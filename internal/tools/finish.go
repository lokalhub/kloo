package tools

import "context"

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
	return "End the run. Call this when the task is complete and verify reflects it, OR when the user's request was a question you have now answered. Provide a short final summary. Do NOT keep running redundant read-only commands once there is nothing left to change."
}

func (finishTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"summary": {Type: "string", Description: "A short final answer or summary of what was done / what the state is."},
		},
		Required: []string{"summary"},
	}
}

func (finishTool) Invoke(ctx context.Context, c Call) (Result, error) {
	s, _ := argString(c.Args, "summary")
	return Result{Output: s}, nil
}
