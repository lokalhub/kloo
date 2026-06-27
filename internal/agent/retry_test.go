package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestLoopRetriesTransientModelError: a 503 (server-side transient — a cold model
// load / overload) on the first model call is retried, and the run proceeds on the
// retry instead of dying. Without retry, that one 503 would end the whole run with
// ReasonError — discarding everything done so far.
func TestLoopRetriesTransientModelError(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Status: 503, Body: `{"error":"model is loading"}`},                         // attempt 1: transient
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})}, // attempt 2: ok
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.RetryBaseDelay = time.Millisecond // keep the test fast

	var retried bool
	loop.OnRetry = func(attempt, max int, err error, wait time.Duration) { retried = true }

	rep, err := loop.Run(context.Background(), "is it set up?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success (the 503 should have been retried, not fatal)", rep.Reason)
	}
	if !retried {
		t.Error("OnRetry was not called for the transient 503")
	}
	if n := len(srv.Requests()); n != 2 {
		t.Errorf("requests = %d, want 2 (one failed + one retry)", n)
	}
}

// TestLoopDoesNotRetryDeterministicError: a 400 (bad request / deterministic) is
// NOT retried — a retry would only repeat it — so the run ends as ReasonError on
// the first try.
func TestLoopDoesNotRetryDeterministicError(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Status: 400, Body: `{"error":"bad request"}`},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.RetryBaseDelay = time.Millisecond

	rep, err := loop.Run(context.Background(), "is it set up?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonError {
		t.Fatalf("reason = %q, want error (a 400 must not be retried)", rep.Reason)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Errorf("requests = %d, want 1 (no retry on a deterministic 400)", n)
	}
	if rep.Err == nil {
		t.Fatal("expected enriched report error")
	}
	msg := rep.Err.Error()
	for _, want := range []string{srv.URL + "/v1", "test-model", "bad request"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("enriched error missing %q: %s", want, msg)
		}
	}
}

// TestLoopRetriesExhaustThenError: when every attempt fails transiently, the loop
// exhausts its retries and surfaces the error (rather than looping forever).
func TestLoopRetriesExhaustThenError(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Status: 503, Body: `{"error":"loading"}`}) // always 503
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.LLMRetries = 2 // 3 attempts total
	loop.RetryBaseDelay = time.Millisecond

	rep, err := loop.Run(context.Background(), "is it set up?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonError {
		t.Fatalf("reason = %q, want error after exhausting retries", rep.Reason)
	}
	if n := len(srv.Requests()); n != 3 {
		t.Errorf("requests = %d, want 3 (first + 2 retries)", n)
	}
	if rep.Err == nil || !strings.Contains(rep.Err.Error(), "endpoint="+srv.URL+"/v1") || !strings.Contains(rep.Err.Error(), "model=test-model") || !strings.Contains(rep.Err.Error(), "loading") {
		t.Fatalf("exhausted retry error should include endpoint/model/body, got %v", rep.Err)
	}
}

func TestLoopConnectionFailureIncludesEndpointAndModelNoSecret(t *testing.T) {
	dead := llmtest.DeadURL(t) + "/v1"
	srv := llmtest.JSON(t, toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "unused"}}))
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.Client = llm.New(dead, "secret-model", llm.WithAPIKey("super-secret-key"))
	loop.Endpoint = dead
	loop.Model = "secret-model"
	loop.LLMRetries = -1

	rep, err := loop.Run(context.Background(), "connect")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonError || rep.Err == nil {
		t.Fatalf("reason/err = %q/%v, want error", rep.Reason, rep.Err)
	}
	msg := rep.Err.Error()
	for _, want := range []string{dead, "secret-model", "connection"} {
		if !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
			t.Fatalf("connection error missing %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "super-secret-key") {
		t.Fatalf("error leaked API key: %s", msg)
	}
}

// TestIsRetryableLLMError pins the classification: transient hiccups retry,
// deterministic errors don't.
func TestIsRetryableLLMError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"stream idle", llm.ErrStreamIdle, true},
		{"stream incomplete", llm.ErrStreamIncomplete, true},
		{"deadline", context.DeadlineExceeded, true},
		{"503", &llm.APIError{StatusCode: 503}, true},
		{"429", &llm.APIError{StatusCode: 429}, true},
		{"408", &llm.APIError{StatusCode: 408}, true},
		{"400", &llm.APIError{StatusCode: 400}, false},
		{"401", &llm.APIError{StatusCode: 401}, false},
		{"404", &llm.APIError{StatusCode: 404}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRetryableLLMError(c.err); got != c.want {
				t.Errorf("isRetryableLLMError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
