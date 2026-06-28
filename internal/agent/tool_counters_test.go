package agent

import (
	"context"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

func TestToolCountersRepeatedReadAndVerifyAttempts(t *testing.T) {
	read := tcSpec{"read_file", map[string]any{"path": "a.go"}}
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, read)},
		llmtest.Mock{Body: toolResp(t, 5, read)},
		llmtest.Mock{Body: toolResp(t, 5, read)},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	loop, _ := newLoop(t, srv, &stubVerifier{results: []VerifyResult{failResult(), failResult(), failResult()}}, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.RepeatNudgeRounds = 10
	loop.RepeatAbortRounds = 20
	loop.StallRounds = 20

	rep, err := loop.Run(context.Background(), "inspect a.go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.RepeatedReadFile != 2 {
		t.Fatalf("RepeatedReadFile = %d, want 2", rep.ToolCounters.RepeatedReadFile)
	}
	if rep.ToolCounters.VerifyAttempts != 4 {
		t.Fatalf("VerifyAttempts = %d, want 4", rep.ToolCounters.VerifyAttempts)
	}
}

func TestToolCountersInvalidArgsAndToolErrors(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "read then finish")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.InvalidToolCalls != 1 || rep.ToolCounters.ToolErrors != 1 {
		t.Fatalf("invalid/tool errors wrong: %+v", rep.ToolCounters)
	}
}

func TestToolCountersFailedNoOpAndRepeatedEdits(t *testing.T) {
	noChange := editFileCall(t, "answer.txt", "foo\n", "foo\n", 5)
	fail := editFileCall(t, "answer.txt", "missing\n", "bar\n", 5)
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: noChange},
		llmtest.Mock{Body: noChange},
		llmtest.Mock{Body: fail},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "done"}})},
	)
	loop, _ := newRealEditLoop(t, srv, "answer.txt", "foo\n", nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.EditFailLimit = 10

	rep, err := loop.Run(context.Background(), "edit answer")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ToolCounters.NoOpEdits != 2 {
		t.Fatalf("NoOpEdits = %d, want 2; counters=%+v", rep.ToolCounters.NoOpEdits, rep.ToolCounters)
	}
	if rep.ToolCounters.RepeatedEdits != 1 {
		t.Fatalf("RepeatedEdits = %d, want 1; counters=%+v", rep.ToolCounters.RepeatedEdits, rep.ToolCounters)
	}
	if rep.ToolCounters.FailedEdits != 1 || rep.ToolCounters.ToolErrors != 1 {
		t.Fatalf("failed/tool errors wrong: %+v", rep.ToolCounters)
	}
}
