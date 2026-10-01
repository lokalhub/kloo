package config

import "strings"

// modelDefault is one row of the bundled per-model defaults table. match is a
// lowercase substring tested against the resolved model id (case-insensitive,
// declared order, first match wins). The three value fields mirror the
// bundled-owned cfg fields; everything else falls through to the built-in
// defaults. See the master plan §5.
//
// maxContextTokens is the MODEL'S context window (cfg.MaxContextTokens / --ctx,
// default 8000) — what the endpoint can hold. It is NOT the curator budget; that
// is the separate CuratorBudgetTokens / --curator-budget field (default 32768),
// which this table does not set. An earlier version of this comment claimed the
// reverse, and the 32768 values on the 256K-window rows below were seeded under
// that misreading: they bound those models to a 32K window rather than to a 32K
// assembled repo map. Left as-is pending a bench run, since widening them changes
// measured behaviour; see the qwen3-next rows for what a window-sized value is.
type modelDefault struct {
	match            string  // lowercase substring of the model id
	toolFormat       string  // "" | "native" | "trained" | "xml" (see tools.SelectAdapter)
	temperature      float64 // coding-appropriate
	maxContextTokens int     // the MODEL'S context window (--ctx), not the curator budget
}

// bundledModelDefaults is the ordered, in-binary table of known-good defaults
// keyed by a lowercase substring of the resolved model id. It is evaluated in
// DECLARED ORDER, first match wins — so specific keys MUST precede family keys
// (deepseek-coder before deepseek). A model that matches nothing falls through to
// genericModelDefault (== the built-in defaults), so unknown models are unchanged.
var bundledModelDefaults = []modelDefault{
	// Qwen2.5-Coder 7B / 14B / 32B share these defaults. ~32K native window;
	// a 24K curator budget leaves headroom for system + history + output.
	{match: "qwen2.5-coder", toolFormat: "native", temperature: 0.1, maxContextTokens: 24576},
	// Qwen3-Coder-30B-A3B. ~256K native window; bounded 32K curator budget (the
	// full window would blow the repo-map curator / cost).
	{match: "qwen3-coder", toolFormat: "native", temperature: 0.1, maxContextTokens: 32768},
	// Qwen3-Next (Qwen3-Next-80B-A3B and the lokalai ids "flash-next" and
	// "qwen3.8-flash-next", which are the same family). Two match keys because the
	// endpoint's short alias does not carry the family name, and a model that
	// matched nothing fell through to the 8000-token default — a ~37x undercount
	// that made kloo compact away a directory listing it had just read.
	//
	// Window measured against the endpoint 2026-10-01 by prompt length, not by
	// max_tokens (this endpoint accepts max_tokens=2097152 without complaint, so
	// that probe proves nothing): a 300,012-token prompt is accepted. /models
	// reports no context_length for any model here, which is why nothing could be
	// discovered at runtime. Set to 131072 — comfortably inside the measured floor,
	// and matching the value already in use for glm-5.3-flash on the same host.
	{match: "qwen3-next", toolFormat: "native", temperature: 0.1, maxContextTokens: 131072},
	{match: "flash-next", toolFormat: "native", temperature: 0.1, maxContextTokens: 131072},
	// Devstral-Small-2-24B. ~128K window; Mistral's published coding temp region.
	{match: "devstral", toolFormat: "native", temperature: 0.15, maxContextTokens: 32768},
	// GLM-4.6 / GLM-5.x (incl. glm-5.3-flash on lokalai). Native function calling
	// verified against the endpoint 2026-09-28: it emits a well-formed tool_calls
	// block and, unlike muse-glimmer-30b, spends no hidden reasoning budget before
	// the first visible token. Raw window measured at 262144 (max_tokens 262130 on a
	// 14-token prompt is accepted; 262145 is rejected) — bounded here to a 32K
	// curator budget, same as the other 256K-window row above.
	{match: "glm", toolFormat: "native", temperature: 0.1, maxContextTokens: 32768},
	// DeepSeek-Coder (the original 16K-window coder line). MUST precede the
	// "deepseek" row below, since "deepseek" is a substring of these ids and
	// first-match-wins would otherwise mis-route it to the v3 row.
	{match: "deepseek-coder", toolFormat: "native", temperature: 0.1, maxContextTokens: 16384},
	// DeepSeek v3 / chat. ~128K window; bounded 32K curator budget.
	{match: "deepseek", toolFormat: "native", temperature: 0.1, maxContextTokens: 32768},
}

// genericModelDefault is the fallback returned when no row matches. It is defined
// in terms of the built-in default constants so it is provably EQUAL to them
// (guarded by TestGenericModelDefaultEqualsBuiltins) — applying it is a no-op,
// which is what keeps "unknown model unchanged" true.
var genericModelDefault = modelDefault{
	match:            "",
	toolFormat:       DefaultToolFormat,
	temperature:      DefaultTemperature,
	maxContextTokens: DefaultMaxContextTokens,
}

// lookupModelDefaults returns the bundled defaults for a model id: the first row
// whose (lowercase) match is a substring of the lowercased model, evaluated in
// declared order; genericModelDefault when nothing matches.
func lookupModelDefaults(model string) modelDefault {
	lower := strings.ToLower(model)
	for _, d := range bundledModelDefaults {
		if strings.Contains(lower, d.match) {
			return d
		}
	}
	return genericModelDefault
}

// applyBundledDefaults overwrites only the bundled-owned cfg fields
// (ToolFormat, Temperature, MaxContextTokens) from the matched table row. It
// touches nothing else — endpoint, model, key, effort budgets, MCP, and few-shot
// are other axes. This is the precedence layer below the user profile and above
// the flat built-in defaults (master plan §4).
func applyBundledDefaults(cfg *Config, model string) {
	d := lookupModelDefaults(model)
	cfg.ToolFormat = d.toolFormat
	cfg.Temperature = d.temperature
	cfg.MaxContextTokens = d.maxContextTokens
}
