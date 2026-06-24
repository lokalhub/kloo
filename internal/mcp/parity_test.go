package mcp

// parity_test.go proves the central design claim: an MCP tool is treated
// IDENTICALLY to a builtin by all three tool-call paths (native_fc, xml_fallback,
// json-in-text) — with NO source changes to internal/tools or internal/agent.
// (Chosen package: internal/mcp, because mcpTool is unexported and the test must
// build a registry containing a real in-memory-backed mcpTool. Recorded in
// decisions.md.)

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

func hasToolNamed(ts []llm.Tool, name string) bool {
	for _, t := range ts {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

// TestAdapterParityMCPToolSameCall: a registry mixing a builtin and an MCP tool
// builds valid requests through native_fc and xml_fallback, and native/xml/json
// replies naming the MCP tool all parse to the SAME Call, which dispatches to the
// MCP tool.
func TestAdapterParityMCPToolSameCall(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))

	reg := tools.NewRegistry()
	reg.Register(stubBuiltin{"list_dir"}) // a builtin alongside the MCP tool
	registerTools(reg, c, []string{"echo"})

	const mcpName = "srv__echo"
	wantArgs := map[string]any{"text": "parity"}

	// --- native_fc: BuildRequest carries BOTH tools; a native reply parses. ---
	nreq := tools.NativeFCAdapter{}.BuildRequest(llm.ChatRequest{}, reg)
	if !hasToolNamed(nreq.Tools, mcpName) {
		t.Errorf("native BuildRequest is missing the MCP tool %q", mcpName)
	}
	if !hasToolNamed(nreq.Tools, "list_dir") {
		t.Errorf("native BuildRequest is missing the builtin")
	}
	nativeMsg := llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
		ID: "c1", Type: "function",
		Function: llm.FunctionCall{Name: mcpName, Arguments: `{"text":"parity"}`},
	}}}
	nativeCall, err := tools.NativeFCAdapter{}.Parse(nativeMsg)
	if err != nil {
		t.Fatalf("native Parse: %v", err)
	}

	// --- xml_fallback: the grammar prompt advertises it; an XML reply parses. ---
	xreq := tools.XMLAdapter{}.BuildRequest(llm.ChatRequest{}, reg)
	if len(xreq.Messages) == 0 || !strings.Contains(xreq.Messages[0].Content, mcpName) {
		t.Errorf("xml BuildRequest prompt does not advertise the MCP tool %q", mcpName)
	}
	xmlMsg := llm.Message{Role: llm.RoleAssistant, Content: "<tool name=\"srv__echo\">\n  <arg name=\"text\">parity</arg>\n</tool>"}
	xmlCall, err := tools.XMLAdapter{}.Parse(xmlMsg)
	if err != nil {
		t.Fatalf("xml Parse: %v", err)
	}

	// --- json-in-text: a JSON tool call in content is recovered by native Parse. ---
	jsonMsg := llm.Message{Role: llm.RoleAssistant, Content: `Sure: {"name":"srv__echo","arguments":{"text":"parity"}}`}
	jsonCall, err := tools.NativeFCAdapter{}.Parse(jsonMsg)
	if err != nil {
		t.Fatalf("json-text Parse: %v", err)
	}

	// All three paths normalise to the IDENTICAL Call.
	if !reflect.DeepEqual(nativeCall, xmlCall) {
		t.Errorf("native vs xml differ:\nnative=%#v\n   xml=%#v", nativeCall, xmlCall)
	}
	if !reflect.DeepEqual(nativeCall, jsonCall) {
		t.Errorf("native vs json-text differ:\nnative=%#v\n  json=%#v", nativeCall, jsonCall)
	}
	if nativeCall.Name != mcpName || !reflect.DeepEqual(nativeCall.Args, wantArgs) {
		t.Errorf("call = %#v, want name=%s args=%#v", nativeCall, mcpName, wantArgs)
	}

	// And the Call dispatches to the MCP tool end-to-end (echoes the text).
	res, err := reg.Dispatch(ctx, nativeCall)
	if err != nil {
		t.Fatalf("dispatch MCP call: %v", err)
	}
	if res.Output != "parity" {
		t.Errorf("dispatched MCP tool Output = %q, want parity", res.Output)
	}
}
