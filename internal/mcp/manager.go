package mcp

import (
	"context"
	"errors"
	"fmt"

	"github.com/lokalhub/kloo/internal/tools"
)

// Manager owns every connected MCP server for a run: it dials every enabled
// server non-fatally at startup (Connect), registers the survivors' tools into
// kloo's tools.Registry per the exposure policy (Register / expose.go), and shuts
// every session down on exit (Close). The cli wiring is just
// Connect → Register → defer Close.
type Manager struct {
	servers    []*serverConn
	maxExposed int // 0 ⇒ DefaultMaxExposedTools
	log        func(format string, args ...any)
}

// serverConn pairs a connected client with the config it was dialed from (the
// config carries the exposure mode / allowlist the policy needs).
type serverConn struct {
	client *Client
	cfg    ServerConfig
}

// logf logs via the injected logger, or no-ops when none is set.
func (m *Manager) logf(format string, args ...any) {
	if m.log != nil {
		m.log(format, args...)
	}
}

// capValue resolves the effective global cap (default when unset).
func (m *Manager) capValue() int {
	if m.maxExposed > 0 {
		return m.maxExposed
	}
	return DefaultMaxExposedTools
}

// Connect dials every ENABLED server in cfg, best-effort: a server that fails to
// connect/list (or is misconfigured) is logged via log and SKIPPED — the returned
// Manager holds only the servers that connected, and the run continues (with the
// builtins, always). A nil/empty cfg yields an empty Manager (no log noise).
// Connect never returns an error for a server problem; a bad server must never
// break a coding run (requirement 5 / overview §7).
//
// After connecting, a one-time trust note is logged (only when ≥1 server
// connected) so the out-of-jail execution boundary is surfaced, not hidden.
func Connect(ctx context.Context, cfg Config, log func(string, ...any)) *Manager {
	m := &Manager{maxExposed: cfg.MaxExposedTools, log: log}
	for _, sc := range cfg.Servers {
		if sc.Disabled {
			m.logf("kloo: mcp · skipped %q — disabled in profile", sc.Name)
			continue
		}
		client, err := dial(ctx, sc, DefaultConnectTimeout)
		if err != nil {
			m.logf("kloo: mcp · skipped %q — connect failed: %v (run continues)", sc.Name, err)
			continue
		}
		m.servers = append(m.servers, &serverConn{client: client, cfg: sc})
		m.logf("kloo: mcp · connected %q (%s) — %d tools", sc.Name, transportKind(sc), len(client.tools))
	}
	if len(m.servers) > 0 {
		m.logf("kloo: mcp · NOTE: MCP tools run inside their server process, OUTSIDE kloo's workspace sandbox.")
	}
	return m
}

// transportKind labels a server's transport for the connect log.
func transportKind(sc ServerConfig) string {
	if sc.Command != "" {
		return "stdio"
	}
	return "http"
}

// Register wraps each exposed tool (per the §5 policy + the global cap) as a
// tools.Tool and registers it into reg, alongside the builtins already there.
// Lazy servers register their meta-trio instead. The cap (`remaining`) is global
// across servers and counts ONLY MCP tools — builtins already in reg are never
// counted or affected.
func (m *Manager) Register(reg *tools.Registry) {
	remaining := m.capValue()
	for _, s := range m.servers {
		if s.client == nil {
			continue
		}
		m.exposeServer(reg, s.client, s.cfg, &remaining)
	}
}

func (m *Manager) Call(ctx context.Context, server, tool string, args map[string]any) (tools.Result, error) {
	if m == nil {
		return tools.Result{}, fmt.Errorf("mcp: memory server %q is not connected", server)
	}
	for _, s := range m.servers {
		if s.client != nil && s.client.Name == server {
			return s.client.Call(ctx, tool, args)
		}
	}
	return tools.Result{}, fmt.Errorf("mcp: memory server %q is not connected", server)
}

// Close shuts every connected session down, best-effort: one client's Close error
// does not skip the rest. Called via defer on loop/TUI exit; it terminates stdio
// child processes (the SDK's CommandTransport SIGTERMs after a grace window).
func (m *Manager) Close() error {
	var errs []error
	for _, s := range m.servers {
		if s.client == nil {
			continue
		}
		if err := s.client.Close(); err != nil {
			errs = append(errs, fmt.Errorf("mcp: close %q: %w", s.client.Name, err))
		}
	}
	return errors.Join(errs...)
}
