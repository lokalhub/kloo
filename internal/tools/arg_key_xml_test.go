package tools

import (
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

// liveGLMFinish is the payload captured from glm-5.3-flash through a logging
// proxy on 2026-10-09, byte-for-byte apart from a shortened summary. Before the
// extractor it cost three corrective re-prompts and ended the run as
// `malformed-tool-call` — with the model's work already done.
const liveGLMFinish = `<tool_call>finish<arg_key>summary</arg_key><arg_value>Explained the directory contents: six template-generated Python modules. No changes were made, per the user's request.</arg_value></tool_call>`

func TestExtractArgKeyDialect(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantName string
		wantArgs map[string]string
	}{
		{
			name:     "the live finish call",
			content:  liveGLMFinish,
			wantName: "finish",
			wantArgs: map[string]string{"summary": "Explained the directory contents: six template-generated Python modules. No changes were made, per the user's request."},
		},
		{
			name:     "newlines between every part",
			content:  "<tool_call>list_dir\n<arg_key>path</arg_key>\n<arg_value>personal/apps</arg_value>\n</tool_call>",
			wantName: "list_dir",
			wantArgs: map[string]string{"path": "personal/apps"},
		},
		{
			name:     "prose before the call",
			content:  "I'll read that file.\n<tool_call>read_file<arg_key>path</arg_key><arg_value>go.mod</arg_value></tool_call>",
			wantName: "read_file",
			wantArgs: map[string]string{"path": "go.mod"},
		},
		{
			name:     "several arguments",
			content:  "<tool_call>run_command<arg_key>command</arg_key><arg_value>go test ./...</arg_value><arg_key>timeout</arg_key><arg_value>60</arg_value></tool_call>",
			wantName: "run_command",
			wantArgs: map[string]string{"command": "go test ./...", "timeout": "60"},
		},
		{
			// A cut-off stream. Discarding the call loses the run; keeping what
			// arrived at least lets the tool report a real argument error.
			name:     "truncated final value",
			content:  "<tool_call>finish<arg_key>summary</arg_key><arg_value>did the thing",
			wantName: "finish",
			wantArgs: map[string]string{"summary": "did the thing"},
		},
		{
			// The value is often code or a diff: interior whitespace is CONTENT.
			name:     "value keeps its interior whitespace",
			content:  "<tool_call>write_file<arg_key>path</arg_key><arg_value>a.py</arg_value><arg_key>content</arg_key><arg_value>\ndef f():\n    return 1\n</arg_value></tool_call>",
			wantName: "write_file",
			wantArgs: map[string]string{"path": "a.py", "content": "def f():\n    return 1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls, err := NativeFCAdapter{}.ParseAll(llm.Message{Role: llm.RoleAssistant, Content: tc.content})
			if err != nil {
				t.Fatalf("ParseAll: %v", err)
			}
			if len(calls) != 1 {
				t.Fatalf("calls = %d, want 1: %+v", len(calls), calls)
			}
			if calls[0].Name != tc.wantName {
				t.Errorf("name = %q, want %q", calls[0].Name, tc.wantName)
			}
			for k, want := range tc.wantArgs {
				got, _ := calls[0].Args[k].(string)
				if got != want {
					t.Errorf("args[%q] = %q, want %q", k, got, want)
				}
			}
			if len(calls[0].Args) != len(tc.wantArgs) {
				t.Errorf("args = %v, want exactly %v", calls[0].Args, tc.wantArgs)
			}
		})
	}
}

// TestArgKeyDialectBatchesSeparately: two calls in one reply must not merge, or
// one call's arguments silently execute under the other's name.
func TestArgKeyDialectBatchesSeparately(t *testing.T) {
	content := "<tool_call>list_dir<arg_key>path</arg_key><arg_value>a</arg_value></tool_call>" +
		"<tool_call>read_file<arg_key>path</arg_key><arg_value>b</arg_value></tool_call>"
	calls, err := NativeFCAdapter{}.ParseAll(llm.Message{Role: llm.RoleAssistant, Content: content})
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %d, want 2: %+v", len(calls), calls)
	}
	if calls[0].Name != "list_dir" || calls[1].Name != "read_file" {
		t.Fatalf("names = %q/%q", calls[0].Name, calls[1].Name)
	}
	if calls[0].Args["path"] != "a" || calls[1].Args["path"] != "b" {
		t.Errorf("arguments crossed between calls: %v / %v", calls[0].Args, calls[1].Args)
	}
}

// TestArgKeyDialectLeavesProseAlone. The one-tool-per-turn rail and the
// conversational-reply path both depend on prose parsing to ZERO calls: a reply
// that merely mentions the markup must not become a call to a tool named after
// whatever preceded it.
func TestArgKeyDialectLeavesProseAlone(t *testing.T) {
	for _, content := range []string{
		"The model emits its calls as <tool_call> text, which kloo could not read.",
		"<tool_call>run this for me please<arg_key>command</arg_key><arg_value>ls</arg_value></tool_call>",
		"<tool_call><arg_key>path</arg_key><arg_value>a.txt</arg_value></tool_call>",
	} {
		if got := extractArgKeyToolCalls(content); len(got) != 0 {
			t.Errorf("content %q became %+v, want no calls", content, got)
		}
	}
}

// TestNativeDialectsStillWin: the GLM extractor is last in the chain, so a reply
// carrying a dialect an earlier extractor handles must still be parsed by that
// one. Guards against the new extractor shadowing the four before it.
func TestNativeDialectsStillWin(t *testing.T) {
	invoke := "<tool_call>\n<invoke_name>list_dir</invoke_name>\n<parameters><path>x</path></parameters>\n</tool_call>"
	calls, err := NativeFCAdapter{}.ParseAll(llm.Message{Role: llm.RoleAssistant, Content: invoke})
	if err != nil || len(calls) != 1 || calls[0].Name != "list_dir" {
		t.Fatalf("invoke_name dialect regressed: calls=%+v err=%v", calls, err)
	}
	if p, _ := calls[0].Args["path"].(string); p != "x" {
		t.Errorf("args = %v, want path=x", calls[0].Args)
	}
}

// TestUnparseableCallStillFailsLoudly: the loud-failure path must survive. A
// reply that is clearly attempting a call but matches NO dialect still has to
// reach the corrective re-prompt, not pass for a conversational answer.
func TestUnparseableCallStillFailsLoudly(t *testing.T) {
	_, err := NativeFCAdapter{}.ParseAll(llm.Message{Role: llm.RoleAssistant,
		Content: "<tool_call>\n{ this is not any dialect at all"})
	if err == nil || !strings.Contains(err.Error(), "unparseable tool call") {
		t.Fatalf("err = %v, want the loud unparseable-call error", err)
	}
}

// TestWrapperlessArgKeyFailsLoudly: the arg pairs with no <tool_call> wrapper
// carry no recoverable tool name. That must reach the corrective re-prompt, not
// pass for a conversational answer — the silent-no-op shape this whole family of
// extractors exists to prevent.
func TestWrapperlessArgKeyFailsLoudly(t *testing.T) {
	_, err := NativeFCAdapter{}.ParseAll(llm.Message{Role: llm.RoleAssistant,
		Content: "<arg_key>path</arg_key><arg_value>a.txt</arg_value>"})
	if err == nil || !strings.Contains(err.Error(), "unparseable tool call") {
		t.Fatalf("err = %v, want the loud unparseable-call error", err)
	}
}
