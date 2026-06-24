package mcp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lokalhub/kloo/internal/tools"
)

type blockIn struct{}

// blockHandler blocks until its call context is cancelled — used to prove the
// per-call timeout aborts a hung tool.
func blockHandler(ctx context.Context, _ *sdk.CallToolRequest, _ blockIn) (*sdk.CallToolResult, any, error) {
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

// TestMcpToolPerCallTimeout: Invoke bounds a hung tool by the client's per-call
// timeout and returns an error rather than blocking forever.
func TestMcpToolPerCallTimeout(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "srv", Version: "t"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "block", Description: "blocks forever"}, blockHandler)
	c := connectInMemory(t, "srv", srv)
	c.timeout = 80 * time.Millisecond // tighten the per-call timeout for the test

	mt, _ := toolByName(c, "block")
	tool := newMcpTool(c, mt)

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := tool.Invoke(context.Background(), tools.Call{Args: map[string]any{}})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("want a timeout error from a hung tool, got nil")
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("Invoke took %v; per-call timeout (80ms) was not applied", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Invoke hung: per-call timeout not applied")
	}
}

// TestMcpToolDispatch: an echo MCP tool registered into a tools.Registry
// dispatches end-to-end through Registry.Dispatch against an in-memory server,
// returning the echoed text.
func TestMcpToolDispatch(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))

	reg := tools.NewRegistry()
	registered, missing := registerTools(reg, c, []string{"echo"})
	if len(missing) != 0 || len(registered) != 1 || registered[0] != "srv__echo" {
		t.Fatalf("registerTools = registered %v missing %v", registered, missing)
	}

	res, err := reg.Dispatch(ctx, tools.Call{Name: "srv__echo", Args: map[string]any{"text": "hello mcp"}})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Output != "hello mcp" {
		t.Errorf("Output = %q, want %q", res.Output, "hello mcp")
	}
}

// TestMcpToolForwardsArgsVerbatim: Invoke forwards the model's args unchanged to
// CallTool (the server echoes the text back).
func TestMcpToolForwardsArgsVerbatim(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	mt, ok := toolByName(c, "echo")
	if !ok {
		t.Fatal("echo not advertised")
	}
	tool := newMcpTool(c, mt)

	const want = "verbatim ✓ payload"
	res, err := tool.Invoke(ctx, tools.Call{Args: map[string]any{"text": want}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Output != want {
		t.Errorf("Output = %q, want %q", res.Output, want)
	}
}

// TestMcpToolIsErrorSurfacesError: a server tool returning IsError ⇒ Dispatch
// surfaces an error (via toResult), carrying the text.
func TestMcpToolIsErrorSurfacesError(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registerTools(reg, c, []string{"boom"})

	_, err := reg.Dispatch(ctx, tools.Call{Name: "srv__boom", Args: map[string]any{"why": "kaboom"}})
	if err == nil {
		t.Fatal("want an error from an IsError result, got nil")
	}
	if !strings.Contains(err.Error(), "boom: kaboom") {
		t.Errorf("error = %q, want it to carry the tool text 'boom: kaboom'", err.Error())
	}
}

// TestMcpToolSchemaAndName: Schema() equals toParamSchema(InputSchema); Name() is
// the namespaced name; Description() is the MCP description.
func TestMcpToolSchemaAndName(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	mt, _ := toolByName(c, "echo")
	tool := newMcpTool(c, mt)

	if tool.Name() != "srv__echo" {
		t.Errorf("Name() = %q, want srv__echo", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() should carry the MCP description")
	}
	if !reflect.DeepEqual(tool.Schema(), toParamSchema(mt.InputSchema)) {
		t.Errorf("Schema() = %+v, want toParamSchema(InputSchema) %+v", tool.Schema(), toParamSchema(mt.InputSchema))
	}
}

// TestMcpToolRequiredArgGuard: a required-arg schema + a Call missing it ⇒
// Registry.Dispatch returns ErrInvalidArgs BEFORE Invoke (no CallTool made).
func TestMcpToolRequiredArgGuard(t *testing.T) {
	ctx := context.Background()
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	mt, _ := toolByName(c, "echo")
	tool := newMcpTool(c, mt)
	if len(tool.Schema().Required) == 0 {
		t.Skip("echo schema has no required field on this SDK; guard test N/A")
	}

	reg := tools.NewRegistry()
	reg.Register(tool)
	_, err := reg.Dispatch(ctx, tools.Call{Name: "srv__echo", Args: map[string]any{}}) // missing "text"
	if !errors.Is(err, tools.ErrInvalidArgs) {
		t.Errorf("missing required arg: err = %v, want ErrInvalidArgs", err)
	}
}

// TestRegisterToolsMissing: an unadvertised remote name is reported as missing,
// not registered, and never panics.
func TestRegisterToolsMissing(t *testing.T) {
	c := connectInMemory(t, "srv", newEchoAddBoomServer("srv"))
	reg := tools.NewRegistry()
	registered, missing := registerTools(reg, c, []string{"echo", "nope"})
	if len(registered) != 1 || registered[0] != "srv__echo" {
		t.Errorf("registered = %v, want [srv__echo]", registered)
	}
	if len(missing) != 1 || missing[0] != "nope" {
		t.Errorf("missing = %v, want [nope]", missing)
	}
}
