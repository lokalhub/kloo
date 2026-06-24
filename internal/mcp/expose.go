package mcp

import (
	"strings"

	"github.com/lokalhub/kloo/internal/tools"
)

// expose.go implements the curated/lazy/all exposure policy — the small-model
// centerpiece (master plan §5) — and the global maxExposedTools backstop. For
// each connected server it decides which tools become first-class mcpTools vs the
// lazy meta-trio, with NO silent truncation: every skip/demotion is logged.

// allModeWarnThreshold: exposing more than this many tools first-class via "all"
// logs a nudge toward curated/lazy (a small model's tool-selection degrades with
// many schemas in-window).
const allModeWarnThreshold = 8

// resolveMode applies the §5 default: an explicit, known ExposeMode wins;
// otherwise curated when an allowlist is present, else lazy. An unknown value
// falls back to lazy (exposeServer logs the warning — this stays pure).
func resolveMode(cfg ServerConfig) ExposeMode {
	switch cfg.ExposeMode {
	case ExposeCurated, ExposeLazy, ExposeAll:
		return cfg.ExposeMode
	case "":
		if len(cfg.Expose) > 0 {
			return ExposeCurated
		}
		return ExposeLazy
	default:
		return ExposeLazy
	}
}

func isKnownMode(m ExposeMode) bool {
	switch m {
	case ExposeCurated, ExposeLazy, ExposeAll:
		return true
	default:
		return false
	}
}

// exposeServer registers one server's tools per the resolved mode, honouring the
// global cap (`remaining`, shared across servers). Lazy ⇒ the 3-tool meta-trio
// (does NOT consume the cap). Curated/all ⇒ first-class mcpTools up to the cap,
// with the overflow demoted to lazy and logged.
func (m *Manager) exposeServer(reg *tools.Registry, c *Client, cfg ServerConfig, remaining *int) {
	if cfg.ExposeMode != "" && !isKnownMode(cfg.ExposeMode) {
		m.logf("kloo: mcp · server %q unknown exposeMode %q; treating as lazy", c.Name, cfg.ExposeMode)
	}

	switch resolveMode(cfg) {
	case ExposeLazy:
		names := registerMetaTrio(reg, c)
		m.logf("kloo: mcp · server %q exposed lazily — meta-trio: %s", c.Name, strings.Join(names, ", "))
	case ExposeAll:
		m.exposeList(reg, c, advertisedNames(c), remaining, ExposeAll)
	case ExposeCurated:
		m.exposeList(reg, c, cfg.Expose, remaining, ExposeCurated)
	}
}

// exposeList registers the candidate tools first-class up to the global cap, then
// demotes the remainder of this server to the lazy meta-trio. Candidates not
// advertised by the server are warned and skipped (they never consume the cap).
func (m *Manager) exposeList(reg *tools.Registry, c *Client, candidates []string, remaining *int, mode ExposeMode) {
	// Partition into advertised vs missing (order preserved) so the cap only ever
	// counts tools that actually exist.
	var advertised, missing []string
	for _, name := range candidates {
		if _, ok := toolByName(c, name); ok {
			advertised = append(advertised, name)
		} else {
			missing = append(missing, name)
		}
	}
	for _, miss := range missing {
		m.logf("kloo: mcp · server %q %s: tool %q not advertised by the server; skipped", c.Name, mode, miss)
	}
	if mode == ExposeCurated && len(candidates) == 0 {
		m.logf("kloo: mcp · server %q curated with an empty expose list — nothing exposed", c.Name)
	}

	// Apply the global cap to the advertised tools (declaration order).
	var toRegister, demoted []string
	for _, name := range advertised {
		if *remaining > 0 {
			toRegister = append(toRegister, name)
			*remaining--
		} else {
			demoted = append(demoted, name)
		}
	}

	registered, _ := registerTools(reg, c, toRegister) // all advertised ⇒ none missing here
	if len(registered) > 0 {
		m.logf("kloo: mcp · server %q exposed %d tool(s) %s: %s", c.Name, len(registered), mode, strings.Join(registered, ", "))
	}
	if mode == ExposeAll && len(registered) > allModeWarnThreshold {
		m.logf("kloo: mcp · server %q exposed %d tools via 'all' — many schemas in-window; consider curated or lazy", c.Name, len(registered))
	}

	// Overflow beyond the cap: the rest of this server goes lazy (meta-trio),
	// logged exactly so there is no silent truncation (a Lead-lens requirement).
	if len(demoted) > 0 {
		names := registerMetaTrio(reg, c)
		m.logf("kloo: mcp · server %q hit maxExposedTools cap (%d) — demoted %d tool(s) to lazy: %s; meta-trio: %s",
			c.Name, m.capValue(), len(demoted), strings.Join(demoted, ", "), strings.Join(names, ", "))
	}
}

// advertisedNames returns the server's advertised tool names in ListTools order.
func advertisedNames(c *Client) []string {
	names := make([]string, 0, len(c.tools))
	for _, mt := range c.tools {
		names = append(names, mt.Name)
	}
	return names
}
