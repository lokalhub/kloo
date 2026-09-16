package cli

import (
	"strings"
	"testing"
)

// TestSubagentDirectiveOnlyWhenEnabled: the prompt must be byte-identical by
// default, and must mention delegation when the tool IS offered — a weak model
// will not infer the strategy from a tool schema alone, and an unmentioned tool
// is the same as no tool.
func TestSubagentDirectiveOnlyWhenEnabled(t *testing.T) {
	t.Setenv("KLOO_SUBAGENTS", "")
	if got := SystemPrompt(); got != defaultSystemPrompt {
		t.Error("default prompt changed while subagents are disabled")
	}
	t.Setenv("KLOO_SUBAGENTS", "1")
	got := SystemPrompt()
	if !strings.Contains(got, "task tool") {
		t.Error("subagent directive missing while enabled")
	}
	if !strings.Contains(got, "NEVER undo or redo something") {
		t.Error("safety guidance lost when appending the subagent directive")
	}
}
