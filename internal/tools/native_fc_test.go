package tools

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

// toolCallMsg builds an assistant message carrying native tool_calls.
func toolCallMsg(calls ...llm.ToolCall) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, ToolCalls: calls}
}

// fnCall builds one tool_call with JSON-marshalled args.
func fnCall(t *testing.T, id, name string, args map[string]any) llm.ToolCall {
	t.Helper()
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: name, Arguments: string(b)}}
}

func TestNativeBuildRequest(t *testing.T) {
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws)
	req := NativeFCAdapter{}.BuildRequest(llm.ChatRequest{Model: "test-model"}, reg)

	if len(req.Tools) != 9 {
		t.Fatalf("want 9 tools attached, got %d", len(req.Tools))
	}
	// The tools param must serialise to valid OpenAI JSON with name + parameters.
	raw, err := json.Marshal(req.Tools)
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	s := string(raw)
	for _, name := range []string{"read_file", "edit_file", "write_file", "list_dir", "run_command", "finish"} {
		if !strings.Contains(s, `"name":"`+name+`"`) {
			t.Errorf("tools param missing %q: %s", name, s)
		}
	}
	if !strings.Contains(s, `"parameters"`) || !strings.Contains(s, `"properties"`) {
		t.Errorf("tools param missing schema: %s", s)
	}
	if req.ToolChoice != "auto" {
		t.Errorf("tool_choice = %v, want auto", req.ToolChoice)
	}
}

func TestNativeParseSingleCall(t *testing.T) {
	diff := "```\n<<<<<<< SEARCH\nold line\n=======\nnew line\n>>>>>>> REPLACE\n```"
	msg := toolCallMsg(fnCall(t, "c1", "edit_file", map[string]any{"path": "src/app.ts", "diff": diff}))

	call, err := NativeFCAdapter{}.Parse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if call.Name != "edit_file" {
		t.Errorf("name = %q", call.Name)
	}
	if call.Args["path"] != "src/app.ts" {
		t.Errorf("path = %v", call.Args["path"])
	}
	if call.Args["diff"] != diff {
		t.Errorf("fenced diff not preserved:\n got: %q\nwant: %q", call.Args["diff"], diff)
	}
}

func TestNativeParseZeroCalls(t *testing.T) {
	msg := llm.Message{Role: llm.RoleAssistant, Content: "I think I'll just chat."}
	if _, err := (NativeFCAdapter{}).Parse(msg); !errors.Is(err, ErrNoToolCall) {
		t.Errorf("want ErrNoToolCall, got %v", err)
	}
}

func TestNativeParseMultipleCalls(t *testing.T) {
	msg := toolCallMsg(
		fnCall(t, "c1", "read_file", map[string]any{"path": "a"}),
		fnCall(t, "c2", "read_file", map[string]any{"path": "b"}),
	)
	if _, err := (NativeFCAdapter{}).Parse(msg); !errors.Is(err, ErrMultipleToolCalls) {
		t.Errorf("want ErrMultipleToolCalls, got %v", err)
	}
}

func TestNativeParseBadJSONArgs(t *testing.T) {
	msg := toolCallMsg(llm.ToolCall{ID: "c1", Type: "function", Function: llm.FunctionCall{
		Name: "read_file", Arguments: `{"path": this is not json`,
	}})
	if _, err := (NativeFCAdapter{}).Parse(msg); !errors.Is(err, ErrMalformedToolCall) {
		t.Errorf("want ErrMalformedToolCall, got %v", err)
	}
}
