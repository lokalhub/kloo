package tools

import (
	"fmt"
	"strings"

	"github.com/lokalhub/kloo/internal/llm"
)

// XMLAdapter is the generic XML-tag fallback for untuned models that can't emit
// reliable native tool_calls (design doc §2). It parses a single <tool> block
// out of the assistant's text and normalises it to the SAME dispatchable Call
// the native adapter produces. Arg values are taken as raw strings — NEVER
// YAML-/type-coerced — and fenced code/diffs inside an <arg> are preserved
// verbatim (the edit engine depends on the exact bytes).
type XMLAdapter struct{}

// xmlGoldExample is the gold few-shot the system prompt and the corrective
// message both show the model.
const xmlGoldExample = "<tool name=\"edit_file\">\n" +
	"  <arg name=\"path\">src/app.ts</arg>\n" +
	"  <arg name=\"diff\">\n" +
	"```\n" +
	"<<<<<<< SEARCH\n" +
	"old line\n" +
	"=======\n" +
	"new line\n" +
	">>>>>>> REPLACE\n" +
	"```\n" +
	"  </arg>\n" +
	"</tool>"

// BuildRequest injects a system prompt that teaches the XML grammar plus the
// gold example and lists the available tools. It does NOT set the tools param —
// the endpoint is treated as untuned.
func (XMLAdapter) BuildRequest(base llm.ChatRequest, reg *Registry) llm.ChatRequest {
	sys := llm.Message{Role: llm.RoleSystem, Content: xmlGrammarPrompt(reg)}
	base.Messages = append([]llm.Message{sys}, base.Messages...)
	return base
}

// ParseAll extracts every <tool> block and its <arg>s into Calls, in order. A
// broken block (unterminated tag, missing name, missing close) → a clear
// ErrMalformedToolCall (never a silent best-guess). The count is not enforced
// here.
func (XMLAdapter) ParseAll(msg llm.Message) ([]Call, error) {
	text := msg.Content
	var calls []Call

	i := 0
	for {
		rel := strings.Index(text[i:], "<tool")
		if rel < 0 {
			break
		}
		openIdx := i + rel
		gt := strings.Index(text[openIdx:], ">")
		if gt < 0 {
			return nil, fmt.Errorf("xml: unterminated <tool> tag: %w", ErrMalformedToolCall)
		}
		openTagEnd := openIdx + gt + 1
		name, ok := attrValue(text[openIdx:openTagEnd], "name")
		if !ok || name == "" {
			return nil, fmt.Errorf("xml: <tool> missing name attribute: %w", ErrMalformedToolCall)
		}
		relClose := strings.Index(text[openTagEnd:], "</tool>")
		if relClose < 0 {
			return nil, fmt.Errorf("xml: missing </tool> close tag: %w", ErrMalformedToolCall)
		}
		inner := text[openTagEnd : openTagEnd+relClose]
		args, err := parseXMLArgs(inner)
		if err != nil {
			return nil, err
		}
		calls = append(calls, Call{Name: name, Args: sanitizeArgs(args)})
		i = openTagEnd + relClose + len("</tool>")
	}
	return calls, nil
}

// Parse extracts exactly one <tool> block (the one-tool-per-turn rail). Zero/many
// → the one-tool-per-turn sentinels.
func (a XMLAdapter) Parse(msg llm.Message) (Call, error) {
	calls, err := a.ParseAll(msg)
	if err != nil {
		return Call{}, err
	}
	return ExactlyOneCall(calls)
}

// Corrective restates the XML grammar + gold example for the one re-prompt.
func (XMLAdapter) Corrective(parseErr error) string {
	return "Your previous reply was not a single valid tool call. Emit EXACTLY ONE tool call " +
		"using this XML format, and nothing else that looks like a tool block:\n\n" +
		xmlGoldExample + "\n\n" +
		"Rules: one <tool name=\"…\"> element; each parameter is an <arg name=\"…\">value</arg>; " +
		"put any code or SEARCH/REPLACE diff inside a ``` fenced block within the <arg>. " +
		"Do not use YAML. Do not emit zero or multiple <tool> blocks."
}

// parseXMLArgs extracts <arg name="…">value</arg> pairs. The value is the inner
// text trimmed of SURROUNDING whitespace only — never YAML-parsed or coerced; a
// fenced block inside stays byte-for-byte intact.
func parseXMLArgs(inner string) (map[string]any, error) {
	args := map[string]any{}
	i := 0
	for {
		rel := strings.Index(inner[i:], "<arg")
		if rel < 0 {
			break
		}
		a := i + rel
		gt := strings.Index(inner[a:], ">")
		if gt < 0 {
			return nil, fmt.Errorf("xml: unterminated <arg> tag: %w", ErrMalformedToolCall)
		}
		argTagEnd := a + gt + 1
		name, ok := attrValue(inner[a:argTagEnd], "name")
		if !ok || name == "" {
			return nil, fmt.Errorf("xml: <arg> missing name attribute: %w", ErrMalformedToolCall)
		}
		relClose := strings.Index(inner[argTagEnd:], "</arg>")
		if relClose < 0 {
			return nil, fmt.Errorf("xml: missing </arg> for %q: %w", name, ErrMalformedToolCall)
		}
		value := inner[argTagEnd : argTagEnd+relClose]
		args[name] = strings.TrimSpace(value) // surrounding whitespace only; no coercion
		i = argTagEnd + relClose + len("</arg>")
	}
	return args, nil
}

// attrValue reads a double- or single-quoted attribute value from a tag string.
func attrValue(tag, key string) (string, bool) {
	for _, q := range []string{`"`, `'`} {
		marker := key + "=" + q
		idx := strings.Index(tag, marker)
		if idx < 0 {
			continue
		}
		rest := tag[idx+len(marker):]
		end := strings.Index(rest, q)
		if end < 0 {
			continue
		}
		return rest[:end], true
	}
	return "", false
}

// xmlGrammarPrompt builds the system prompt teaching the XML tool grammar and
// listing the registry's tools (so the prompt is derived from the registry, not
// a second hand-maintained list).
func xmlGrammarPrompt(reg *Registry) string {
	var b strings.Builder
	b.WriteString("You can call tools. Emit EXACTLY ONE tool call per reply using this XML format:\n\n")
	b.WriteString(xmlGoldExample)
	b.WriteString("\n\nRules: one <tool name=\"…\"> per reply; each parameter is <arg name=\"…\">value</arg>; ")
	b.WriteString("put code or SEARCH/REPLACE diffs inside a ``` fenced block within the <arg>; never use YAML.\n\n")
	b.WriteString("Available tools:\n")
	for _, t := range reg.Tools() {
		b.WriteString("- " + t.Name() + ": " + t.Description() + "\n")
		for argName, p := range t.Schema().Properties {
			b.WriteString("    - " + argName + " (" + p.Type + "): " + p.Description + "\n")
		}
	}
	return b.String()
}
