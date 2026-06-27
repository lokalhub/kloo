package llm

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestFinalizeReasoning: a thinking model that leaves content empty but fills
// reasoning_content has its reasoning promoted to content (so the loop doesn't see a
// blank turn); a model that answered in content keeps it (reasoning is discarded).
func TestFinalizeReasoning(t *testing.T) {
	cases := []struct {
		name, content, reasoning, want string
	}{
		{"empty content uses reasoning", "", "the answer is 42", "the answer is 42"},
		{"whitespace content uses reasoning", "  \n ", "real output", "real output"},
		{"content present wins", "actual answer", "some thinking", "actual answer"},
		{"both empty stays empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := Message{Role: RoleAssistant, Content: c.content, ReasoningContent: c.reasoning}
			m.FinalizeReasoning()
			if m.Content != c.want {
				t.Errorf("Content = %q, want %q", m.Content, c.want)
			}
		})
	}
}

// TestStreamReasoningFallback: when reasoning_content streams in deltas while content
// stays empty, the assembled response's Content is the reasoning.
func TestStreamReasoningFallback(t *testing.T) {
	a := newAccumulator()
	a.absorbDelta(Delta{Role: RoleAssistant, ReasoningContent: "I think "})
	a.absorbDelta(Delta{ReasoningContent: "the build passes."})
	resp := a.response()
	msg := resp.Choices[0].Message
	if msg.Content != "I think the build passes." {
		t.Errorf("assembled content = %q, want the reasoning text", msg.Content)
	}
	if msg.RawContent != "" || msg.RawReasoningContent != "I think the build passes." {
		t.Errorf("raw reasoning metadata not preserved: %+v", msg)
	}
}

// TestStreamContentWinsOverReasoning: real content streamed alongside reasoning keeps
// the content (reasoning is just the model's thinking).
func TestStreamContentWinsOverReasoning(t *testing.T) {
	a := newAccumulator()
	a.absorbDelta(Delta{ReasoningContent: "thinking..."})
	a.absorbDelta(Delta{Content: "DONE"})
	if got := a.response().Choices[0].Message.Content; got != "DONE" {
		t.Errorf("content = %q, want DONE", got)
	}
}

func TestStreamReasoningDeltaIsNotForwardedAsDisplayContent(t *testing.T) {
	transcript := `data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"private chain of thought"},"finish_reason":"length"}]}

data: [DONE]

`
	var displayed strings.Builder
	resp, err := parseSSE(context.Background(), strings.NewReader(transcript), func(d Delta) error {
		displayed.WriteString(d.Content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if displayed.String() != "" {
		t.Fatalf("reasoning_content should not be forwarded as display content, got %q", displayed.String())
	}
	msg := resp.Choices[0].Message
	if msg.RawReasoningContent != "private chain of thought" || msg.FinishReason != "length" {
		t.Fatalf("reasoning metadata should still be preserved: %+v", msg)
	}
}

func TestNonStreamingReasoningMetadataSurvivesFinalize(t *testing.T) {
	srv := llmtest.JSON(t, `{
		"choices": [{
			"message": {"role": "assistant", "content": "", "reasoning_content": "fallback answer"},
			"finish_reason": "stop"
		}]
	}`)
	client := New(srv.URL+"/v1", "test-model")

	resp, err := client.Complete(context.Background(), ChatRequest{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	msg := resp.Choices[0].Message
	if msg.Content != "fallback answer" {
		t.Fatalf("reasoning fallback content = %q", msg.Content)
	}
	if msg.RawContent != "" || msg.RawReasoningContent != "fallback answer" || msg.FinishReason != "stop" {
		t.Fatalf("raw reasoning metadata not preserved: %+v", msg)
	}
}

func TestStreamingReasoningFinishMetadataSurvivesFinalize(t *testing.T) {
	a := newAccumulator()
	a.finishReason = "length"
	a.absorbDelta(Delta{Role: RoleAssistant, ReasoningContent: "unfinished"})
	msg := a.response().Choices[0].Message
	if msg.Content != "unfinished" || msg.RawReasoningContent != "unfinished" || msg.FinishReason != "length" {
		t.Fatalf("streaming metadata not preserved: %+v", msg)
	}
}
