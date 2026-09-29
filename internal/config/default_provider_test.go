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

// TestProviderDefaultModel closes the second half of the same papercut. Selecting a
// provider resolved the endpoint and key correctly and then sent the built-in
// "local", which a hosted endpoint rejects — config right, run still dead:
//
//	kloo: model "local" is not in the endpoint's catalog
//
// A provider's own defaultModel fixes it, and must go through alias expansion so
// the short name in "models" is usable here too.
func TestProviderDefaultModel(t *testing.T) {
	path := writeProfile(t, `{
	  "defaultProvider": "lokalai",
	  "providers": {
	    "lokalai": {
	      "endpoint": "https://a.invalid/v1",
	      "defaultModel": "glm",
	      "models": {"glm": "glm-5.3-flash"}
	    }
	  },
	  "glm-5.3-flash": {"maxContextTokens": 131072}
	}`)
	cfg, err := Resolve(Flags{}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Model != "glm-5.3-flash" {
		t.Errorf("model = %q, want the provider default expanded through its alias", cfg.Model)
	}
	if cfg.MaxContextTokens != 131072 {
		t.Errorf("ctx = %d: the per-model entry for the expanded id was missed", cfg.MaxContextTokens)
	}
}

// TestModelPrecedence walks every rung: flag > env > provider default > top-level
// default > built-in. Each one must beat the one below it and nothing else.
func TestModelPrecedence(t *testing.T) {
	const body = `{
	  "defaultProvider": "lokalai",
	  "defaultModel": "top-level-model",
	  "providers": {"lokalai": {"endpoint": "https://a.invalid/v1", "defaultModel": "provider-model"}}
	}`
	path := writeProfile(t, body)

	cfg, err := Resolve(Flags{Model: strp("flag-model")}, envFunc(map[string]string{EnvModel: "env-model"}), path)
	if err != nil || cfg.Model != "flag-model" {
		t.Errorf("flag should win: model=%q err=%v", cfg.Model, err)
	}
	cfg, err = Resolve(Flags{}, envFunc(map[string]string{EnvModel: "env-model"}), path)
	if err != nil || cfg.Model != "env-model" {
		t.Errorf("env should beat the profile defaults: model=%q err=%v", cfg.Model, err)
	}
	cfg, err = Resolve(Flags{}, envFunc(nil), path)
	if err != nil || cfg.Model != "provider-model" {
		t.Errorf("provider default should beat the top-level one: model=%q err=%v", cfg.Model, err)
	}

	// Same profile minus the provider's own default: the top-level one takes over.
	path2 := writeProfile(t, `{
	  "defaultProvider": "lokalai",
	  "defaultModel": "top-level-model",
	  "providers": {"lokalai": {"endpoint": "https://a.invalid/v1"}}
	}`)
	cfg, err = Resolve(Flags{}, envFunc(nil), path2)
	if err != nil || cfg.Model != "top-level-model" {
		t.Errorf("top-level default should apply: model=%q err=%v", cfg.Model, err)
	}

	// And with neither, the built-in stands — an existing llama.cpp setup that
	// names no default must keep working exactly as before.
	path3 := writeProfile(t, `{"providers":{"lokalai":{"endpoint":"https://a.invalid/v1"}}}`)
	cfg, err = Resolve(Flags{}, envFunc(nil), path3)
	if err != nil || cfg.Model != DefaultModel {
		t.Errorf("built-in default should stand: model=%q want %q err=%v", cfg.Model, DefaultModel, err)
	}
}

// TestTopLevelDefaultModelAppliesWithoutAnyProvider: defaultModel is not tied to
// providers. A bare llama.cpp user naming their served model in the profile should
// not have to repeat --model on every run either.
func TestTopLevelDefaultModelAppliesWithoutAnyProvider(t *testing.T) {
	path := writeProfile(t, `{"defaultModel": "qwen2.5-coder-7b"}`)
	cfg, err := Resolve(Flags{}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Model != "qwen2.5-coder-7b" {
		t.Errorf("model = %q, want the top-level default", cfg.Model)
	}
	if cfg.Endpoint != DefaultEndpoint {
		t.Errorf("endpoint = %q, want the built-in — defaultModel must not imply a provider", cfg.Endpoint)
	}
	// The bundled table is keyed by the resolved id, so this also proves the default
	// lands before that layer runs.
	if cfg.MaxContextTokens != 24576 {
		t.Errorf("ctx = %d, want the bundled qwen2.5-coder row (24576)", cfg.MaxContextTokens)
	}
}
