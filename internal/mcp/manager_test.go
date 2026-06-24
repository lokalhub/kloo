package mcp

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lokalhub/kloo/internal/tools"
)

// TestConnectNonFatalGoodAndBad: a good (HTTP) server connects; a bad server
// (non-existent command) is logged and skipped; the Manager holds only the
// survivor and the run proceeds.
func TestConnectNonFatalGoodAndBad(t *testing.T) {
	ctx := context.Background()
	good := httpMCPServerURL(t, newEchoAddBoomServer("good"))

	lc := &logCapture{}
	cfg := Config{
		Servers: []ServerConfig{
			{Name: "bad", Command: "kloo-definitely-not-a-real-binary-zzz"},
			{Name: "good", URL: good, ExposeMode: ExposeAll},
		},
		MaxExposedTools: 16,
	}
	m := Connect(ctx, cfg, lc.logf)
	t.Cleanup(func() { _ = m.Close() })

	if len(m.servers) != 1 || m.servers[0].client.Name != "good" {
		t.Fatalf("Manager should hold only the survivor, got %d servers", len(m.servers))
	}
	log := lc.joined()
	if !strings.Contains(log, `skipped "bad"`) || !strings.Contains(log, "run continues") {
		t.Errorf("bad server not logged as a non-fatal skip:\n%s", log)
	}
	if !strings.Contains(log, `connected "good" (http)`) {
		t.Errorf("good server connect line missing:\n%s", log)
	}
	if !strings.Contains(log, "OUTSIDE kloo's workspace sandbox") {
		t.Errorf("trust note not emitted on a successful connect:\n%s", log)
	}

	reg := tools.NewRegistry()
	m.Register(reg)
	if _, ok := reg.Lookup("good__echo"); !ok {
		t.Error("survivor's tools should be registered")
	}
}

// TestConnectAllBadEmptyManager: every server fails ⇒ empty Manager, Register adds
// nothing, no trust note (the run still has only builtins).
func TestConnectAllBadEmptyManager(t *testing.T) {
	ctx := context.Background()
	lc := &logCapture{}
	cfg := Config{Servers: []ServerConfig{
		{Name: "bad1", Command: "kloo-nope-1-zzz"},
		{Name: "bad2"}, // neither command nor url ⇒ ErrBadServerConfig
	}}
	m := Connect(ctx, cfg, lc.logf)

	if len(m.servers) != 0 {
		t.Fatalf("all-bad ⇒ empty Manager, got %d", len(m.servers))
	}
	reg := tools.NewRegistry()
	m.Register(reg)
	if len(reg.Tools()) != 0 {
		t.Errorf("Register on an empty Manager should add nothing, got %d tools", len(reg.Tools()))
	}
	if strings.Contains(lc.joined(), "OUTSIDE kloo's workspace") {
		t.Error("no trust note should be emitted when no server connected")
	}
}

// TestConnectEmptyConfigNoLogNoise: nil/empty Servers ⇒ empty Manager, zero log
// lines (byte-identical to a no-MCP run).
func TestConnectEmptyConfigNoLogNoise(t *testing.T) {
	lc := &logCapture{}
	m := Connect(context.Background(), Config{}, lc.logf)
	if len(m.servers) != 0 {
		t.Fatalf("empty config ⇒ empty Manager")
	}
	if len(lc.lines) != 0 {
		t.Errorf("empty config should produce no log lines, got:\n%s", lc.joined())
	}
}

// TestConnectDisabledSkipped: a Disabled server is not dialed.
func TestConnectDisabledSkipped(t *testing.T) {
	ctx := context.Background()
	good := httpMCPServerURL(t, newEchoAddBoomServer("d"))
	lc := &logCapture{}
	cfg := Config{Servers: []ServerConfig{
		{Name: "d", URL: good, Disabled: true},
	}}
	m := Connect(ctx, cfg, lc.logf)
	if len(m.servers) != 0 {
		t.Errorf("disabled server should not be connected")
	}
	if !strings.Contains(lc.joined(), `skipped "d" — disabled`) {
		t.Errorf("disabled skip not logged:\n%s", lc.joined())
	}
}

// TestRegisterGlobalCapSpansServers: the cap is global — two servers whose
// combined tools exceed the cap demote across the server boundary, logged.
func TestRegisterGlobalCapSpansServers(t *testing.T) {
	ctx := context.Background()
	urlA := httpMCPServerURL(t, newEchoAddBoomServer("a")) // echo, add, boom (3)
	urlB := httpMCPServerURL(t, newEchoAddBoomServer("b")) // echo, add, boom (3)

	lc := &logCapture{}
	cfg := Config{
		Servers: []ServerConfig{
			{Name: "a", URL: urlA, ExposeMode: ExposeAll},
			{Name: "b", URL: urlB, ExposeMode: ExposeAll},
		},
		MaxExposedTools: 4, // 3 from A + 1 from B, then B demotes the rest to lazy
	}
	m := Connect(ctx, cfg, lc.logf)
	t.Cleanup(func() { _ = m.Close() })

	reg := tools.NewRegistry()
	m.Register(reg)

	firstClass := 0
	for _, n := range mcpToolNames(reg) {
		if !strings.HasSuffix(n, "list_tools") && !strings.HasSuffix(n, "describe_tool") && !strings.HasSuffix(n, "call_tool") {
			firstClass++
		}
	}
	if firstClass != 4 {
		t.Errorf("first-class MCP tools across servers = %d, want 4 (the global cap)", firstClass)
	}
	// Server B is the one that hit the cap and demoted to lazy.
	if _, ok := reg.Lookup("b__list_tools"); !ok {
		t.Error("server b's demoted remainder should register the lazy meta-trio")
	}
	if !strings.Contains(lc.joined(), `server "b" hit maxExposedTools cap (4)`) {
		t.Errorf("cross-server demotion not logged for server b:\n%s", lc.joined())
	}
}

// TestCloseBestEffort: Close closes all clients and is safe to call; a second
// Close does not panic.
func TestCloseBestEffort(t *testing.T) {
	ctx := context.Background()
	urlA := httpMCPServerURL(t, newEchoAddBoomServer("a"))
	urlB := httpMCPServerURL(t, newEchoAddBoomServer("b"))
	cfg := Config{Servers: []ServerConfig{
		{Name: "a", URL: urlA, ExposeMode: ExposeAll},
		{Name: "b", URL: urlB, ExposeMode: ExposeAll},
	}}
	m := Connect(ctx, cfg, nil) // nil logger ⇒ no-op
	if len(m.servers) != 2 {
		t.Fatalf("want 2 connected, got %d", len(m.servers))
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close (healthy clients) = %v, want nil", err)
	}
	// After Close, a tool call must fail (session closed) — proves they really shut.
	if _, err := m.servers[0].client.session.CallTool(ctx, &sdk.CallToolParams{
		Name: "echo", Arguments: map[string]any{"text": "x"},
	}); err == nil {
		t.Error("CallTool after Close should fail (session closed)")
	}
	// Second Close is safe.
	_ = m.Close()
}
