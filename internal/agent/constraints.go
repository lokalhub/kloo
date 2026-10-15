package agent

import (
	"regexp"
	"strings"
)

// taskConstraints re-states the user's explicit prohibitions at the END of the
// system prompt, for the same reason houseRules restates the repository's.
//
// Measured: a task that said, in its own sentence, "do not change the template",
// and the model changed the template — adding an inline style attribute to it,
// which also broke a layout the previous turn had just fixed. The instruction was
// in the conversation, in the first user message, several thousand tokens back by
// the time the edit was made. A small model obeys what it read last.
//
// Deliberately dumb: no model call, no paraphrase. It lifts the sentences that
// read as prohibitions, verbatim, so the restatement cannot drift from what the
// user actually said. Capped, because a long list is the dilution this exists to
// fix, and empty when the task states no prohibition — most tasks do not.
func taskConstraints(task string) string {
	const (
		maxItems = 4
		maxBytes = 400
	)
	var out []string
	used := 0
	seen := map[string]bool{}
	for _, raw := range splitSentences(task) {
		s := strings.TrimSpace(raw)
		if len(s) < 8 || len(s) > 200 || !reProhibition.MatchString(s) {
			continue
		}
		key := strings.ToLower(s)
		if seen[key] {
			continue
		}
		if used+len(s) > maxBytes {
			break
		}
		seen[key] = true
		out = append(out, "- "+s)
		used += len(s)
		if len(out) >= maxItems {
			break
		}
	}
	if len(out) == 0 {
		return ""
	}
	return "\n\nTHE USER'S OWN CONSTRAINTS on this task — breaking one makes the work wrong " +
		"even if everything else passes:\n" + strings.Join(out, "\n")
}

// reProhibition matches the shapes a user writes a limit in. "only" is included
// because "only touch X" and "change only the stylesheet" are the same instruction
// as "do not touch anything else", and both were given in the session that
// motivated this.
var reProhibition = regexp.MustCompile(`(?i)\b(do not|don't|never|must not|without (?:changing|touching|modifying)|only (?:touch|change|edit|modify)|(?:touch|change|edit|modify|use)\s+only|leave .{0,20} alone)\b`)

// splitSentences breaks a task into sentence-ish units. Newlines and bullets count
// as boundaries, because a task written as a list puts one instruction per line and
// never ends them with a period.
func splitSentences(s string) []string {
	repl := strings.NewReplacer("\n", "\x00", ". ", ".\x00", "; ", ";\x00", " - ", "\x00")
	return strings.Split(repl.Replace(s), "\x00")
}
