package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// runFailsTool is a run_command stub that ALWAYS exits non-zero (no Go error — a
// failing command is captured in Result.ExitCode, like the real tool), to exercise
// the failure-recovery branch of the promise rail.
type runFailsTool struct{}

func (runFailsTool) Name() string        { return "run_command" }
func (runFailsTool) Description() string { return "fails" }
func (runFailsTool) Schema() tools.ParamSchema {
	return tools.ParamSchema{Properties: map[string]tools.Property{"command": {Type: "string"}}, Required: []string{"command"}}
}
func (runFailsTool) Invoke(ctx context.Context, c tools.Call) (tools.Result, error) {
	return tools.Result{ExitCode: 1, Stderr: "npm ERR! missing script: start"}, nil
}

// TestPromiseRailRecoversAfterFailingCommand: a single failing run_command (exit 1)
// followed by a tool-free prose reply must NOT silently stop the run as `answered`
// ("a wrong command would stop it"). The failure arms the rail even without promise
// language, so the model is nudged to recover and acts again.
func TestPromiseRailRecoversAfterFailingCommand(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"run_command", map[string]any{"command": "npm start"}})}, // fails (exit 1)
		llmtest.Mock{Body: proseResp(t, "The simulation did not start; npm start is not configured.")},    // gives up in prose (no promise words)
		llmtest.Mock{Body: proseResp(t, "The simulation did not start; npm start is not configured.")},    // act() re-prompt: still prose → ErrNoToolCall → recovery nudge
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "package.json"}})},   // recovers (acts)
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "inspected scripts"}})},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 100}, &stubChurn{})
	reg := tools.NewRegistry()
	reg.Register(runFailsTool{})
	reg.Register(recordTool{name: "read_file", calls: new([]tools.Call)})
	loop.Registry = reg

	rep, err := loop.Run(context.Background(), "run the simulation")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonAnswered {
		t.Fatalf("reason = answered — a failing command must not stop the run; the rail should nudge recovery")
	}
	var nudged bool
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "command FAILED") {
			nudged = true
		}
	}
	if !nudged {
		t.Error("expected the failure-recovery nudge after the failing command")
	}
}

// TestPromiseRailRescuesNarratedAction: the model ends a turn by NARRATING a next
// action ("Let me run the worker simulation") with no tool call — the pattern that
// makes a run keep stopping as `answered` mid-task. The promised-but-didn't-act rail
// nudges it once; the model then actually emits the call, so the run does NOT stop
// as answered. Each prose turn costs TWO mocks: act() re-prompts once internally
// before it surfaces ErrNoToolCall.
func TestPromiseRailRescuesNarratedAction(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: proseResp(t, "Let me run the worker simulation")},                         // initial prose
		llmtest.Mock{Body: proseResp(t, "Let me run the worker simulation now")},                     // act() re-prompt: still prose → ErrNoToolCall → nudge
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "worker.ts"}})}, // model ACTS (emits a real call) → reset
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "ran it"}})},    // done
	)
	loop, calls := newLoop(t, srv, nil, &stubBudget{tripAt: 100}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "simulate this locally")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonAnswered {
		t.Fatalf("reason = answered — the promise rail should have nudged the model into acting, not stopped")
	}
	var acted bool
	for _, c := range *calls {
		if c.Name == "read_file" {
			acted = true
		}
	}
	if !acted {
		t.Error("expected the model to emit a real tool call after the promise nudge")
	}
	var nudged bool
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "did NOT call a tool") {
			nudged = true
		}
	}
	if !nudged {
		t.Error("expected the promise-to-act nudge in the transcript")
	}
}

// TestPromiseRailBoundedForPureNarrator: a model that ONLY ever narrates ("let me
// check…") and never acts is nudged up to the limit, then the run accepts the calm
// answered-stop — the rail rescues, it doesn't spin forever. With limit=2 and two
// prose mocks per episode, the run stops on the 3rd episode (6 mocks).
func TestPromiseRailBoundedForPureNarrator(t *testing.T) {
	p := func() llmtest.Mock { return llmtest.Mock{Body: proseResp(t, "Let me check the config")} }
	srv := llmtest.Sequence(t,
		p(), p(), // episode 1 → nudge 1
		p(), p(), // episode 2 → nudge 2
		p(), p(), // episode 3 → limit reached → answered
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 100}, &stubChurn{})
	loop.PromiseNudgeLimit = 2 // tight for the test

	rep, err := loop.Run(context.Background(), "simulate this locally")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered (pure narrator stops after the nudge budget)", rep.Reason)
	}
}

// TestPromiseRailIgnoresGenuineAnswer: a real conversational answer with NO action
// language stops immediately as answered — the rail must not false-rescue a genuine
// reply (e.g. one containing "let me know if…").
func TestPromiseRailIgnoresGenuineAnswer(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: proseResp(t, "The build is green and everything works. Let me know if you need anything else.")},
		llmtest.Mock{Body: proseResp(t, "The build is green and everything works. Let me know if you need anything else.")},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 100}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "is the build ok?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered (a genuine answer is not a promise)", rep.Reason)
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "did NOT call a tool") {
			t.Error("the promise nudge must NOT fire on a genuine conversational answer")
		}
	}
}

func TestPromisesToAct(t *testing.T) {
	yes := []string{
		"Let me run the worker simulation",
		"let me check if the lokal CLI is available",
		"I'll run the test now",
		"Now let me examine a few more files",
		"I'm going to try running it",
		"let's see what happens",
		"I'll build and upload the bundle",                    // broadened verb
		"Let me start by running the deploy",                  // "start by"
		"I'll deploy the new version",                         // "i'll deploy"
		"Here is the command:\n```bash\nlokal mp deploy\n```", // fenced code block, no tool call
	}
	for _, s := range yes {
		if !promisesToAct(s) {
			t.Errorf("promisesToAct(%q) = false, want true", s)
		}
	}
	no := []string{
		"The build is green. Let me know if you need anything else.",
		"Here is how the simulation works: it spins up a worker.",
		"That's done — the tests pass.",
		"I cannot read that directory; it escapes the workspace root.",
	}
	for _, s := range no {
		if promisesToAct(s) {
			t.Errorf("promisesToAct(%q) = true, want false", s)
		}
	}
}
