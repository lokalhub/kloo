package agent

import (
	"regexp"
	"strings"

	"github.com/lokalhub/kloo/internal/llm"
)

// The enumerated-requirements checklist.
//
// greenVerifyConfirmCorrective asks the model to "re-read the ORIGINAL task and
// check EVERY requirement in it". On a task with one requirement that works. On a
// task that enumerates its requirements it does not, because the model has to
// reconstruct the list from several thousand tokens back and it reconstructs the
// part it just worked on.
//
// Measured, on a task reading "DEFECT 4 … DEFECT 5 … DEFECT 6", each with its own
// write-the-gate-then-fix steps: the run wrote the gate for defect 4, never ran
// it, never fixed anything, never touched 5 or 6 — and stopped as SUCCESS in
// 2m31s. The generic "check every requirement" had been asked, and answered yes.
//
// So the checklist is put in front of the model as a list, lifted verbatim from
// the task. Deliberately dumb for the same reason as taskConstraints: no model
// call, no paraphrase, so the restatement cannot drift from what was asked.

// reEnumerated matches the headers people actually number requirements with:
// "DEFECT 4", "STEP 2:", "Task 3 -", "1." and "2)" at the start of a clause.
// Case-insensitive on the word, because tasks shout them as often as not.
var reEnumerated = regexp.MustCompile(`(?i)(?:^|[.;\n]\s*)((?:defect|step|task|item|part|phase|fix|requirement)\s+\d+|\d+[.)])\s*[:.\-–—]?\s*`)

// maxChecklistItems bounds the list. A task with twenty numbered items is a plan,
// not a prompt, and restating all of it is the dilution this exists to fix.
const maxChecklistItems = 8

// checklistItemChars is how much of each item's own text is quoted back. Enough to
// identify which requirement it is, short enough that eight of them stay scannable.
const checklistItemChars = 90

// taskChecklist returns the task's enumerated requirements, one per line, or ""
// when the task does not enumerate any — which is most tasks, and they keep the
// behaviour they had.
func taskChecklist(task string) string {
	locs := reEnumerated.FindAllStringSubmatchIndex(task, -1)
	if len(locs) < 2 {
		return "" // one marker is a stray "1." in prose, not a list
	}
	var items []string
	for i, loc := range locs {
		if len(items) >= maxChecklistItems {
			break
		}
		label := strings.TrimSpace(task[loc[2]:loc[3]])
		// The item's text runs to the next marker, or to the end of the task.
		end := len(task)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		body := strings.Join(strings.Fields(task[loc[1]:end]), " ")
		if len(body) > checklistItemChars {
			body = body[:checklistItemChars] + "…"
		}
		if body == "" {
			continue
		}
		items = append(items, "  - "+label+": "+body)
	}
	if len(items) < 2 {
		return ""
	}
	return strings.Join(items, "\n")
}

// checklistCorrective is the green-verify completion probe for an ENUMERATED task:
// the same question, asked against a list the model can read rather than one it
// has to remember.
func checklistCorrective(list string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: "Your change is applied and the verify command passes — that part works. " +
		"But this task asked for several separate things, and the verify does not know about most of them:\n" +
		list + "\n" +
		"Go through that list one item at a time and check each against the tree as it stands NOW, not against what you intended. " +
		"For every item that is not finished — a gate written but never run, a fix not made, a file not touched — do the next one THIS turn with a tool call. " +
		"Only call finish when you can say what you did for every item above."}
}
