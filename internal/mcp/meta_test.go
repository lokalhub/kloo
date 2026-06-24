package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/tools"
)

func TestMetaTrioNamespacedAndRegistered(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	names := registerMetaTrio(reg, c)

	want := []string{"srv__list_tools", "srv__describe_tool", "srv__call_tool"}
	if len(names) != 3 {
		t.Fatalf("registerMetaTrio returned %v, want 3", names)
	}
	for _, n := range want {
		tl, ok := reg.Lookup(n)
		if !ok {
			t.Errorf("%s not registered", n)
			continue
		}
		var _ tools.Tool = tl // each is a tools.Tool
	}
}

func TestMetaListTools(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerMetaTrio(reg, c)

	res, err := reg.Dispatch(ctx, tools.Call{Name: "srv__list_tools"})
	if err != nil {
		t.Fatalf("list_tools: %v", err)
	}
	for _, n := range []string{"echo", "add", "boom"} {
		if !strings.Contains(res.Output, n) {
			t.Errorf("list_tools output missing %q:\n%s", n, res.Output)
		}
	}
	// One-line summary only: echo's description is two lines; the second must not leak.
	if strings.Contains(res.Output, "second line ignored") {
		t.Errorf("list_tools leaked a non-first description line:\n%s", res.Output)
	}
}

func TestMetaListToolsCapAndCursor(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", manyToolServer("srv", 60))
	reg := tools.NewRegistry()
	registerMetaTrio(reg, c)

	res, err := reg.Dispatch(ctx, tools.Call{Name: "srv__list_tools"})
	if err != nil {
		t.Fatalf("list_tools: %v", err)
	}
	if len(res.Output) > metaListMaxChars+200 { // cap + the "more" footer
		t.Errorf("list_tools output %d chars exceeds the documented cap", len(res.Output))
	}
	if !strings.Contains(res.Output, "more; call") || !strings.Contains(res.Output, "cursor=") {
		t.Errorf("a capped list must advertise the cursor for the rest:\n%s", res.Output)
	}

	// Follow the cursor to page the remainder.
	cur := extractCursor(t, res.Output)
	res2, err := reg.Dispatch(ctx, tools.Call{Name: "srv__list_tools", Args: map[string]any{"cursor": cur}})
	if err != nil {
		t.Fatalf("list_tools page 2: %v", err)
	}
	if strings.TrimSpace(res2.Output) == "" {
		t.Error("paged list_tools returned nothing")
	}
}

func extractCursor(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "cursor=")
	if i < 0 {
		t.Fatalf("no cursor in: %s", out)
	}
	rest := out[i+len("cursor="):]
	rest = strings.Trim(rest, "\"")
	// cursor is followed by a newline; take up to it.
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[:nl]
	}
	return strings.Trim(rest, "\"")
}

func TestMetaDescribeTool(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerMetaTrio(reg, c)

	res, err := reg.Dispatch(ctx, tools.Call{Name: "srv__describe_tool", Args: map[string]any{"name": "echo"}})
	if err != nil {
		t.Fatalf("describe_tool: %v", err)
	}
	if !strings.Contains(res.Output, "input schema") || !strings.Contains(res.Output, "text") {
		t.Errorf("describe_tool should show echo's schema (with the 'text' property):\n%s", res.Output)
	}

	// Unknown name → helpful message (not a panic, not an error here).
	res, err = reg.Dispatch(ctx, tools.Call{Name: "srv__describe_tool", Args: map[string]any{"name": "ghost"}})
	if err != nil {
		t.Fatalf("describe_tool unknown: %v", err)
	}
	if !strings.Contains(res.Output, "no such tool") || !strings.Contains(res.Output, "list_tools") {
		t.Errorf("unknown describe_tool should point at list_tools:\n%s", res.Output)
	}
}

func TestMetaCallToolObjectArgs(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerMetaTrio(reg, c)

	res, err := reg.Dispatch(ctx, tools.Call{Name: "srv__call_tool", Args: map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"text": "via-meta"},
	}})
	if err != nil {
		t.Fatalf("call_tool object args: %v", err)
	}
	if res.Output != "via-meta" {
		t.Errorf("call_tool output = %q, want via-meta", res.Output)
	}
}

func TestMetaCallToolJSONStringArgs(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerMetaTrio(reg, c)

	res, err := reg.Dispatch(ctx, tools.Call{Name: "srv__call_tool", Args: map[string]any{
		"name":      "echo",
		"arguments": `{"text":"json-string"}`,
	}})
	if err != nil {
		t.Fatalf("call_tool json-string args: %v", err)
	}
	if res.Output != "json-string" {
		t.Errorf("call_tool (json string) output = %q, want json-string", res.Output)
	}
}

func TestMetaCallToolUnknownAndError(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerMetaTrio(reg, c)

	// Unknown tool name → helpful tool error.
	_, err := reg.Dispatch(ctx, tools.Call{Name: "srv__call_tool", Args: map[string]any{"name": "ghost"}})
	if err == nil || !strings.Contains(err.Error(), "no such tool") {
		t.Errorf("unknown call_tool should error helpfully, got %v", err)
	}

	// An underlying IsError result surfaces as an error.
	_, err = reg.Dispatch(ctx, tools.Call{Name: "srv__call_tool", Args: map[string]any{
		"name":      "boom",
		"arguments": map[string]any{"why": "nope"},
	}})
	if err == nil || !strings.Contains(err.Error(), "boom: nope") {
		t.Errorf("call_tool on an IsError tool should surface the error, got %v", err)
	}
}
