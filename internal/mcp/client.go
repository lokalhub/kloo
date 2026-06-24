package mcp

import (
	"context"
	"fmt"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Client is one connected MCP server session: the live SDK session, a snapshot of
// the server's tool list taken at connect time, and the resolved per-call timeout.
// kloo's loop is single-threaded, so a Client is only ever touched from the loop
// goroutine during a tool Invoke; the tool list is captured once at connect and
// server-side tools/list_changed notifications are out of scope for v1.
type Client struct {
	Name    string
	session *sdk.ClientSession
	tools   []*sdk.Tool   // snapshot from ListTools at connect time (paginated)
	timeout time.Duration // per-call CallTool timeout
}

// Tools returns the server's snapshotted tool list (read-only; the bridge in
// Phase 02 maps these into tools.Tool values).
func (c *Client) Tools() []*sdk.Tool { return c.tools }

// dial connects one server and snapshots its full (paginated) tool list, bounded
// by connectTimeout on the connect+initialize handshake. A connect or list
// failure is returned as an error (never a panic) for the Manager to log and skip
// non-fatally (Phase 03). The returned Client owns the session until Close.
func dial(ctx context.Context, cfg ServerConfig, connectTimeout time.Duration) (*Client, error) {
	transport, err := cfg.transport()
	if err != nil {
		return nil, err
	}
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}

	client := sdk.NewClient(&sdk.Implementation{Name: clientImplName, Version: clientVersion}, nil)

	// Bound only the connect/initialize handshake; the parent ctx governs the
	// subsequent ListTools (a hung server can't stall startup past the timeout).
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	session, err := client.Connect(cctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect %q: %w", cfg.Name, err)
	}

	tools, err := listAllTools(ctx, session)
	if err != nil {
		_ = session.Close()
		return nil, fmt.Errorf("mcp: list tools %q: %w", cfg.Name, err)
	}

	return &Client{
		Name:    cfg.Name,
		session: session,
		tools:   tools,
		timeout: cfg.callTimeout(),
	}, nil
}

// listAllTools pages through ListTools following NextCursor until exhausted,
// returning the full accumulated tool list.
func listAllTools(ctx context.Context, session *sdk.ClientSession) ([]*sdk.Tool, error) {
	var all []*sdk.Tool
	params := &sdk.ListToolsParams{}
	for {
		res, err := session.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}
	return all, nil
}

// Close shuts the session down (terminating a stdio child process). Safe on a nil
// Client or an already-closed session.
func (c *Client) Close() error {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.Close()
}
