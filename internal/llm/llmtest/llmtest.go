// Package llmtest provides a reusable mocked OpenAI-compatible endpoint for
// kloo's LLM client tests. It stands up an httptest.Server that replays either a
// JSON body or a recorded SSE transcript, captures request bodies for
// assertions, and registers cleanup so callers never manage Close themselves.
//
// Phase 04's autonomous-loop tests reuse this harness (scripting a sequence of
// mocked tool-call responses) instead of re-rolling httptest servers, so the
// mocked-LLM contract lives in exactly one place.
//
// It intentionally does NOT import internal/llm (to avoid an import cycle):
// callers build their llm.Client against the returned Server.URL, and captured
// requests are exposed as raw bytes.
package llmtest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Mock configures the mocked endpoint's single canned response.
type Mock struct {
	// Status is the HTTP status to return (default 200 when zero).
	Status int
	// Body is the raw response body. For SSE set it to a transcript and SSE=true.
	Body string
	// SSE sends a text/event-stream content type (else application/json).
	SSE bool
	// Delay sleeps before responding — used by client-timeout tests.
	Delay time.Duration
}

// Server is a mocked endpoint plus the request bodies it received.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	requests [][]byte
}

// Requests returns a copy of the captured request bodies, in arrival order.
func (s *Server) Requests() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.requests))
	copy(out, s.requests)
	return out
}

// NewServer starts a mocked endpoint for the given Mock, closing it via
// tb.Cleanup. The handler accepts any path (callers append /v1 etc. to the URL).
func NewServer(tb testing.TB, m Mock) *Server {
	tb.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, body)
		s.mu.Unlock()

		if m.Delay > 0 {
			select {
			case <-time.After(m.Delay):
			case <-r.Context().Done():
				return // client cancelled/timed out
			}
		}

		status := m.Status
		if status == 0 {
			status = http.StatusOK
		}
		if m.SSE {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, m.Body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	tb.Cleanup(s.Server.Close)
	return s
}

// JSON is a 200 endpoint returning the given JSON body.
func JSON(tb testing.TB, body string) *Server {
	return NewServer(tb, Mock{Body: body})
}

// Sequence serves a different canned response per request: the i-th request gets
// mocks[i]; once the list is exhausted the final mock repeats. It captures
// request bodies like NewServer (use Requests() to assert the request count and
// contents). This is the scripted-conversation harness the multi-turn flows
// (tool-call re-prompt, Phase-04 loop) drive their tests with.
func Sequence(tb testing.TB, mocks ...Mock) *Server {
	tb.Helper()
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		idx := len(s.requests)
		s.requests = append(s.requests, body)
		s.mu.Unlock()

		if idx >= len(mocks) {
			idx = len(mocks) - 1
		}
		var m Mock
		if idx >= 0 {
			m = mocks[idx]
		}

		status := m.Status
		if status == 0 {
			status = http.StatusOK
		}
		if m.SSE {
			w.Header().Set("Content-Type", "text/event-stream")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, m.Body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	tb.Cleanup(s.Server.Close)
	return s
}

// SSE is a 200 endpoint replaying the given event-stream transcript.
func SSE(tb testing.TB, transcript string) *Server {
	return NewServer(tb, Mock{Body: transcript, SSE: true})
}

// Status is an endpoint returning a non-2xx status with a body.
func Status(tb testing.TB, code int, body string) *Server {
	return NewServer(tb, Mock{Status: code, Body: body})
}

// ReadTranscript reads a fixture (e.g. SSE transcript or JSON) from the caller's
// testdata directory, relative to the test's working directory.
func ReadTranscript(tb testing.TB, parts ...string) string {
	tb.Helper()
	path := filepath.Join(parts...)
	b, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("llmtest: read transcript %s: %v", path, err)
	}
	return string(b)
}

// DeadURL returns the URL of a server that has already been closed, so a client
// dialing it gets a connection-refused error.
func DeadURL(tb testing.TB) string {
	tb.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	return url
}
