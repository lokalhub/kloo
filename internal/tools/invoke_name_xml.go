package tools

import "strings"

// A fourth text tool-call dialect, observed from deepseek-v4-flash via
// OpenRouter: a bare <function> wrapper around a <tool_call> whose name sits in
// its own element and whose arguments are child elements of <parameters>:
//
//	<function>
//	<tool_call>
//	<invoke_name>run_command</invoke_name>
//	<parameters>
//	<command>go test ./...</command>
//	</parameters>
//	</tool_call>
//	</function>
//
// None of the existing extractors match it: <function=…> requires the `=name`
// form, the DSML extractor keys on `invoke name="…"` (an ATTRIBUTE), and there is
// no JSON. Before this, a reply in this dialect parsed to zero calls and the run
// stopped at step one reporting `answered` — a silent no-op dressed as a clean
// finish, the same failure shape as the edit_file fence bug.
//
// As with the other dialects we key on the STABLE inner markers (<invoke_name>
// and the <parameters> block) rather than the wrapper, so a reply that drops
// <function> or renames the outer tag still parses.

const (
	parametersOpen  = "<parameters>"
	parametersClose = "</parameters>"
)

// nameTags are the element spellings observed carrying the tool NAME in this
// dialect. deepseek-v4-flash emitted <invoke_name> on one turn and <tool_name>
// on the very next (the corrective re-prompt), so the spelling is not stable
// even within a single run — match the family, not one variant.
var nameTags = []string{"invoke_name", "tool_name", "function_name", "name"}

// nextNameTag finds the earliest tool-name element in s, returning its value,
// the text following it, and whether one was found.
func nextNameTag(s string) (name, rest string, ok bool) {
	best := -1
	var bestOpen, bestClose string
	for _, t := range nameTags {
		open := "<" + t + ">"
		if i := strings.Index(s, open); i >= 0 && (best < 0 || i < best) {
			best, bestOpen, bestClose = i, open, "</"+t+">"
		}
	}
	if best < 0 {
		return "", "", false
	}
	after := s[best+len(bestOpen):]
	end := strings.Index(after, bestClose)
	if end < 0 {
		return "", "", false // unterminated: nothing trustworthy to recover
	}
	return strings.TrimSpace(after[:end]), after[end+len(bestClose):], true
}

// extractInvokeNameToolCalls recovers tool calls written in the
// <invoke_name>/<parameters> dialect. Returns nil when the content has none.
func extractInvokeNameToolCalls(content string) []Call {
	var out []Call
	s := content
	for {
		name, after, ok := nextNameTag(s)
		if !ok {
			break
		}
		// One call's body runs until the NEXT call's name element, so a batched
		// reply yields separate calls instead of one swallowing the rest.
		body := after
		if _, _, more := nextNameTag(after); more {
			if cut := nextNameTagIndex(after); cut >= 0 {
				body = after[:cut]
			}
		}
		if name != "" {
			out = append(out, Call{Name: name, Args: parseNamedTagParams(body)})
		}
		if len(body) == len(after) {
			break
		}
		s = after[len(body):]
	}
	return out
}

// nextNameTagIndex is the offset of the earliest tool-name element in s, or -1.
func nextNameTagIndex(s string) int {
	best := -1
	for _, t := range nameTags {
		if i := strings.Index(s, "<"+t+">"); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// parseNamedTagParams reads one call's arguments, where each argument is an
// element named for the parameter: <command>go test ./...</command>.
//
// Scoped to the <parameters> block when one is present, so the surrounding
// wrapper tags (</tool_call>, </function>) are never mistaken for arguments.
func parseNamedTagParams(body string) map[string]any {
	args := map[string]any{}
	if i := strings.Index(body, parametersOpen); i >= 0 {
		body = body[i+len(parametersOpen):]
		if j := strings.Index(body, parametersClose); j >= 0 {
			body = body[:j]
		}
	}

	s := body
	for {
		open := strings.IndexByte(s, '<')
		if open < 0 {
			break
		}
		gt := strings.IndexByte(s[open:], '>')
		if gt < 0 {
			break
		}
		tag := s[open+1 : open+gt]
		afterTag := s[open+gt+1:]

		// Skip closing tags and anything that isn't a plain element name; an
		// argument value may legitimately contain '<' (it is often code), so we
		// bound on the matching close tag rather than the next '<'.
		if tag == "" || strings.HasPrefix(tag, "/") {
			s = afterTag
			continue
		}
		if sp := strings.IndexAny(tag, " \t\n"); sp >= 0 {
			tag = tag[:sp] // tolerate attributes on the parameter element
		}
		closeTag := "</" + tag + ">"
		e := strings.Index(afterTag, closeTag)
		if e < 0 {
			s = afterTag
			continue
		}
		args[tag] = strings.TrimSpace(afterTag[:e])
		s = afterTag[e+len(closeTag):]
	}
	return args
}

// toolCallMarkers are substrings that mean "the model was TRYING to call a tool"
// even when no extractor could parse the result.
var toolCallMarkers = []string{
	"<tool_call", "<invoke_name", "<tool_name", "<invoke ", "<function=", "<function>",
	"\"tool_call\"", "tool_calls", "<｜DSML｜",
}

// looksLikeToolCall reports whether content appears to contain an attempted tool
// call. Used to turn an UNPARSEABLE call into a loud error instead of a silent
// `answered` stop.
//
// There will be a fifth dialect. The cost of discovering it should be a
// corrective re-prompt, not a run that reports success having done nothing.
func looksLikeToolCall(content string) bool {
	for _, m := range toolCallMarkers {
		if strings.Contains(content, m) {
			return true
		}
	}
	return false
}
