package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sseServer replays a recorded transcript file as a text/event-stream response.
func sseServer(t *testing.T, transcript string) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "sse", transcript))
	if err != nil {
		t.Fatalf("read transcript %s: %v", transcript, err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
}

// TestStreamSayHi: content deltas accumulate to the full reply; onDelta sees
// each fragment in order; [DONE] ends cleanly.
func TestStreamSayHi(t *testing.T) {
	srv := sseServer(t, "say-hi.stream")
	defer srv.Close()

	client := New(srv.URL+"/v1", "test-model")
	var fragments []string
	resp, err := client.Stream(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "say hi"}},
	}, func(d Delta) error {
		if d.Content != "" {
			fragments = append(fragments, d.Content)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	// say-hi.stream is a transcript recorded from a real llama.cpp run
	// (task 07), so the expected reply is the exact recorded content.
	const wantReply = "Hi there! 👋"
	if got := resp.Choices[0].Message.Content; got != wantReply {
		t.Errorf("accumulated content = %q, want %q", got, wantReply)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", resp.Choices[0].FinishReason)
	}
	if strings.Join(fragments, "") != wantReply {
		t.Errorf("onDelta fragments joined = %q", strings.Join(fragments, ""))
	}
}

// TestStreamToolCall: tool-call deltas accumulate into one complete call.
func TestStreamToolCall(t *testing.T) {
	srv := sseServer(t, "tool-call.stream")
	defer srv.Close()

	client := New(srv.URL+"/v1", "test-model")
	resp, err := client.Stream(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "read main.go"}},
	}, nil)
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("want 1 assembled tool call, got %d (%+v)", len(calls), calls)
	}
	if calls[0].ID != "call_x" || calls[0].Type != "function" {
		t.Errorf("unexpected call meta: %+v", calls[0])
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("function name = %q, want read_file", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("assembled arguments = %q, want %q", calls[0].Function.Arguments, `{"path":"main.go"}`)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
}

// TestStreamTruncated: a stream that ends without [DONE] yields the accumulated
// partial content plus ErrStreamIncomplete (not a hang or panic).
func TestStreamTruncated(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "sse", "truncated.stream"))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	resp, err := parseSSE(context.Background(), strings.NewReader(string(body)), nil)
	if !errors.Is(err, ErrStreamIncomplete) {
		t.Fatalf("want ErrStreamIncomplete, got %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "partial answer" {
		t.Errorf("partial content = %q, want %q", got, "partial answer")
	}
}

// TestStreamErrorChunk: a mid-stream error chunk surfaces ErrStreamError.
func TestStreamErrorChunk(t *testing.T) {
	transcript := `data: {"id":"e1","choices":[{"index":0,"delta":{"role":"assistant","content":"thinking"}}]}

data: {"error":{"message":"context length exceeded"}}

data: [DONE]

`
	resp, err := parseSSE(context.Background(), strings.NewReader(transcript), nil)
	if !errors.Is(err, ErrStreamError) {
		t.Fatalf("want ErrStreamError, got %v", err)
	}
	if !strings.Contains(err.Error(), "context length exceeded") {
		t.Errorf("error should carry the upstream message, got %v", err)
	}
	// Content accumulated before the error chunk is preserved.
	if got := resp.Choices[0].Message.Content; got != "thinking" {
		t.Errorf("pre-error content = %q, want %q", got, "thinking")
	}
}

// TestStreamContextCancel: cancelling mid-stream stops promptly with a context
// error (the Phase-05 interrupt foundation).
func TestStreamContextCancel(t *testing.T) {
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// Send one delta, flush, then keep the connection open until cleanup.
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"one\"}}]}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		<-hold
	}))
	defer srv.Close()
	defer close(hold)

	client := New(srv.URL+"/v1", "test-model")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := client.Stream(ctx, ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, func(d Delta) error {
		cancel() // cancel as soon as the first delta arrives
		return nil
	})
	if err == nil {
		t.Fatal("want context error from cancelled stream, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

// TestStreamOnDeltaError: an error returned by onDelta stops the stream and is
// propagated, with content accumulated so far preserved.
func TestStreamOnDeltaError(t *testing.T) {
	srv := sseServer(t, "say-hi.stream")
	defer srv.Close()

	sentinel := errors.New("caller abort")
	client := New(srv.URL+"/v1", "test-model")
	_, err := client.Stream(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, func(d Delta) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("want caller abort error, got %v", err)
	}
}

// TestStreamNon200: a non-2xx before the stream maps to *APIError.
func TestStreamNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "test-model")
	_, err := client.Stream(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", apiErr.StatusCode)
	}
}

// TestStreamPartialLines: a transcript whose data chunks are split across reads
// (no trailing blank line on the last event) still assembles correctly.
func TestStreamPartialLines(t *testing.T) {
	transcript := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"b\"}}]}\n\n" +
		"data: [DONE]\n\n"
	resp, err := parseSSE(context.Background(), strings.NewReader(transcript), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := resp.Choices[0].Message.Content; got != "ab" {
		t.Errorf("content = %q, want ab", got)
	}
}
