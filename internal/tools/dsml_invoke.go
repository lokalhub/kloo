package tools

import "strings"

// The Claude-style "invoke"/"parameter" tool-call dialect, as emitted by some
// DeepSeek models (e.g. deepseek-v4-*) wrapped in DeepSeek's <｜DSML｜…｜> special
// tokens. When the provider does NOT fold this into native tool_calls, it arrives
// verbatim in the assistant's text content, e.g.:
//
//	<｜DSML｜tool_calls>
//	<｜DSML｜invoke name="finish">
//	<｜DSML｜parameter name="summary" string="true">Built the app…</｜DSML｜parameter>
//	</｜DSML｜invoke>
//	</｜DSML｜tool_calls>
//
// We key on the STABLE substrings `invoke name="…"` and `parameter name="…"` and
// ignore the surrounding DSML/special-token noise, so the exact token spelling
// (｜ = U+FF5C, the "DSML" wrapper, etc.) doesn't matter — only the invoke/parameter
// shape does. Like the <function=…> dialect, every value is bounded so a batched or
// mis-closed reply yields clean calls instead of one call swallowing the rest.

// extractInvokeToolCalls recovers tool calls written in the invoke/parameter
// dialect as text. Returns nil when the content has none.
func extractInvokeToolCalls(content string) []Call {
	var out []Call
	s := content
	for {
		open := strings.Index(s, "invoke name=")
		if open < 0 {
			break
		}
		name, afterName := quotedAttrValue(s[open+len("invoke name="):])
		// The invoke body runs until the NEXT call opener (multiple invokes in one
		// reply split here); parameter-level bounding handles the close tags.
		body, rest := afterName, ""
		if next := strings.Index(afterName, "invoke name="); next >= 0 {
			body, rest = afterName[:next], afterName[next:]
		}
		if name != "" {
			out = append(out, Call{Name: name, Args: parseInvokeParams(body)})
		}
		if rest == "" {
			break
		}
		s = rest
	}
	return out
}

// parseInvokeParams reads the `parameter name="KEY" …>VALUE</…parameter>` pairs of
// one invoke body. Extra attributes on the tag (e.g. string="true") are skipped;
// the value is whatever sits between the tag's '>' and its closing parameter tag.
func parseInvokeParams(body string) map[string]any {
	args := map[string]any{}
	s := body
	for {
		open := strings.Index(s, "parameter name=")
		if open < 0 {
			break
		}
		key, afterKey := quotedAttrValue(s[open+len("parameter name="):])
		// Skip any remaining attributes to the end of the opening tag.
		gt := strings.IndexByte(afterKey, '>')
		if gt < 0 {
			break // unterminated opening tag
		}
		val, adv := boundedInvokeValue(afterKey[gt+1:])
		if key != "" {
			args[key] = val
		}
		if adv == "" {
			break
		}
		s = adv
	}
	return args
}

// quotedAttrValue reads a "…"-quoted attribute value at the start of s (after an
// optional run of spaces/=) and returns the value plus the text after the closing
// quote. Returns ("", s) when no quoted value is present.
func quotedAttrValue(s string) (value, rest string) {
	q1 := strings.IndexByte(s, '"')
	if q1 < 0 {
		return "", s
	}
	q2 := strings.IndexByte(s[q1+1:], '"')
	if q2 < 0 {
		return "", s
	}
	return s[q1+1 : q1+1+q2], s[q1+1+q2+1:]
}

// boundedInvokeValue returns a parameter's value and the remaining text to scan
// from. The value ends at its closing parameter tag (matched on the stable
// `parameter>` tail of `</…parameter>`); if that's missing it ends at the next
// parameter/invoke marker, never swallowing the following call. The closing tag's
// own `<` opener is trimmed off the value, and surrounding whitespace is removed
// while inner content (e.g. an HTML/template snippet) is preserved.
func boundedInvokeValue(s string) (value, remainder string) {
	end, advance := -1, 0
	if i := strings.Index(s, "parameter>"); i >= 0 { // tail of </…parameter>
		end, advance = i, i+len("parameter>")
	}
	for _, m := range []string{"parameter name=", "invoke>"} {
		if i := strings.Index(s, m); i >= 0 && (end < 0 || i < end) {
			end, advance = i, i // keep the marker for the next iteration
		}
	}
	if end < 0 {
		return strings.TrimSpace(s), ""
	}
	// Cut the value at the '<' that opens the closing/next tag, so the value never
	// keeps a `</｜DSML｜` fragment. The nearest '<' to the left of the marker is the
	// tag opener; any '<' inside the value is further left.
	cut := end
	if lt := strings.LastIndexByte(s[:end], '<'); lt >= 0 {
		cut = lt
	}
	return strings.TrimSpace(s[:cut]), s[advance:]
}
