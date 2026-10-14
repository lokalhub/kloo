package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/session"
)

// TestIsContinuationOnlyOnTheWholeMessage: "continue" alone is a resume; a real task
// that merely contains the word is a new task and must not be rewritten into one.
func TestIsContinuationOnlyOnTheWholeMessage(t *testing.T) {
	for _, yes := range []string{"continue", "Continue.", "  keep going  ", "please continue", "carry on!", "resume"} {
		if !isContinuation(yes) {
			t.Errorf("%q should be a continuation", yes)
		}
	}
	for _, no := range []string{"continue the migration in src/db", "keep going until the tests pass", "resume the upload worker", "", "fix the build"} {
		if isContinuation(no) {
			t.Errorf("%q is a task, not a bare continuation", no)
		}
	}
}

// TestResumeBriefHandsBackTheThread is the point of the change: "continue" used to
// start a fresh run whose only knowledge of the last one was a one-line recap, so
// the model re-derived the problem and sometimes redid work. The brief must carry
// the original ask, the actions already taken, the failing verify, and an explicit
// instruction not to start over.
func TestResumeBriefHandsBackTheThread(t *testing.T) {
	prev := &runMemo{
		task: "make the three tabs Home/Apps/Profile",
		log: []session.DisplayItem{
			{Kind: dispUser, Text: "make the three tabs Home/Apps/Profile"},
			{Kind: dispTool, Text: "read src/app/tabs/tabs.routes.ts"},
			{Kind: dispTool, Text: "edited src/app/tabs/tabs.routes.ts"},
			{Kind: dispTool, Text: "ran: npm run build [exit 1]"},
		},
		rep: &agent.Report{
			Reason: agent.ReasonChurn, Steps: 6, Elapsed: 31 * time.Second,
			FinalVerify: agent.VerifyResult{Command: "npm run build", ExitCode: 1, Stderr: "TS2304: cannot find name 'Profile'"},
			Transcript:  []llm.Message{{Role: llm.RoleAssistant, Content: "I renamed two of the three tabs."}},
		},
	}
	got := resumeBrief("continue", prev)
	for _, want := range []string{
		"must NOT start it over",
		"make the three tabs Home/Apps/Profile", // the original ask
		"edited src/app/tabs/tabs.routes.ts",    // what it got through
		"do not repeat these",
		"npm run build",                      // the gate it was failing
		"TS2304: cannot find name 'Profile'", // and why
		"I renamed two of the three tabs.",   // where it left off
		"decide the NEXT action",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("resume brief missing %q:\n%s", want, got)
		}
	}
}

// TestResumeBriefSaysWhenWorkWasRolledBack: a rolled-back run's edits are GONE, and
// a model told to "pick up where you left off" would otherwise assume they landed.
func TestResumeBriefSaysWhenWorkWasRolledBack(t *testing.T) {
	got := resumeBrief("continue", &runMemo{
		task: "add the endpoint",
		rep:  &agent.Report{Reason: agent.ReasonBudgetExceeded, Steps: 40, RolledBack: true},
	})
	if !strings.Contains(got, "rolled back") || !strings.Contains(got, "must be made again") {
		t.Errorf("a rolled-back resume must say the changes are gone:\n%s", got)
	}
}

// TestResumeBriefBoundsTheActionLog: a long run's whole log would crowd out the task
// it is resuming, so only the tail is replayed.
func TestResumeBriefBoundsTheActionLog(t *testing.T) {
	prev := &runMemo{task: "a long job", rep: &agent.Report{Reason: agent.ReasonChurn}}
	for i := 0; i < 60; i++ {
		prev.log = append(prev.log, session.DisplayItem{Kind: dispTool, Text: "read file" + string(rune('a'+i%26)) + ".go"})
	}
	got := resumeBrief("continue", prev)
	if n := strings.Count(got, "  - "); n > maxResumeActions {
		t.Errorf("replayed %d actions, want at most %d", n, maxResumeActions)
	}
}
