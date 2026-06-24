package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// ─── test seam stubs ────────────────────────────────────────────────────────

type stubVerifier struct {
	results  []VerifyResult
	i        int
	onVerify func()
}

func (v *stubVerifier) Verify(ctx context.Context) VerifyResult {
	if v.onVerify != nil {
		v.onVerify()
	}
	r := v.results[min(v.i, len(v.results)-1)]
	v.i++
	return r
}

type stubBudget struct {
	tripAt int // steps at which Check trips (0 ⇒ never)
	kind   BudgetKind
	steps  int
	tokens int
}

func (b *stubBudget) Observe(s int)   { b.steps = s }
func (b *stubBudget) AddTokens(n int) { b.tokens += n }
func (b *stubBudget) Check() (bool, BudgetKind) {
	if b.tripAt > 0 && b.steps >= b.tripAt {
		return true, b.kind
	}
	return false, ""
}
func (b *stubBudget) Stats() BudgetStats {
	return BudgetStats{Steps: b.steps, MaxSteps: b.tripAt, Tokens: b.tokens, MaxTokens: 0}
}
func (b *stubBudget) Reset() { b.steps, b.tokens = 0, 0 }

type stubChurn struct {
	churnAfter int // churn once Observe has been called this many times (0 ⇒ never)
	observes   int
	kind       ChurnKind
}

func (c *stubChurn) Observe(t Turn) { c.observes++ }
func (c *stubChurn) Check() (bool, ChurnKind) {
	if c.churnAfter > 0 && c.observes >= c.churnAfter {
		return true, c.kind
	}
	return false, ""
}
func (c *stubChurn) Artifact() string { return "repeated-artifact" }
func (c *stubChurn) Reset()           { c.observes = 0 }

// recordTool is a registry Tool that records the calls dispatched to it.
type recordTool struct {
	name  string
	calls *[]tools.Call
}

func (t recordTool) Name() string        { return t.name }
func (t recordTool) Description() string { return "record" }
func (t recordTool) Schema() tools.ParamSchema {
	return tools.ParamSchema{Properties: map[string]tools.Property{"path": {Type: "string"}}, Required: []string{"path"}}
}
func (t recordTool) Invoke(ctx context.Context, c tools.Call) (tools.Result, error) {
	*t.calls = append(*t.calls, c)
	return tools.Result{Output: "ok"}, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ─── helpers ────────────────────────────────────────────────────────────────

type tcSpec struct {
	name string
	args map[string]any
}

// toolResp renders a ChatResponse carrying the given native tool calls + usage.
func toolResp(t *testing.T, totalTokens int, calls ...tcSpec) string {
	t.Helper()
	var tcs []llm.ToolCall
	for i, c := range calls {
		ab, _ := json.Marshal(c.args)
		tcs = append(tcs, llm.ToolCall{ID: fmt.Sprintf("c%d", i), Type: "function", Function: llm.FunctionCall{Name: c.name, Arguments: string(ab)}})
	}
	resp := llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: tcs}}},
		Usage:   llm.Usage{TotalTokens: totalTokens},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// newLoop builds a loop wired to the mocked server and a registry with a
// recording read_file tool. Returns the loop + the recorded calls slice.
func newLoop(t *testing.T, srv *llmtest.Server, v Verifier, b Budget, c ChurnDetector) (*Loop, *[]tools.Call) {
	t.Helper()
	var calls []tools.Call
	reg := tools.NewRegistry()
	reg.Register(recordTool{name: "read_file", calls: &calls})
	reg.Register(recordTool{name: "edit_file", calls: &calls})   // an edit ⇒ a verify-pass counts as success
	reg.Register(recordTool{name: "run_command", calls: &calls}) // shell action (Acted) with no edit signature
	return &Loop{
		Client:   llm.New(srv.URL+"/v1", "test-model"),
		Adapter:  tools.NativeFCAdapter{},
		Registry: reg,
		Verifier: v,
		Budget:   b,
		Churn:    c,
		System:   "you are kloo",
	}, &calls
}

func passResult() VerifyResult { return VerifyResult{Command: "test", ExitCode: 0, Passed: true} }
func failResult() VerifyResult {
	return VerifyResult{Command: "test", ExitCode: 1, Passed: false, Stdout: "FAIL"}
}

// proseResp is a model reply with prose content and NO tool call (a conversational
// answer) — what triggers the ReasonAnswered path.
func proseResp(t *testing.T, text string) string {
	t.Helper()
	resp := llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, Content: text}}},
		Usage:   llm.Usage{TotalTokens: 5},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// ─── tests ──────────────────────────────────────────────────────────────────

func TestLoopTransitionsInOrder(t *testing.T) {
	// An edit_file so the passing verify counts as success (verify-pass is success
	// only after a real change this run).
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 10, tcSpec{"edit_file", map[string]any{"path": "a.go"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{}, &stubChurn{})

	var seq []State
	loop.OnState = func(s State) { seq = append(seq, s) }

	rep, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Errorf("reason = %q, want success", rep.Reason)
	}
	want := []State{StateAct, StateApply, StateVerify, StateDecide, StateStop}
	if fmt.Sprint(seq) != fmt.Sprint(want) {
		t.Errorf("state sequence = %v, want %v", seq, want)
	}
}

// TestLoopConversationalReplyAnswers: a prose reply with no tool call (on the turn
// and its corrective re-prompt) stops the loop calmly with ReasonAnswered — not
// ReasonError/ReasonChurn. This is the "asked a question / said hi" case; the answer
// is already streamed, so the loop must not churn on a (failing) verify.
func TestLoopConversationalReplyAnswers(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: proseResp(t, "I listed the dir to understand the project.")},
		llmtest.Mock{Body: proseResp(t, "No tool needed — just answering your question.")},
	)
	loop, calls := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult()}}, &stubBudget{}, &stubChurn{})
	rep, err := loop.Run(context.Background(), "why did you list the dir?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Errorf("reason = %q, want %q (conversational reply)", rep.Reason, ReasonAnswered)
	}
	if len(*calls) != 0 {
		t.Errorf("no tools should have been dispatched for a prose answer, got %v", *calls)
	}
}

// TestLoopReadonlyRunDoesNotChurn is the end-to-end guard for the "hello churns"
// bug: with the REAL churn detector and a verify that always fails (e.g. the
// default `go test` on a non-Go app), a run that only reads/explores — never
// edits — must NOT trip repeated-failure churn, no matter how many failing steps.
// It runs until the model answers in prose (ReasonAnswered) instead.
func TestLoopReadonlyRunDoesNotChurn(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 3, tcSpec{"read_file", map[string]any{"path": "a.go"}})},
		llmtest.Mock{Body: toolResp(t, 3, tcSpec{"read_file", map[string]any{"path": "b.go"}})},
		llmtest.Mock{Body: toolResp(t, 3, tcSpec{"read_file", map[string]any{"path": "c.go"}})},
		llmtest.Mock{Body: toolResp(t, 3, tcSpec{"read_file", map[string]any{"path": "d.go"}})},
		llmtest.Mock{Body: proseResp(t, "This is an Angular project — what would you like me to do?")},
	)
	// churn after 3 repeats; verify fails every step; budget never trips.
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult()}}, &stubBudget{}, NewChurnDetector(3))
	rep, err := loop.Run(context.Background(), "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonChurn {
		t.Fatalf("read-only run churned with no edits (the 'hello churns' bug); reason=%q", rep.Reason)
	}
	if rep.Reason != ReasonAnswered {
		t.Errorf("reason = %q, want %q", rep.Reason, ReasonAnswered)
	}
}

// TestLoopSessionHistoryReachesModel is the end-to-end guard for session memory:
// after a first run, seeding Loop.SessionHistory with its transcript (what the TUI
// runner does) makes a SECOND run's request to the model carry that prior context —
// so a follow-up like "what's the issue?" is answerable. Uses the REAL working
// memory and inspects the captured request body.
func TestLoopSessionHistoryReachesModel(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: proseResp(t, "Done — I renamed the tabs.")},                    // run 1: answers, no tool call
		llmtest.Mock{Body: proseResp(t, "The build failed because of a missing import.")}, // run 2
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult()}}, &stubBudget{}, &stubChurn{})
	loop.Memory = NewWorkingMemory() // session history flows through working memory

	rep1, err := loop.Run(context.Background(), "rework the tabs into Home/Apps/Profile")
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if len(rep1.Transcript) == 0 {
		t.Fatal("run 1 report carried no transcript to seed the session")
	}

	// Seed the next run with run 1's transcript (the runner's job) and ask a follow-up.
	loop.SessionHistory = rep1.Transcript
	if _, err := loop.Run(context.Background(), "what's the issue?"); err != nil {
		t.Fatalf("run 2: %v", err)
	}

	// The SECOND request must include run 1's task as carried context.
	reqs := srv.Requests()
	if len(reqs) < 2 {
		t.Fatalf("expected ≥2 requests, got %d", len(reqs))
	}
	last := string(reqs[len(reqs)-1])
	if !strings.Contains(last, "rework the tabs into Home/Apps/Profile") {
		t.Errorf("second run's request did not carry prior-session context:\n%s", last)
	}
	if !strings.Contains(last, "what's the issue?") {
		t.Errorf("second run's request is missing the current task")
	}
}

func TestLoopOneToolPerTurn(t *testing.T) {
	body := toolResp(t, 5,
		tcSpec{"edit_file", map[string]any{"path": "first.go"}}, // edit ⇒ verify-pass = success this turn
		tcSpec{"read_file", map[string]any{"path": "second.go"}},
	)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: body})
	loop, calls := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].Args["path"] != "first.go" {
		t.Errorf("expected only the first tool dispatched, got %+v", *calls)
	}
	if len(rep.Ignored) != 1 || rep.Ignored[0] != "read_file" {
		t.Errorf("expected the second call recorded as ignored, got %v", rep.Ignored)
	}
}

func TestLoopStopsOnVerifySuccess(t *testing.T) {
	// edit_file each turn so the passing verify counts as success (verify-pass is
	// success only after a real change this run).
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 1, tcSpec{"edit_file", map[string]any{"path": "a"}})})
	// First verify fails, second passes → loop runs two turns then stops success.
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult(), passResult()}}, &stubBudget{}, &stubChurn{})

	rep, _ := loop.Run(context.Background(), "fix it")
	if rep.Reason != ReasonSuccess {
		t.Errorf("reason = %q, want success", rep.Reason)
	}
	if rep.Steps != 2 {
		t.Errorf("steps = %d, want 2", rep.Steps)
	}
	if !rep.FinalVerify.Passed {
		t.Errorf("final verify should be the green one")
	}
}

func TestLoopStopsOnBudgetSeam(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "a"}})})
	// Budget trips at step 1 → loop stops before acting at all.
	loop, calls := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult()}}, &stubBudget{tripAt: 1, kind: BudgetSteps}, &stubChurn{})

	rep, _ := loop.Run(context.Background(), "x")
	if rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want budget-exceeded", rep.Reason)
	}
	if rep.Budget == nil || rep.Budget.Kind != BudgetSteps {
		t.Errorf("budget evidence missing/wrong: %+v", rep.Budget)
	}
	if len(*calls) != 0 {
		t.Errorf("no tool should dispatch when budget trips first")
	}
}

func TestLoopStopsOnChurnSeam(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "a"}})})
	// Churn after one observation; verify always fails so the loop continues to
	// turn 2 where churn.Check trips.
	loop, _ := newLoop(t, srv,
		&stubVerifier{results: []VerifyResult{failResult()}},
		&stubBudget{},
		&stubChurn{churnAfter: 1, kind: ChurnRepeatedFailure},
	)
	rep, _ := loop.Run(context.Background(), "x")
	if rep.Reason != ReasonChurn {
		t.Fatalf("reason = %q, want churn", rep.Reason)
	}
	if rep.Churn == nil || rep.Churn.Kind != ChurnRepeatedFailure {
		t.Errorf("churn evidence missing/wrong: %+v", rep.Churn)
	}
}

// TestLoopBoundsConversationHistory proves the per-request prompt stays bounded
// across a long run: each request carries at most MaxConversation history
// messages (plus the system prompt), even though the transcript keeps growing.
func TestLoopBoundsConversationHistory(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "a"}})})
	// A real budget that stops after 12 steps; verify never passes; churn never fires.
	loop, _ := newLoop(t, srv,
		&stubVerifier{results: []VerifyResult{failResult()}},
		NewBudget(config.Config{MaxSteps: 12}, time.Now),
		&stubChurn{},
	)
	const maxConv = 6
	loop.MaxConversation = maxConv

	rep, _ := loop.Run(context.Background(), "look around for a long time")
	if rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("expected the long run to hit the step budget, got %q", rep.Reason)
	}

	reqs := srv.Requests()
	if len(reqs) < 10 {
		t.Fatalf("expected a long run (many requests) to exercise the bound, got %d", len(reqs))
	}
	maxSeen := 0
	for i, raw := range reqs {
		var body struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request %d not JSON: %v", i, err)
		}
		if n := len(body.Messages); n > maxSeen {
			maxSeen = n
		}
	}
	// system prompt (1) + at most maxConv history messages.
	if maxSeen > maxConv+1 {
		t.Errorf("per-request message count = %d, want ≤ %d (transcript not bounded)", maxSeen, maxConv+1)
	}
	// Sanity: without bounding, a 12-step run would send ~23 history messages on
	// the last turn — so the bound is doing real work.
	if maxSeen >= 20 {
		t.Errorf("transcript appears unbounded: max per-request messages = %d", maxSeen)
	}
}

func TestLoopCtxCancelInterrupts(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "a"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult()}}, &stubBudget{}, &stubChurn{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the loop starts

	rep, _ := loop.Run(ctx, "x")
	if rep.Reason != ReasonInterrupted {
		t.Errorf("reason = %q, want interrupted", rep.Reason)
	}
}
