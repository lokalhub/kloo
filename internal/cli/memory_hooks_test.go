package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/mcp"
	"github.com/lokalhub/kloo/internal/tools"
)

type memoryCapture struct {
	mu     sync.Mutex
	recall int
	store  int
	args   map[string]any
}

type memoryIn map[string]any

func TestMemoryHooksRecallAndStoreThroughMCP(t *testing.T) {
	cap := &memoryCapture{}
	srv := sdk.NewServer(&sdk.Implementation{Name: "memory", Version: "test"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "recall", Description: "recall"}, func(ctx context.Context, req *sdk.CallToolRequest, in memoryIn) (*sdk.CallToolResult, any, error) {
		cap.mu.Lock()
		cap.recall++
		cap.mu.Unlock()
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "remember to run tests"}}}, nil, nil
	})
	sdk.AddTool(srv, &sdk.Tool{Name: "store", Description: "store"}, func(ctx context.Context, req *sdk.CallToolRequest, in memoryIn) (*sdk.CallToolResult, any, error) {
		cap.mu.Lock()
		cap.store++
		cap.args = map[string]any(in)
		cap.mu.Unlock()
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "stored"}}}, nil, nil
	})
	httpSrv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	defer httpSrv.Close()

	ws, err := tools.NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Model:              "m",
		Endpoint:           "http://endpoint.test/v1",
		MCPServers:         map[string]config.MCPServerEntry{"memory": {URL: httpSrv.URL}},
		MCPMaxExposedTools: config.DefaultMCPMaxExposedTools,
		Memory: config.MemoryConfig{
			Enabled:        true,
			Server:         "memory",
			RecallTool:     "recall",
			StoreTool:      "store",
			MaxRecallBytes: 64,
			StoreOnFailure: true,
		},
	}
	mgr := mcp.Connect(t.Context(), mcp.ConfigFromEntries(cfg.MCPServers, cfg.MCPMaxExposedTools), nil)
	defer mgr.Close()
	if got := memoryRecall(t.Context(), cfg, mgr, ws.Root(), "task", nil); !strings.Contains(got, "remember") {
		t.Fatalf("recall = %q", got)
	}
	rep := &agent.Report{Reason: agent.ReasonError}
	summary := buildRunSummary(cfg, "go test", rep, 0, nil)
	memoryStore(t.Context(), cfg, mgr, ws.Root(), "task", summary, rep, nil)

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.recall != 1 || cap.store != 1 {
		t.Fatalf("recall/store calls = %d/%d", cap.recall, cap.store)
	}
	if cap.args["model"] != "m" || cap.args["failure_code"] != "internal_error" {
		t.Fatalf("store payload missing model/failure code: %+v", cap.args)
	}
	if _, ok := cap.args["touched_files"].([]any); !ok {
		t.Fatalf("store payload should include touched_files: %+v", cap.args)
	}
}

func TestMemoryRecallRedactsSecretsBeforeSystemPrompt(t *testing.T) {
	const modelSecret = "model-recall-secret"
	const mcpAuthSecret = "Bearer recall-auth-secret"
	const mcpAPISecret = "recall-api-key"
	const mcpEnvSecret = "recall-env-secret"
	srv := sdk.NewServer(&sdk.Implementation{Name: "memory", Version: "test"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "recall", Description: "recall"}, func(ctx context.Context, req *sdk.CallToolRequest, in memoryIn) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{
			Text: fmt.Sprintf("remember model=%s auth=%s api=%s env=%s", modelSecret, mcpAuthSecret, mcpAPISecret, mcpEnvSecret),
		}}}, nil, nil
	})
	httpSrv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	defer httpSrv.Close()

	cfg := config.Config{
		Model:    "m",
		Endpoint: "http://endpoint.test/v1",
		APIKey:   modelSecret,
		MCPServers: map[string]config.MCPServerEntry{"memory": {
			URL:     httpSrv.URL,
			Env:     map[string]string{"MEMORY_TOKEN": mcpEnvSecret},
			Headers: map[string]string{"Authorization": mcpAuthSecret, "X-API-Key": mcpAPISecret},
		}},
		MCPMaxExposedTools: config.DefaultMCPMaxExposedTools,
		Memory: config.MemoryConfig{
			Enabled:        true,
			Server:         "memory",
			RecallTool:     "recall",
			MaxRecallBytes: 512,
		},
	}
	mgr := mcp.Connect(t.Context(), mcp.ConfigFromEntries(cfg.MCPServers, cfg.MCPMaxExposedTools), nil)
	defer mgr.Close()

	recall := memoryRecall(t.Context(), cfg, mgr, t.TempDir(), "task", nil)
	section := memoryRecallSystemSection(recall)
	for _, leaked := range []string{modelSecret, mcpAuthSecret, mcpAPISecret, mcpEnvSecret} {
		if strings.Contains(recall, leaked) || strings.Contains(section, leaked) {
			t.Fatalf("recall/system section leaked secret %q:\nrecall=%s\nsection=%s", leaked, recall, section)
		}
	}
	if !strings.Contains(recall, "[redacted]") || !strings.Contains(section, "External memory recall") {
		t.Fatalf("recall should be redacted before system injection:\nrecall=%s\nsection=%s", recall, section)
	}
}

func TestMemoryHookFailuresAreNonFatalAndRedacted(t *testing.T) {
	const modelSecret = "model-secret-token"
	const mcpAuthSecret = "Bearer mcp-auth-token"
	const mcpAPISecret = "mcp-api-key"
	const mcpEnvSecret = "mcp-env-secret"
	cap := &memoryCapture{}
	srv := sdk.NewServer(&sdk.Implementation{Name: "memory", Version: "test"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "recall", Description: "recall"}, func(ctx context.Context, req *sdk.CallToolRequest, in memoryIn) (*sdk.CallToolResult, any, error) {
		cap.mu.Lock()
		cap.recall++
		cap.mu.Unlock()
		return nil, nil, fmt.Errorf("recall rejected model=%s auth=%s api=%s env=%s", modelSecret, mcpAuthSecret, mcpAPISecret, mcpEnvSecret)
	})
	sdk.AddTool(srv, &sdk.Tool{Name: "store", Description: "store"}, func(ctx context.Context, req *sdk.CallToolRequest, in memoryIn) (*sdk.CallToolResult, any, error) {
		cap.mu.Lock()
		cap.store++
		cap.mu.Unlock()
		return nil, nil, fmt.Errorf("store rejected model=%s auth=%s api=%s env=%s", modelSecret, mcpAuthSecret, mcpAPISecret, mcpEnvSecret)
	})
	httpSrv := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil))
	defer httpSrv.Close()

	cfg := config.Config{
		Model:    "m",
		Endpoint: "http://endpoint.test/v1",
		APIKey:   modelSecret,
		MCPServers: map[string]config.MCPServerEntry{"memory": {
			URL:     httpSrv.URL,
			Env:     map[string]string{"MEMORY_TOKEN": mcpEnvSecret},
			Headers: map[string]string{"Authorization": mcpAuthSecret, "X-API-Key": mcpAPISecret},
		}},
		MCPMaxExposedTools: config.DefaultMCPMaxExposedTools,
		Memory: config.MemoryConfig{
			Enabled:        true,
			Server:         "memory",
			RecallTool:     "recall",
			StoreTool:      "store",
			MaxRecallBytes: 64,
			StoreOnFailure: true,
		},
	}
	mgr := mcp.Connect(t.Context(), mcp.ConfigFromEntries(cfg.MCPServers, cfg.MCPMaxExposedTools), nil)
	defer mgr.Close()

	var logs []string
	logf := func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	if got := memoryRecall(t.Context(), cfg, mgr, t.TempDir(), "task", logf); got != "" {
		t.Fatalf("failed recall should return empty context, got %q", got)
	}
	rep := &agent.Report{Reason: agent.ReasonError}
	summary := buildRunSummary(cfg, "go test", rep, 0, nil)
	memoryStore(t.Context(), cfg, mgr, t.TempDir(), "task", summary, rep, logf)

	cap.mu.Lock()
	recall, store := cap.recall, cap.store
	cap.mu.Unlock()
	if recall != 1 || store != 1 {
		t.Fatalf("recall/store calls = %d/%d, want 1/1", recall, store)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "recall skipped") || !strings.Contains(joined, "store skipped") {
		t.Fatalf("expected non-fatal skip logs, got:\n%s", joined)
	}
	for _, leaked := range []string{modelSecret, mcpAuthSecret, mcpAPISecret, mcpEnvSecret} {
		if strings.Contains(joined, leaked) {
			t.Fatalf("memory hook logs leaked secret %q:\n%s", leaked, joined)
		}
	}
	if !strings.Contains(joined, "[redacted]") {
		t.Fatalf("memory hook logs should show redaction marker:\n%s", joined)
	}
}

func TestMemoryHookSecretValuesIncludesMCPEnvAndHeaders(t *testing.T) {
	cfg := config.Config{
		APIKey: "model-secret",
		MCPServers: map[string]config.MCPServerEntry{
			"memory": {
				Env:     map[string]string{"TOKEN": "env-secret"},
				Headers: map[string]string{"Authorization": "Bearer header-secret", "X-API-Key": "api-secret"},
			},
		},
	}
	got := strings.Join(memoryHookSecretValues(cfg), "\n")
	for _, want := range []string{"model-secret", "env-secret", "Bearer header-secret", "api-secret"} {
		if !strings.Contains(got, want) {
			t.Fatalf("secret values missing %q from %q", want, got)
		}
	}
}
