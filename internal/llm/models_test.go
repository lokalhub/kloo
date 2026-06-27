package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelsParsesAndSortsOpenAIShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		if accept := r.Header.Get("Accept"); accept != "application/json" {
			t.Errorf("Accept = %q, want application/json", accept)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"zeta","context_length":4096},{"id":"alpha","context_length":8192}]}`))
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "unused")
	got, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	want := []ModelInfo{{ID: "alpha", ContextLength: 8192}, {ID: "zeta", ContextLength: 4096}}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestModelsParsesOpenRouterTopProviderContextLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"openrouter/model","top_provider":{"context_length":131072}}]}`))
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "unused")
	got, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(got) != 1 || got[0].ContextLength != 131072 {
		t.Fatalf("got %+v, want OpenRouter context length 131072", got)
	}
}

func TestModelsSendsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "unused", WithAPIKey("secret-token"))
	if _, err := client.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization = %q, want bearer token", gotAuth)
	}
}

func TestModelsNon2xxReturnsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no models"}`, http.StatusBadGateway)
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "unused")
	_, err := client.Models(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusBadGateway)
	}
	if !strings.Contains(apiErr.Body, "no models") {
		t.Errorf("Body = %q, want upstream body", apiErr.Body)
	}
}

func TestModelsNon2xxRedactsAPIKeyFromBodyAndError(t *testing.T) {
	const key = "sk-test-models-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `models failed with `+r.Header.Get("Authorization"), http.StatusForbidden)
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "unused", WithAPIKey(key))
	_, err := client.Models(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	for _, got := range []string{apiErr.Body, err.Error()} {
		if strings.Contains(got, key) {
			t.Fatalf("API key leaked in models error body/string: %q", got)
		}
		if !strings.Contains(got, "Bearer [REDACTED]") {
			t.Fatalf("redacted bearer marker missing: %q", got)
		}
	}
}

func TestModelsMalformedJSONReturnsDecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[`))
	}))
	defer srv.Close()

	client := New(srv.URL+"/v1", "unused")
	_, err := client.Models(context.Background())
	if err == nil {
		t.Fatal("want decode error, got nil")
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("malformed 200 body should not be APIError: %v", err)
	}
	if !strings.Contains(err.Error(), "decode models response") {
		t.Fatalf("error = %v, want decode models response", err)
	}
}
