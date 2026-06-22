package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureBody starts a server that records the request body and replays the
// given response (SSE when sse is true). Returns the server and a pointer to the
// captured body (populated after a request is made).
func captureBody(t *testing.T, sse bool, response string) (*httptest.Server, *[]byte) {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if sse {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// streamOptionsPresent reports whether the serialized request body carries a
// stream_options object and, if so, its include_usage value.
func streamOptions(t *testing.T, body []byte) (present bool, includeUsage bool) {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("request body not JSON: %v\n%s", err, body)
	}
	so, ok := raw["stream_options"]
	if !ok {
		return false, false
	}
	var opts struct {
		IncludeUsage bool `json:"include_usage"`
	}
	if err := json.Unmarshal(so, &opts); err != nil {
		t.Fatalf("stream_options not an object: %v", err)
	}
	return true, opts.IncludeUsage
}

// TestStreamRequestsIncludeUsage: a streaming call carries
// stream_options.include_usage == true (task 01).
func TestStreamRequestsIncludeUsage(t *testing.T) {
	srv, body := captureBody(t, true, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	client := New(srv.URL+"/v1", "snappy")
	if _, err := client.Stream(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	present, include := streamOptions(t, *body)
	if !present {
		t.Fatalf("streaming request missing stream_options:\n%s", *body)
	}
	if !include {
		t.Errorf("stream_options.include_usage = false, want true:\n%s", *body)
	}
}

// TestCompleteOmitsStreamOptions: a non-streaming Complete must serialize
// identically to before — no stream_options key at all (task 01).
func TestCompleteOmitsStreamOptions(t *testing.T) {
	srv, body := captureBody(t, false, string(readFixture(t, "response.json")))
	client := New(srv.URL+"/v1", "snappy")
	if _, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if present, _ := streamOptions(t, *body); present {
		t.Errorf("Complete request must not carry stream_options:\n%s", *body)
	}
}

// TestStreamParsesUsage: a transcript whose penultimate chunk carries usage with
// an empty choices array yields a populated resp.Usage (tasks 02).
func TestStreamParsesUsage(t *testing.T) {
	srv := sseServer(t, "usage.stream")
	defer srv.Close()

	client := New(srv.URL+"/v1", "snappy")
	resp, err := client.Stream(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Usage.TotalTokens != 1410 {
		t.Errorf("TotalTokens = %d, want 1410", resp.Usage.TotalTokens)
	}
	if resp.Usage.PromptTokens != 1200 || resp.Usage.CompletionTokens != 210 {
		t.Errorf("usage = %+v, want prompt 1200 / completion 210", resp.Usage)
	}
	// The content still accumulates around the usage chunk.
	if got := resp.Choices[0].Message.Content; got != "Hello world" {
		t.Errorf("content = %q, want %q", got, "Hello world")
	}
}

// TestStreamAbsentUsageZero: a transcript with no usage chunk returns a valid
// response with a zero Usage and no error (the estimate fallback covers this
// downstream in internal/agent) (task 02).
func TestStreamAbsentUsageZero(t *testing.T) {
	srv := sseServer(t, "say-hi.stream")
	defer srv.Close()

	client := New(srv.URL+"/v1", "snappy")
	resp, err := client.Stream(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if resp.Usage != (Usage{}) {
		t.Errorf("absent-usage stream should yield zero Usage, got %+v", resp.Usage)
	}
}
