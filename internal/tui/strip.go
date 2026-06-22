package tui

import (
	"regexp"
	"strings"
)

// Models without native function-calling (e.g. Qwen2.5-Coder) emit tool calls
// inline as raw text — JSON {"name":…,"arguments":…} objects, or XML-ish
// <tool_call>…</tool_call> / <function=…>…</function> wrappers. The real action
// is already shown as a clean tool card + the activity line, so the assistant
// prose should NOT also carry the raw syntax (it reads as noise). stripToolCallSyntax
// removes it while preserving the model's actual prose.
var (
	reToolCallTag = regexp.MustCompile(`(?s)<tool_call>.*?</tool_call>`)
	reFunctionTag = regexp.MustCompile(`(?s)<function\s*=?[^>]*>.*?</function>`)
	reStrayTag    = regexp.MustCompile(`(?s)</?(tool_call|function|parameter)\b[^>]*>`)
	reBlankRuns   = regexp.MustCompile(`\n{3,}`)
)

func stripToolCallSyntax(s string) string {
	s = reToolCallTag.ReplaceAllString(s, "")
	s = reFunctionTag.ReplaceAllString(s, "")
	s = stripJSONToolObjects(s)
	s = reStrayTag.ReplaceAllString(s, "")
	s = reBlankRuns.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// toolMarkers are the opening signatures of an inline tool call across the formats
// local models emit. Used to truncate a STILL-STREAMING partial call that
// stripToolCallSyntax can't match yet (it has no closing brace/tag).
var toolMarkers = []string{`{"name"`, `{ "name"`, "<tool_call", "<function", "<|tool_call"}

// firstToolMarker returns the index of the earliest tool-call opener in s, or -1.
func firstToolMarker(s string) int {
	best := -1
	for _, m := range toolMarkers {
		if i := strings.Index(s, m); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}

// cleanAssistantText prepares streamed assistant content for display: complete
// tool-call blocks are stripped, and a still-streaming PARTIAL tool call (opener
// present, not yet closed) is truncated at its opener — so the reasoning prose
// shows while the JSON/XML never does (the action is shown via the card + activity
// line). This is what removes the streaming remnants.
func cleanAssistantText(s string) string {
	// Truncate at the FIRST tool-call opener: everything from there on is the tool
	// call (these models emit it last), whether complete or still streaming. Do
	// this BEFORE stripping, so a partial <function=…>/<tool_call> isn't reduced to
	// dangling fragments by the stray-tag pass.
	if i := firstToolMarker(s); i >= 0 {
		s = s[:i]
	}
	// Clean any residual markup + collapse blank runs in the kept prose.
	return strings.TrimSpace(stripToolCallSyntax(s))
}

// stripJSONToolObjects removes balanced {"name":…,"arguments":…} JSON objects
// (string/escape aware) from text, leaving surrounding prose intact.
func stripJSONToolObjects(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '{' {
			if end := matchBraceFrom(s, i); end > i {
				cand := s[i : end+1]
				if strings.Contains(cand, `"name"`) &&
					(strings.Contains(cand, `"arguments"`) || strings.Contains(cand, `"parameters"`)) {
					i = end + 1 // drop the whole tool-call object
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// matchBraceFrom returns the index of the '}' closing the '{' at start, honoring
// strings and escapes, or -1 if unbalanced.
func matchBraceFrom(s string, start int) int {
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
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
