package config

import "strings"

// modelDefault is one row of the bundled per-model defaults table. match is a
// lowercase substring tested against the resolved model id (case-insensitive,
// declared order, first match wins). The three value fields mirror the
// bundled-owned cfg fields; everything else falls through to the built-in
// defaults. See the master plan §5.
//
// maxContextTokens is kloo's curator per-step context-assembly budget (see
// DefaultMaxContextTokens / config.go), NOT the model's raw maximum context
// length. The seeded values therefore reflect the model's real window ordinally
// (bigger window ⇒ bigger budget) while staying bounded — pouring a 256K window
// straight into the curator would regress the OOM-cap work (commit 171fcbf).
type modelDefault struct {
	match            string  // lowercase substring of the model id
	toolFormat       string  // "" | "native" | "trained" | "xml" (see tools.SelectAdapter)
	temperature      float64 // coding-appropriate
	maxContextTokens int     // curator per-step budget (NOT the model's raw max)
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
	// Devstral-Small-2-24B. ~128K window; Mistral's published coding temp region.
	{match: "devstral", toolFormat: "native", temperature: 0.15, maxContextTokens: 32768},
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
