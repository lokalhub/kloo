package agent

import (
	"regexp"
	"strings"

	"github.com/lokalhub/kloo/internal/llm"
)

// The no-edit-finish rail.
//
// A real run: the task listed SIX visual defects to fix, each with the file to
// change. The model ran the capture command, looked at three screenshots, called
// finish — and the run was labelled SUCCESS, because the verify command happened
// to be green before the run started. Seven steps, zero edits, a green tick.
//
// None of the existing rails could see it. The confirm-finish rail fires only on
// a bare PROSE stop, and this stop was an explicit finish call. The finish-claim
// check reads the summary for claims the tools cannot back, and this summary was
// honest about having only looked. The green-verify confirm needs `edited`. The
// one thing nobody checked was the plainest: the task said change things, and
// nothing was changed.
//
// The rail is one-shot and it never refuses the second finish, so a model that
// genuinely has nothing to change says so and stops on the next turn.

// changeVerbs are the imperative verbs that make a task a CHANGE request. A task
// matching none of them — "which file handles auth?", "explain this function" —
// never trips the rail, because finishing one without an edit is correct.
// "set" is deliberately NOT here. It was, and it matched "is it set up?" — the
// task string in kloo's own retry tests, which is a question. The suite caught it.
// A verb generic enough to appear inside an ordinary question costs more in false
// positives than it earns: the rail's job is to catch a run that changed nothing,
// and missing one phrasing just leaves today's behaviour.
var changeVerbs = regexp.MustCompile(`(?i)\b(fix|fixes|add|adds|change|changes|implement|implements|make|update|updates|remove|removes|delete|rename|refactor|create|write|wire|replace|migrate|port|bump|style|restyle|redesign|correct|repair|resolve|patch|introduce|extract|rewrite|convert|improve|clean up|tidy)\b`)

// taskWantsChange reports whether the task asks for the tree to be modified.
//
// It is deliberately conservative in both directions: a task matching no change
// verb never trips the rail, and neither does a bare question, however it is
// worded. Finishing either without an edit is correct, and a false nudge spends a
// turn telling a model to change something nobody asked it to change.
func taskWantsChange(task string) bool {
	t := strings.TrimSpace(task)
	if t == "" {
		return false
	}
	if strings.HasSuffix(t, "?") && !strings.ContainsAny(t, ".\n") {
		return false // a single question, not a change request
	}
	return changeVerbs.MatchString(t)
}

// changeRailsApply reports whether the rails that tell the model to CHANGE
// something may fire at all.
//
// The write scope outranks the task text, and it has to: a validation run launched
// with `--allow 'nothing/**'` and a prompt beginning "You are the VALIDATOR. Do not
// edit any file." QUOTES the spec it validates, so it is full of change verbs —
// measured, 26 of them matched. taskWantsChange concluded the task asked for edits
// and nudged a validator to make edits every write tool would refuse; the model
// answered again, four times, each finish carrying the whole answer. The task text
// can never be a reliable signal, because a prompt that reviews code necessarily
// contains the vocabulary of changing code. The write scope can be, and it is
// already known.
//
// taskWantsChange is unchanged and still correct about the text — the text was
// never the problem. Only the rails that demand a change are wrong when no change
// is possible; the confirm-finish rail ("you stopped in prose without calling
// finish") stays a sensible nudge for a read-only run.
func changeRailsApply(task string, mayWrite bool) bool {
	return mayWrite && taskWantsChange(task)
}

// noEditFinishCorrective challenges a finish that changed nothing. It states the
// fact, gives both legitimate exits, and names the second one explicitly — a model
// told only to "keep going" will invent an edit to satisfy the nudge, which is
// worse than the premature stop it replaces.
func noEditFinishCorrective(task string) llm.Message {
	first := taskOpening(task)
	return llm.Message{Role: llm.RoleUser, Content: "You called finish, but this run has not changed a single file — " +
		"every tool call so far only read, looked or ran a command. The task asked for changes: \"" + first + "\" " +
		"Re-read it and check each thing it asked for against the tree as it stands now. " +
		"If ANY of it is still not done, make that change THIS turn with an edit_file or write_file call. " +
		"If every part is genuinely already satisfied and no change is needed, call finish again and say in the summary " +
		"WHY no change was needed. Do not invent an edit just to answer this message."}
}

// taskOpening returns a short, single-line opening of the task for quoting back.
// (report.go's firstLine cuts at the first newline only; a task is usually one
// long line, so it needs collapsing and a length cap instead.)
func taskOpening(task string) string {
	s := strings.Join(strings.Fields(task), " ")
	const max = 160
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// failedLastActionCorrective challenges a run that is about to be called a success
// while its last tool call errored. It quotes the error, because the model has
// usually moved on and a bare "something failed" invites it to re-affirm that
// everything is fine.
func failedLastActionCorrective(errText string) llm.Message {
	e := strings.Join(strings.Fields(errText), " ")
	const max = 300
	if len(e) > max {
		e = e[:max] + "…"
	}
	return llm.Message{Role: llm.RoleUser, Content: "Before this run ends: your most recent tool call FAILED — " + e + " " +
		"The verify command passes, but it can pass for reasons that have nothing to do with that call, " +
		"so it does not tell you the change landed. Check whether the thing that call was supposed to do " +
		"actually happened (read the file back and look). If it did not, do it now, THIS turn, with a tool call. " +
		"If the failed call turned out to be unnecessary, call finish and say so."}
}
