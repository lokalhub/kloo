package config

import "testing"

// noEnv is an empty environment: these tests exercise the profile layer only.
func noEnv(string) string { return "" }

// TestProviderModelAliasExpands: --model dsv4 resolves to the provider's real id,
// and — the point of expanding EARLY — the per-model profile entry keyed by that
// real id is then found, so the window comes out right instead of falling through
// to the 8000-token built-in.
func TestProviderModelAliasExpands(t *testing.T) {
	profile := writeProfile(t, `{
	  "providers": {
	    "openrouter": {
	      "endpoint": "https://openrouter.ai/api/v1",
	      "models": {"dsv4": "deepseek/deepseek-v4-flash"}
	    }
	  },
	  "deepseek/deepseek-v4-flash": {"maxContextTokens": 900000}
	}`)
	provider, model := "openrouter", "dsv4"
	cfg, err := Resolve(Flags{Provider: &provider, Model: &model}, noEnv, profile)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("model = %q, want the expanded id", cfg.Model)
	}
	if cfg.MaxContextTokens != 900000 {
		t.Errorf("window = %d, want 900000 — the alias must expand BEFORE the per-model lookup",
			cfg.MaxContextTokens)
	}
}

// TestProviderModelAliasFeedsBundledDefaults: expansion must also happen before
// the bundled table, which matches on a substring of the id ("deepseek").
func TestProviderModelAliasFeedsBundledDefaults(t *testing.T) {
	profile := writeProfile(t, `{
	  "providers": {"openrouter": {"models": {"ds": "deepseek/deepseek-v3.2"}}}
	}`)
	provider, model := "openrouter", "ds"
	cfg, err := Resolve(Flags{Provider: &provider, Model: &model}, noEnv, profile)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if want := lookupModelDefaults("deepseek/deepseek-v3.2").maxContextTokens; cfg.MaxContextTokens != want {
		t.Errorf("window = %d, want the bundled deepseek row (%d)", cfg.MaxContextTokens, want)
	}
}

// TestUnaliasedModelIsUnchanged: a model id that is not an alias passes through
// verbatim, and a provider with no models block changes nothing.
func TestUnaliasedModelIsUnchanged(t *testing.T) {
	profile := writeProfile(t, `{"providers": {"openrouter": {"endpoint": "https://openrouter.ai/api/v1"}}}`)
	provider, model := "openrouter", "deepseek/deepseek-v4-flash"
	cfg, err := Resolve(Flags{Provider: &provider, Model: &model}, noEnv, profile)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("model = %q, want it passed through verbatim", cfg.Model)
	}
}

// TestAliasIsProviderScoped: the same short name can mean different ids under
// different providers, and an alias never leaks to a run with no --provider.
func TestAliasIsProviderScoped(t *testing.T) {
	profile := writeProfile(t, `{
	  "providers": {
	    "openrouter": {"models": {"fast": "deepseek/deepseek-v4-flash"}},
	    "local":      {"models": {"fast": "qwen3-coder"}}
	  }
	}`)
	for _, tc := range []struct{ provider, want string }{
		{"openrouter", "deepseek/deepseek-v4-flash"},
		{"local", "qwen3-coder"},
	} {
		provider, model := tc.provider, "fast"
		cfg, err := Resolve(Flags{Provider: &provider, Model: &model}, noEnv, profile)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", tc.provider, err)
		}
		if cfg.Model != tc.want {
			t.Errorf("provider %s: model = %q, want %q", tc.provider, cfg.Model, tc.want)
		}
	}

	model := "fast"
	cfg, err := Resolve(Flags{Model: &model}, noEnv, profile)
	if err != nil {
		t.Fatalf("Resolve(no provider): %v", err)
	}
	if cfg.Model != "fast" {
		t.Errorf("with no --provider the alias must not apply, got %q", cfg.Model)
	}
}
