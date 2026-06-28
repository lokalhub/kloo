package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// TestWireMCPNonFatalBadServer: wireMCP connects a configured (but broken) server
// non-fatally — the registry still has its builtins, the run continues, and the
// skip is logged. (The positive "good server contributes tools" path is proven in
// internal/mcp's manager_test, which carries the SDK; cli stays SDK-free.)
func TestWireMCPNonFatalBadServer(t *testing.T) {
	ws, err := tools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }

	cfg := config.Config{
		MCPServers:         map[string]config.MCPServerEntry{"bad": {Command: "kloo-definitely-not-real-zzz"}},
		MCPMaxExposedTools: 16,
	}
	reg, _, closeMCP := wireMCP(context.Background(), cfg, ws, logf)
	defer closeMCP()

	if _, ok := reg.Lookup("read_file"); !ok {
		t.Error("builtins must remain when an MCP server fails")
	}
	for _, tl := range reg.Tools() {
		if strings.Contains(tl.Name(), "__") {
			t.Errorf("no MCP tool should register from a failed server, got %s", tl.Name())
		}
	}
	if !strings.Contains(strings.Join(lines, "\n"), `skipped "bad"`) {
		t.Errorf("bad server not logged as a non-fatal skip: %v", lines)
	}
}

// TestWireMCPDisabledIsSilent: MCPDisabled ⇒ wireMCP attempts no connection, logs
// nothing, and returns a no-op Close (mock §3 regression safety).
func TestWireMCPDisabledIsSilent(t *testing.T) {
	ws, err := tools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	logf := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }

	cfg := config.Config{
		MCPDisabled: true,
		MCPServers:  map[string]config.MCPServerEntry{"x": {Command: "whatever"}},
	}
	reg, _, closeMCP := wireMCP(context.Background(), cfg, ws, logf)
	if len(lines) != 0 {
		t.Errorf("disabled MCP must log nothing, got: %v", lines)
	}
	if _, ok := reg.Lookup("read_file"); !ok {
		t.Error("builtins must be present")
	}
	if err := closeMCP(); err != nil {
		t.Errorf("disabled Close should be a no-op, got %v", err)
	}
}

// TestNoMCPFlagAndEnvPrecedence: --no-mcp (flag) > KLOO_MCP (env) > default, wired
// through the real root command to cfg.MCPDisabled (captured via a fake LaunchTUI).
func TestNoMCPFlagAndEnvPrecedence(t *testing.T) {
	noProfile := filepath.Join(t.TempDir(), "none.json") // isolate from the user's real profile
	cases := []struct {
		name         string
		args         []string
		env          map[string]string
		wantDisabled bool
	}{
		{"default enabled", nil, nil, false},
		{"env 0 disables", nil, map[string]string{config.EnvMCP: "0"}, true},
		{"flag disables", []string{"--no-mcp"}, nil, true},
		{"flag overrides env-enable", []string{"--no-mcp"}, map[string]string{config.EnvMCP: "1"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotDisabled bool
			deps := Deps{
				Getenv: func(k string) string { return tc.env[k] },
				LaunchTUI: func(cfg config.Config, _ string, _ lintOpts, _ SessionOpts, _ string, _ func(string) string) error {
					gotDisabled = cfg.MCPDisabled
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
			if gotDisabled != tc.wantDisabled {
				t.Errorf("MCPDisabled = %v, want %v", gotDisabled, tc.wantDisabled)
			}
		})
	}
}

// TestHeadlessWiresMCPNonFatal: the headless path connects MCP (skip line proves
// Connect ran with the configured server) and a broken server does not break the
// run — it still reaches success on the real verify signal.
func TestHeadlessWiresMCPNonFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "answer.txt"), []byte("wrong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeAnswerStream(t, "right\n"), SSE: true})
	cfg := config.Config{
		Endpoint: srv.URL + "/v1", Model: "test-model", ToolFormat: config.DefaultToolFormat,
		MaxSteps: 10, ChurnRounds: 10, MaxContextTokens: 8000,
		MCPServers:         map[string]config.MCPServerEntry{"bad": {Command: "kloo-definitely-not-real-zzz"}},
		MCPMaxExposedTools: 16,
	}

	var out strings.Builder
	if err := defaultRunHeadless(cfg, "make the check pass", "grep -qx right answer.txt", lintOpts{}, &out); err != nil {
		t.Fatalf("non-fatal MCP: headless run should still succeed: %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, `mcp · skipped "bad"`) {
		t.Errorf("headless did not wire MCP Connect (no skip line):\n%s", got)
	}
	if !strings.Contains(got, "SUCCESS") {
		t.Errorf("a broken MCP server must not break the run:\n%s", got)
	}
}

// TestHeadlessMCPDisabledNoLines: with MCP disabled, the headless output carries no
// mcp lines (byte-identical-to-pre-MCP behaviour; mock §3).
func TestHeadlessMCPDisabledNoLines(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "answer.txt"), []byte("wrong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	srv := llmtest.Sequence(t, llmtest.Mock{Body: writeAnswerStream(t, "right\n"), SSE: true})
	cfg := config.Config{
		Endpoint: srv.URL + "/v1", Model: "test-model", ToolFormat: config.DefaultToolFormat,
		MaxSteps: 10, ChurnRounds: 10, MaxContextTokens: 8000,
		MCPDisabled: true,
		MCPServers:  map[string]config.MCPServerEntry{"bad": {Command: "kloo-nope"}},
	}
	var out strings.Builder
	if err := defaultRunHeadless(cfg, "make the check pass", "grep -qx right answer.txt", lintOpts{}, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "mcp ·") {
		t.Errorf("disabled MCP must emit no mcp lines:\n%s", out.String())
	}
}
