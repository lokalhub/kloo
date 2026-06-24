package mcp

// Phase-00 task-02 connectivity proof (gate-of-record).
//
// This proves the REAL exec + stdio MCP path end-to-end BEFORE any bridge/registry
// code exists: a kloo process connects to a real external stdio MCP server over
// mcp.CommandTransport (real exec.Command, real newline-delimited JSON over stdin/
// stdout — NOT the in-memory transport), lists its tools, calls one, and asserts a
// text result comes back.
//
// Hermetic: the "external server" is this very test binary re-executed in
// "be a server" mode (the os/exec TestHelperProcess pattern). No node, no network —
// so it runs under `go test ./internal/mcp -run Connectivity` and reproduces anywhere.
//
// The dated transcript captured from `go test -v -run Connectivity` lives in
// docs/plans/mcp-client/phases/00-go-bump-sdk-connectivity/tasks/02-connectivity-proof/
// artifacts/connectivity-proof.txt.

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// helperEnv, when set in the child process, makes TestHelperMCPServer run a real
// stdio MCP server instead of being a no-op test.
const helperEnv = "KLOO_MCP_CONNECTIVITY_HELPER"

// ── server side (the "external" MCP server, run in the re-exec'd child) ──────────

type echoIn struct {
	Text string `json:"text" jsonschema:"the text to echo back"`
}

type addIn struct {
	A int `json:"a" jsonschema:"first addend"`
	B int `json:"b" jsonschema:"second addend"`
}

func echoHandler(_ context.Context, _ *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: in.Text}},
	}, nil, nil
}

func addHandler(_ context.Context, _ *sdk.CallToolRequest, in addIn) (*sdk.CallToolResult, any, error) {
	sum := in.A + in.B
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: "sum=" + itoa(sum)}},
	}, nil, nil
}

// itoa avoids pulling strconv into a tiny helper; the values are small.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestHelperMCPServer is NOT a real test: in the normal suite (env unset) it returns
// immediately. When the parent re-execs this binary with helperEnv=1, it serves a
// real stdio MCP server (echo + add) over the process's stdin/stdout and exits when
// the client disconnects — so the test framework never prints to stdout and corrupts
// the JSON-RPC stream.
func TestHelperMCPServer(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return // ordinary run: no-op
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "kloo-proof-server", Version: "test"}, nil)
	sdk.AddTool(server, &sdk.Tool{Name: "echo", Description: "echo back the provided text"}, echoHandler)
	sdk.AddTool(server, &sdk.Tool{Name: "add", Description: "add two integers a and b"}, addHandler)

	if err := server.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		// stderr is captured by the parent for debugging; never write to stdout.
		os.Stderr.WriteString("helper server error: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(0) // clean exit before the test framework writes its summary to stdout
}

// ── client side (the proof) ─────────────────────────────────────────────────────

func TestConnectivityStdioProof(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Launch the "external" server as a real OS process over real stdio.
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperMCPServer$", "-test.timeout=25s")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	cmd.Stderr = os.Stderr // surface helper failures in the test log

	client := sdk.NewClient(&sdk.Implementation{Name: "kloo", Version: "test"}, nil)
	session, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("Connect over CommandTransport (real stdio) failed: %v", err)
	}
	defer session.Close()

	// ListTools must return ≥ 1 tool.
	lt, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(lt.Tools) < 1 {
		t.Fatalf("ListTools returned %d tools, want ≥ 1", len(lt.Tools))
	}
	names := make([]string, 0, len(lt.Tools))
	for _, tool := range lt.Tools {
		names = append(names, tool.Name)
	}
	t.Logf("ListTools returned %d tool(s): %v", len(lt.Tools), names)

	if !contains(names, "echo") {
		t.Fatalf("expected an 'echo' tool in %v", names)
	}

	// CallTool 'echo' → assert a *mcp.TextContent comes back with the echoed text.
	const want = "kloo↔mcp stdio proof"
	res, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": want},
	})
	if err != nil {
		t.Fatalf("CallTool(echo) failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("CallTool(echo) returned IsError=true: %+v", res.Content)
	}
	if len(res.Content) == 0 {
		t.Fatal("CallTool(echo) returned no content")
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("CallTool(echo) content[0] is %T, want *mcp.TextContent", res.Content[0])
	}
	if tc.Text != want {
		t.Fatalf("CallTool(echo) text = %q, want %q", tc.Text, want)
	}
	t.Logf("CallTool(echo) returned TextContent: %q", tc.Text)

	// Second tool (add) for good measure — proves typed args round-trip.
	addRes, err := session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "add",
		Arguments: map[string]any{"a": 2, "b": 3},
	})
	if err != nil {
		t.Fatalf("CallTool(add) failed: %v", err)
	}
	addTC, ok := addRes.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("CallTool(add) content[0] is %T, want *mcp.TextContent", addRes.Content[0])
	}
	if addTC.Text != "sum=5" {
		t.Fatalf("CallTool(add) text = %q, want %q", addTC.Text, "sum=5")
	}
	t.Logf("CallTool(add) returned TextContent: %q", addTC.Text)

	// Close() must terminate the child process cleanly (deferred above; assert no panic).
	if err := session.Close(); err != nil {
		t.Fatalf("session.Close() failed: %v", err)
	}
	t.Log("session.Close() returned; child process terminated")
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
