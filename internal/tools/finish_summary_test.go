package tools

import (
	"context"
	"testing"
)

// TestFinishSummaryAcceptsArgumentAliases pins the second way a finish reply was
// lost: the loop read only Args["summary"], and Qwen3-Next emits the call as
// <function=finish><parameter=message>, so the answer resolved to "" and the user
// saw a counters-only box — indistinguishable from the model saying nothing.
func TestFinishSummaryAcceptsArgumentAliases(t *testing.T) {
	for _, key := range finishSummaryKeys {
		got := FinishSummary(map[string]any{key: "the workspace has 13 entries"})
		if got != "the workspace has 13 entries" {
			t.Errorf("arg %q: FinishSummary = %q, want the text", key, got)
		}
	}
}

// The schema's own name must win when a model supplies more than one, so a
// tolerant read can never reorder a well-behaved call's meaning.
func TestFinishSummaryPrefersSchemaName(t *testing.T) {
	got := FinishSummary(map[string]any{"message": "fallback", "summary": "canonical"})
	if got != "canonical" {
		t.Errorf("FinishSummary = %q, want the summary arg to win", got)
	}
}

func TestFinishSummaryEmptyCases(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"nil args", nil},
		{"no recognised key", map[string]any{"nonsense": "x"}},
		{"blank string", map[string]any{"summary": "   "}},
		{"non-string value", map[string]any{"summary": 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FinishSummary(tc.args); got != "" {
				t.Errorf("FinishSummary = %q, want empty", got)
			}
		})
	}
}

// A blank canonical arg must not mask a real alias — a model that emits both an
// empty "summary" and a populated "message" still has something to say.
func TestFinishSummarySkipsBlankCanonical(t *testing.T) {
	got := FinishSummary(map[string]any{"summary": "", "message": "the real answer"})
	if got != "the real answer" {
		t.Errorf("FinishSummary = %q, want the non-blank alias", got)
	}
}

// Invoke echoes through the same tolerant path, so the observation appended to the
// conversation matches what the user is shown.
func TestFinishInvokeUsesTolerantRead(t *testing.T) {
	res, err := finishTool{}.Invoke(context.Background(), Call{
		Name: NameFinish,
		Args: map[string]any{"message": "done, 13 entries"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Output != "done, 13 entries" {
		t.Errorf("Output = %q, want the message text", res.Output)
	}
}
