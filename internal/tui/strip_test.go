package tui

import "testing"

func TestStripToolCallSyntax(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"json-inline", `Next, apps.page.html: {"name": "edit_file", "arguments": {"diff": "x"}}`, "Next, apps.page.html:"},
		{"tool_call-wrapper", `<tool_call>{"name":"run_command","arguments":{"command":"ls"}}</tool_call>done`, "done"},
		{"function-wrapper", `<function=run><parameter=command>go run main.go</parameter></function>`, ""},
		{"params-key", `{"name":"x","parameters":{"a":1}}`, ""},
		{"tool-args-variant", `Here you go: {"tool":"read","args":{"path":"README.md"}}`, "Here you go:"},
		{"plain-prose", "just prose, no tools", "just prose, no tools"},
		{"non-tool-json-kept", `keep {"foo": 1} non-tool json`, `keep {"foo": 1} non-tool json`},
	}
	for _, c := range cases {
		if got := stripToolCallSyntax(c.in); got != c.want {
			t.Errorf("%s: strip(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestCleanAssistantText(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"partial-streaming-json", `I'll edit it. {"name": "edit_file", "arguments": {"diff": "incomplete`, "I'll edit it."},
		{"complete-stripped", `Done {"name":"run_command","arguments":{"command":"ls"}}`, "Done"},
		{"tool-args-variant", `I listed the dir. {"tool":"read","args":{"path":"README.md"}}`, "I listed the dir."},
		// glued to the sentence end + pretty-printed (the real snappy case): "…in.{\n  \"tool\"…"
		{"glued-multiline", "…interested in.{\n  \"tool\": \"read\",\n  \"args\": {\n    \"path\": \"README.md\"\n  }\n}", "…interested in."},
		{"partial-xml", `Next: <function=run><parameter=command>go`, "Next:"},
		{"plain-prose", "just prose", "just prose"},
	}
	for _, c := range cases {
		if got := cleanAssistantText(c.in); got != c.want {
			t.Errorf("%s: clean(%q)=%q want %q", c.name, c.in, got, c.want)
		}
	}
}
