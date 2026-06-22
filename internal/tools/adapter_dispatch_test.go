package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// nativeToolCallBody renders a ChatResponse carrying one native tool_call (built
// via the llm types + json.Marshal so the arguments are correctly escaped).
func nativeToolCallBody(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	ab, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	resp := llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{
		Role: llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{
			Name: name, Arguments: string(ab),
		}}},
	}, FinishReason: "tool_calls"}}}
	b, _ := json.Marshal(resp)
	return string(b)
}

// xmlContentBody renders a ChatResponse whose assistant content is an XML block.
func xmlContentBody(t *testing.T, content string) string {
	t.Helper()
	resp := llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{
		Role: llm.RoleAssistant, Content: content,
	}}}}
	b, _ := json.Marshal(resp)
	return string(b)
}

// spyRegistry registers a recording edit_file tool and returns the registry plus
// a pointer to the Call it last received.
func spyRegistry() (*Registry, *Call) {
	var got Call
	reg := NewRegistry()
	reg.Register(&spyTool{name: "edit_file", got: &got, schema: ParamSchema{
		Properties: map[string]Property{"path": {Type: "string"}, "diff": {Type: "string"}},
		Required:   []string{"path", "diff"},
	}})
	return reg, &got
}

// TestNativeAndXMLDispatchSameTool is the Phase-02 DoD "same tool" proof: a
// mocked native-FC reply and a mocked XML reply, encoding the SAME edit_file call
// with the SAME fenced diff, both normalise to the SAME Call and dispatch to the
// SAME handler with identical args.
func TestNativeAndXMLDispatchSameTool(t *testing.T) {
	ctx := context.Background()
	diff := "```\n<<<<<<< SEARCH\nold line\n=======\nnew line\n>>>>>>> REPLACE\n```"
	wantArgs := map[string]any{"path": "src/app.ts", "diff": diff}

	// --- native path through the mocked-LLM harness ---
	nativeSrv := llmtest.Sequence(t, llmtest.Mock{Body: nativeToolCallBody(t, "edit_file", wantArgs)})
	nativeCall, err := ParseWithRetry(ctx, clientFor(nativeSrv), NativeFCAdapter{}, baseReq())
	if err != nil {
		t.Fatalf("native ParseWithRetry: %v", err)
	}

	// --- XML path through the mocked-LLM harness ---
	xmlBlock := "<tool name=\"edit_file\">\n  <arg name=\"path\">src/app.ts</arg>\n  <arg name=\"diff\">\n" + diff + "\n  </arg>\n</tool>"
	xmlSrv := llmtest.Sequence(t, llmtest.Mock{Body: xmlContentBody(t, xmlBlock)})
	xmlCall, err := ParseWithRetry(ctx, clientFor(xmlSrv), XMLAdapter{}, baseReq())
	if err != nil {
		t.Fatalf("xml ParseWithRetry: %v", err)
	}

	// Both adapters normalise to the IDENTICAL Call before dispatch.
	if !reflect.DeepEqual(nativeCall, xmlCall) {
		t.Fatalf("adapters produced different calls:\nnative=%#v\n   xml=%#v", nativeCall, xmlCall)
	}
	if nativeCall.Name != "edit_file" || !reflect.DeepEqual(nativeCall.Args, wantArgs) {
		t.Errorf("call = %#v, want name=edit_file args=%#v", nativeCall, wantArgs)
	}

	// Both dispatch to the SAME handler with the same args (fenced diff intact).
	regN, gotN := spyRegistry()
	if _, err := regN.Dispatch(ctx, nativeCall); err != nil {
		t.Fatalf("dispatch native: %v", err)
	}
	regX, gotX := spyRegistry()
	if _, err := regX.Dispatch(ctx, xmlCall); err != nil {
		t.Fatalf("dispatch xml: %v", err)
	}
	if !reflect.DeepEqual(*gotN, *gotX) {
		t.Errorf("handler received different calls:\nnative=%#v\n   xml=%#v", *gotN, *gotX)
	}
	if gotN.Args["diff"] != diff {
		t.Errorf("fenced diff not preserved into the handler: %q", gotN.Args["diff"])
	}
}

// TestRunCommandDispatchesThroughRegistry is the end-to-end smoke that the
// registered run_command tool dispatches and captures a non-zero exit (full
// security cases live in run_command_test.go).
func TestRunCommandDispatchesThroughRegistry(t *testing.T) {
	skipIfNoSh(t)
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws)

	res, err := reg.Dispatch(context.Background(), Call{
		Name: "run_command",
		Args: map[string]any{"command": "echo hi; exit 2"},
	})
	if err != nil {
		t.Fatalf("dispatch run_command: %v", err)
	}
	if !strings.Contains(res.Output, "hi") {
		t.Errorf("stdout = %q", res.Output)
	}
	if res.ExitCode != 2 {
		t.Errorf("exit = %d, want 2 (the verify signal)", res.ExitCode)
	}
}
