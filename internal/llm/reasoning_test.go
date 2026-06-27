package llm

import "testing"

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
	got := a.response().Choices[0].Message.Content
	if got != "I think the build passes." {
		t.Errorf("assembled content = %q, want the reasoning text", got)
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
