package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// httpMCPServerURL mounts an in-process MCP server behind a loopback httptest
// HTTP endpoint (so Manager.Connect, which dials via ServerConfig.URL, can reach
// it hermetically — no node, no external network) and returns its URL.
func httpMCPServerURL(t *testing.T, srv *sdk.Server) string {
	t.Helper()
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

// connectInMemory wires a *Client to an in-process server over the in-memory
// transport (no node, no network) and registers cleanup. The Client's Name (used
// for tool namespacing) is `name`.
func connectInMemory(t *testing.T, name string, srv *sdk.Server) *Client {
	t.Helper()
	ctx := context.Background()
	clientT, serverT := sdk.NewInMemoryTransports()

	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	cl := sdk.NewClient(&sdk.Implementation{Name: clientImplName, Version: clientVersion}, nil)
	session, err := cl.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	tools, err := listAllTools(ctx, session)
	if err != nil {
		t.Fatalf("listAllTools: %v", err)
	}
	c := &Client{Name: name, session: session, tools: tools, timeout: DefaultCallTimeout}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// boomIn is the input for the error tool.
type boomIn struct {
	Why string `json:"why" jsonschema:"why it failed"`
}

// boomHandler returns an IsError tool result carrying its text (exercising the
// IsError→error mapping path).
func boomHandler(_ context.Context, _ *sdk.CallToolRequest, in boomIn) (*sdk.CallToolResult, any, error) {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: "boom: " + in.Why}},
	}, nil, nil
}

// newEchoAddBoomServer is an in-process server with echo, add, and a failing
// `boom` tool — enough to cover success, args round-trip, and IsError paths.
func newEchoAddBoomServer(name string) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: name, Version: "test"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "echo", Description: "echo text\nsecond line ignored in summary"}, echoHandler)
	sdk.AddTool(srv, &sdk.Tool{Name: "add", Description: "add a and b"}, addHandler)
	sdk.AddTool(srv, &sdk.Tool{Name: "boom", Description: "always fails"}, boomHandler)
	return srv
}
