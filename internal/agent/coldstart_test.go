package agent

import (
	"context"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// powering is the exact rejection the kloo-bench gateway returns while a
// serverless worker cold-starts. It asks the client to resubmit.
const powering = `{"error":{"message":"worker is powering on; this request was rejected, not queued -- submit a new request to retry","type":"server_error","code":"scheduler_busy"}}`

func coldLoop(t *testing.T, mocks ...llmtest.Mock) *Loop {
	t.Helper()
	loop, _ := newLoop(t, llmtest.Sequence(t, mocks...), &stubVerifier{results: []VerifyResult{passResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.LLMRetries = 2 // 3 normal attempts
	loop.RetryBaseDelay = time.Millisecond
	loop.RetryMaxDelay = time.Millisecond
	return loop
}

func finishMock(t *testing.T) llmtest.Mock {
	return llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})}
}

// TestColdStartOutlastsTheNormalRetryBudget: five "powering on" rejections exceed
// the 3-attempt budget; the run must keep resubmitting and succeed. Measured on
// kloo-bench, four cases across both arms of a paired run died at step 1 on this.
func TestColdStartOutlastsTheNormalRetryBudget(t *testing.T) {
	var m []llmtest.Mock
	for i := 0; i < 5; i++ {
		m = append(m, llmtest.Mock{Status: 503, Body: powering})
	}
	loop := coldLoop(t, append(m, finishMock(t))...)
	rep, err := loop.Run(context.Background(), "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success after the worker came up", rep.Reason)
	}
}

// TestColdStartPatienceIsBounded: a worker that never comes up must still fail.
func TestColdStartPatienceIsBounded(t *testing.T) {
	loop := coldLoop(t, llmtest.Mock{Status: 503, Body: powering}) // repeats forever
	loop.ColdStartPatience = 50 * time.Millisecond
	done := make(chan *Report, 1)
	go func() { rep, _ := loop.Run(context.Background(), "do it"); done <- rep }()
	select {
	case rep := <-done:
		if rep.Reason != ReasonError {
			t.Fatalf("reason = %q, want error once patience is spent", rep.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cold-start retry never gave up")
	}
}

// TestColdStartRespectsDisabledRetries: LLMRetries = 0 means never retry, cold
// start or not.
func TestColdStartRespectsDisabledRetries(t *testing.T) {
	loop := coldLoop(t, llmtest.Mock{Status: 503, Body: powering}, finishMock(t))
	loop.LLMRetries = 0
	rep, _ := loop.Run(context.Background(), "do it")
	if rep.Reason != ReasonError {
		t.Fatalf("reason = %q, want error with retries disabled", rep.Reason)
	}
}

// TestPlain503StillExhausts: only the explicit starting-up signal earns patience.
// A generic 503 is a server that is DOWN and must fail after the normal budget.
func TestPlain503StillExhausts(t *testing.T) {
	var m []llmtest.Mock
	for i := 0; i < 5; i++ {
		m = append(m, llmtest.Mock{Status: 503, Body: `{"error":"service unavailable"}`})
	}
	loop := coldLoop(t, append(m, finishMock(t))...)
	rep, _ := loop.Run(context.Background(), "do it")
	if rep.Reason != ReasonError {
		t.Fatalf("reason = %q, want error: a plain 503 must not get cold-start patience", rep.Reason)
	}
}

// TestWorkerFailedMidGenerationIsRetried: the serving worker dying mid-request is
// transient. On kloo-bench it ended A04 at step 8 before any edit.
func TestWorkerFailedMidGenerationIsRetried(t *testing.T) {
	loop := coldLoop(t,
		llmtest.Mock{Status: 500, Body: `{"error":"stream error chunk: worker failed while generating the completion"}`},
		finishMock(t))
	loop.RetryableStatusCodes = []int{} // prove the SUBSTRING match retries it, not the 500 status
	rep, _ := loop.Run(context.Background(), "do it")
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success after retrying the worker failure", rep.Reason)
	}
}

// TestWorkerTimedOutMidGenerationIsRetried: the same fault in a different wording.
// The first fix matched only "worker failed while generating", and a kloo-bench
// rerun then died at step 9 on "worker timed out while generating the completion"
// without a single retry.
func TestWorkerTimedOutMidGenerationIsRetried(t *testing.T) {
	loop := coldLoop(t,
		llmtest.Mock{Status: 500, Body: `{"error":"stream error chunk: worker timed out while generating the completion"}`},
		finishMock(t))
	loop.RetryableStatusCodes = []int{} // prove the substring match retries it, not the 500
	rep, _ := loop.Run(context.Background(), "do it")
	if rep.Reason != ReasonSuccess {
		t.Fatalf("reason = %q, want success after retrying the worker timeout", rep.Reason)
	}
}
