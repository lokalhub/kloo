package tools

import (
	"errors"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

func xmlMsg(content string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: content}
}

func TestXMLParseSimpleArgs(t *testing.T) {
	msg := xmlMsg(`<tool name="read_file">
  <arg name="path">internal/app.go</arg>
</tool>`)
	call, err := XMLAdapter{}.Parse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if call.Name != "read_file" {
		t.Errorf("name = %q", call.Name)
	}
	if call.Args["path"] != "internal/app.go" {
		t.Errorf("path = %v", call.Args["path"])
	}
}

func TestXMLPreservesFencedDiffVerbatim(t *testing.T) {
	diff := "```\n<<<<<<< SEARCH\n    return 1\n=======\n    return 2\n>>>>>>> REPLACE\n```"
	msg := xmlMsg("Here is my edit:\n\n<tool name=\"edit_file\">\n  <arg name=\"path\">a.go</arg>\n  <arg name=\"diff\">\n" + diff + "\n  </arg>\n</tool>\n\nDone.")

	call, err := XMLAdapter{}.Parse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if call.Args["path"] != "a.go" {
		t.Errorf("path = %v", call.Args["path"])
	}
	if call.Args["diff"] != diff {
		t.Errorf("fenced diff not byte-preserved:\n got: %q\nwant: %q", call.Args["diff"], diff)
	}
	// Indentation inside the fence must survive.
	if !strings.Contains(call.Args["diff"].(string), "    return 1") {
		t.Errorf("indentation lost: %q", call.Args["diff"])
	}
}

func TestXMLProseTolerated(t *testing.T) {
	msg := xmlMsg("Sure, let me read that file for you.\n\n<tool name=\"read_file\">\n  <arg name=\"path\">x</arg>\n</tool>\n\nThat will show the contents.")
	call, err := XMLAdapter{}.Parse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if call.Name != "read_file" || call.Args["path"] != "x" {
		t.Errorf("call = %+v", call)
	}
}

func TestXMLZeroToolBlocks(t *testing.T) {
	if _, err := (XMLAdapter{}).Parse(xmlMsg("Just thinking out loud, no tool yet.")); !errors.Is(err, ErrNoToolCall) {
		t.Errorf("want ErrNoToolCall, got nil/other")
	}
}

func TestXMLMultipleToolBlocks(t *testing.T) {
	msg := xmlMsg(`<tool name="read_file"><arg name="path">a</arg></tool>
<tool name="read_file"><arg name="path">b</arg></tool>`)
	if _, err := (XMLAdapter{}).Parse(msg); !errors.Is(err, ErrMultipleToolCalls) {
		t.Errorf("want ErrMultipleToolCalls, got other")
	}
}

func TestXMLMalformed(t *testing.T) {
	cases := map[string]string{
		"unclosed tool":    `<tool name="read_file"><arg name="path">a</arg>`,
		"missing name":     `<tool><arg name="path">a</arg></tool>`,
		"unclosed arg":     `<tool name="read_file"><arg name="path">a</tool>`,
		"arg missing name": `<tool name="read_file"><arg>a</arg></tool>`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := (XMLAdapter{}).Parse(xmlMsg(content)); !errors.Is(err, ErrMalformedToolCall) {
				t.Errorf("want ErrMalformedToolCall, got %v", err)
			}
		})
	}
}

func TestXMLNoTypeCoercion(t *testing.T) {
	msg := xmlMsg(`<tool name="run_command">
  <arg name="command">echo hi</arg>
  <arg name="timeout_seconds">42</arg>
  <arg name="flag">true</arg>
</tool>`)
	call, err := XMLAdapter{}.Parse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// All arg values stay strings — the parser never YAML/JSON-coerces.
	for _, key := range []string{"command", "timeout_seconds", "flag"} {
		if _, isStr := call.Args[key].(string); !isStr {
			t.Errorf("arg %q = %T, want string (no coercion)", key, call.Args[key])
		}
	}
	if call.Args["timeout_seconds"] != "42" || call.Args["flag"] != "true" {
		t.Errorf("values coerced: %+v", call.Args)
	}
}

func TestXMLBuildRequestInjectsGrammarNoTools(t *testing.T) {
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws)
	req := XMLAdapter{}.BuildRequest(llm.ChatRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "do it"}}}, reg)

	if len(req.Tools) != 0 {
		t.Errorf("XML adapter must not set the tools param, got %d", len(req.Tools))
	}
	if len(req.Messages) == 0 || req.Messages[0].Role != llm.RoleSystem {
		t.Fatalf("expected a system prompt first, got %+v", req.Messages)
	}
	sys := req.Messages[0].Content
	if !strings.Contains(sys, "<tool name=") || !strings.Contains(sys, "read_file") {
		t.Errorf("system prompt missing grammar/tool list: %q", sys)
	}
}
