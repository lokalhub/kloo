package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// badArgsResp renders a native tool call whose Arguments are not valid JSON — the
// shape a model produces when it is TRYING to act and botches the syntax, which
// NativeFCAdapter.ParseAll rejects with ErrMalformedToolCall.
func badArgsResp(t *testing.T, name string) string {
	t.Helper()
	resp := llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID: "c0", Type: "function",
				Function: llm.FunctionCall{Name: name, Arguments: `{"path": "a.go", `},
			}},
		}}},
		Usage: llm.Usage{TotalTokens: 1},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestMalformedToolCallGetsTheEmptyTurnBudget: a model that keeps emitting an
// unparseable tool call gets maxMalformedRecoveries corrective re-prompts, not one.
//
// Before this, ONE botched call after a single nudge ended the run. That ranked a
// model which is at least trying BELOW a model returning nothing at all, which
// already got maxEmptyTurnRecoveries attempts. The sequence here recovers on the
// third corrective, which the old one-strike rule could never reach.
func TestMalformedToolCallGetsTheEmptyTurnBudget(t *testing.T) {
	if maxMalformedRecoveries != maxEmptyTurnRecoveries {
		t.Fatalf("malformed budget %d must match the empty-turn budget %d",
			maxMalformedRecoveries, maxEmptyTurnRecoveries)
	}
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: badArgsResp(t, "read_file")},                                         // the turn
		llmtest.Mock{Body: badArgsResp(t, "read_file")},                                         // corrective 1
		llmtest.Mock{Body: badArgsResp(t, "read_file")},                                         // corrective 2
		llmtest.Mock{Body: toolResp(t, 1, tcSpec{"read_file", map[string]any{"path": "a.go"}})}, // corrective 3: readable at last
	)
	loop, calls := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{}, &stubChurn{})
	rep, err := loop.Run(context.Background(), "read a.go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason == ReasonError || rep.Reason == ReasonMalformedToolCall {
		t.Fatalf("reason = %q: the model recovered on corrective %d and the run must continue",
			rep.Reason, maxMalformedRecoveries)
	}
	if len(*calls) == 0 {
		t.Error("the recovered tool call was never dispatched")
	}
}

// TestPersistentMalformedKeepsTheRun: when the model botches the format through the
// WHOLE corrective budget, the run stops as ReasonMalformedToolCall — not
// ReasonError.
//
// The distinction is not cosmetic. ReasonError means "the tree is untrustworthy":
// it rolls the run's edits back and it is excluded from answerableStop, so a run
// that explored for minutes handed back a banner instead of its findings. But the
// endpoint answered every single request here — it is the model's tool-call DIALECT
// kloo cannot read. Nothing about that makes the work it already did suspect.
func TestPersistentMalformedKeepsTheRun(t *testing.T) {
	mocks := make([]llmtest.Mock, 0, maxMalformedRecoveries+1)
	for i := 0; i <= maxMalformedRecoveries; i++ {
		mocks = append(mocks, llmtest.Mock{Body: badArgsResp(t, "edit_file")})
	}
	srv := llmtest.Sequence(t, mocks...)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{}, &stubChurn{})
	rep, err := loop.Run(context.Background(), "fix a.go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonMalformedToolCall {
		t.Fatalf("reason = %q, want %q", rep.Reason, ReasonMalformedToolCall)
	}
	if !answerableStop(rep.Reason) {
		t.Error("a malformed-format stop must earn its closing answer: the endpoint is healthy")
	}
	if rep.RolledBack {
		t.Error("a format problem must not roll the run's work back")
	}
}

// TestChatGateSeesTheAttachedImage: the conversational gate is a model call that
// can END the run by itself, so it must receive the task's attachments.
//
// Measured live, and the reason this test exists: a user dragged a screenshot in and
// asked "do you see this image?". The attachment was fine, the loop carried it
// correctly — and the gate answered first, from text alone, with a confident "I
// can't see images in this session". The run stopped at step 0, having never reached
// the code that sends images. Every other link being green is exactly what made it
// hard to see.
func TestChatGateSeesTheAttachedImage(t *testing.T) {
	const img = "data:image/png;base64,iVBORw0KGgo="
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: proseResp(t, "Yes — it's a terminal screenshot.")}, // the gate's own call
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{}, &stubChurn{})
	loop.ChatSystem = "classify: reply TASK or answer directly"
	loop.TaskImages = []string{img}

	rep, err := loop.Run(context.Background(), "do you see this image?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want %q (the gate answered)", rep.Reason, ReasonAnswered)
	}
	reqs := srv.Requests()
	if len(reqs) == 0 {
		t.Fatal("the gate made no request")
	}
	got := string(reqs[0])
	if !strings.Contains(got, `"type":"image_url"`) || !strings.Contains(got, img) {
		t.Errorf("the gate's request carries no image:\n%s", got)
	}
}
