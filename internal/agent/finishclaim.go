package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

// Claim-checking the finish summary.
//
// kloo's standing rule is that it trusts tool OUTPUT and never the model's word —
// success hinges on a green verify, never on a self-report. The finish tool was the
// one place that rule did not hold: whatever the summary asserted became the run's
// closing message to the user, unchecked.
//
// Observed live on a long resumed session (37 prior runs, no verify command): the
// model called read_file, then write_file reply.md, then finish with "I've written
// the reply to reply.md and created sent.txt with reported". No run_command ever
// dispatched that run and sent.txt did not exist. The user was told the work was
// done. On a short fresh session the same task ran the command in 4 steps, so this
// is a late-context failure, not a prompt problem — which is exactly why it has to
// be caught at the boundary rather than argued about in the system prompt.
//
// So before a finish is honoured, the summary's concrete claims are checked against
// what this run's tools actually did. An unsupported claim refuses the finish ONCE,
// with a corrective naming the missing action. One-shot is the safety valve: a model
// that genuinely has nothing left stops calmly on the very next turn, so the worst
// case of a false positive is one extra step.

// createVerbs anchor a claim that a file was PRODUCED. Matched lower-cased against
// the whole summary: the file check only runs when one is present, so a summary that
// merely mentions a filename ("x.txt is empty", "see notes.md") is never challenged.
var createVerbs = []string{
	"created", "i create", "wrote", "written", "writing",
	"saved", "generated", "produced", "added a file", "new file",
}

// ranClaimRe matches a claim of having EXECUTED something. Anchored on the subject
// ("i ran", "the command ran") rather than on the bare verb, so prose like "running
// the suite would take a while" is not read as a claim of having done it.
var ranClaimRe = regexp.MustCompile(`(?i)\b(?:i (?:ran|executed|invoked|posted|curled)|i(?:'ve| have) (?:run|executed|posted|sent)|(?:then|and|also|successfully|subsequently) (?:ran|executed|posted)|(?:the|that) command (?:ran|executed|succeeded)|ran the (?:command|script|curl)|executed the (?:command|script))\b`)

// ranIntoRe is the idiom that shares the verb and means the opposite of acting. It is
// rewritten out of the text before ranClaimRe sees it, rather than fought with
// lookahead, because "I ran into a problem and stopped" is a common honest closer.
var ranIntoRe = regexp.MustCompile(`(?i)\bran into\b`)

// fileTokenRe picks filename-shaped tokens out of prose. The extension must START
// with a letter, which is what keeps version numbers (v0.28.0, 1.5) out.
var fileTokenRe = regexp.MustCompile("(?:^|[\\s`'\"(\\[*])([A-Za-z0-9_][A-Za-z0-9_./-]*\\.[A-Za-z][A-Za-z0-9]{0,7})")

// notFileExt are extensions that are nearly always a hostname, not a file. A miss
// here only costs a check we do not make, so the list leans toward skipping.
var notFileExt = map[string]bool{
	"com": true, "org": true, "net": true, "io": true, "dev": true,
	"app": true, "ai": true, "co": true, "eg": true, "ie": true, "me": true,
}

// negationRe spots a filename mentioned as ABSENT. "There is no config.yaml" is a
// true statement about a file that does not exist, and must not be challenged for
// being one.
var negationRe = regexp.MustCompile(`(?i)(?:\b(?:no|not|never|without|missing|absent|empty|lacks|lacking|nonexistent|non-existent|instead of|rather than)\b|n't\b)`)

// unsupportedFinishClaim returns a sentence naming the most concrete action the
// finish summary asserts and this run's tool output does not support, or "" when
// every claim checks out. wrote is the set of paths an edit/write tool actually
// landed this run; ranCommand reports whether run_command ever dispatched cleanly.
func (l *Loop) unsupportedFinishClaim(summary string, wrote map[string]bool, ranCommand bool) string {
	s := strings.TrimSpace(summary)
	if s == "" {
		return ""
	}
	if f := l.claimedMissingFile(s, wrote); f != "" {
		return "the summary says you created `" + f + "`, but no tool call this run wrote it and it does not exist in the workspace"
	}
	if !ranCommand && ranClaimRe.MatchString(ranIntoRe.ReplaceAllString(s, "encountered")) {
		return "the summary says you ran a command, but run_command never executed this run"
	}
	return ""
}

// claimedMissingFile returns the first filename the summary claims to have produced
// that neither exists on disk nor was written by a tool this run.
func (l *Loop) claimedMissingFile(summary string, wrote map[string]bool) string {
	low := strings.ToLower(summary)
	claimed := false
	for _, v := range createVerbs {
		if strings.Contains(low, v) {
			claimed = true
			break
		}
	}
	if !claimed {
		return ""
	}
	// No root means no tree to check against — and NewWorkspace("") resolves against
	// the process cwd, which would adjudicate a claim against the wrong directory.
	if l.Root == "" {
		return ""
	}
	ws, err := tools.NewWorkspace(l.Root)
	if err != nil {
		return "" // cannot check the tree ⇒ never block on a guess
	}
	for _, m := range fileTokenRe.FindAllStringSubmatchIndex(summary, -1) {
		tok := summary[m[2]:m[3]]
		if notFileExt[strings.ToLower(filepath.Ext(tok)[1:])] {
			continue
		}
		if wroteThisRun(wrote, tok) {
			continue
		}
		// A mention in a negative clause is a statement ABOUT absence, not a claim.
		if from := m[2] - 56; negationRe.MatchString(summary[max(0, from):m[2]]) {
			continue
		}
		abs, rerr := ws.Resolve(tok)
		if rerr != nil {
			continue // outside the workspace: not kloo's to adjudicate
		}
		if _, serr := os.Stat(abs); serr == nil {
			continue // it is really there
		}
		return tok
	}
	return ""
}

// wroteThisRun matches a summary's token against the paths tools landed, which may be
// spelled differently ("./reply.md", "docs/reply.md") than the prose names them.
func wroteThisRun(wrote map[string]bool, tok string) bool {
	t := filepath.Clean(tok)
	for p := range wrote {
		c := filepath.Clean(p)
		if c == t || filepath.Base(c) == filepath.Base(t) {
			return true
		}
	}
	return false
}

// finishClaimCorrective hands the model the specific gap rather than a generic
// scolding, so the next turn has one obvious move: perform the missing action.
func finishClaimCorrective(missing string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: "Hold on — " + missing + ". " +
		"kloo reports what its tools did, never what a summary says, so that claim cannot be passed to the user. " +
		"If the action is still required by the task, DO IT THIS TURN with a real tool call. " +
		"If it turns out not to be required, call finish again with a summary that claims only what actually happened."}
}
