package tools

import "encoding/json"

// extractJSONToolCalls recovers tool calls that a model emitted as JSON in its
// text content instead of via native `tool_calls`. Models without function-calling
// in their chat template (e.g. Qwen2.5-Coder) do this — they print one or more
// {"name": "...", "arguments": {...}} objects, sometimes wrapped in prose. We scan
// for balanced JSON objects and keep the ones that look like a tool call. The loop
// still enforces one-call-per-turn (it takes the first and records the rest), so
// returning several here is safe.
func extractJSONToolCalls(content string) []Call {
	var out []Call
	b := []byte(content)
	for i := 0; i < len(b); i++ {
		if b[i] != '{' {
			continue
		}
		end := matchBrace(b, i)
		if end < 0 {
			break // unbalanced from here on
		}
		var obj map[string]any
		if err := json.Unmarshal(b[i:end+1], &obj); err == nil {
			if c, ok := callFromObject(obj); ok {
				out = append(out, c)
			}
		}
		i = end // skip past this object regardless (don't rescan its interior)
	}
	return out
}

// callFromObject recognizes the tool-call JSON shapes small local models emit —
// all keyed off a tool-name field, with flexible arguments:
//
//	{"name"|"tool": "<tool>", "arguments"|"args": { … }}   nested object
//	{"name"|"tool": "<tool>", "arguments"|"args": "{…}"}   args as a JSON string
//	{"tool": "<tool>", "<arg>": …, … }                     args as SIBLING keys (flat)
//
// Returns ok=false when there's no recognizable tool name. Being permissive here is
// the difference between dispatching the call and a "no tool call → re-prompt → error".
func callFromObject(obj map[string]any) (Call, bool) {
	name, _ := obj["name"].(string)
	if name == "" {
		name, _ = obj["tool"].(string)
	}
	if name == "" {
		return Call{}, false
	}
	// Explicit args under "arguments" or "args" — an object, or a serialized JSON string.
	for _, k := range []string{"arguments", "args"} {
		switch v := obj[k].(type) {
		case map[string]any:
			return Call{Name: name, Args: v}, true
		case string:
			return Call{Name: name, Args: decodeArgs(json.RawMessage(v))}, true
		}
	}
	// Flat form: the siblings of the name/tool key ARE the arguments.
	args := map[string]any{}
	for k, v := range obj {
		if k == "name" || k == "tool" {
			continue
		}
		args[k] = v
	}
	return Call{Name: name, Args: args}, true
}

// matchBrace returns the index of the '}' that closes the '{' at start, honoring
// strings and escapes, or -1 if it never balances.
func matchBrace(b []byte, start int) int {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(b); i++ {
		c := b[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// decodeArgs unmarshals tool-call arguments, tolerating models that double-encode
// them as a JSON string (e.g. "arguments": "{\"command\":\"ls\"}").
func decodeArgs(raw json.RawMessage) map[string]any {
	args := map[string]any{}
	if len(raw) == 0 {
		return args
	}
	if err := json.Unmarshal(raw, &args); err == nil {
		return args
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		_ = json.Unmarshal([]byte(s), &args)
	}
	return args
}
