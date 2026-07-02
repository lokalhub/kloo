package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

type recordingLLM struct {
	completeReqs []llm.ChatRequest
	streamReqs   []llm.ChatRequest
	streamDeltas []llm.Delta
	resp         llm.ChatResponse
}

func (c *recordingLLM) Complete(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	c.completeReqs = append(c.completeReqs, req)
	return c.resp, nil
}

func (c *recordingLLM) Stream(ctx context.Context, req llm.ChatRequest, onDelta func(llm.Delta) error) (llm.ChatResponse, error) {
	c.streamReqs = append(c.streamReqs, req)
	for _, d := range c.streamDeltas {
		if onDelta != nil {
			if err := onDelta(d); err != nil {
				return llm.ChatResponse{}, err
			}
		}
	}
	return c.resp, nil
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
		Endpoint: srv.URL + "/v1",
		Model:    "test-model",
		System:   "you are kloo",
		// Production config.Resolve supplies this default. Individual tests set 0
		// explicitly when they need to prove retry is disabled.
		LLMRetries: DefaultLLMRetries,
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

func toolResponse(calls ...tcSpec) llm.ChatResponse {
	var tcs []llm.ToolCall
	for i, c := range calls {
		ab, _ := json.Marshal(c.args)
		tcs = append(tcs, llm.ToolCall{ID: fmt.Sprintf("c%d", i), Type: "function", Function: llm.FunctionCall{Name: c.name, Arguments: string(ab)}})
	}
	return llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: tcs}}},
		Usage:   llm.Usage{TotalTokens: 1},
	}
}

// newRealEditLoop builds a loop wired to the mocked server and a REAL tool
// registry (DefaultRegistry) jailed to a temp workspace seeded with fileName =
// content, so edit_file actually runs the engine and the repair builder can
// re-read the file. Returns the loop + the canonical workspace root.
func newRealEditLoop(t *testing.T, srv *llmtest.Server, fileName, content string, v Verifier, b Budget, c ChurnDetector) (*Loop, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, fileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := tools.NewWorkspace(canon)
	if err != nil {
		t.Fatal(err)
	}
	return &Loop{
		Client:   llm.New(srv.URL+"/v1", "test-model"),
		Adapter:  tools.NativeFCAdapter{},
		Registry: tools.DefaultRegistry(ws),
		Verifier: v,
		Budget:   b,
		Churn:    c,
		Root:     canon,
		Endpoint: srv.URL + "/v1",
		Model:    "test-model",
		System:   "you are kloo",
	}, canon
}

// editFileCall renders a native edit_file tool call with a bare SEARCH/REPLACE
// diff (search/replace bodies should carry their trailing newline).
func editFileCall(t *testing.T, path, search, replace string, totalTokens int) string {
	t.Helper()
	diff := "<<<<<<< SEARCH\n" + search + "=======\n" + replace + ">>>>>>> REPLACE\n"
	return toolResp(t, totalTokens, tcSpec{"edit_file", map[string]any{"path": path, "diff": diff}})
}

// transcriptContains reports whether any message in the transcript contains all
// of the given substrings in a single message.
func msgWithAll(msgs []llm.Message, subs ...string) bool {
	for _, m := range msgs {
		all := true
		for _, s := range subs {
			if !strings.Contains(m.Content, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// countMsgsContaining counts transcript messages containing sub.
func countMsgsContaining(msgs []llm.Message, sub string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Content, sub) {
			n++
		}
	}
	return n
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

// TestLoopConfirmFinishNudgeOnActedBareStop: a run that EXECUTED a real action
// (run_command) and then tries to stop with a bare prose "done" — no tool call, no
// finish — is nudged ONCE to call finish or do the next step. This is the premature
// `answered` stop seen live: dsv4 ran step 1 of a multi-step deploy, said "done", and
// stopped before the remaining steps. The nudge adds EXACTLY one round: the second bare
// turn then stops calmly as answered (the flag is one-shot), so the run terminates at
// step 3 instead of step 2.
func TestLoopConfirmFinishNudgeOnActedBareStop(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 1, tcSpec{"run_command", map[string]any{"path": "x"}})},
		llmtest.Mock{Body: proseResp(t, "All done — the deploy is complete.")},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult()}}, &stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "register the version then upgrade the instance")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered (after the one-shot confirm-finish nudge)", rep.Reason)
	}
	if rep.Steps != 3 {
		t.Errorf("steps = %d, want 3 — the confirm-finish nudge must add exactly one round (without it the acted run would stop at step 2)", rep.Steps)
	}
	// The rail fire is recorded so it is observable in the run summary / headless JSON.
	if got := rep.RailFires[string(RailConfirmFinish)]; got != 1 {
		t.Errorf("RailFires[%q] = %d, want 1 (the fire must be tallied for validation)", RailConfirmFinish, got)
	}
}

// TestLoopReadOnlyRunGetsConfirmFinishNudge: a run that only READ (read_file) then
// answers in prose now fires the confirm-finish rail once — it returns ReasonAnswered
// at step 3, not step 2. This catches the pattern where a model reads task files and
// reports findings without actually making edits (seen with dsv4-flash on coding tasks).
// The one-shot flag means a genuinely-done answer still stops calmly at step 3.
func TestLoopReadOnlyRunGetsConfirmFinishNudge(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "x"}})},
		llmtest.Mock{Body: proseResp(t, "Here is the answer to your question.")},
		llmtest.Mock{Body: proseResp(t, "Task is complete — no more steps needed.")},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult()}}, &stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "what does x do?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered (after the one-shot confirm-finish nudge)", rep.Reason)
	}
	if rep.Steps != 3 {
		t.Errorf("steps = %d, want 3 — the confirm-finish rail now fires on any tool-using run (step > 1)", rep.Steps)
	}
	if got := rep.RailFires[string(RailConfirmFinish)]; got != 1 {
		t.Errorf("RailFires[%q] = %d, want 1", RailConfirmFinish, got)
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
	// This test repeats reads to drive a 12-step run; disable the repetition AND
	// exploration rails (off their defaults) so neither pre-empts the step budget.
	loop.RepeatNudgeRounds, loop.RepeatAbortRounds = 1000, 1000
	loop.ExploreNudgeRounds, loop.ExploreAbortRounds = 1000, 1000

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

// ─── repair-loop tests (task 03) ────────────────────────────────────────────

// TestLoopRepairsNonMatchingEdit is the acceptance test: a 1st edit_file with a
// non-matching SEARCH yields a repair observation (actual content + fix
// instruction); the model's 2nd edit applies; the run reaches ReasonSuccess after
// a green verify.
func TestLoopRepairsNonMatchingEdit(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "WRONG\n", "right\n", 5)}, // turn 1: no-match
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "wrong\n", "right\n", 5)}, // turn 2: applies
	)
	loop, root := newRealEditLoop(t, srv, "answer.txt", "wrong\n",
		&stubVerifier{results: []VerifyResult{failResult(), passResult()}},
		&stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "make answer.txt say right")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success", rep.Reason)
	}
	if !msgWithAll(rep.Transcript, "Failing SEARCH block", "wrong", "Fix this edit") {
		t.Errorf("transcript missing the repair observation (actual content + fix instruction)")
	}
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "right\n" {
		t.Errorf("answer.txt = %q, want %q", got, "right\n")
	}
}

// TestLoopRepairIsBounded proves the per-target enrichment cap: a model that emits
// a DISTINCT non-matching edit_file to the same path every turn gets at most
// MaxRepairAttempts (2) enriched observations; later failing edits get the BARE
// error; the run terminates via budget (not infinitely).
func TestLoopRepairIsBounded(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "NOPE1\n", "x\n", 5)},
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "NOPE2\n", "x\n", 5)},
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "NOPE3\n", "x\n", 5)},
		llmtest.Mock{Body: editFileCall(t, "answer.txt", "NOPE4\n", "x\n", 5)},
	)
	loop, _ := newRealEditLoop(t, srv, "answer.txt", "wrong\n",
		&stubVerifier{results: []VerifyResult{failResult()}},
		&stubBudget{tripAt: 5, kind: BudgetSteps}, &stubChurn{})
	loop.MaxRepairAttempts = 2

	rep, err := loop.Run(context.Background(), "spin on distinct non-matching edits")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonBudgetExceeded {
		t.Fatalf("reason = %q, want budget-exceeded (terminated, not infinite)", rep.Reason)
	}
	if n := countMsgsContaining(rep.Transcript, "Failing SEARCH block"); n != 2 {
		t.Errorf("enriched observations = %d, want exactly 2 (the cap)", n)
	}
	// Past the cap the bare observation is used.
	if countMsgsContaining(rep.Transcript, "tool edit_file error:") == 0 {
		t.Errorf("expected bare 'tool edit_file error:' observations past the cap")
	}
}

// TestLoopRepairRepeatedEditChurns proves the repair path does not suppress the
// existing churn rail: when the model repeats the SAME non-matching edit, the real
// churn detector still fires (it sees the repeated editSignature, not the repair
// text), and at most MaxRepairAttempts enriched observations appeared.
func TestLoopRepairRepeatedEditChurns(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: editFileCall(t, "answer.txt", "SAME\n", "x\n", 5)}) // same edit every turn
	loop, _ := newRealEditLoop(t, srv, "answer.txt", "wrong\n",
		&stubVerifier{results: []VerifyResult{failResult()}},
		&stubBudget{}, NewChurnDetector(2))
	loop.MaxRepairAttempts = 2

	rep, err := loop.Run(context.Background(), "repeat the same broken edit")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonChurn {
		t.Fatalf("reason = %q, want churn (the repair path must not suppress churn)", rep.Reason)
	}
	if n := countMsgsContaining(rep.Transcript, "Failing SEARCH block"); n > 2 {
		t.Errorf("enriched observations = %d, want <= MaxRepairAttempts (2)", n)
	}
}

// TestLoopCleanEditNoRepairObservation (O5) proves clean-apply is byte-identical:
// a matching first edit reaches ReasonSuccess with NO repair text in the
// transcript and a single apply round-trip.
func TestLoopCleanEditNoRepairObservation(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: editFileCall(t, "answer.txt", "wrong\n", "right\n", 5)}) // matches first try
	loop, root := newRealEditLoop(t, srv, "answer.txt", "wrong\n",
		&stubVerifier{results: []VerifyResult{passResult()}},
		&stubBudget{}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix it in one shot")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success", rep.Reason)
	}
	if countMsgsContaining(rep.Transcript, "Failing SEARCH block") != 0 {
		t.Errorf("clean apply must produce no repair observation in the transcript")
	}
	if rep.Steps != 1 {
		t.Errorf("steps = %d, want 1 (single round-trip)", rep.Steps)
	}
	if got := readFile(t, filepath.Join(root, "answer.txt")); got != "right\n" {
		t.Errorf("answer.txt = %q, want %q", got, "right\n")
	}
}

func TestLoopNoThinkReachesChatRequests(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(recordTool{name: "read_file", calls: &[]tools.Call{}})
	resp := toolResponse(tcSpec{"read_file", map[string]any{"path": "README.md"}})

	t.Run("non-streaming act", func(t *testing.T) {
		client := &recordingLLM{resp: resp}
		loop := &Loop{
			Client:      client,
			Adapter:     tools.NativeFCAdapter{},
			Registry:    reg,
			Budget:      &stubBudget{},
			Churn:       &stubChurn{},
			System:      "you are kloo",
			Model:       "test-model",
			Temperature: 0.1,
			NoThink:     true,
		}
		if _, _, _, _, err := loop.act(context.Background(), "read", []llm.Message{{Role: llm.RoleUser, Content: "read"}}, VerifyResult{}, ""); err != nil {
			t.Fatal(err)
		}
		if len(client.completeReqs) != 1 || client.completeReqs[0].ReasoningEffort != "none" {
			t.Fatalf("no-think missing from non-streaming request: %+v", client.completeReqs)
		}
	})

	t.Run("streaming act", func(t *testing.T) {
		client := &recordingLLM{resp: resp}
		loop := &Loop{
			Client:      client,
			Adapter:     tools.NativeFCAdapter{},
			Registry:    reg,
			Budget:      &stubBudget{},
			Churn:       &stubChurn{},
			System:      "you are kloo",
			Model:       "test-model",
			Temperature: 0.1,
			NoThink:     true,
			OnDelta:     func(string) {},
		}
		if _, _, _, _, err := loop.act(context.Background(), "read", []llm.Message{{Role: llm.RoleUser, Content: "read"}}, VerifyResult{}, ""); err != nil {
			t.Fatal(err)
		}
		if len(client.streamReqs) != 1 || client.streamReqs[0].ReasoningEffort != "none" {
			t.Fatalf("no-think missing from streaming request: %+v", client.streamReqs)
		}
	})

	t.Run("chat gate", func(t *testing.T) {
		client := &recordingLLM{resp: llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, Content: "TASK"}}}}}
		loop := &Loop{
			Client:      client,
			ChatSystem:  "classify",
			Model:       "test-model",
			Temperature: 0.1,
			NoThink:     true,
		}
		loop.chatGate(context.Background(), "do work")
		if len(client.completeReqs) != 1 || client.completeReqs[0].ReasoningEffort != "none" {
			t.Fatalf("no-think missing from chat-gate request: %+v", client.completeReqs)
		}
	})

	t.Run("retry prompt", func(t *testing.T) {
		client := &recordingLLM{resp: llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, Content: "plain prose"}}}}}
		loop := &Loop{
			Client:      client,
			Adapter:     tools.NativeFCAdapter{},
			Registry:    reg,
			Budget:      &stubBudget{},
			Churn:       &stubChurn{},
			System:      "you are kloo",
			Model:       "test-model",
			Temperature: 0.1,
			NoThink:     true,
		}
		_, _, _, _, err := loop.act(context.Background(), "read", []llm.Message{{Role: llm.RoleUser, Content: "read"}}, VerifyResult{}, "")
		if !errors.Is(err, ErrNoToolCall) {
			t.Fatalf("act err = %v, want ErrNoToolCall after retry", err)
		}
		if len(client.completeReqs) != 2 {
			t.Fatalf("expected initial + retry requests, got %d", len(client.completeReqs))
		}
		for i, req := range client.completeReqs {
			if req.ReasoningEffort != "none" {
				t.Fatalf("no-think missing from request %d: %+v", i, req)
			}
		}
	})
}

func TestLoopRunawayThinkingProducesRecoverableError(t *testing.T) {
	longReasoning := strings.Repeat("thinking ", 300)
	client := &recordingLLM{resp: llm.ChatResponse{Choices: []llm.Choice{{
		Message: llm.Message{
			Role:                llm.RoleAssistant,
			Content:             longReasoning,
			ReasoningContent:    longReasoning,
			RawContent:          "",
			RawReasoningContent: longReasoning,
			FinishReason:        "stop",
		},
		FinishReason: "stop",
	}}}}
	loop := &Loop{
		Client:      client,
		Adapter:     tools.NativeFCAdapter{},
		Registry:    tools.NewRegistry(),
		Budget:      &stubBudget{},
		Churn:       &stubChurn{},
		System:      "you are kloo",
		Model:       "test-model",
		Temperature: 0.1,
	}

	rep, err := loop.Run(context.Background(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reason != ReasonError || rep.Err == nil {
		t.Fatalf("reason/err = %q/%v, want recoverable error", rep.Reason, rep.Err)
	}
	if msg := rep.Err.Error(); !strings.Contains(msg, "reasoning chars") || !strings.Contains(msg, "--no-think") || !strings.Contains(msg, "output budget") {
		t.Fatalf("recoverable error missing guidance: %q", msg)
	}
}

func TestLoopRunawayThinkingDoesNotStreamReasoningText(t *testing.T) {
	longReasoning := strings.Repeat("thinking ", 300)
	client := &recordingLLM{
		streamDeltas: []llm.Delta{{Role: llm.RoleAssistant, ReasoningContent: longReasoning}},
		resp: llm.ChatResponse{Choices: []llm.Choice{{
			Message: llm.Message{
				Role:                llm.RoleAssistant,
				Content:             longReasoning,
				ReasoningContent:    longReasoning,
				RawContent:          "",
				RawReasoningContent: longReasoning,
				FinishReason:        "length",
			},
			FinishReason: "length",
		}}},
	}
	var streamed strings.Builder
	loop := &Loop{
		Client:      client,
		Adapter:     tools.NativeFCAdapter{},
		Registry:    tools.NewRegistry(),
		Budget:      &stubBudget{},
		Churn:       &stubChurn{},
		System:      "you are kloo",
		Model:       "test-model",
		Temperature: 0.1,
		OnDelta:     func(s string) { streamed.WriteString(s) },
	}

	rep, err := loop.Run(context.Background(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reason != ReasonError || rep.Err == nil {
		t.Fatalf("reason/err = %q/%v, want recoverable error", rep.Reason, rep.Err)
	}
	if streamed.String() != "" {
		t.Fatalf("raw reasoning should not stream to renderer, got %q", streamed.String())
	}
}

func TestLoopLengthFinishWithEmptyContentProducesRecoverableError(t *testing.T) {
	client := &recordingLLM{resp: llm.ChatResponse{Choices: []llm.Choice{{
		Message:      llm.Message{Role: llm.RoleAssistant, FinishReason: "length"},
		FinishReason: "length",
	}}}}
	loop := &Loop{
		Client:      client,
		Adapter:     tools.NativeFCAdapter{},
		Registry:    tools.NewRegistry(),
		Budget:      &stubBudget{},
		Churn:       &stubChurn{},
		System:      "you are kloo",
		Model:       "test-model",
		Temperature: 0.1,
	}

	rep, err := loop.Run(context.Background(), "do work")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reason != ReasonError || rep.Err == nil {
		t.Fatalf("reason/err = %q/%v, want recoverable error", rep.Reason, rep.Err)
	}
	if msg := rep.Err.Error(); !strings.Contains(msg, "0 reasoning chars") || !strings.Contains(msg, "--no-think") {
		t.Fatalf("length recoverable error missing guidance/count: %q", msg)
	}
}

func TestLoopShortReasoningFallbackRemainsAnswered(t *testing.T) {
	reasoning := "fallback answer"
	client := &recordingLLM{resp: llm.ChatResponse{Choices: []llm.Choice{{
		Message: llm.Message{
			Role:                llm.RoleAssistant,
			Content:             reasoning,
			ReasoningContent:    reasoning,
			RawContent:          "",
			RawReasoningContent: reasoning,
			FinishReason:        "stop",
		},
		FinishReason: "stop",
	}}}}
	loop := &Loop{
		Client:      client,
		Adapter:     tools.NativeFCAdapter{},
		Registry:    tools.NewRegistry(),
		Budget:      &stubBudget{},
		Churn:       &stubChurn{},
		System:      "you are kloo",
		Model:       "test-model",
		Temperature: 0.1,
	}

	rep, err := loop.Run(context.Background(), "answer")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reason != ReasonAnswered || rep.Err != nil {
		t.Fatalf("short reasoning fallback should remain answered, got %q/%v", rep.Reason, rep.Err)
	}
}
