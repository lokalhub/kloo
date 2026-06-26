package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/mcp"
	"github.com/lokalhub/kloo/internal/tools"
)

// wireMCP builds the tool registry and — unless MCP is globally disabled — connects
// every configured MCP server (non-fatally) and registers their tools alongside the
// builtins. It returns the registry the loop will use and a Close function to defer.
// Both launch paths (defaultLaunchTUI and defaultRunHeadless) call this, so their
// MCP wiring is identical by construction (overview §6).
//
// When cfg.MCPDisabled is set (--no-mcp / KLOO_MCP=0 / no servers configured), this
// is a no-op beyond DefaultRegistry: no connection is attempted and no lines are
// logged, so an MCP-off run is byte-identical to pre-MCP kloo (mock §3).
//
// logf receives the startup connection / trust lines (mock §1); callers point it at
// stderr (TUI) or the headless out writer. internal/cli stays SDK-free: the only
// MCP types it touches are mcp.Manager and mcp.ConfigFromEntries (the converter that
// keeps internal/config from importing the SDK).
func wireMCP(ctx context.Context, cfg config.Config, ws tools.Workspace, logf func(string, ...any)) (*tools.Registry, func() error) {
	reg := tools.DefaultRegistry(ws, tools.WithAllowedEnv(cfg.AllowedEnv))
	if cfg.MCPDisabled {
		return reg, func() error { return nil }
	}
	mgr := mcp.Connect(ctx, mcp.ConfigFromEntries(cfg.MCPServers, cfg.MCPMaxExposedTools), logf)
	mgr.Register(reg)
	return reg, mgr.Close
}

// writerLogf adapts an io.Writer into the printf-style logger wireMCP/Manager use,
// appending a newline per line (the headless path; the TUI uses a stderr variant).
func writerLogf(w io.Writer) func(string, ...any) {
	return func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...)
	}
}
