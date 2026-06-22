package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// decodeJSON decodes a request body into v (test-only convenience).
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// TestCompleteSuccess: a 200 with a canned JSON body parses into ChatResponse.
func TestCompleteSuccess(t *testing.T) {
	body := readFixture(t, "response.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("want Content-Type application/json, got %q", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "snappy")
	resp, err := client.Complete(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "say hi"}},
	})
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "Hi there!" {
		t.Errorf("unexpected response: %+v", resp)
	}
}

// TestCompleteDefaultsModel: an empty request Model is filled with the client's
// default model before the request is sent.
func TestCompleteDefaultsModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		_ = decodeJSON(r, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(readFixture(t, "response.json"))
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "snappy")
	if _, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}}); err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if gotModel != "snappy" {
		t.Errorf("want default model snappy on the wire, got %q", gotModel)
	}
}

// TestCompleteNon2xx: a non-2xx status maps to an *APIError carrying status+body.
func TestCompleteNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "snappy")
	_, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("want error for 500, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("want status 500, got %d", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Body, "model not found") {
		t.Errorf("want body to carry upstream message, got %q", apiErr.Body)
	}
}

// TestCompleteContextCancel: cancelling the context aborts the in-flight request.
func TestCompleteContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang until the test releases, so cancel happens first
	}))
	defer srv.Close()
	defer close(release)

	client := New(srv.URL+"/v1", "snappy")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	_, err := client.Complete(ctx, ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("want error from cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

// TestCompleteTimeout: a per-call timeout aborts a slow request.
func TestCompleteTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	client := New(srv.URL+"/v1", "snappy", WithTimeout(30*time.Millisecond))
	_, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want context.DeadlineExceeded, got %v", err)
	}
}
