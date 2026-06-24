package mcp

import (
	"context"
	"fmt"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lokalhub/kloo/internal/tools"
)

// bridge.go is the single most important integration point: it wraps one
// discovered MCP tool as a tools.Tool so the existing registry, the three
// adapters, and the loop dispatch it with zero awareness that it is remote. The
// seam is the tools.Tool interface — exactly the abstraction the builtins already
// satisfy (see internal/tools/builtins.go); nothing in internal/tools or
// internal/agent is modified.

// mcpTool adapts ONE discovered MCP tool to tools.Tool. Unexported: only this
// package (the exposure policy) constructs and registers it. It stores both the
// kloo-facing namespaced name and the original remote name sent to CallTool.
type mcpTool struct {
	name   string // namespaced, kloo-facing (toolName)
	remote string // original MCP tool name (sent to CallTool)
	desc   string
	schema tools.ParamSchema
	client *Client
}

// compile-time proof that the bridge satisfies the tool vocabulary interface.
var _ tools.Tool = (*mcpTool)(nil)

func (t *mcpTool) Name() string              { return t.name }
func (t *mcpTool) Description() string       { return t.desc }
func (t *mcpTool) Schema() tools.ParamSchema { return t.schema }

// Invoke calls the remote tool with the model's args (forwarded verbatim),
// bounded by the per-call timeout, and maps the result via toResult. A
// transport/protocol error is wrapped; an IsError tool result becomes an error
// too (toResult), so both reach the loop's tool-error/self-correction channel.
func (t *mcpTool) Invoke(ctx context.Context, c tools.Call) (tools.Result, error) {
	return invokeRemote(ctx, t.client, t.remote, t.name, c.Args)
}

// invokeRemote is the shared CallTool path used by both mcpTool.Invoke and the
// lazy call_tool meta-tool (task 03): apply the per-call timeout, call the tool
// with the given args, and map the result. displayName labels wrapped errors.
func invokeRemote(ctx context.Context, c *Client, remote, displayName string, args map[string]any) (tools.Result, error) {
	cctx, cancel := callContext(ctx, c.timeout)
	defer cancel()
	res, err := c.session.CallTool(cctx, &sdk.CallToolParams{Name: remote, Arguments: args})
	if err != nil {
		return tools.Result{}, fmt.Errorf("mcp %s: %w", displayName, err)
	}
	return toResult(res)
}

// newMcpTool builds one bridge from a client and a discovered *sdk.Tool: the
// kloo-facing name is toolName(server, remote), the description and schema come
// from the MCP tool.
func newMcpTool(c *Client, mt *sdk.Tool) *mcpTool {
	return &mcpTool{
		name:   toolName(c.Name, mt.Name),
		remote: mt.Name,
		desc:   mt.Description,
		schema: toParamSchema(mt.InputSchema),
		client: c,
	}
}

// toolByName returns the client's snapshotted *sdk.Tool with the given remote
// name (ok=false when the server doesn't advertise it).
func toolByName(c *Client, remote string) (*sdk.Tool, bool) {
	for _, mt := range c.tools {
		if mt.Name == remote {
			return mt, true
		}
	}
	return nil, false
}

// registerTools builds an mcpTool for each remote name the client actually
// advertises and registers it into reg, returning the kloo-facing names it
// registered and the remote names that were missing (not advertised). The
// exposure policy (task 02) decides WHICH remotes to pass (and what to do with
// the missing/over-cap remainder); this helper only constructs + registers.
func registerTools(reg *tools.Registry, c *Client, remotes []string) (registered, missing []string) {
	for _, remote := range remotes {
		mt, ok := toolByName(c, remote)
		if !ok {
			missing = append(missing, remote)
			continue
		}
		tool := newMcpTool(c, mt)
		reg.Register(tool)
		registered = append(registered, tool.name)
	}
	return registered, missing
}
