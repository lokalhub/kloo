package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// The question this pins: are project instructions (AGENTS.md, which the CLI
// concatenates into Loop.System) still in the prompt AFTER a compaction, or does a
// long run quietly lose the rules it was given?
//
// They survive by construction rather than by care: buildPrompt rebuilds message 0
// from l.System every single turn, and working memory is handed the CONVERSATION
// plus the system block's token COUNT — never its text — so compaction has nothing
// to shed there. This test exists because that is an invariant worth failing loudly
// on, and because it is the exact failure a skills system would introduce if a skill
// body were injected as a conversation message instead: compaction would eventually
// drop it, mid-run, with no signal.
func TestProjectInstructionsSurviveCompaction(t *testing.T) {
	const marker = "NEVER EDIT generated/ BY HAND."
	const nFiles = 14
	root := bigRepo(t, nFiles, 3200)
	srv := llmtest.Sequence(t, dodScript(t, nFiles)...)
	loop := dodLoop(t, root, srv, NewWorkingMemory())
	// What the CLI does: SystemPrompt() + scope suffix + AGENTS.md, as one string.
	loop.System = "you are kloo, fix the failing check\n\n## Project instructions\n" + marker

	rep, err := loop.Run(context.Background(), "make the failing check pass by reading the files first")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Compactions == 0 {
		t.Fatalf("the run never compacted, so this proves nothing about compaction (%s)", rep.String())
	}

	reqs := srv.Requests()
	var turns, withMarker, inSystem int
	for _, raw := range reqs {
		if len(raw) == 0 {
			continue // the bodyless /models catalog probe
		}
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		turns++
		for _, m := range body.Messages {
			if strings.Contains(m.Content, marker) {
				withMarker++
				if m.Role == "system" {
					inSystem++
				}
				break
			}
		}
	}
	if turns == 0 {
		t.Fatal("captured no model calls")
	}
	if withMarker != turns {
		t.Errorf("project instructions present in %d of %d turns, want all of them (%d compactions)",
			withMarker, turns, rep.Compactions)
	}
	if inSystem != turns {
		t.Errorf("instructions carried outside the system message on %d turns — they belong in the "+
			"stable prefix, or the prompt cache re-prefills and compaction can reach them",
			turns-inSystem)
	}
	t.Logf("%d turns, %d compactions, instructions in the system block on all %d", turns, rep.Compactions, inSystem)
}
