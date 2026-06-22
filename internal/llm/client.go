package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTimeout bounds a single non-streaming Complete call. Streaming (Stream,
// task 05) deliberately does NOT apply this overall deadline — a long stream is
// governed by the caller's context instead.
const DefaultTimeout = 120 * time.Second

// completionsPath is appended to the configured endpoint (e.g. ".../v1").
const completionsPath = "/chat/completions"

// LLMClient is the behaviour the CLI and the loop depend on, so they can inject
// fakes in tests.
type LLMClient interface {
	Complete(ctx context.Context, req ChatRequest) (ChatResponse, error)
	Stream(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (ChatResponse, error)
}

// Compile-time proof that *Client satisfies LLMClient.
var _ LLMClient = (*Client)(nil)

// Client is a hand-rolled OpenAI-compatible HTTP client over net/http.
type Client struct {
	endpoint   string // e.g. http://127.0.0.1:8080/v1 (no trailing slash)
	model      string // default model when a request leaves Model empty
	apiKey     string // optional; sent as Authorization: Bearer when set
	httpClient *http.Client
	timeout    time.Duration // per-call deadline for Complete
}

// Option customises a Client.
type Option func(*Client)

// WithHTTPClient injects a custom *http.Client (tests, custom transports).
// The client's own Timeout is left untouched here; Complete applies its own
// per-call deadline via context so streaming is not capped.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithTimeout sets the per-call deadline used by Complete.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithAPIKey sets a bearer token (unused against local llama-swap, needed for
// hosted OpenAI-compatible endpoints).
func WithAPIKey(key string) Option {
	return func(c *Client) { c.apiKey = key }
}

// New builds a Client for endpoint (".../v1") defaulting requests to model.
func New(endpoint, model string, opts ...Option) *Client {
	c := &Client{
		endpoint:   strings.TrimRight(endpoint, "/"),
		model:      model,
		httpClient: &http.Client{}, // no global Timeout: streaming must run long
		timeout:    DefaultTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{}
	}
	return c
}

// Complete performs a non-streaming POST /chat/completions and returns the
// parsed response. A non-2xx status maps to an *APIError carrying status + body.
// ctx cancellation and the per-call timeout both abort the request.
func (c *Client) Complete(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	req.Stream = false
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	httpResp, err := c.do(ctx, req)
	if err != nil {
		return ChatResponse{}, err
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: read response body: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return ChatResponse{}, &APIError{
			StatusCode: httpResp.StatusCode,
			Status:     httpResp.Status,
			Body:       string(body),
		}
	}

	var resp ChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return ChatResponse{}, fmt.Errorf("llm: decode response: %w", err)
	}
	return resp, nil
}

// do marshals req and issues the POST, returning the live *http.Response (the
// caller owns Body). Shared by Complete and Stream (task 05).
func (c *Client) do(ctx context.Context, req ChatRequest) (*http.Response, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+completionsPath, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: request %s: %w", c.endpoint+completionsPath, err)
	}
	return resp, nil
}

// APIError is returned when the endpoint responds with a non-2xx status. It
// carries the HTTP status and the raw body so callers can surface the upstream
// error message (small-model endpoints put useful detail in the body).
type APIError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *APIError) Error() string {
	status := e.Status
	if status == "" {
		status = http.StatusText(e.StatusCode)
	}
	body := strings.TrimSpace(e.Body)
	if body == "" {
		return fmt.Sprintf("llm: api error %d %s", e.StatusCode, status)
	}
	return fmt.Sprintf("llm: api error %d %s: %s", e.StatusCode, status, body)
}
