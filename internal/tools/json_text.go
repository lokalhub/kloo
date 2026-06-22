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
		// Accept both the OpenAI shape {"name","arguments"} and the common variant
		// {"tool","args"} that some local models emit (e.g. snappy on a conversational
		// turn). Without the variant the call parses as "no tool call" → re-prompt → error.
		var raw struct {
			Name      string          `json:"name"`
			Tool      string          `json:"tool"`
			Arguments json.RawMessage `json:"arguments"`
			Args      json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(b[i:end+1], &raw); err == nil {
			name := raw.Name
			if name == "" {
				name = raw.Tool
			}
			argsRaw := raw.Arguments
			if len(argsRaw) == 0 {
				argsRaw = raw.Args
			}
			if name != "" {
				out = append(out, Call{Name: name, Args: decodeArgs(argsRaw)})
			}
		}
		i = end // skip past this object regardless (don't rescan its interior)
	}
	return out
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
