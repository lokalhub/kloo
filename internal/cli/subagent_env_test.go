package cli

import "testing"

// TestEnvIntParsesTheSubagentCap guards the wiring that silently failed once: the
// KLOO_SUBAGENT_STEPS edit did not apply after a gofmt realignment, so the cap
// existed in the agent package but was never set from the environment.
func TestEnvIntParsesTheSubagentCap(t *testing.T) {
	t.Setenv("KLOO_SUBAGENT_STEPS", "12")
	if got := envInt("KLOO_SUBAGENT_STEPS"); got != 12 {
		t.Errorf("envInt = %d, want 12", got)
	}
	for _, bad := range []string{"", "x", "-3"} {
		t.Setenv("KLOO_SUBAGENT_STEPS", bad)
		if got := envInt("KLOO_SUBAGENT_STEPS"); got != 0 {
			t.Errorf("envInt(%q) = %d, want 0", bad, got)
		}
	}
}
