package cli

import (
	"strings"
	"testing"
)

// TestGrokPromptIsActionFirst: the default prompt's first instruction after "make
// a tool call" is a prohibition, and on kloo-bench 30 of 62 glimmer runs never
// called an edit tool. The flagged prompt must lead with acting, and must still
// carry the two guardrails that encode real incidents (never undo an explicit
// request; verify is a signal, not a goal).
func TestGrokPromptIsActionFirst(t *testing.T) {
	t.Setenv("KLOO_GROK_PROMPT", "1")
	t.Setenv("KLOO_SUBAGENTS", "")
	p := SystemPrompt()
	for _, want := range []string{
		"DO IT in the current turn",
		"Reading is not progress",
		"only when tool output supports the claim",
		"never undo or redo something the user explicitly asked for",
		"it is a signal, not the goal",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("grok-aligned prompt missing %q", want)
		}
	}
}

// TestGrokPromptIsOptIn: the released binary's prompt must not change until the
// bench says it should.
func TestGrokPromptIsOptIn(t *testing.T) {
	t.Setenv("KLOO_GROK_PROMPT", "")
	t.Setenv("KLOO_SUBAGENTS", "")
	if SystemPrompt() != defaultSystemPrompt {
		t.Fatal("default prompt changed with the flag off")
	}
}
