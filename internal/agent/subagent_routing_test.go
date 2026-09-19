package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestSubagentModelRoutingUsesItsOwnEndpoint: a delegated subtask may run on a
// different model than the parent. Measured on kloo-bench, kloo passes 5 of the 9
// glimmer-gap cases on qwen with the identical harness — the harness can solve
// them, the model will not act — so routing the delegated work to a model that
// does act is the honest use of that finding.
//
// Two servers: the parent's and the child's. The child's request must land on the
// CHILD server, and the parent's config must be untouched afterwards.
func TestSubagentModelRoutingUsesItsOwnEndpoint(t *testing.T) {
	childSrv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "CHILD-RAN-HERE"}})},
	)
	parentSrv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{NameTask, map[string]any{"instruction": "do the thing"}})},
		llmtest.Mock{Body: toolResp(t, 5, finishCall)},
	)
	loop, _ := newLoop(t, parentSrv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.EnableSubagents = true
	loop.SubagentModel = "other-model"
	loop.SubagentEndpoint = childSrv.URL + "/v1"
	loop.NewSubagentClient = func(ep, model string) llm.LLMClient { return llm.New(ep, model) }

	parentModel, parentEndpoint := loop.Model, loop.Endpoint

	rep, err := loop.Run(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !msgWithAll(rep.Transcript, "CHILD-RAN-HERE") {
		t.Error("the routed child never ran, or its summary did not come back")
	}
	if loop.Model != parentModel || loop.Endpoint != parentEndpoint {
		t.Errorf("routing mutated the PARENT: model %q->%q endpoint %q->%q",
			parentModel, loop.Model, parentEndpoint, loop.Endpoint)
	}
}

// TestSubagentRoutingSkippedWithoutAClientFactory: a routed child with no factory
// would be built with no API key and fail auth on every real endpoint. Degrade to
// the parent's client instead of producing a silently unauthenticated child.
func TestSubagentRoutingSkippedWithoutAClientFactory(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{NameTask, map[string]any{"instruction": "x"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "ran-on-parent"}})},
		llmtest.Mock{Body: toolResp(t, 5, finishCall)},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.EnableSubagents = true
	loop.SubagentModel = "other-model"      // routing requested...
	loop.SubagentEndpoint = "http://unused" // ...at an endpoint that would fail
	loop.NewSubagentClient = nil            // ...but no factory

	rep, err := loop.Run(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !msgWithAll(rep.Transcript, "ran-on-parent") {
		t.Error("routing without a factory did not fall back to the parent's client")
	}
}

// TestSubagentRoutingOffByDefault: with no routing configured the child shares the
// parent's model, so the default behaviour is unchanged.
func TestSubagentRoutingOffByDefault(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{NameTask, map[string]any{"instruction": "x"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "same-endpoint"}})},
		llmtest.Mock{Body: toolResp(t, 5, finishCall)},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.EnableSubagents = true
	if loop.SubagentModel != "" {
		t.Fatal("routing must be unset by default")
	}
	rep, err := loop.Run(context.Background(), "delegate")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !msgWithAll(rep.Transcript, "same-endpoint") {
		t.Error("child did not run on the parent's endpoint")
	}
}
