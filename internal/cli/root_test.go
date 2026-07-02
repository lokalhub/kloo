package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
)

// runCmd builds the root command with injected deps and the given args.
func runCmd(t *testing.T, deps Deps, args ...string) (out, errOut *bytes.Buffer, err error) {
	t.Helper()
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	deps.Out, deps.Err = out, errOut
	if deps.Getenv == nil {
		deps.Getenv = func(string) string { return "" } // isolate from real env
	}
	cmd := NewRootCmd(deps)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out, errOut, err
}

// TestFlagsMapToConfig: every documented flag parses and reaches config.Resolve,
// and the task argument is forwarded to the autonomous loop.
func TestFlagsMapToConfig(t *testing.T) {
	var gotCfg config.Config
	var gotTask string
	deps := Deps{
		RunHeadless: func(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error {
			gotCfg = cfg
			gotTask = task
			return nil
		},
	}

	out, _, err := runCmd(t, deps,
		"--model", "alt-model",
		"--endpoint", "http://host:9000/v1",
		"--mode", "manual",
		"--max-steps", "9",
		"--temperature", "0.5",
		"--llm-max-retries", "0",
		"--no-think",
		"--json-only",
		"--status-file", filepath.Join(t.TempDir(), "status.json"),
		"say hi",
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if gotCfg.Model != "alt-model" {
		t.Errorf("model = %q, want alt-model", gotCfg.Model)
	}
	if gotCfg.Endpoint != "http://host:9000/v1" {
		t.Errorf("endpoint = %q", gotCfg.Endpoint)
	}
	if gotCfg.Mode != "manual" {
		t.Errorf("mode = %q, want manual", gotCfg.Mode)
	}
	if gotCfg.MaxSteps != 9 {
		t.Errorf("maxSteps = %d, want 9", gotCfg.MaxSteps)
	}
	if gotCfg.Temperature != 0.5 {
		t.Errorf("temperature = %v, want 0.5", gotCfg.Temperature)
	}
	if gotCfg.LLMMaxRetries != 0 {
		t.Errorf("llm max retries = %d, want explicit zero", gotCfg.LLMMaxRetries)
	}
	if gotTask != "say hi" {
		t.Errorf("task forwarded = %q, want \"say hi\"", gotTask)
	}
	if !gotCfg.NoThink {
		t.Errorf("--no-think should reach config, cfg=%+v", gotCfg)
	}
	if !gotCfg.JSONOnly || gotCfg.StatusFile == "" {
		t.Errorf("--json-only/--status-file should reach config, cfg=%+v", gotCfg)
	}
	if out.Len() != 0 {
		t.Errorf("stubbed loop should not print, out = %q", out.String())
	}
}

// TestDefaultsWhenNoFlags: with no flags, resolved config is the documented
// defaults (proves unset flags do NOT override via Changed()-gating).
func TestDefaultsWhenNoFlags(t *testing.T) {
	var gotCfg config.Config
	deps := Deps{RunHeadless: func(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error {
		gotCfg = cfg
		return nil
	}}

	if _, _, err := runCmd(t, deps, "do a thing"); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotCfg.Model != config.DefaultModel || gotCfg.Endpoint != config.DefaultEndpoint {
		t.Errorf("want defaults, got model=%q endpoint=%q", gotCfg.Model, gotCfg.Endpoint)
	}
}

// TestNoArgsLaunchesTUI: no task argument launches the interactive TUI session
// (with the resolved config + the default verify command), not a single-call path.
func TestNoArgsLaunchesTUI(t *testing.T) {
	var launched bool
	var gotCfg config.Config
	var gotVerify string
	var gotLint lintOpts
	var gotProfile string
	var gotGetenv func(string) string
	profile := filepath.Join(t.TempDir(), "profiles.json")
	getenv := func(k string) string {
		if k == "KLOO_TEST_THREAD" {
			return "threaded"
		}
		return ""
	}
	deps := Deps{
		LaunchTUI: func(cfg config.Config, baseFlags config.Flags, verifyCmd string, lint lintOpts, sess SessionOpts, profilePath string, getenv func(string) string) error {
			launched = true
			gotCfg = cfg
			gotVerify = verifyCmd
			gotLint = lint
			gotProfile = profilePath
			gotGetenv = getenv
			return nil
		},
		Getenv: getenv,
	}

	statusFile := filepath.Join(t.TempDir(), "status.json")
	if _, _, err := runCmd(t, deps, "--profile", profile, "--no-think", "--json-only", "--status-file", statusFile); err != nil {
		t.Fatalf("no-arg invocation should exit 0, got %v", err)
	}
	if !launched {
		t.Error("no task argument should launch the TUI")
	}
	if gotCfg.Model != config.DefaultModel {
		t.Errorf("TUI should receive resolved config, got model %q", gotCfg.Model)
	}
	if !gotCfg.NoThink {
		t.Errorf("TUI should receive --no-think in resolved config")
	}
	if !gotCfg.JSONOnly || gotCfg.StatusFile != statusFile {
		t.Errorf("TUI should receive observability flags, cfg=%+v", gotCfg)
	}
	// --verify now defaults to "" (no longer go-test): the actual command is
	// auto-detected downstream in defaultLaunchTUI, so the flag passes "" through.
	if gotVerify != "" {
		t.Errorf("TUI should receive an empty verify command (auto-detected downstream), got %q", gotVerify)
	}
	// No --lint/--no-lint flags ⇒ zero lintOpts (resolved/auto-detected downstream).
	if (gotLint != lintOpts{}) {
		t.Errorf("TUI should receive zero lintOpts by default, got %+v", gotLint)
	}
	if gotProfile != profile {
		t.Errorf("TUI should receive profile path %q, got %q", profile, gotProfile)
	}
	if gotGetenv == nil || gotGetenv("KLOO_TEST_THREAD") != "threaded" {
		t.Errorf("TUI should receive deps.Getenv")
	}
}

// TestTaskRoutesToRunHeadless: any task arg runs the non-interactive autonomous
// loop (not the TUI), passing
// the resolved config, task, and verify command.
func TestTaskRoutesToRunHeadless(t *testing.T) {
	var ran bool
	var gotTask, gotVerify string
	var gotLint lintOpts
	var gotCfg config.Config
	tuiCalled := false
	deps := Deps{
		LaunchTUI: func(cfg config.Config, baseFlags config.Flags, verifyCmd string, lint lintOpts, sess SessionOpts, profilePath string, getenv func(string) string) error {
			tuiCalled = true
			return nil
		},
		RunHeadless: func(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error {
			ran, gotCfg, gotTask, gotVerify, gotLint = true, cfg, task, verifyCmd, lint
			return nil
		},
	}

	statusFile := filepath.Join(t.TempDir(), "status.json")
	if _, _, err := runCmd(t, deps, "--no-think", "--json-only", "--status-file", statusFile, "--verify", "npm run build", "--lint", "golangci-lint run", "rework the tabs"); err != nil {
		t.Fatalf("task should run autonomous loop, got %v", err)
	}
	if !ran {
		t.Fatal("task should route to RunHeadless")
	}
	if tuiCalled {
		t.Error("task must not use the TUI")
	}
	if gotTask != "rework the tabs" {
		t.Errorf("task = %q, want \"rework the tabs\"", gotTask)
	}
	if gotVerify != "npm run build" {
		t.Errorf("verify = %q, want \"npm run build\"", gotVerify)
	}
	if gotLint.Override != "golangci-lint run" || gotLint.Disabled {
		t.Errorf("--lint should thread into RunHeadless as an override, got %+v", gotLint)
	}
	if gotCfg.Model != config.DefaultModel {
		t.Errorf("loop should receive resolved config, got model %q", gotCfg.Model)
	}
	if !gotCfg.NoThink {
		t.Errorf("loop should receive --no-think in resolved config")
	}
	if !gotCfg.JSONOnly || gotCfg.StatusFile != statusFile {
		t.Errorf("loop should receive observability flags, cfg=%+v", gotCfg)
	}
}

func TestVerifyRoutingNoDeprecationWarning(t *testing.T) {
	var gotVerify string
	deps := Deps{
		RunHeadless: func(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error {
			gotVerify = verifyCmd
			return nil
		},
	}
	out, errOut, err := runCmd(t, deps, "--verify", "go test ./...", "fix x")
	if err != nil {
		t.Fatalf("--verify task should run loop: %v", err)
	}
	if gotVerify != "go test ./..." {
		t.Fatalf("verify override = %q, want go test ./...", gotVerify)
	}
	if strings.Contains(out.String(), "deprecated") || strings.Contains(errOut.String(), "deprecated") {
		t.Fatalf("--verify should not emit deprecation warnings\nstdout=%s\nstderr=%s", out.String(), errOut.String())
	}
}

func TestBenchmarkModeRoutesAndImpliesJSON(t *testing.T) {
	var gotCfg config.Config
	var gotTask string
	deps := Deps{
		RunHeadless: func(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error {
			gotCfg = cfg
			gotTask = task
			return nil
		},
	}
	if _, _, err := runCmd(t, deps, "--benchmark", "fix benchmark"); err != nil {
		t.Fatalf("--benchmark task should run loop: %v", err)
	}
	if gotTask != "fix benchmark" {
		t.Fatalf("task = %q", gotTask)
	}
	if !gotCfg.BenchmarkMode || !gotCfg.JSONSummary {
		t.Fatalf("--benchmark should set BenchmarkMode and JSONSummary, cfg=%+v", gotCfg)
	}
}

func TestBenchmarkWithoutTaskReturnsExit17(t *testing.T) {
	_, _, err := runCmd(t, Deps{}, "--benchmark")
	var ee exitError
	if !errors.As(err, &ee) {
		t.Fatalf("want exitError, got %T %v", err, err)
	}
	if ee.code != benchmarkExitConfigError {
		t.Fatalf("exit code = %d, want %d", ee.code, benchmarkExitConfigError)
	}
}

func TestHeadlessFlagRemoved(t *testing.T) {
	_, _, err := runCmd(t, Deps{}, "--headless", "x")
	if err == nil || !strings.Contains(err.Error(), "unknown flag") {
		t.Fatalf("--headless should be removed as an unknown flag, got %v", err)
	}
}

// TestLintOptsResolution: --lint/--no-lint and KLOO_LINT/KLOO_NO_LINT resolve into
// lintOpts with flag-beats-env precedence, mirroring the verify/MCP knobs. KLOO_LINT
// "0"/"false" disables (it is not treated as a command), per the EnvMCP convention.
func TestLintOptsResolution(t *testing.T) {
	none := func(string) string { return "" }
	env := func(pairs map[string]string) func(string) string {
		return func(k string) string { return pairs[k] }
	}

	cases := []struct {
		name          string
		lintChanged   bool
		noLintChanged bool
		flagLint      string
		flagNoLint    bool
		getenv        func(string) string
		want          lintOpts
	}{
		{"--no-lint disables", false, true, "", true, none, lintOpts{Disabled: true}},
		{"--lint sets override", true, false, "x", false, none, lintOpts{Override: "x"}},
		{"KLOO_NO_LINT=1 disables", false, false, "", false, env(map[string]string{config.EnvNoLint: "1"}), lintOpts{Disabled: true}},
		{"KLOO_LINT=y sets override", false, false, "", false, env(map[string]string{config.EnvLint: "y"}), lintOpts{Override: "y"}},
		{"flag beats env override", true, false, "flag", false, env(map[string]string{config.EnvLint: "env"}), lintOpts{Override: "flag"}},
		{"explicit --no-lint=false beats KLOO_NO_LINT", false, true, "", false, env(map[string]string{config.EnvNoLint: "1"}), lintOpts{Disabled: false}},
		{"KLOO_LINT=0 disables, not override", false, false, "", false, env(map[string]string{config.EnvLint: "0"}), lintOpts{Disabled: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lintOptsFrom(tc.lintChanged, tc.noLintChanged, tc.flagLint, tc.flagNoLint, tc.getenv)
			if got != tc.want {
				t.Errorf("lintOptsFrom = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestLintFlagsRegistered: --lint and --no-lint appear in help (they exist and are
// parseable in the real command path).
func TestLintFlagsRegistered(t *testing.T) {
	out, _, err := runCmd(t, Deps{}, "--help")
	if err != nil {
		t.Fatalf("--help should exit 0, got %v", err)
	}
	for _, want := range []string{"--lint", "--no-lint"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help missing flag %s; out = %q", want, out.String())
		}
	}
}

// TestHelpFlagExitsZero: --help prints usage and exits 0.
func TestHelpFlagExitsZero(t *testing.T) {
	out, _, err := runCmd(t, Deps{}, "--help")
	if err != nil {
		t.Fatalf("--help should exit 0, got %v", err)
	}
	for _, want := range []string{"--model", "--endpoint", "--mode", "--profile", "--max-steps", "--temperature"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help missing flag %s; out = %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "--headless") || strings.Contains(out.String(), "deprecated") {
		t.Errorf("help should not mention --headless or deprecated verify, out = %q", out.String())
	}
}

func TestDoctorJSONRedactsAndDoesNotRun(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "profiles.json")
	if err := os.WriteFile(profile, []byte(`{
		"providers": {"hosted": {"endpoint": "https://example.test/v1", "apiKey": "${SECRET_KEY}"}},
		"mcpServers": {"memory": {"command": "mem", "env": {"TOKEN": "${SECRET_KEY}"}}},
		"local": {"maxContextTokens": 12345, "toolFormat": "xml"},
		"memory": {"enabled": true, "server": "memory", "recallTool": "recall", "storeTool": "store", "maxRecallBytes": 4096, "storeOnFailure": true}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECRET_KEY", "test-token-value")
	tuiCalled, headlessCalled := false, false
	deps := Deps{
		LaunchTUI: func(cfg config.Config, baseFlags config.Flags, verifyCmd string, lint lintOpts, sess SessionOpts, profilePath string, getenv func(string) string) error {
			tuiCalled = true
			return nil
		},
		RunHeadless: func(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error {
			headlessCalled = true
			return nil
		},
		Getenv: func(k string) string { return "" },
	}
	out, _, err := runCmd(t, deps, "doctor", "--json", "--profile", profile, "--provider", "hosted", "--model", "local",
		"--llm-max-retries", "0",
		"--llm-retry-base-delay", "500ms",
		"--llm-retry-max-delay", "5s",
		"--llm-cold-load-timeout", "7m",
		"--llm-stream-idle-timeout", "3m",
		"--allow-env", "ADMIN_PASSWORD")
	if err != nil {
		t.Fatalf("doctor --json: %v", err)
	}
	if tuiCalled || headlessCalled {
		t.Fatalf("doctor must not start run deps: tui=%t headless=%t", tuiCalled, headlessCalled)
	}
	if strings.Contains(out.String(), "test-token-value") {
		t.Fatalf("doctor leaked a secret:\n%s", out.String())
	}
	var got resolvedConfigDiagnostic
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("doctor emitted invalid JSON: %v\n%s", err, out.String())
	}
	if got.Provider != "hosted" || got.Endpoint != "https://example.test/v1" || got.APIKey != (secretState{Set: true, Redacted: true}) {
		t.Fatalf("provider/API key diagnostics wrong: %+v", got)
	}
	if got.Verify.Source != "auto-detect" || got.Verify.Command != "go test ./..." {
		t.Fatalf("verify diagnostic wrong: %+v", got.Verify)
	}
	if got.MCP.ConfiguredServers != 1 || len(got.MCP.EnabledServers) != 1 || got.MCP.EnabledServers[0] != "memory" {
		t.Fatalf("mcp diagnostic wrong: %+v", got.MCP)
	}
	if got.Memory != (memoryDiagnostic{Enabled: true, Server: "memory", RecallTool: "recall", StoreTool: "store", MaxRecallBytes: 4096, StoreOnFailure: true}) {
		t.Fatalf("memory diagnostic wrong: %+v", got.Memory)
	}
	if len(got.AllowedEnvNames) != 1 || got.AllowedEnvNames[0] != "ADMIN_PASSWORD" {
		t.Fatalf("allowed env should expose names only, got %+v", got.AllowedEnvNames)
	}
	if got.Retry.LLMMaxRetries != 0 || got.Retry.LLMRetryBaseDelay != "500ms" ||
		got.Retry.LLMRetryMaxDelay != "5s" || got.Retry.LLMColdLoadTimeout != "7m" ||
		got.Retry.LLMStreamIdleTimeout != "3m" {
		t.Fatalf("retry diagnostic wrong: %+v", got.Retry)
	}
}

// TestUnknownFlagErrors: an unknown flag is a clear error.
func TestUnknownFlagErrors(t *testing.T) {
	_, _, err := runCmd(t, Deps{}, "--bogus", "x")
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("want unknown-flag error, got %v", err)
	}
}

// TestScopeFlagsMapToConfig: the A1/A2/A4/A7 flags parse and reach config.Resolve.
func TestScopeFlagsMapToConfig(t *testing.T) {
	var gotCfg config.Config
	deps := Deps{RunHeadless: func(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error {
		gotCfg = cfg
		return nil
	}}
	_, _, err := runCmd(t, deps,
		"--allow", "src/**,cmd/**",
		"--deny", ".env",
		"--read-only", "tests/**",
		"--patch-only",
		"--stop-on", "off-scope-edit,repeated-verify=3",
		"do the thing",
	)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if len(gotCfg.ScopeAllow) != 2 || gotCfg.ScopeAllow[0] != "src/**" || gotCfg.ScopeAllow[1] != "cmd/**" {
		t.Errorf("ScopeAllow = %v (comma-split expected)", gotCfg.ScopeAllow)
	}
	if len(gotCfg.ScopeDeny) != 1 || gotCfg.ScopeDeny[0] != ".env" {
		t.Errorf("ScopeDeny = %v", gotCfg.ScopeDeny)
	}
	if len(gotCfg.ScopeReadOnly) != 1 || gotCfg.ScopeReadOnly[0] != "tests/**" {
		t.Errorf("ScopeReadOnly = %v", gotCfg.ScopeReadOnly)
	}
	if !gotCfg.PatchOnly {
		t.Errorf("PatchOnly should be true")
	}
	if !gotCfg.StopOn.OffScopeEdit || gotCfg.StopOn.RepeatedVerify != 3 {
		t.Errorf("StopOn = %+v", gotCfg.StopOn)
	}
}

// TestInvalidStopOnRuleFailsResolution: a malformed --stop-on rule fails config
// resolution (config_error) before any run starts.
func TestInvalidStopOnRuleFailsResolution(t *testing.T) {
	called := false
	deps := Deps{RunHeadless: func(cfg config.Config, task, verifyCmd string, lint lintOpts, out io.Writer) error {
		called = true
		return nil
	}}
	_, _, err := runCmd(t, deps, "--stop-on", "not-a-rule", "do it")
	if err == nil {
		t.Fatal("expected an error for an invalid --stop-on rule")
	}
	if called {
		t.Fatal("the loop must not run when config resolution fails")
	}
}
