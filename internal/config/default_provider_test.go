package config

import (
	"strings"
	"testing"
)

const dpProfile = `{
  "defaultProvider": "lokalai",
  "providers": {
    "lokalai":    {"endpoint": "https://a.invalid/v1", "apiKey": "k1", "models": {"glm": "glm-5.3-flash"}},
    "openrouter": {"endpoint": "https://b.invalid/v1", "models": {"glm": "z-ai/glm-5.3"}}
  },
  "glm-5.3-flash": {"maxContextTokens": 131072}
}`

// TestDefaultProviderApplies is the feature: with no --provider and no
// KLOO_PROVIDER, the profile's own default supplies the endpoint, the key AND the
// alias expansion — everything the flag would have.
func TestDefaultProviderApplies(t *testing.T) {
	path := writeProfile(t, dpProfile)
	cfg, err := Resolve(Flags{Model: strp("glm")}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Provider != "lokalai" || cfg.ProviderSource != "profile" {
		t.Errorf("provider = %q (source %q), want lokalai from the profile", cfg.Provider, cfg.ProviderSource)
	}
	if cfg.Endpoint != "https://a.invalid/v1" {
		t.Errorf("endpoint = %q, want the default provider's", cfg.Endpoint)
	}
	if cfg.APIKey != "k1" {
		t.Errorf("api key = %q, want the default provider's", cfg.APIKey)
	}
	if cfg.Model != "glm-5.3-flash" {
		t.Errorf("model = %q, want the alias expanded via the default provider", cfg.Model)
	}
	// The per-model entry is keyed by the REAL id, so it is only found when the
	// alias expanded — this pins that the default runs early enough.
	if cfg.MaxContextTokens != 131072 {
		t.Errorf("ctx = %d, want the per-model entry for the expanded id", cfg.MaxContextTokens)
	}
}

// TestDefaultProviderPrecedence: the profile default sits BELOW flag and env, so
// either can still point a single run somewhere else.
func TestDefaultProviderPrecedence(t *testing.T) {
	path := writeProfile(t, dpProfile)

	cfg, err := Resolve(Flags{Provider: strp("openrouter"), Model: strp("glm")}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("flag: %v", err)
	}
	if cfg.Provider != "openrouter" || cfg.ProviderSource != "flag" {
		t.Errorf("flag: provider = %q (source %q), want openrouter from the flag", cfg.Provider, cfg.ProviderSource)
	}
	if cfg.Model != "z-ai/glm-5.3" {
		t.Errorf("flag: model = %q, want the flagged provider's alias target", cfg.Model)
	}

	env := envFunc(map[string]string{EnvProvider: "openrouter"})
	cfg, err = Resolve(Flags{Model: strp("glm")}, env, path)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	if cfg.Provider != "openrouter" || cfg.ProviderSource != "env" {
		t.Errorf("env: provider = %q (source %q), want openrouter from the env", cfg.Provider, cfg.ProviderSource)
	}
}

// TestDefaultProviderUnknownNamesTheFile: a typo'd default must blame the profile
// line, not a flag the user never passed — otherwise they search their command
// line for a mistake that is in the file.
func TestDefaultProviderUnknownNamesTheFile(t *testing.T) {
	path := writeProfile(t, `{"defaultProvider":"typo","providers":{"lokalai":{"endpoint":"https://a.invalid/v1"}}}`)
	_, err := Resolve(Flags{}, envFunc(nil), path)
	if err == nil {
		t.Fatal("an unknown defaultProvider resolved successfully")
	}
	msg := err.Error()
	for _, want := range []string{path, "defaultProvider", "typo", "it defines: lokalai"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error is missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "unknown --provider") {
		t.Errorf("a profile typo must not be reported as a bad flag:\n%s", msg)
	}
}

// TestNoDefaultProviderIsNotAnError: the key is optional. A profile without it
// behaves exactly as before — no provider, built-in endpoint.
func TestNoDefaultProviderIsNotAnError(t *testing.T) {
	path := writeProfile(t, `{"providers":{"lokalai":{"endpoint":"https://a.invalid/v1"}}}`)
	cfg, err := Resolve(Flags{}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("a profile with no defaultProvider must still resolve: %v", err)
	}
	if cfg.Provider != "" || cfg.Endpoint != DefaultEndpoint {
		t.Errorf("provider=%q endpoint=%q, want no provider and the built-in endpoint",
			cfg.Provider, cfg.Endpoint)
	}
}

// TestScalarKeyDoesNotBreakTheProfile is the bug found while adding this. The
// top level used to decode wholesale into map[string]profileEntry, so ANY scalar
// setting — defaultProvider being the first one — failed with "cannot unmarshal
// string into Go value of type config.profileEntry" and took the entire profile
// down with it, providers block included.
func TestScalarKeyDoesNotBreakTheProfile(t *testing.T) {
	path := writeProfile(t, `{
	  "defaultProvider": "lokalai",
	  "someFutureScalar": 42,
	  "providers": {"lokalai": {"endpoint": "https://a.invalid/v1"}},
	  "glm-5.3-flash": {"maxContextTokens": 131072}
	}`)
	cfg, err := Resolve(Flags{Model: strp("glm-5.3-flash")}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("a scalar top-level key must not break the profile: %v", err)
	}
	if cfg.MaxContextTokens != 131072 {
		t.Errorf("ctx = %d: the per-model entry was lost alongside the scalar key", cfg.MaxContextTokens)
	}
}
