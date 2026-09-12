package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultTimeout bounds a single non-streaming Complete call. Streaming (Stream,
// task 05) deliberately does NOT apply this overall deadline — a long stream is
// governed by the caller's context instead.
const DefaultTimeout = 120 * time.Second

// DefaultStreamIdleTimeout aborts a stream that produces NO new token for this
// long. It is NOT an overall deadline — the timer resets on every token, so it
// never cuts a stream that is still flowing. The only window it really bounds is
// time-to-FIRST-token (prefill), which for a small local model chewing a large
// context on weak hardware can take minutes — so the default is deliberately
// generous (it's a backstop for an indefinite hang, not a tight SLA). Override per
// run with WithStreamIdleTimeout (0 ⇒ disabled, the old no-timeout behaviour).
const DefaultStreamIdleTimeout = 5 * time.Minute

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
	streamIdle time.Duration // abort a stream after this long with no new token
	// cacheRejected remembers that THIS endpoint refused cache_control, so the
	// marker is dropped for the rest of the run instead of costing a retry per
	// request. It is per-Client and atomic on purpose: kloo runs more than one
	// endpoint in a single binary (the main loop plus the curator/compactor
	// paths), requests are issued from multiple goroutines, and a package-level
	// var would both race and let one rejecting endpoint silently disable
	// caching for every other endpoint in the process.
	cacheRejected atomic.Bool
	logf          func(format string, args ...any)
}

// WithLogf installs a one-line notice sink (nil ⇒ silent). Used for the
// prompt-cache rejection notice, which must be visible but must never fail a run.
func WithLogf(f func(format string, args ...any)) Option {
	return func(c *Client) { c.logf = f }
}

// notify emits a single run notice when a sink is installed.
func (c *Client) notify(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
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

// WithStreamIdleTimeout sets the no-token idle timeout for Stream (0 ⇒ disabled).
func WithStreamIdleTimeout(d time.Duration) Option {
	return func(c *Client) { c.streamIdle = d }
}

// WithAPIKey sets a bearer token (unused against a local llama.cpp/Ollama server, needed for
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
		streamIdle: DefaultStreamIdleTimeout,
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

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return ChatResponse{}, &APIError{
			StatusCode: httpResp.StatusCode,
			Status:     httpResp.Status,
			Body:       readAPIErrorBody(httpResp.Body, c.apiKey),
		}
	}

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm: read response body: %w", err)
	}

	var resp ChatResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return ChatResponse{}, fmt.Errorf("llm: decode response: %w", err)
	}
	// Reasoning-content fallback: a thinking model may leave content empty and put its
	// output in reasoning_content — fold it back so the loop never sees a blank turn.
	for i := range resp.Choices {
		msg := &resp.Choices[i].Message
		msg.RawContent = msg.Content
		msg.RawReasoningContent = msg.ReasoningContent
		msg.FinishReason = resp.Choices[i].FinishReason
		msg.FinalizeReasoning()
	}
	return resp, nil
}

// do issues the POST, recovering once from an endpoint that rejects
// cache_control. Shared by Complete and Stream; the caller owns the Body.
//
// The recovery is deliberately narrow. It engages ONLY when this request actually
// carried a marker and the reply was a 4xx whose body names the field, and it
// retries exactly once without the marker. It does not widen the transient-retry
// ladder (DefaultLLMRetryableStatusCodes): a 4xx is still not retryable in
// general, and TestNonRetryable4xxStillNotRetried pins that.
func (c *Client) do(ctx context.Context, req ChatRequest) (*http.Response, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	raw := req.Messages
	if c.cacheRejected.Load() {
		raw = stripCacheControl(raw)
	}
	req.Messages = normalizeMessages(raw)

	resp, err := c.post(ctx, req)
	if err != nil || !hasCacheControl(req.Messages) {
		return resp, err
	}
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		return resp, nil
	}

	// A 4xx on a request that asked for caching: read the body to see whether the
	// field is what the endpoint objected to. If not, hand the response back
	// untouched (with its body restored) so the caller reports the real error.
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil || !mentionsCacheControl(body) {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	}

	if c.cacheRejected.CompareAndSwap(false, true) {
		c.notify("prompt cache rejected by endpoint (http %d: %s) — retried without it; prompt caching is off for the rest of this run",
			resp.StatusCode, firstLineOf(redactSecrets(string(body), c.apiKey)))
	}
	retry := req
	retry.Messages = normalizeMessages(stripCacheControl(raw))
	return c.post(ctx, retry)
}

// hasCacheControl reports whether any message carries a breakpoint.
func hasCacheControl(msgs []Message) bool {
	for _, m := range msgs {
		if m.CacheControl != nil {
			return true
		}
	}
	return false
}

// stripCacheControl returns msgs with every breakpoint removed. It copies rather
// than mutating: the caller's slice is shared with the agent loop.
func stripCacheControl(msgs []Message) []Message {
	if !hasCacheControl(msgs) {
		return msgs
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].CacheControl = nil
	}
	return out
}

// mentionsCacheControl reports whether an error body blames the cache field or the
// content-parts shape it arrives in. Endpoints word this differently ("unknown
// field", "unexpected keyword", "content must be a string"), so match on the
// field and shape names rather than on any one provider's phrasing.
func mentionsCacheControl(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, needle := range []string{"cache_control", "cache control", "content parts", "content must be a string"} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// firstLineOf trims an error body to one line so the notice stays a single line.
func firstLineOf(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	const max = 160
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// post marshals an already-normalized request and issues the POST.
func (c *Client) post(ctx context.Context, req ChatRequest) (*http.Response, error) {
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

// normalizeMessages merges consecutive same-role messages so the request never
// sends two adjacent same-role turns. Strict OpenAI-compatible servers (llama.cpp)
// reject e.g. two trailing assistant messages — which kloo's working-memory
// assembler can produce when it filters the edit-path turns out of the recent tail,
// leaving two assistants adjacent. Lenient providers (OpenRouter) accept it; this
// makes the request well-formed for both. System messages are never merged.
func normalizeMessages(in []Message) []Message {
	if len(in) < 2 {
		return in
	}
	out := make([]Message, 0, len(in))
	for _, m := range in {
		n := len(out)
		// A cache breakpoint is a semantic boundary: merging the message that
		// carries it with the one after would move the breakpoint BELOW content the
		// marker was placed above, caching something volatile. Unmarked messages
		// (every message when caching is off) merge exactly as before.
		if n > 0 && out[n-1].Role == m.Role && m.Role != RoleSystem && out[n-1].CacheControl == nil {
			prev := out[n-1]
			switch {
			case prev.Content == "":
				prev.Content = m.Content
			case m.Content != "":
				prev.Content = strings.TrimRight(prev.Content, "\n") + "\n" + m.Content
			}
			prev.ToolCalls = append(prev.ToolCalls, m.ToolCalls...)
			out[n-1] = prev
			continue
		}
		out = append(out, m)
	}
	return out
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
