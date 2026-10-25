package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// TestVerifyTimeoutFlagExists: the incident's error message told the user to
// "re-run it with a larger timeout_seconds". There was no flag, no env var and no
// profile key that could do that — CommandVerifier had the option and the CLI never
// called it. A message prescribing a remedy the product does not offer is the defect;
// this pins the remedy into existence.
func TestVerifyTimeoutFlagExists(t *testing.T) {
	cmd := NewRootCmd(Deps{})
	f := cmd.Flags().Lookup("verify-timeout")
	if f == nil {
		t.Fatalf("--verify-timeout is not registered on the root command")
	}
	// duration, not a bare number of seconds: the sibling timeouts (--llm-cold-load-timeout,
	// --llm-stream-idle-timeout) are durations, and the value this incident needed is far
	// more legible as "12m" than as "720".
	if f.Value.Type() != "duration" {
		t.Errorf("--verify-timeout is %q, want duration to match the other timeout flags", f.Value.Type())
	}
	if f.DefValue != config.DefaultVerifyTimeout.String() {
		t.Errorf("--verify-timeout default = %q, want %q", f.DefValue, config.DefaultVerifyTimeout)
	}
}

// TestVerifyTimeoutDefaultIsTheRealCeiling: the five minutes in the incident was NOT
// a verify setting — it was tools.DefaultCommandTimeout, inherited because the
// interactive path passed no option at all. The new default must be that same number,
// or naming it would have silently changed every existing run's ceiling.
func TestVerifyTimeoutDefaultIsTheRealCeiling(t *testing.T) {
	if config.DefaultVerifyTimeout != tools.DefaultCommandTimeout {
		t.Errorf("DefaultVerifyTimeout = %s but the inherited run_command ceiling is %s — the default is no longer byte-identical",
			config.DefaultVerifyTimeout, tools.DefaultCommandTimeout)
	}
}

// TestVerifyTimeoutFlagReachesConfig walks the flag through the real cobra parse and
// the real config resolution, because "the flag exists" and "the flag does anything"
// were different facts here: the option already existed in internal/agent and was
// never called from a user-reachable path.
func TestVerifyTimeoutFlagReachesConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want time.Duration
	}{
		{"passed", []string{"--verify-timeout", "12m"}, 12 * time.Minute},
		{"absent", nil, config.DefaultVerifyTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got config.Config
			cmd := NewRootCmd(Deps{
				Getenv: func(string) string { return "" },
				LaunchTUI: func(cfg config.Config, _ config.Flags, _ string, _ lintOpts, _ SessionOpts, _ string, _ func(string) string) error {
					got = cfg
					return nil
				},
			})
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if got.VerifyTimeout != tc.want {
				t.Errorf("resolved VerifyTimeout = %s, want %s", got.VerifyTimeout, tc.want)
			}
		})
	}
}

// TestVerifyTimeoutReachesBothVerifiers: the value has to arrive at the verifier the
// run actually uses, and there are TWO construction sites. The headless one already
// passed a hard-coded 300; the TUI one — the path a person is most likely to be
// sitting in front of when a gate takes twelve minutes — passed nothing. Both must
// now source the ceiling from the resolved config, or the flag is inert in whichever
// one was missed.
func TestVerifyTimeoutReachesBothVerifiers(t *testing.T) {
	for _, file := range []string{"headless.go", "tui.go"} {
		src := readCLISource(t, file)
		if !strings.Contains(src, "agent.WithVerifyTimeout(cfg.VerifyTimeout.Seconds())") {
			t.Errorf("%s: the verifier is not built with the resolved verify timeout", file)
		}
	}
}

// readCLISource returns a cli source file's text. The assertion is on the source
// rather than on a built verifier because the TUI path takes over the terminal the
// moment it is entered, so its construction site cannot be exercised from a test —
// the literal's own shape is what stays checkable there.
func readCLISource(t *testing.T, file string) string {
	t.Helper()
	path := filepath.Join(".", file)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestVerifyTimeoutBoundsTheRealRun is the outcome-level proof: a real headless run,
// a gate that sleeps past its ceiling, and the resolved VerifyTimeout as the only
// thing deciding when it dies. Asserting that the option is passed would not have
// caught a value plumbed to the wrong place; this asserts the kill.
func TestVerifyTimeoutBoundsTheRealRun(t *testing.T) {
	t.Chdir(t.TempDir())

	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeAnswerStream(t, "right\n"), SSE: true})
	cfg := config.Config{
		Endpoint: srv.URL + "/v1", Model: "test-model", ToolFormat: config.DefaultToolFormat,
		MaxSteps: 2, ChurnRounds: 20, MaxContextTokens: 8000,
		VerifyTimeout: 300 * time.Millisecond,
	}

	var out strings.Builder
	start := time.Now()
	_ = defaultRunHeadless(cfg, "write the answer", "sleep 30", lintOpts{}, &out)
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("a 300ms verify ceiling did not bound the run: %s", elapsed)
	}
	got := out.String()
	if !strings.Contains(got, "killed after 300ms") {
		t.Errorf("the run must report the ceiling it was killed at:\n%s", got)
	}
	if !strings.Contains(got, "--verify-timeout") {
		t.Errorf("the run must name the flag that raises the ceiling:\n%s", got)
	}
}
