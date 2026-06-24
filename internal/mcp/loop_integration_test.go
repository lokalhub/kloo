package mcp

// loop_integration_test.go drives the REAL agent loop (mocked LLM + in-memory MCP
// server) and proves it threads an MCP tool call through act→apply→verify exactly
// like a builtin — with no changes to internal/agent/loop.go.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// fakeVerifier is a trivial always-passing verifier (no shell/git needed).
type fakeVerifier struct{}

func (fakeVerifier) Verify(context.Context) agent.VerifyResult {
	return agent.VerifyResult{Passed: true}
}

// toolCallBody renders a native tool_calls ChatResponse for one call.
func toolCallBody(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	ab, _ := json.Marshal(args)
	resp := llm.ChatResponse{
		Choices: []llm.Choice{{Message: llm.Message{
			Role: llm.RoleAssistant,
			ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{
				Name: name, Arguments: string(ab),
			}}},
		}, FinishReason: "tool_calls"}},
		Usage: llm.Usage{TotalTokens: 20},
	}
	b, _ := json.Marshal(resp)
	return string(b)
}

// TestLoopDispatchesMCPTool: the loop calls an MCP tool (turn 1) then finishes
// (turn 2); the MCP observation is threaded like a builtin's.
func TestLoopDispatchesMCPTool(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerTools(reg, c, []string{"echo"})

	srv := llmtest.Sequence(t,
		// Turn 1: call the MCP tool.
		llmtest.Mock{Body: toolCallBody(t, "srv__echo", map[string]any{"text": "ping"})},
		// Turn 2: finish (loop intercepts NameFinish as a terminal stop).
		llmtest.Mock{Body: toolCallBody(t, tools.NameFinish, map[string]any{"summary": "done"})},
	)

	var (
		sawMCP    bool
		mcpOutput string
	)
	cfg := config.Config{MaxSteps: 10, ChurnRounds: 10}
	loop := &agent.Loop{
		Client:        llm.New(srv.URL+"/v1", "test-model"),
		Adapter:       tools.NativeFCAdapter{},
		Registry:      reg,
		Verifier:      fakeVerifier{},
		Budget:        agent.NewBudget(cfg, time.Now),
		Churn:         agent.NewChurnDetector(cfg.ChurnRounds),
		ContextTokens: 500,
		System:        "call the mcp tool, then finish",
		OnTool: func(call tools.Call, res tools.Result, err error) {
			if call.Name == "srv__echo" {
				sawMCP = true
				mcpOutput = res.Output
				if err != nil {
					t.Errorf("MCP tool dispatch errored: %v", err)
				}
			}
		},
	}

	rep, err := loop.Run(context.Background(), "use the mcp tool")
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if !sawMCP {
		t.Fatal("the loop never dispatched the MCP tool")
	}
	if mcpOutput != "ping" {
		t.Errorf("MCP observation Output = %q, want ping (mapped via toResult)", mcpOutput)
	}
	// Finished cleanly via the finish interceptor (verify passes ⇒ success).
	if rep.Reason != agent.ReasonSuccess {
		t.Errorf("reason = %q, want success (%s)", rep.Reason, rep.String())
	}
	if rep.Steps != 2 {
		t.Errorf("steps = %d, want 2 (MCP call then finish)", rep.Steps)
	}
}
