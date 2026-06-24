package mcp

// race_test.go is the concurrency-safety proof for the brief's single-threaded
// integration claim. Driven under `CGO_ENABLED=1 go test -race ./internal/mcp/...`
// (run locally during Phase-03 validation; the race detector needs CGO, kept
// separate from the CGO_ENABLED=0 release build), it exercises a full mocked-LLM
// agent.Loop.Run that dispatches an in-memory MCP tool across several turns. No
// data race must be reported: the ClientSession is touched only from the loop
// goroutine, and tools are snapshotted at connect (see the package doc).

import (
	"context"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// failVerifier never passes, so the loop neither short-circuits to success nor
// trips the (pass-only) stall backstop — letting the scripted multi-turn run play
// out and end on the finish turn (ReasonAnswered).
type failVerifier struct{}

func (failVerifier) Verify(context.Context) agent.VerifyResult {
	return agent.VerifyResult{Passed: false}
}

func TestLoopMultiTurnMCPRaceSafe(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerTools(reg, c, []string{"echo"})

	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolCallBody(t, "srv__echo", map[string]any{"text": "ping1"})},
		llmtest.Mock{Body: toolCallBody(t, "srv__echo", map[string]any{"text": "ping2"})},
		llmtest.Mock{Body: toolCallBody(t, "srv__echo", map[string]any{"text": "ping3"})},
		llmtest.Mock{Body: toolCallBody(t, tools.NameFinish, map[string]any{"summary": "done"})},
	)

	calls := 0
	cfg := config.Config{MaxSteps: 20, ChurnRounds: 100}
	loop := &agent.Loop{
		Client:        llm.New(srv.URL+"/v1", "test-model"),
		Adapter:       tools.NativeFCAdapter{},
		Registry:      reg,
		Verifier:      failVerifier{},
		Budget:        agent.NewBudget(cfg, time.Now),
		Churn:         agent.NewChurnDetector(cfg.ChurnRounds),
		StallRounds:   100,
		ContextTokens: 500,
		System:        "call echo a few times, then finish",
		OnTool: func(call tools.Call, _ tools.Result, _ error) {
			if call.Name == "srv__echo" {
				calls++
			}
		},
	}

	rep, err := loop.Run(context.Background(), "exercise the MCP tool repeatedly")
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if calls < 3 {
		t.Errorf("expected ≥ 3 MCP tool dispatches, got %d (rep: %s)", calls, rep.String())
	}
}
