package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// asMap normalises JSON bytes to a generic map so round-trip equality ignores
// struct field ordering while still catching any renamed/dropped wire field.
func asMap(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal to map: %v\n%s", err, b)
	}
	return m
}

// TestRequestRoundTrip: a ChatRequest unmarshalled from the wire fixture and
// re-marshalled is semantically identical (snake_case field names preserved).
func TestRequestRoundTrip(t *testing.T) {
	fixture := readFixture(t, "request.json")

	var req ChatRequest
	if err := json.Unmarshal(fixture, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	if req.Model != "test-model" || len(req.Messages) != 2 || req.Temperature != 0.1 {
		t.Fatalf("unexpected request parse: %+v", req)
	}
	if req.Messages[1].Role != RoleUser || req.Messages[1].Content != "say hi" {
		t.Errorf("unexpected user message: %+v", req.Messages[1])
	}

	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if got, want := asMap(t, out), asMap(t, fixture); !reflect.DeepEqual(got, want) {
		t.Errorf("request round-trip mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestChatRequestNoThinkSerialization(t *testing.T) {
	plain, err := json.Marshal(ChatRequest{Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := asMap(t, plain)["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort should be omitted by default: %s", plain)
	}

	req := ChatRequest{Model: "test-model", ReasoningEffort: "none"}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := asMap(t, body)["reasoning_effort"]; got != "none" {
		t.Fatalf("reasoning_effort = %v, want none; body=%s", got, body)
	}
}

// TestResponseRoundTrip: a plain assistant completion round-trips.
func TestResponseRoundTrip(t *testing.T) {
	fixture := readFixture(t, "response.json")

	var resp ChatResponse
	if err := json.Unmarshal(fixture, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hi there!" || resp.Choices[0].FinishReason != "stop" {
		t.Errorf("unexpected choice: %+v", resp.Choices[0])
	}
	if resp.Usage.TotalTokens != 15 {
		t.Errorf("want total_tokens 15, got %d", resp.Usage.TotalTokens)
	}

	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	if got, want := asMap(t, out), asMap(t, fixture); !reflect.DeepEqual(got, want) {
		t.Errorf("response round-trip mismatch\n got: %v\nwant: %v", got, want)
	}
}

// TestToolCallResponseRoundTrip: a tool_calls response round-trips and the
// function name/arguments parse.
func TestToolCallResponseRoundTrip(t *testing.T) {
	fixture := readFixture(t, "response_toolcall.json")

	var resp ChatResponse
	if err := json.Unmarshal(fixture, &resp); err != nil {
		t.Fatalf("unmarshal tool-call response: %v", err)
	}
	calls := resp.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(calls))
	}
	if calls[0].ID != "call_abc" || calls[0].Type != "function" {
		t.Errorf("unexpected tool call meta: %+v", calls[0])
	}
	if calls[0].Function.Name != "read_file" || calls[0].Function.Arguments != `{"path":"main.go"}` {
		t.Errorf("unexpected function call: %+v", calls[0].Function)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("want finish_reason tool_calls, got %q", resp.Choices[0].FinishReason)
	}

	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal tool-call response: %v", err)
	}
	if got, want := asMap(t, out), asMap(t, fixture); !reflect.DeepEqual(got, want) {
		t.Errorf("tool-call round-trip mismatch\n got: %v\nwant: %v", got, want)
	}
}
