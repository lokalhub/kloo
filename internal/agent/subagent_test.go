package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

func taskCall(inst string) tcSpec {
	return tcSpec{NameTask, map[string]any{"instruction": inst}}
}

// TestSubagentReturnsOnlyItsSummary is the whole point of delegation: the child's
// exploration must NOT enter the parent's context. If the child's reads leak into
// the parent transcript, delegation costs context instead of saving it.
func TestSubagentReturnsOnlyItsSummary(t *testing.T) {
	srv := llmtest.Sequence(t,
		// parent delegates
		llmtest.Mock{Body: toolResp(t, 5, taskCall("look at a.go and report"))},
		// child reads (this must not reach the parent), then finishes
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "SECRET-CHILD-READ.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "CHILD-SUMMARY: a.go defines Foo"}})},
		// parent finishes
		llmtest.Mock{Body: toolResp(t, 5, finishCall)},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.EnableSubagents = true

	rep, err := loop.Run(context.Background(), "delegate it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !msgWithAll(rep.Transcript, "CHILD-SUMMARY") {
		t.Error("the child's summary never reached the parent")
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "SECRET-CHILD-READ.go") {
			t.Error("the child's tool call leaked into the parent transcript — delegation saved no context")
		}
	}
}

// TestSubagentCannotDelegateFurther: unbounded nesting turns one bad plan into an
// exponential fan-out against a paid endpoint. At the depth limit the tool must be
// ABSENT from the child's vocabulary, not merely error.
//
// This exercises the REGISTRATION path in Run, which is where the real bypass was:
// the child inherited EnableSubagents and re-registered the task tool at depth 0
// on its own Run, making the limit cosmetic. Checking registryForDepth alone does
// NOT catch that — an earlier version of this test passed with the guard deleted.
func TestSubagentCannotDelegateFurther(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, finishCall)})
	child, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	child.EnableSubagents = true
	child.subagentDepth = 1 // already at the default limit
	child.Registry = tools.Without(child.Registry, NameTask)

	if _, err := child.Run(context.Background(), "child work"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := child.Registry.Lookup(NameTask); ok {
		t.Error("a subagent at the depth limit registered the task tool on its own Run — nesting is unbounded")
	}
}

// TestParentRegistersTaskBelowTheLimit is the paired positive: the guard must not
// be so strict that the top-level agent loses the tool.
func TestParentRegistersTaskBelowTheLimit(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, finishCall)})
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.EnableSubagents = true

	if _, err := loop.Run(context.Background(), "top-level work"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := loop.Registry.Lookup(NameTask); !ok {
		t.Error("the top-level agent did not get the task tool")
	}
}

// TestSubagentHasNoVerifier: the parent owns the success gate. A child that could
// verify could make a run look successful by weakening a test, invisibly.
func TestSubagentHasNoVerifier(t *testing.T) {
	verifies := 0
	v := &stubVerifier{results: []VerifyResult{passResult()}, onVerify: func() { verifies++ }}
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, taskCall("child work"))},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "a.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "edited"}})},
		llmtest.Mock{Body: toolResp(t, 5, finishCall)},
	)
	loop, _ := newLoop(t, srv, v, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.EnableSubagents = true

	before := verifies
	if _, err := loop.Run(context.Background(), "delegate"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = before
	// The child edited; if it had a verifier it would have verified mid-child.
	// Only the parent's own verifies may appear, and the parent made no edit itself.
	if verifies > 2 {
		t.Errorf("verify ran %d times — the subagent appears to own a verifier", verifies)
	}
}

// TestSubagentToolAbsentWhenDisabled: default vocabulary must be unchanged.
func TestSubagentToolAbsentWhenDisabled(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: toolResp(t, 5, finishCall)})
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	if loop.EnableSubagents {
		t.Fatal("subagents must be off by default")
	}
	if _, err := loop.Run(context.Background(), "do it"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := loop.Registry.Lookup(NameTask); ok {
		t.Error("the task tool was registered even though subagents are disabled")
	}
}

// TestWithoutDropsNamedTool pins the registry helper the depth limit relies on.
func TestWithoutDropsNamedTool(t *testing.T) {
	r := tools.NewRegistry()
	r.Register(recordTool{name: "read_file"})
	r.Register(recordTool{name: NameTask})
	out := tools.Without(r, NameTask)
	if _, ok := out.Lookup(NameTask); ok {
		t.Error("task survived Without")
	}
	if _, ok := out.Lookup("read_file"); !ok {
		t.Error("Without dropped an unrelated tool")
	}
}
