// Package mcp is kloo's MCP (Model Context Protocol) client. It is the ONLY
// package that imports the go-sdk (github.com/modelcontextprotocol/go-sdk); the
// rest of kloo (internal/tools, internal/agent, the adapters) stays SDK-free and
// treats a discovered MCP tool as an ordinary tools.Tool.
//
// This file owns the connection-shaped config: the transport-agnostic
// ServerConfig/Config value types, the per-server transport() constructor
// (stdio vs HTTP), and the package-level timeout/cap constants. internal/config
// decodes the profile JSON into config.MCPServerEntry values; ConfigFromEntries
// adapts those into this package's Config so internal/config never depends on the
// SDK.
//
// # Concurrency model (v1)
//
// kloo's agent loop is single-threaded: exactly one tool call per turn, dispatched
// sequentially through tools.Registry.Dispatch (internal/agent/loop.go). An mcpTool
// — and the *sdk.ClientSession it wraps — is therefore only ever touched from the
// loop goroutine during Invoke. Tool discovery happens once at connect (dial →
// ListTools), and each Client's tool list is a snapshot taken then; nothing mutates
// shared state across goroutines after Connect returns. Server-initiated
// tools/list_changed notifications are intentionally NOT handled in v1 (the snapshot
// is authoritative until restart). This property is guarded by race_test.go, run
// under the race detector during Phase-03 validation.
package mcp

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lokalhub/kloo/internal/config"
)

// Package defaults (master plan §6). These are vars (not consts) only so a test
// or future CLI flag could tighten them; treat them as constants in normal use.
var (
	// DefaultCallTimeout bounds a single CallTool invocation.
	DefaultCallTimeout = 30 * time.Second
	// DefaultConnectTimeout bounds dialing + the initial ListTools at startup.
	DefaultConnectTimeout = 10 * time.Second
	// DefaultMaxExposedTools caps total first-class MCP tools across all servers
	// (Phase 02 enforces the cap; mirrors config.DefaultMCPMaxExposedTools).
	DefaultMaxExposedTools = 16
)

// ErrBadServerConfig is returned when a server declares neither or both of
// command/url. The Manager (Phase 03) logs it and skips that server (non-fatal).
var ErrBadServerConfig = errors.New("mcp: server needs exactly one of command/url")

// clientImplName / clientVersion identify kloo to servers in the MCP handshake.
// clientVersion is a var so the CLI layer (Phase 03) can stamp the real build
// version without internal/mcp importing internal/cli (which would cycle).
const clientImplName = "kloo"

var clientVersion = "dev"

// ExposeMode selects how a server's tools enter kloo's registry (Phase 02 acts on
// it; Phase 01 only carries it). "" ⇒ curated when Expose is non-empty, else lazy.
type ExposeMode string

const (
	ExposeCurated ExposeMode = "curated"
	ExposeLazy    ExposeMode = "lazy"
	ExposeAll     ExposeMode = "all"
)

// ServerConfig is one MCP server's connection config, already decoded and
// path/env-expanded by internal/config. Exactly one of Command (stdio) or URL
// (HTTP) must be set.
type ServerConfig struct {
	Name       string            // map key (used for namespacing + logs)
	Command    string            // stdio: executable
	Args       []string          // stdio: args
	Env        map[string]string // stdio: extra env, merged over os.Environ()
	URL        string            // HTTP: endpoint (mutually exclusive with Command)
	Headers    map[string]string // HTTP: static headers, injected only for URL origin
	ExposeMode ExposeMode        // "" ⇒ curated if Expose non-empty else lazy
	Expose     []string          // curated allowlist (original MCP tool names)
	TimeoutSec int               // per-call timeout; 0 ⇒ DefaultCallTimeout
	Disabled   bool              // per-server kill-switch
}

// Config is the whole MCP config for one run.
type Config struct {
	Servers         []ServerConfig
	MaxExposedTools int // 0 ⇒ DefaultMaxExposedTools
}

// ConfigFromEntries adapts the profile-decoded config.MCPServerEntry map into this
// package's Config. The conversion lives here (not in internal/config) so the SDK
// dependency stays confined to internal/mcp; the cli wiring (Phase 03) calls this
// with cfg.MCPServers / cfg.MCPMaxExposedTools. Disabled servers are kept in the
// slice (the Manager skips them) so logs can report them. Server order is sorted
// by name for deterministic exposure/cap behaviour.
func ConfigFromEntries(entries map[string]config.MCPServerEntry, maxExposedTools int) Config {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	servers := make([]ServerConfig, 0, len(entries))
	for _, name := range names {
		e := entries[name]
		servers = append(servers, ServerConfig{
			Name:       name,
			Command:    e.Command,
			Args:       e.Args,
			Env:        e.Env,
			URL:        e.URL,
			Headers:    e.Headers,
			ExposeMode: ExposeMode(e.ExposeMode),
			Expose:     e.Expose,
			TimeoutSec: e.TimeoutSeconds,
			Disabled:   e.Disabled,
		})
	}
	return Config{Servers: servers, MaxExposedTools: maxExposedTools}
}

// transport builds the SDK transport for this server: a stdio CommandTransport
// (shell-less exec.Command, env merged over the process environment) when Command
// is set, or an HTTP StreamableClientTransport when URL is set. Neither or both ⇒
// ErrBadServerConfig.
func (s ServerConfig) transport() (sdk.Transport, error) {
	hasCmd, hasURL := s.Command != "", s.URL != ""
	switch {
	case hasCmd && !hasURL && len(s.Headers) == 0:
		cmd := exec.Command(s.Command, s.Args...)
		cmd.Env = append(os.Environ(), envPairs(s.Env)...)
		return &sdk.CommandTransport{Command: cmd}, nil
	case hasURL && !hasCmd:
		httpClient := http.DefaultClient
		if len(s.Headers) > 0 {
			endpoint, err := url.Parse(s.URL)
			if err != nil {
				return nil, fmt.Errorf("%w (server %q: parse url)", ErrBadServerConfig, s.Name)
			}
			httpClient = &http.Client{Transport: headerRoundTripper{
				base:           http.DefaultTransport,
				endpointScheme: endpoint.Scheme,
				endpointHost:   endpoint.Host,
				headers:        s.Headers,
			}}
		}
		return &sdk.StreamableClientTransport{Endpoint: s.URL, HTTPClient: httpClient}, nil
	default:
		reason := "needs exactly one of command/url"
		if len(s.Headers) > 0 && hasCmd && !hasURL {
			reason = "headers require url transport"
		}
		return nil, fmt.Errorf("%w (server %q: %s)", ErrBadServerConfig, s.Name, reason)
	}
}

type headerRoundTripper struct {
	base           http.RoundTripper
	endpointScheme string
	endpointHost   string
	headers        map[string]string
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	base := h.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.URL == nil || req.URL.Scheme != h.endpointScheme || req.URL.Host != h.endpointHost || len(h.headers) == 0 {
		return base.RoundTrip(req)
	}

	clone := req.Clone(req.Context())
	for name, value := range h.headers {
		clone.Header.Set(name, value)
	}
	return base.RoundTrip(clone)
}

// envPairs renders an env map as sorted "KEY=VALUE" strings (deterministic order
// so a transport built twice from the same config is identical — aids testing).
func envPairs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(env))
	for _, k := range keys {
		pairs = append(pairs, k+"="+env[k])
	}
	return pairs
}

// callTimeout resolves the per-call timeout for a server config.
func (s ServerConfig) callTimeout() time.Duration {
	if s.TimeoutSec > 0 {
		return time.Duration(s.TimeoutSec) * time.Second
	}
	return DefaultCallTimeout
}
