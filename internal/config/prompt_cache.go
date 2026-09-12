package config

import (
	"net/url"
	"strings"
)

// Prompt-cache modes (--prompt-cache / KLOO_PROMPT_CACHE / profile promptCache).
const (
	// PromptCacheAuto enables caching only for a provider on the allowlist below.
	// Default: an unknown endpoint must never be sent a field it may reject.
	PromptCacheAuto = "auto"
	// PromptCacheOff never asks for caching.
	PromptCacheOff = "off"
	// PromptCacheOn forces it, for a provider the allowlist does not know yet.
	PromptCacheOn = "on"
)

// DefaultPromptCache is the shipped mode.
const DefaultPromptCache = PromptCacheAuto

// IsPromptCacheMode reports whether s is one of the three accepted modes.
func IsPromptCacheMode(s string) bool {
	switch s {
	case PromptCacheAuto, PromptCacheOff, PromptCacheOn:
		return true
	}
	return false
}

// PromptCacheModes lists the accepted modes, for flag help and error text.
func PromptCacheModes() []string {
	return []string{PromptCacheAuto, PromptCacheOff, PromptCacheOn}
}

// promptCacheAllowlist is the deliberately SMALL set of providers known to accept
// an OpenAI-style prompt-cache breakpoint on a content-parts message. Every entry
// carries the reason it is here; anything not matched resolves to off, and the
// client's rejection fallback is the second net under that.
//
// The wire field's NAME is deliberately not written here: internal/llm owns the
// serialization, this package owns only the policy.
//
// This is the one place in the tree that names a provider. The marker type lives
// in internal/llm and the placement in internal/agent, neither of which may learn
// a provider name (AGENTS.md: "if a fix only works for one provider or one model,
// it is in the wrong layer").
var promptCacheAllowlist = []struct {
	// provider matches the resolved --provider name, host matches the endpoint's
	// hostname suffix, and model matches a substring of the resolved model id.
	// Empty fields do not participate in the match.
	provider string
	host     string
	model    string
	why      string
}{
	{
		provider: "anthropic", host: "api.anthropic.com",
		why: "Anthropic defined the breakpoint; its OpenAI-compatible surface accepts it verbatim.",
	},
	{
		provider: "openrouter", host: "openrouter.ai",
		why: "OpenRouter forwards the breakpoint to upstreams that honour it and ignores it for those that do not.",
	},
	{
		model: "claude",
		why:   "A Claude model id reaches kloo through a gateway that proxies Anthropic, which is where the breakpoint is understood.",
	},
}

// PromptCacheEnabled resolves a mode against the allowlist: "off" is always off,
// "on" is always on, and "auto" is on only for an allowlisted provider/host/model.
// Matching is case-insensitive; an unparsable endpoint simply fails to match.
func PromptCacheEnabled(mode, provider, endpoint, model string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case PromptCacheOff:
		return false
	case PromptCacheOn:
		return true
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	host := ""
	if u, err := url.Parse(strings.TrimSpace(endpoint)); err == nil {
		host = strings.ToLower(u.Hostname())
	}

	for _, e := range promptCacheAllowlist {
		if e.provider != "" && provider == e.provider {
			return true
		}
		if e.host != "" && host != "" && (host == e.host || strings.HasSuffix(host, "."+e.host)) {
			return true
		}
		if e.model != "" && model != "" && strings.Contains(model, e.model) {
			return true
		}
	}
	return false
}

// PromptCacheEnabled resolves this config's mode against its own provider,
// endpoint and model — what a run will actually do.
func (c Config) PromptCacheEnabled() bool {
	return PromptCacheEnabled(c.PromptCache, c.Provider, c.Endpoint, c.Model)
}
