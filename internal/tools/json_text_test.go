package tools

import "testing"

// TestExtractJSONToolCallsVariants: the inline-JSON parser accepts both the OpenAI
// shape {"name","arguments"} and the {"tool","args"} variant some local models emit
// (snappy did this on a conversational turn, which used to parse as "no tool call"
// → re-prompt → ERROR).
func TestExtractJSONToolCallsVariants(t *testing.T) {
	cases := []struct {
		name, content, wantTool, wantPath string
	}{
		{"openai-shape", `prose {"name":"read_file","arguments":{"path":"a.go"}}`, "read_file", "a.go"},
		{"tool-args-variant", `Sure! {"tool":"read","args":{"path":"README.md"}}`, "read", "README.md"},
	}
	for _, c := range cases {
		calls := extractJSONToolCalls(c.content)
		if len(calls) != 1 {
			t.Fatalf("%s: got %d calls, want 1 (%+v)", c.name, len(calls), calls)
		}
		if calls[0].Name != c.wantTool {
			t.Errorf("%s: name = %q, want %q", c.name, calls[0].Name, c.wantTool)
		}
		if p, _ := calls[0].Args["path"].(string); p != c.wantPath {
			t.Errorf("%s: args[path] = %q, want %q", c.name, p, c.wantPath)
		}
	}
}
