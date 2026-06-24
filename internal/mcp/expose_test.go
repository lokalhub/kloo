package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lokalhub/kloo/internal/tools"
)

// logCapture records formatted log lines for assertions.
type logCapture struct{ lines []string }

func (l *logCapture) logf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}
func (l *logCapture) joined() string { return strings.Join(l.lines, "\n") }

// manyToolServer builds an in-process server advertising n tools (tool00..).
func manyToolServer(name string, n int) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: name, Version: "t"}, nil)
	for i := 0; i < n; i++ {
		sdk.AddTool(srv, &sdk.Tool{Name: fmt.Sprintf("tool%02d", i), Description: "tool " + strconv.Itoa(i)}, echoHandler)
	}
	return srv
}

// mcpToolNames returns the registry's MCP tool names (those carrying the "__"
// namespacing) — used to assert which MCP tools were exposed.
func mcpToolNames(reg *tools.Registry) []string {
	var out []string
	for _, t := range reg.Tools() {
		if strings.Contains(t.Name(), "__") {
			out = append(out, t.Name())
		}
	}
	return out
}

func registerOne(t *testing.T, reg *tools.Registry, c *Client, cfg ServerConfig, maxExposed int) *logCapture {
	t.Helper()
	lc := &logCapture{}
	m := &Manager{servers: []*serverConn{{client: c, cfg: cfg}}, maxExposed: maxExposed, log: lc.logf}
	m.Register(reg)
	return lc
}

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name string
		cfg  ServerConfig
		want ExposeMode
	}{
		{"explicit curated", ServerConfig{ExposeMode: ExposeCurated}, ExposeCurated},
		{"explicit lazy", ServerConfig{ExposeMode: ExposeLazy}, ExposeLazy},
		{"explicit all", ServerConfig{ExposeMode: ExposeAll}, ExposeAll},
		{"unset + expose ⇒ curated", ServerConfig{Expose: []string{"x"}}, ExposeCurated},
		{"unset + no expose ⇒ lazy", ServerConfig{}, ExposeLazy},
		{"unknown ⇒ lazy", ServerConfig{ExposeMode: ExposeMode("weird")}, ExposeLazy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMode(tc.cfg); got != tc.want {
				t.Errorf("resolveMode = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExposeCurated(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerOne(t, reg, c, ServerConfig{ExposeMode: ExposeCurated, Expose: []string{"echo", "boom"}}, 16)

	if _, ok := reg.Lookup("srv__echo"); !ok {
		t.Error("srv__echo not registered")
	}
	if _, ok := reg.Lookup("srv__boom"); !ok {
		t.Error("srv__boom not registered")
	}
	if _, ok := reg.Lookup("srv__add"); ok {
		t.Error("srv__add should NOT be registered (not in allowlist)")
	}
	if _, ok := reg.Lookup("srv__list_tools"); ok {
		t.Error("no meta-trio expected for a within-cap curated server")
	}
}

func TestExposeCuratedMissingWarns(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	lc := registerOne(t, reg, c, ServerConfig{ExposeMode: ExposeCurated, Expose: []string{"echo", "ghost"}}, 16)

	if _, ok := reg.Lookup("srv__echo"); !ok {
		t.Error("srv__echo should be registered")
	}
	if _, ok := reg.Lookup("srv__ghost"); ok {
		t.Error("srv__ghost must not be registered (server doesn't advertise it)")
	}
	if !strings.Contains(lc.joined(), "ghost") {
		t.Errorf("missing tool not warned; log:\n%s", lc.joined())
	}
}

func TestExposeAll(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerOne(t, reg, c, ServerConfig{ExposeMode: ExposeAll}, 16)

	for _, n := range []string{"srv__echo", "srv__add", "srv__boom"} {
		if _, ok := reg.Lookup(n); !ok {
			t.Errorf("%s should be registered in all-mode", n)
		}
	}
	if _, ok := reg.Lookup("srv__list_tools"); ok {
		t.Error("all-mode within cap should not register the meta-trio")
	}
}

func TestExposeLazyMetaTrio(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerOne(t, reg, c, ServerConfig{ExposeMode: ExposeLazy}, 16)

	want := []string{"srv__list_tools", "srv__describe_tool", "srv__call_tool"}
	for _, n := range want {
		if _, ok := reg.Lookup(n); !ok {
			t.Errorf("lazy server should register %s", n)
		}
	}
	// And NOT the individual tools.
	for _, n := range []string{"srv__echo", "srv__add", "srv__boom"} {
		if _, ok := reg.Lookup(n); ok {
			t.Errorf("lazy server should NOT register first-class %s", n)
		}
	}
	if got := len(mcpToolNames(reg)); got != 3 {
		t.Errorf("lazy server registered %d MCP tools, want exactly 3 (constant window cost)", got)
	}
}

func TestExposeUnknownModeWarnsAndGoesLazy(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	lc := registerOne(t, reg, c, ServerConfig{ExposeMode: ExposeMode("weird")}, 16)

	if _, ok := reg.Lookup("srv__list_tools"); !ok {
		t.Error("unknown mode should fall back to lazy (meta-trio)")
	}
	if !strings.Contains(lc.joined(), "unknown exposeMode") {
		t.Errorf("unknown mode not warned; log:\n%s", lc.joined())
	}
}

func TestExposeCapDemotesToLazy(t *testing.T) {
	c := connectInMemory(t, "srv", manyToolServer("srv", 5))
	reg := tools.NewRegistry()
	lc := registerOne(t, reg, c, ServerConfig{ExposeMode: ExposeAll}, 2) // cap = 2

	// Exactly 2 first-class tools + the 3 meta-trio tools for the demoted remainder.
	firstClass := 0
	for _, n := range mcpToolNames(reg) {
		if !strings.HasSuffix(n, "list_tools") && !strings.HasSuffix(n, "describe_tool") && !strings.HasSuffix(n, "call_tool") {
			firstClass++
		}
	}
	if firstClass != 2 {
		t.Errorf("first-class MCP tools = %d, want 2 (the cap)", firstClass)
	}
	if _, ok := reg.Lookup("srv__list_tools"); !ok {
		t.Error("demoted remainder should register the lazy meta-trio")
	}
	if !strings.Contains(lc.joined(), "cap (2)") || !strings.Contains(lc.joined(), "demoted 3") {
		t.Errorf("demotion not logged with cap+count; log:\n%s", lc.joined())
	}
}

func TestCapCountsOnlyMCPToolsNotBuiltins(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	// Three pre-existing builtins in the registry; they must not consume the MCP cap.
	reg.Register(stubBuiltin{"read_file"})
	reg.Register(stubBuiltin{"list_dir"})
	reg.Register(stubBuiltin{"finish"})

	registerOne(t, reg, c, ServerConfig{ExposeMode: ExposeAll}, 3) // cap exactly fits the 3 MCP tools

	for _, n := range []string{"srv__echo", "srv__add", "srv__boom"} {
		if _, ok := reg.Lookup(n); !ok {
			t.Errorf("%s should register — builtins must not count against the MCP cap", n)
		}
	}
	// Builtins untouched, no demotion.
	if _, ok := reg.Lookup("srv__list_tools"); ok {
		t.Error("no demotion expected: the 3 MCP tools fit the cap of 3")
	}
	for _, n := range []string{"read_file", "list_dir", "finish"} {
		if _, ok := reg.Lookup(n); !ok {
			t.Errorf("builtin %s should be untouched", n)
		}
	}
}

// stubBuiltin is a minimal tools.Tool standing in for a builtin in cap tests.
type stubBuiltin struct{ name string }

func (s stubBuiltin) Name() string              { return s.name }
func (s stubBuiltin) Description() string       { return s.name }
func (s stubBuiltin) Schema() tools.ParamSchema { return tools.ParamSchema{} }
func (s stubBuiltin) Invoke(_ context.Context, _ tools.Call) (tools.Result, error) {
	return tools.Result{}, nil
}
