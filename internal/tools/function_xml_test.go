package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

func TestStripToolCallTail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"clean diff untouched", "<<<<<<< SEARCH\nfoo\n=======\nbar\n>>>>>>> REPLACE", "<<<<<<< SEARCH\nfoo\n=======\nbar\n>>>>>>> REPLACE"},
		{"the observed leak", "export class AppsPageModule {}\n>>>>>>> REPLACE\n</parameter>\n</function>\n</tool_call>\n<tool_call>\n<function=edit_file>", "export class AppsPageModule {}\n>>>>>>> REPLACE"},
		{"closing function tag", "content here</function>", "content here"},
		{"next param leaked", "value one<parameter=next>value two", "value one"},
		{"no markup", "just a path", "just a path"},
	}
	for _, c := range cases {
		if got := stripToolCallTail(c.in); got != c.want {
			t.Errorf("%s: stripToolCallTail(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestExtractFunctionCallsSingle(t *testing.T) {
	content := "I'll edit the file.\n<tool_call>\n<function=edit_file>\n<parameter=path>src/app.ts</parameter>\n<parameter=content>hello world</parameter>\n</function>\n</tool_call>"
	calls := extractFunctionCalls(content)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Name != "edit_file" {
		t.Errorf("name = %q, want edit_file", calls[0].Name)
	}
	if calls[0].Args["path"] != "src/app.ts" || calls[0].Args["content"] != "hello world" {
		t.Errorf("args = %+v", calls[0].Args)
	}
}

func TestExtractFunctionCallsBatched(t *testing.T) {
	// Two calls in one reply — the parser must yield BOTH, cleanly separated.
	content := "<function=edit_file>\n<parameter=path>a.ts</parameter>\n<parameter=content>aaa</parameter>\n</function>\n" +
		"<function=edit_file>\n<parameter=path>b.ts</parameter>\n<parameter=content>bbb</parameter>\n</function>"
	calls := extractFunctionCalls(content)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Args["path"] != "a.ts" || calls[1].Args["path"] != "b.ts" {
		t.Errorf("paths = %q, %q; want a.ts, b.ts", calls[0].Args["path"], calls[1].Args["path"])
	}
	// The first call's content must NOT have swallowed the second call.
	if c := calls[0].Args["content"].(string); strings.Contains(c, "function") || strings.Contains(c, "b.ts") {
		t.Errorf("first call's content leaked the next call: %q", c)
	}
}

func TestExtractFunctionCallsMisclosedParameter(t *testing.T) {
	// The model never closed <parameter=content>; the next call's opener must bound
	// the value instead of being absorbed into it.
	content := "<function=edit_file>\n<parameter=path>a.ts</parameter>\n<parameter=content>line1\nline2\n" +
		"<function=read_file>\n<parameter=path>b.ts</parameter>\n</function>"
	calls := extractFunctionCalls(content)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2 (mis-closed param must not eat the next call)", len(calls))
	}
	if got := calls[0].Args["content"].(string); got != "line1\nline2" {
		t.Errorf("content = %q, want bounded %q", got, "line1\nline2")
	}
	if calls[1].Name != "read_file" || calls[1].Args["path"] != "b.ts" {
		t.Errorf("second call = %+v, want read_file b.ts", calls[1])
	}
}

// TestNativeFCSanitisesLeakedStructuredArgs is the regression for the live bug:
// the endpoint parsed the edit into a STRUCTURED tool_call, but the model's
// mis-closed parameter folded the next call's markup into the content arg. The
// adapter must strip that tail so the applied edit + rendered card are clean.
func TestNativeFCSanitisesLeakedStructuredArgs(t *testing.T) {
	leaked := "<<<<<<< SEARCH\nold\n=======\nnew\n>>>>>>> REPLACE\n</parameter>\n</function>\n</tool_call>\n<tool_call>\n<function=edit_file>\n<parameter=path>next.ts"
	argsJSON, _ := json.Marshal(map[string]any{"path": "apps.module.ts", "diff": leaked})
	msg := llm.Message{
		Role:      llm.RoleAssistant,
		ToolCalls: []llm.ToolCall{{Type: "function", Function: llm.FunctionCall{Name: "edit_file", Arguments: string(argsJSON)}}},
	}
	calls, err := NativeFCAdapter{}.ParseAll(msg)
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	diff := calls[0].Args["diff"].(string)
	if strings.Contains(diff, "function") || strings.Contains(diff, "tool_call") || strings.Contains(diff, "next.ts") {
		t.Errorf("structured arg still carries leaked markup: %q", diff)
	}
	if want := "<<<<<<< SEARCH\nold\n=======\nnew\n>>>>>>> REPLACE"; diff != want {
		t.Errorf("diff = %q, want %q", diff, want)
	}
}

// TestNativeFCParsesFunctionDialectAsText: when the endpoint returns NO native
// tool_calls and the call is in the <function=…> dialect as text, the adapter's
// fallback recovers it.
func TestNativeFCParsesFunctionDialectAsText(t *testing.T) {
	msg := llm.Message{
		Role:    llm.RoleAssistant,
		Content: "Sure.\n<tool_call>\n<function=read_file>\n<parameter=path>README.md</parameter>\n</function>\n</tool_call>",
	}
	calls, err := NativeFCAdapter{}.ParseAll(msg)
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "read_file" || calls[0].Args["path"] != "README.md" {
		t.Fatalf("got %+v, want one read_file{path:README.md}", calls)
	}
}
