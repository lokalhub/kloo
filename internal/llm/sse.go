package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// Streaming sentinel errors.
var (
	// ErrStreamIncomplete is returned when the stream ends (EOF) before the
	// terminating "data: [DONE]" sentinel. The accumulated partial response is
	// still returned alongside it.
	ErrStreamIncomplete = errors.New("stream ended without [DONE]")
	// ErrStreamError wraps an error chunk surfaced mid-stream by the endpoint.
	ErrStreamError = errors.New("stream error chunk")
	// ErrStreamIdle is returned when a stream produced no new token for the idle
	// timeout (a stalled server) — it breaks the hang without capping a live stream.
	ErrStreamIdle = errors.New("stream stalled (idle timeout)")
)

// streamChunk is one SSE "data:" payload of a streaming chat completion. It
// reuses Choice (whose Delta field holds the incremental fragment) and adds an
// optional top-level error block that some endpoints emit mid-stream.
type streamChunk struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Created int64    `json:"created"`
	Choices []Choice `json:"choices"`
	// Usage is present only on the final pre-[DONE] chunk when the request opted
	// in via stream_options.include_usage. That chunk carries choices:[] and a
	// populated usage, so it is absorbed outside the choices loop. A pointer
	// distinguishes "absent on this chunk" (the common case) from all-zeros.
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Stream performs a streaming POST /chat/completions, invoking onDelta for each
// incremental delta (content or tool-call fragments) as it arrives, and returns
// the fully assembled ChatResponse once the stream completes with [DONE].
//
// onDelta may be nil (accumulate only). If onDelta returns an error, streaming
// stops and that error is returned with the response accumulated so far. A
// stream that ends without [DONE] returns the partial response plus
// ErrStreamIncomplete; a mid-stream error chunk returns ErrStreamError. ctx
// cancellation stops the stream promptly (the Phase-05 interrupt foundation).
func (c *Client) Stream(ctx context.Context, req ChatRequest, onDelta func(Delta) error) (ChatResponse, error) {
	req.Stream = true
	// Opt into the final usage chunk: under the OpenAI/llama.cpp streaming
	// protocol the server only emits usage for a stream when asked. Honour a
	// caller that set its own options.
	if req.StreamOptions == nil {
		req.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	// Idle watchdog: cancel the request if no token arrives for streamIdle, and
	// reset the clock on every delta. This breaks a STALLED stream (a server that
	// accepted the request then went silent — e.g. a hung local model) without ever
	// capping a stream that is still flowing.
	streamCtx := ctx
	var idled atomic.Bool
	if c.streamIdle > 0 {
		var cancel context.CancelFunc
		streamCtx, cancel = context.WithCancel(ctx)
		defer cancel()
		timer := time.AfterFunc(c.streamIdle, func() { idled.Store(true); cancel() })
		defer timer.Stop()
		inner := onDelta
		onDelta = func(d Delta) error {
			timer.Reset(c.streamIdle)
			if inner != nil {
				return inner(d)
			}
			return nil
		}
	}

	httpResp, err := c.do(streamCtx, req)
	if err != nil {
		return ChatResponse{}, err
	}
	defer httpResp.Body.Close()

	// A non-2xx arrives before the event stream — read the body as an APIError.
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(httpResp.Body)
		return ChatResponse{}, &APIError{
			StatusCode: httpResp.StatusCode,
			Status:     httpResp.Status,
			Body:       string(body),
		}
	}

	resp, err := parseSSE(streamCtx, httpResp.Body, onDelta)
	if idled.Load() {
		// The watchdog cancelled the context; surface a clear stall error (not a
		// generic context.Canceled) so the loop reports it and the rails take over.
		return resp, fmt.Errorf("llm: no token for %s: %w", c.streamIdle, ErrStreamIdle)
	}
	return resp, err
}

// parseSSE consumes an SSE body, accumulating deltas into a ChatResponse. It is
// separated from Stream so it can be unit-tested directly against transcripts.
func parseSSE(ctx context.Context, body io.Reader, onDelta func(Delta) error) (ChatResponse, error) {
	reader := bufio.NewReader(body)

	acc := newAccumulator()
	var dataBuf strings.Builder // accumulates consecutive data: lines of one event
	done := false

	// flushEvent processes one complete SSE event's data payload.
	flushEvent := func() (stop bool, err error) {
		if dataBuf.Len() == 0 {
			return false, nil
		}
		data := dataBuf.String()
		dataBuf.Reset()

		if data == "[DONE]" {
			done = true
			return true, nil
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true, fmt.Errorf("llm: decode stream chunk: %w", err)
		}
		if chunk.Error != nil {
			return true, fmt.Errorf("llm: %w: %s", ErrStreamError, chunk.Error.Message)
		}
		acc.absorbMeta(chunk)
		// Capture usage outside the choices loop: the usage-bearing chunk has an
		// empty choices array, so the loop below never runs for it.
		if chunk.Usage != nil {
			acc.usage = *chunk.Usage
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != "" {
				acc.finishReason = ch.FinishReason
			}
			if ch.Delta == nil {
				continue
			}
			acc.absorbDelta(*ch.Delta)
			if onDelta != nil {
				d := *ch.Delta
				// Show a thinking model's reasoning live (else the UI looks frozen for
				// hundreds of thinking tokens); the final fallback still owns assembly.
				if d.Content == "" && d.ReasoningContent != "" {
					d.Content = d.ReasoningContent
				}
				if err := onDelta(d); err != nil {
					return true, err
				}
			}
		}
		return false, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return acc.response(), err
		}

		line, readErr := reader.ReadString('\n')
		// Process the line content even when ReadString returns io.EOF with a
		// final unterminated line.
		trimmed := strings.TrimRight(line, "\r\n")

		switch {
		case trimmed == "":
			// Event boundary (or keep-alive blank line).
			stop, err := flushEvent()
			if err != nil {
				return acc.response(), err
			}
			if stop {
				return acc.response(), nil
			}
		case strings.HasPrefix(trimmed, ":"):
			// SSE comment / keep-alive — ignore.
		case strings.HasPrefix(trimmed, "data:"):
			payload := strings.TrimPrefix(trimmed, "data:")
			payload = strings.TrimPrefix(payload, " ")
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(payload)
		default:
			// Other SSE fields (event:, id:, retry:) are not used here.
		}

		if readErr != nil {
			if readErr == io.EOF {
				// Flush any final event that lacked a trailing blank line.
				stop, err := flushEvent()
				if err != nil {
					return acc.response(), err
				}
				if done || stop {
					return acc.response(), nil
				}
				return acc.response(), ErrStreamIncomplete
			}
			return acc.response(), fmt.Errorf("llm: read stream: %w", readErr)
		}
	}
}

// accumulator assembles streamed deltas into a single ChatResponse.
type accumulator struct {
	id           string
	model        string
	created      int64
	role         string
	content      strings.Builder
	reasoning    strings.Builder // thinking model's reasoning_content (folded into content if content is empty)
	toolCalls    []ToolCall
	indexPos     map[int]int // delta tool-call index -> position in toolCalls
	finishReason string
	usage        Usage // captured from the final include_usage chunk (zero if absent)
}

func newAccumulator() *accumulator {
	return &accumulator{role: RoleAssistant, indexPos: map[int]int{}}
}

func (a *accumulator) absorbMeta(chunk streamChunk) {
	if a.id == "" {
		a.id = chunk.ID
	}
	if a.model == "" {
		a.model = chunk.Model
	}
	if a.created == 0 {
		a.created = chunk.Created
	}
}

func (a *accumulator) absorbDelta(d Delta) {
	if d.Role != "" {
		a.role = d.Role
	}
	a.content.WriteString(d.Content)
	a.reasoning.WriteString(d.ReasoningContent)
	for _, tc := range d.ToolCalls {
		pos, ok := a.indexPos[tc.Index]
		if !ok {
			pos = len(a.toolCalls)
			a.indexPos[tc.Index] = pos
			a.toolCalls = append(a.toolCalls, ToolCall{Index: tc.Index, Type: "function"})
		}
		cur := &a.toolCalls[pos]
		if tc.ID != "" {
			cur.ID = tc.ID
		}
		if tc.Type != "" {
			cur.Type = tc.Type
		}
		if tc.Function.Name != "" {
			cur.Function.Name = tc.Function.Name
		}
		cur.Function.Arguments += tc.Function.Arguments
	}
}

// response materialises the accumulated state into a ChatResponse.
func (a *accumulator) response() ChatResponse {
	msg := Message{Role: a.role, Content: a.content.String(), ReasoningContent: a.reasoning.String()}
	msg.FinalizeReasoning() // fold reasoning into content when a thinking model left content blank
	if len(a.toolCalls) > 0 {
		// Drop the streaming-only Index from the assembled (non-streaming) calls.
		calls := make([]ToolCall, len(a.toolCalls))
		for i, tc := range a.toolCalls {
			tc.Index = 0
			calls[i] = tc
		}
		msg.ToolCalls = calls
	}
	return ChatResponse{
		ID:      a.id,
		Object:  "chat.completion",
		Created: a.created,
		Model:   a.model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: a.finishReason}},
		Usage:   a.usage,
	}
}
