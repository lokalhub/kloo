package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// --- Harness exercised by one non-streaming and one streaming test ----------

// TestHarnessNonStreaming proves the shared llmtest harness drives a Complete
// call and captures the request body.
func TestHarnessNonStreaming(t *testing.T) {
	srv := llmtest.JSON(t, `{"id":"h1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"}}]}`)
	client := New(srv.URL+"/v1", "test-model")

	resp, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "ping"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Choices[0].Message.Content != "pong" {
		t.Errorf("content = %q, want pong", resp.Choices[0].Message.Content)
	}
	if reqs := srv.Requests(); len(reqs) != 1 || !strings.Contains(string(reqs[0]), "ping") {
		t.Errorf("harness did not capture the request body: %v", reqs)
	}
}

// TestHarnessStreaming proves the shared harness drives a Stream call from a
// recorded transcript.
func TestHarnessStreaming(t *testing.T) {
	transcript := llmtest.ReadTranscript(t, "testdata", "sse", "say-hi.stream")
	srv := llmtest.SSE(t, transcript)
	client := New(srv.URL+"/v1", "test-model")

	resp, err := client.Stream(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "say hi"}}}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Choices[0].Message.Content != "Hi there! 👋" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Hi there! 👋")
	}
}

// --- Edge cases small-model runs actually hit -------------------------------

func TestCompleteEdgeErrors(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantAPI   bool   // expect *APIError
		wantInBdy string // substring expected in APIError.Body
	}{
		{name: "429 rate limited", status: http.StatusTooManyRequests, body: `{"error":"slow down"}`, wantAPI: true, wantInBdy: "slow down"},
		{name: "500 server error", status: http.StatusInternalServerError, body: `{"error":"boom"}`, wantAPI: true, wantInBdy: "boom"},
		{name: "503 with empty body", status: http.StatusServiceUnavailable, body: "", wantAPI: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := llmtest.Status(t, tc.status, tc.body)
			client := New(srv.URL+"/v1", "test-model")
			_, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("want *APIError, got %T: %v", err, err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", apiErr.StatusCode, tc.status)
			}
			if tc.wantInBdy != "" && !strings.Contains(apiErr.Body, tc.wantInBdy) {
				t.Errorf("body %q missing %q", apiErr.Body, tc.wantInBdy)
			}
		})
	}
}

// TestCompleteMalformedJSON: a 200 with a non-JSON body is a decode error (NOT
// an APIError — the HTTP call succeeded, the payload is the problem).
func TestCompleteMalformedJSON(t *testing.T) {
	srv := llmtest.JSON(t, `{"choices": [ this is not json`)
	client := New(srv.URL+"/v1", "test-model")
	_, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err == nil {
		t.Fatal("want decode error, got nil")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Errorf("malformed 200 body should not be an APIError, got %v", err)
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("want a decode error, got %v", err)
	}
}

// TestCompleteEmptyChoices: a valid 200 with empty choices is not an error; the
// caller gets an empty Choices slice to handle.
func TestCompleteEmptyChoices(t *testing.T) {
	srv := llmtest.JSON(t, `{"id":"e","object":"chat.completion","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	client := New(srv.URL+"/v1", "test-model")
	resp, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if err != nil {
		t.Fatalf("empty choices should not error, got %v", err)
	}
	if len(resp.Choices) != 0 {
		t.Errorf("want 0 choices, got %d", len(resp.Choices))
	}
}

// TestCompleteSlowServerTimeout: a slow server vs a short per-call timeout aborts
// with DeadlineExceeded (via the shared harness Delay).
func TestCompleteSlowServerTimeout(t *testing.T) {
	srv := llmtest.NewServer(t, llmtest.Mock{Body: `{"choices":[]}`, Delay: 500 * time.Millisecond})
	client := New(srv.URL+"/v1", "test-model", WithTimeout(40*time.Millisecond))
	_, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want DeadlineExceeded, got %v", err)
	}
}

// TestConnectionRefused: dialing a dead endpoint yields a clear error (not panic).
func TestConnectionRefused(t *testing.T) {
	dead := llmtest.DeadURL(t)
	client := New(dead+"/v1", "test-model", WithTimeout(2*time.Second))

	_, cErr := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}})
	if cErr == nil || !strings.Contains(cErr.Error(), "llm: request") {
		t.Errorf("Complete: want a clear request error, got %v", cErr)
	}
	_, sErr := client.Stream(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}}, nil)
	if sErr == nil || !strings.Contains(sErr.Error(), "llm: request") {
		t.Errorf("Stream: want a clear request error, got %v", sErr)
	}
}

// --- No leaked response bodies (cleanup verified) ---------------------------

// trackingBody records whether Close was called.
type trackingBody struct {
	io.Reader
	closed *int32
}

func (b trackingBody) Close() error { atomic.StoreInt32(b.closed, 1); return nil }

// trackingTransport returns a fixed response whose body tracks Close.
type trackingTransport struct {
	body   string
	header string
	closed *int32
}

func (tt trackingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	h := make(http.Header)
	h.Set("Content-Type", tt.header)
	return &http.Response{
		StatusCode: 200,
		Status:     "200 OK",
		Header:     h,
		Body:       trackingBody{Reader: strings.NewReader(tt.body), closed: tt.closed},
		Request:    r,
	}, nil
}

// TestResponseBodiesClosed proves Complete and Stream both close the response
// body (no fd/goroutine leak), checked via a tracking transport.
func TestResponseBodiesClosed(t *testing.T) {
	t.Run("complete", func(t *testing.T) {
		var closed int32
		hc := &http.Client{Transport: trackingTransport{
			body:   `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`,
			header: "application/json",
			closed: &closed,
		}}
		client := New("http://example/v1", "test-model", WithHTTPClient(hc))
		if _, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if atomic.LoadInt32(&closed) != 1 {
			t.Error("Complete did not close the response body")
		}
	})

	t.Run("stream", func(t *testing.T) {
		var closed int32
		hc := &http.Client{Transport: trackingTransport{
			body:   "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n",
			header: "text/event-stream",
			closed: &closed,
		}}
		client := New("http://example/v1", "test-model", WithHTTPClient(hc))
		if _, err := client.Stream(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "x"}}}, nil); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if atomic.LoadInt32(&closed) != 1 {
			t.Error("Stream did not close the response body")
		}
	})
}
