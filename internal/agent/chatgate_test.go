package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestChatGateAnswersConversationalWithoutRunning: with the gate enabled, a
// conversational message ("thanks") is answered by ONE no-tools model call and the
// run stops as ReasonAnswered — NO tools dispatched, NO agent loop. This is the fix
// for a weak model re-doing finished work on a vague input.
func TestChatGateAnswersConversationalWithoutRunning(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: proseResp(t, "You're welcome! Glad it helped.")})
	loop, calls := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.ChatSystem = "classify the message: TASK or a reply"
	var streamed string
	loop.OnDelta = func(s string) { streamed += s }

	rep, err := loop.Run(context.Background(), "thanks")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonAnswered {
		t.Fatalf("reason = %q, want answered", rep.Reason)
	}
	if len(*calls) != 0 {
		t.Errorf("no tools should run for a conversational message, got %v", *calls)
	}
	if rep.Steps != 0 {
		t.Errorf("steps = %d, want 0 (the loop never ran)", rep.Steps)
	}
	if !strings.Contains(streamed, "You're welcome") {
		t.Errorf("the conversational reply should be streamed to the user, got %q", streamed)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Errorf("requests = %d, want 1 (the gate only — no loop turns)", n)
	}
}

// TestChatGateProceedsOnTaskVerdict: when the gate returns the TASK sentinel, Run
// falls through into the real agent loop and does the work.
func TestChatGateProceedsOnTaskVerdict(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: proseResp(t, "TASK")}, // gate: actionable
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.ChatSystem = "classify the message: TASK or a reply"

	rep, err := loop.Run(context.Background(), "add a profile tab")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success (TASK verdict → loop ran finish → green verify)", rep.Reason)
	}
	if n := len(srv.Requests()); n != 2 {
		t.Errorf("requests = %d, want 2 (gate + one loop turn)", n)
	}
}

// TestChatGateFailsOpen: a gate model-call error must NOT block work — Run falls
// through to the loop rather than refusing the task on a classifier hiccup.
func TestChatGateFailsOpen(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Status: 500, Body: `{"error":"boom"}`}, // gate call fails
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.ChatSystem = "classify the message: TASK or a reply"

	rep, err := loop.Run(context.Background(), "add a profile tab")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success (gate error fails open → loop ran)", rep.Reason)
	}
}

func TestChatGateRunawayThinkingStopsAsRecoverableError(t *testing.T) {
	reasoning := strings.Repeat("thinking ", 300)
	srv := llmtest.Sequence(t, llmtest.Mock{Body: `{
		"choices": [{
			"message": {"role":"assistant","content":"","reasoning_content":` + strconv.Quote(reasoning) + `},
			"finish_reason":"length"
		}],
		"usage": {"total_tokens": 77}
	}`})
	loop, calls := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.ChatSystem = "classify the message: TASK or a reply"
	var streamed string
	loop.OnDelta = func(s string) { streamed += s }

	rep, err := loop.Run(context.Background(), "thanks")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonError || rep.Err == nil {
		t.Fatalf("reason/err = %q/%v, want recoverable error", rep.Reason, rep.Err)
	}
	if !strings.Contains(rep.Err.Error(), "--no-think") || !strings.Contains(rep.Err.Error(), "output budget") {
		t.Fatalf("recoverable error missing guidance: %q", rep.Err.Error())
	}
	if streamed != "" {
		t.Fatalf("chat gate should not stream raw reasoning before diagnostic, got %q", streamed)
	}
	if len(*calls) != 0 || rep.Steps != 0 {
		t.Fatalf("chat-gate runaway should stop before act loop, calls=%v steps=%d", *calls, rep.Steps)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Fatalf("requests = %d, want gate only", n)
	}
}

// TestChatGateDisabledByDefault: with no ChatSystem set (headless/benchmark), there
// is NO gate call — the very first request is the agent loop's turn.
func TestChatGateDisabledByDefault(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})})
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	// ChatSystem deliberately left empty.

	rep, err := loop.Run(context.Background(), "build it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success", rep.Reason)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Errorf("requests = %d, want 1 (no gate when ChatSystem is empty)", n)
	}
}

func TestIsTaskVerdict(t *testing.T) {
	task := []string{"TASK", "task", "TASK.", "Task:", "  TASK  ", "TASK\n"}
	for _, s := range task {
		if !isTaskVerdict(s) {
			t.Errorf("isTaskVerdict(%q) = false, want true", s)
		}
	}
	chat := []string{"You're welcome!", "Sure, I can help with that.", "", "The task is already done.", "Hi there"}
	for _, s := range chat {
		if isTaskVerdict(s) {
			t.Errorf("isTaskVerdict(%q) = true, want false", s)
		}
	}
}
