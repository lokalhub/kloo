package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lokalhub/kloo/internal/tools"
)

// (The package timeout/cap constants DefaultCallTimeout, DefaultConnectTimeout,
// and DefaultMaxExposedTools are declared in config.go alongside the config types
// that first use them, so they are defined exactly once for the package.)

// toResult flattens an MCP CallToolResult into kloo's tools.Result. Every
// *TextContent.Text is concatenated (one block per line); any non-text content
// (image/audio/resource) is rendered as a short "[non-text content: <kind>]"
// placeholder so the model is informed without flooding the window. When
// res.IsError is set, the joined text is returned as an error (Result empty), so
// the loop surfaces it through the same tool-error/self-correction channel as a
// builtin tool error (internal/agent/loop.go observation()). A nil result or
// empty content yields an empty Output with no error (no panic).
func toResult(res *sdk.CallToolResult) (tools.Result, error) {
	if res == nil {
		return tools.Result{}, nil
	}

	var parts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *sdk.TextContent:
			parts = append(parts, v.Text)
		case *sdk.ImageContent:
			parts = append(parts, "[non-text content: image]")
		case *sdk.AudioContent:
			parts = append(parts, "[non-text content: audio]")
		case *sdk.ResourceLink, *sdk.EmbeddedResource:
			parts = append(parts, "[non-text content: resource]")
		default:
			parts = append(parts, "[non-text content: unknown]")
		}
	}
	text := strings.Join(parts, "\n")

	if res.IsError {
		return tools.Result{}, fmt.Errorf("%s", text)
	}
	return tools.Result{Output: text}, nil
}

// callContext derives the per-call context for a CallTool invocation, applying the
// server's timeout (DefaultCallTimeout when timeout ≤ 0). The caller must call the
// returned CancelFunc. Phase 02's mcpTool.Invoke uses this to bound every MCP tool
// call so a slow/hung server can't stall the single-threaded loop.
func callContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	return context.WithTimeout(ctx, timeout)
}
