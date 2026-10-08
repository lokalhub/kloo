package cli

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
)

// TestNoChatGateFlagDisablesTheGate: --no-chat-gate reaches cfg.NoChatGate through
// the real root command, and the TUI's ChatSystem (which is what enables the gate
// in internal/agent) goes empty — the same way headless and subagents disable it.
func TestNoChatGateFlagDisablesTheGate(t *testing.T) {
	noProfile := filepath.Join(t.TempDir(), "none.json") // isolate from the user's real profile
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"on by default", nil, false},
		{"flag disables", []string{"--no-chat-gate"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got bool
			deps := Deps{
				Getenv: func(string) string { return "" },
				LaunchTUI: func(cfg config.Config, _ config.Flags, _ string, _ lintOpts, _ SessionOpts, _ string, _ func(string) string) error {
					got = cfg.NoChatGate
					return nil
				},
				Out: io.Discard,
				Err: io.Discard,
			}
			cmd := NewRootCmd(deps)
			cmd.SetArgs(append([]string{"--profile", noProfile}, tc.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got != tc.want {
				t.Fatalf("cfg.NoChatGate = %v, want %v", got, tc.want)
			}
			if sys := chatGateSystem(got); (sys == "") != tc.want {
				t.Fatalf("chatGateSystem(%v) empty = %v, want %v", got, sys == "", tc.want)
			}
		})
	}
}
