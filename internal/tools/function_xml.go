package tools

import "strings"

// The Hermes / llama.cpp "<function=NAME>" tool-call dialect. Qwen3-Coder and
// similar small models emit calls in this shape (optionally wrapped in
// <tool_call>…</tool_call>):
//
//	<tool_call>
//	<function=edit_file>
//	<parameter=path>src/app.ts</parameter>
//	<parameter=content>… maybe a SEARCH/REPLACE block …</parameter>
//	</function>
//	</tool_call>
//
// Two things go wrong with it in the wild, and both leak markup into a value:
//   - the model BATCHES several calls in one reply, and
//   - it forgets to close a <parameter=…> before starting the next call,
//
// so a greedy capture (the model's own, or a server-side tool parser's) folds the
// trailing "</parameter></function></tool_call><tool_call><function=…>" of the
// NEXT call into the CURRENT call's argument. The helpers here parse the dialect
// out of text AND bound every captured value so one call can never swallow the
// next.

// toolCallTailMarkers are the opening/closing tokens of the <tool_call> /
// <function=…> dialect. A captured argument value must never contain one — if it
// does, the model (or the endpoint's tool parser) failed to close a parameter and
// the next call leaked in. We cut the value at the earliest marker. These literals
// don't occur in real source/diffs (Angular/Ionic templates use <ion-…> tags, not
// <function=…>/<parameter=…>), so the cut is safe.
var toolCallTailMarkers = []string{
	"</parameter>", "</function>", "</tool_call>", "<tool_call>", "<function=", "<parameter=",
}

// stripToolCallTail trims a leaked tool-call-markup tail from a captured value.
// It returns s unchanged when no marker is present (the common case). The cut is
// at the EARLIEST marker, and trailing whitespace before it is removed so a clean
// "…>>>>>>> REPLACE" diff isn't left with a dangling blank line.
func stripToolCallTail(s string) string {
	cut := -1
	for _, m := range toolCallTailMarkers {
		if i := strings.Index(s, m); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return s
	}
	return strings.TrimRight(s[:cut], " \t\r\n")
}

// sanitizeArgs strips a leaked tool-call tail from every string argument value.
// Applied to every Call the adapters produce (native, JSON-in-text, and the
// <function=…> dialect) so a batched/mis-closed call can't pollute the argument —
// and therefore the file edit and the rendered card — of the call before it.
func sanitizeArgs(args map[string]any) map[string]any {
	for k, v := range args {
		if s, ok := v.(string); ok {
			args[k] = stripToolCallTail(s)
		}
	}
	return args
}

// extractFunctionCalls recovers tool calls emitted in the <function=NAME> dialect
// as text (the path taken when the endpoint's chat template does NOT parse them
// into native tool_calls). It is bounded at every level: the function body ends at
// </function> (or, if unclosed, at the next <function=), and each parameter value
// ends at </parameter> (or, if unclosed, at the next dialect marker) — so a
// batched or mis-closed reply yields several clean calls instead of one call that
// ate the rest. The loop still enforces one-call-per-turn downstream.
func extractFunctionCalls(content string) []Call {
	var out []Call
	s := content
	for {
		open := strings.Index(s, "<function=")
		if open < 0 {
			break
		}
		nameStart := open + len("<function=")
		gt := strings.Index(s[nameStart:], ">")
		if gt < 0 {
			break // unterminated <function=…> tag
		}
		name := strings.Trim(s[nameStart:nameStart+gt], "\"' \t")
		rest := s[nameStart+gt+1:]

		// Body ends at </function>; if the model never closed it, stop at the next
		// call's opener so this call doesn't consume the following one.
		var body string
		endFn := strings.Index(rest, "</function>")
		nextFn := strings.Index(rest, "<function=")
		switch {
		case endFn >= 0 && (nextFn < 0 || endFn < nextFn):
			body = rest[:endFn]
			s = rest[endFn+len("</function>"):]
		case nextFn >= 0:
			body = rest[:nextFn]
			s = rest[nextFn:]
		default:
			body = rest
			s = ""
		}

		if name != "" {
			out = append(out, Call{Name: name, Args: parseFunctionParams(body)})
		}
		if s == "" {
			break
		}
	}
	return out
}

// parseFunctionParams reads the <parameter=KEY>VALUE</parameter> pairs of one
// <function=…> body. Each value is bounded by boundedParamValue, so an unclosed
// parameter stops at the next dialect marker rather than swallowing it.
func parseFunctionParams(body string) map[string]any {
	args := map[string]any{}
	s := body
	for {
		open := strings.Index(s, "<parameter=")
		if open < 0 {
			break
		}
		keyStart := open + len("<parameter=")
		gt := strings.Index(s[keyStart:], ">")
		if gt < 0 {
			break // unterminated <parameter=…> tag
		}
		key := strings.Trim(s[keyStart:keyStart+gt], "\"' \t")
		rest := s[keyStart+gt+1:]
		val, adv := boundedParamValue(rest)
		if key != "" {
			args[key] = val
		}
		s = adv
	}
	return args
}

// boundedParamValue returns the value of a <parameter=…> and the remaining text to
// continue scanning from. The value ends at the closing </parameter>; if that's
// missing (the mis-close that causes leaks), it ends at the earliest other dialect
// marker — never consuming the next call. Surrounding whitespace is trimmed; inner
// content (e.g. a fenced SEARCH/REPLACE diff) is preserved verbatim.
func boundedParamValue(rest string) (value, remainder string) {
	end := -1
	advance := 0
	if i := strings.Index(rest, "</parameter>"); i >= 0 {
		end, advance = i, i+len("</parameter>")
	}
	for _, m := range []string{"<parameter=", "</function>", "<tool_call>", "</tool_call>", "<function="} {
		if i := strings.Index(rest, m); i >= 0 && (end < 0 || i < end) {
			end, advance = i, i // keep the marker for the next iteration
		}
	}
	if end < 0 {
		return strings.TrimSpace(rest), ""
	}
	return strings.TrimSpace(rest[:end]), rest[advance:]
}
