package tools

import "strings"

// A fifth text tool-call dialect, observed live from glm-5.3-flash (GLM's own
// chat template): the name sits as bare text right after the opening tag, and
// each argument is a <arg_key>/<arg_value> PAIR rather than an element named for
// the parameter:
//
//	<tool_call>finish<arg_key>summary</arg_key><arg_value>explained the layout</arg_value></tool_call>
//
// None of the four earlier extractors match it. extractJSONToolCalls needs JSON,
// <function=…> needs the `=name` form, the DSML extractor keys on an `invoke
// name="…"` attribute, and the <invoke_name>/<parameters> extractor needs the
// name inside an element — here it is loose text, and the parameter NAME is a
// value rather than a tag. `<tool_call` is already in toolCallMarkers, so a reply
// in this dialect did not fall through silently: it failed LOUDLY as a malformed
// call, three corrective re-prompts in a row, and ended the run as
// `malformed-tool-call`.
//
// Measured on the user's live session before this extractor existed (captured
// through a logging proxy, glm-5.3-flash on an OpenAI-compatible endpoint): the
// model read the workspace, answered in prose, was nudged by the confirm-finish
// rail to call `finish`, and then emitted `finish` in THIS dialect on all three
// corrective attempts. kloo threw away a run the model had actually completed,
// having spent three extra full-prompt re-prefills (~40k tokens/turn, 1% cached)
// correcting syntax it could simply have read.
//
// Unlike the other dialects, the NAME here is loose text whose only anchor is
// the <tool_call> wrapper, so the wrapper is required to recover a call. A reply
// that carries the arg pairs without it has no recoverable name — `<arg_key>` is
// in toolCallMarkers so that case fails loudly for a corrective re-prompt rather
// than passing for prose.

const (
	argKeyOpen    = "<arg_key>"
	argKeyClose   = "</arg_key>"
	argValueOpen  = "<arg_value>"
	argValueClose = "</arg_value>"
	toolCallOpen  = "<tool_call>"
	toolCallClose = "</tool_call>"
)

// extractArgKeyToolCalls recovers tool calls written in the
// <arg_key>/<arg_value> dialect. Returns nil when the content has none.
func extractArgKeyToolCalls(content string) []Call {
	if !strings.Contains(content, argKeyOpen) {
		return nil
	}
	var out []Call
	s := content
	for {
		open := strings.Index(s, toolCallOpen)
		if open < 0 {
			break
		}
		body := s[open+len(toolCallOpen):]
		// One call's body ends at its close tag, else at the next call's opening
		// tag, else at the end of the reply — a batched or truncated reply must
		// still yield the calls it did spell out.
		rest := ""
		if end := strings.Index(body, toolCallClose); end >= 0 {
			body, rest = body[:end], body[end+len(toolCallClose):]
		} else if next := strings.Index(body, toolCallOpen); next >= 0 {
			body, rest = body[:next], body[next:]
		}
		if name, args, ok := parseArgKeyCall(body); ok {
			out = append(out, Call{Name: name, Args: args})
		}
		if rest == "" {
			break
		}
		s = rest
	}
	return out
}

// parseArgKeyCall reads one call body: a bare name followed by <arg_key>/
// <arg_value> pairs. It reports false when there is no usable name, so a stray
// wrapper never becomes a call to the empty tool.
func parseArgKeyCall(body string) (string, map[string]any, bool) {
	head := body
	if i := strings.Index(body, argKeyOpen); i >= 0 {
		head = body[:i]
	}
	name := strings.TrimSpace(head)
	// The name is bare text, so anything containing markup or whitespace is not a
	// name — most likely a prose reply that merely mentions the tag.
	if name == "" || strings.ContainsAny(name, "<> \t\n") {
		return "", nil, false
	}

	args := map[string]any{}
	s := body
	for {
		k := strings.Index(s, argKeyOpen)
		if k < 0 {
			break
		}
		after := s[k+len(argKeyOpen):]
		ke := strings.Index(after, argKeyClose)
		if ke < 0 {
			break // unterminated key: nothing trustworthy to recover
		}
		key := strings.TrimSpace(after[:ke])
		after = after[ke+len(argKeyClose):]

		v := strings.Index(after, argValueOpen)
		if v < 0 {
			break // a key with no value at all
		}
		val := after[v+len(argValueOpen):]
		if ve := strings.Index(val, argValueClose); ve >= 0 {
			s = val[ve+len(argValueClose):]
			val = val[:ve]
		} else {
			// Truncated last value (a cut-off stream). Keep what arrived rather than
			// discarding the whole call: the alternative, measured, is a dead run.
			s = ""
		}
		if key != "" {
			// Never trim the value's interior — an edit payload or a diff is often
			// the value, and whitespace in it is content. Surrounding newlines come
			// from the markup, so those go.
			args[key] = strings.Trim(val, "\n")
		}
		if s == "" {
			break
		}
	}
	return name, args, true
}
