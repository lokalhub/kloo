package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
)

// fakeClient is an offline llm.LLMClient that records what it was asked to do
// and replays a canned streamed reply.
type fakeClient struct {
	reply    string
	gotReq   llm.ChatRequest
	streamed bool
}

func (f *fakeClient) Complete(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	f.gotReq = req
	return llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, Content: f.reply}}},
	}, nil
}

func (f *fakeClient) Stream(ctx context.Context, req llm.ChatRequest, onDelta func(llm.Delta) error) (llm.ChatResponse, error) {
	f.streamed = true
	f.gotReq = req
	if onDelta != nil {
		if err := onDelta(llm.Delta{Role: llm.RoleAssistant, Content: f.reply}); err != nil {
			return llm.ChatResponse{}, err
		}
	}
	return llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, Content: f.reply}}}}, nil
}

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
// and the task argument is forwarded to the client.
func TestFlagsMapToConfig(t *testing.T) {
	var gotCfg config.Config
	fake := &fakeClient{reply: "ok"}
	deps := Deps{
		NewClient: func(cfg config.Config) llm.LLMClient { gotCfg = cfg; return fake },
	}

	out, _, err := runCmd(t, deps,
		"--model", "alt-model",
		"--endpoint", "http://host:9000/v1",
		"--mode", "manual",
		"--max-steps", "9",
		"--temperature", "0.5",
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
	if !fake.streamed {
		t.Error("expected the client to be streamed")
	}
	if got := fake.gotReq.Messages[len(fake.gotReq.Messages)-1].Content; got != "say hi" {
		t.Errorf("task forwarded = %q, want \"say hi\"", got)
	}
	if !strings.Contains(out.String(), "ok") {
		t.Errorf("streamed reply not printed; out = %q", out.String())
	}
}

// TestDefaultsWhenNoFlags: with no flags, resolved config is the documented
// defaults (proves unset flags do NOT override via Changed()-gating).
func TestDefaultsWhenNoFlags(t *testing.T) {
	var gotCfg config.Config
	deps := Deps{NewClient: func(cfg config.Config) llm.LLMClient { gotCfg = cfg; return &fakeClient{reply: "hi"} }}

	if _, _, err := runCmd(t, deps, "do a thing"); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotCfg.Model != config.DefaultModel || gotCfg.Endpoint != config.DefaultEndpoint {
		t.Errorf("want defaults, got model=%q endpoint=%q", gotCfg.Model, gotCfg.Endpoint)
	}
}

// TestNoArgsLaunchesTUI: no task argument launches the interactive TUI session
// (with the resolved config + the default verify command), not the one-shot path.
func TestNoArgsLaunchesTUI(t *testing.T) {
	var launched bool
	var gotCfg config.Config
	var gotVerify string
	clientCalled := false
	deps := Deps{
		NewClient: func(cfg config.Config) llm.LLMClient { clientCalled = true; return &fakeClient{} },
		LaunchTUI: func(cfg config.Config, verifyCmd string, sess SessionOpts) error {
			launched = true
			gotCfg = cfg
			gotVerify = verifyCmd
			return nil
		},
	}

	if _, _, err := runCmd(t, deps); err != nil {
		t.Fatalf("no-arg invocation should exit 0, got %v", err)
	}
	if !launched {
		t.Error("no task argument should launch the TUI")
	}
	if clientCalled {
		t.Error("the one-shot client should not be constructed for the interactive session")
	}
	if gotCfg.Model != config.DefaultModel {
		t.Errorf("TUI should receive resolved config, got model %q", gotCfg.Model)
	}
	// --verify now defaults to "" (no longer go-test): the actual command is
	// auto-detected downstream in defaultLaunchTUI, so the flag passes "" through.
	if gotVerify != "" {
		t.Errorf("TUI should receive an empty verify command (auto-detected downstream), got %q", gotVerify)
	}
}

// TestHeadlessWithTaskRoutesToRunHeadless: a task arg + --headless runs the
// non-interactive autonomous loop (not the one-shot stream, not the TUI), passing
// the resolved config, task, and verify command.
func TestHeadlessWithTaskRoutesToRunHeadless(t *testing.T) {
	var ran bool
	var gotTask, gotVerify string
	var gotCfg config.Config
	clientCalled, tuiCalled := false, false
	deps := Deps{
		NewClient: func(cfg config.Config) llm.LLMClient { clientCalled = true; return &fakeClient{} },
		LaunchTUI: func(cfg config.Config, verifyCmd string, sess SessionOpts) error { tuiCalled = true; return nil },
		RunHeadless: func(cfg config.Config, task, verifyCmd string, out io.Writer) error {
			ran, gotCfg, gotTask, gotVerify = true, cfg, task, verifyCmd
			return nil
		},
	}

	if _, _, err := runCmd(t, deps, "--headless", "--verify", "npm run build", "rework the tabs"); err != nil {
		t.Fatalf("--headless with a task should exit 0, got %v", err)
	}
	if !ran {
		t.Fatal("--headless with a task should route to RunHeadless")
	}
	if clientCalled || tuiCalled {
		t.Error("--headless must not use the one-shot client or the TUI")
	}
	if gotTask != "rework the tabs" {
		t.Errorf("task = %q, want \"rework the tabs\"", gotTask)
	}
	if gotVerify != "npm run build" {
		t.Errorf("verify = %q, want \"npm run build\"", gotVerify)
	}
	if gotCfg.Model != config.DefaultModel {
		t.Errorf("headless should receive resolved config, got model %q", gotCfg.Model)
	}
}

// TestHeadlessWithoutTaskErrors: --headless with no task argument is a usage
// error (it must not silently launch the TUI).
func TestHeadlessWithoutTaskErrors(t *testing.T) {
	tuiCalled := false
	deps := Deps{
		LaunchTUI:   func(cfg config.Config, verifyCmd string, sess SessionOpts) error { tuiCalled = true; return nil },
		RunHeadless: func(cfg config.Config, task, verifyCmd string, out io.Writer) error { return nil },
	}
	if _, _, err := runCmd(t, deps, "--headless"); err == nil {
		t.Fatal("--headless with no task should be an error")
	}
	if tuiCalled {
		t.Error("--headless with no task must not fall back to launching the TUI")
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
