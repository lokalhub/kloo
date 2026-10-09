package config

import (
	"strings"
	"testing"
)

// distillProfile exercises both axes the distiller can be configured on: its own
// named provider (a different endpoint — the single-GPU case) and a plain second
// model served by the same endpoint as the run.
const distillProfile = `{
  "defaultProvider": "main",
  "providers": {
    "main":  {"endpoint": "https://main.invalid/v1", "apiKey": "mk", "models": {"big": "big-model-1"}},
    "cheap": {"endpoint": "https://cheap.invalid/v1", "apiKey": "ck", "models": {"small": "small-model-1"}, "defaultModel": "small"}
  },
  "distill": {"model": "big", "maxWords": 300}
}`

// TestDistillDefaultsToNothing is the generic-by-construction test: a profile
// with no distill block, no env and no flags must resolve to "on, no route, no
// override" — kloo's behaviour before the knobs existed. If this ever fails, the
// feature has started shipping somebody's backend as a default.
func TestDistillDefaultsToNothing(t *testing.T) {
	cfg, err := Resolve(Flags{}, envFunc(nil), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !cfg.DistillEnabled {
		t.Error("distillation must stay ON by default")
	}
	if cfg.DistillProvider != "" || cfg.DistillModel != "" || cfg.DistillEndpoint != "" || cfg.DistillAPIKey != "" {
		t.Errorf("an unconfigured distiller resolved a route: %+v", cfg)
	}
	if cfg.DistillMaxWords != 0 {
		t.Errorf("max words = %d, want 0 (⇒ the agent package's built-in)", cfg.DistillMaxWords)
	}
}

// TestDistillFromProfile: the whole block is readable from kloo.json, and a model
// written as an alias expands through the SELECTED provider — so naming the
// provider twice is not required for a second model on the same service.
func TestDistillFromProfile(t *testing.T) {
	path := writeProfile(t, distillProfile)
	cfg, err := Resolve(Flags{}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.DistillModel != "big-model-1" {
		t.Errorf("distill model = %q, want the alias expanded via the run's provider", cfg.DistillModel)
	}
	if cfg.DistillEndpoint != "" || cfg.DistillAPIKey != "" {
		t.Errorf("no distill provider was named, so the route must stay the run's: %q / key set=%t",
			cfg.DistillEndpoint, cfg.DistillAPIKey != "")
	}
	if cfg.DistillMaxWords != 300 {
		t.Errorf("max words = %d, want 300 from the profile", cfg.DistillMaxWords)
	}
}

// TestDistillProviderResolvesItsOwnRoute: a named distill provider supplies the
// endpoint, the key, the alias map and even the model, exactly as --provider does
// for the run. This is the configuration that keeps compaction off a busy local
// server.
func TestDistillProviderResolvesItsOwnRoute(t *testing.T) {
	path := writeProfile(t, distillProfile)
	cfg, err := Resolve(Flags{DistillProvider: strp("cheap"), DistillModel: strp("small")}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.DistillEndpoint != "https://cheap.invalid/v1" {
		t.Errorf("distill endpoint = %q, want the provider's", cfg.DistillEndpoint)
	}
	if cfg.DistillAPIKey != "ck" {
		t.Errorf("distill key = %q, want the provider's own", cfg.DistillAPIKey)
	}
	if cfg.DistillModel != "small-model-1" {
		t.Errorf("distill model = %q, want the alias expanded in the DISTILL provider", cfg.DistillModel)
	}
	// The run itself must be untouched by any of it.
	if cfg.Endpoint != "https://main.invalid/v1" || cfg.APIKey != "mk" {
		t.Errorf("the distill route moved the RUN's endpoint/key: %q / %q", cfg.Endpoint, cfg.APIKey)
	}
}

// TestDistillProviderDefaultModel: naming only the provider takes its
// defaultModel, so `--distill-provider cheap` is a complete configuration.
func TestDistillProviderDefaultModel(t *testing.T) {
	// No distill.model in the block: the provider's own default must fill it.
	path := writeProfile(t, `{
  "providers": {
    "cheap": {"endpoint": "https://cheap.invalid/v1", "models": {"small": "small-model-1"}, "defaultModel": "small"}
  }
}`)
	cfg, err := Resolve(Flags{DistillProvider: strp("cheap")}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.DistillModel != "small-model-1" {
		t.Errorf("distill model = %q, want the provider's defaultModel (alias-expanded)", cfg.DistillModel)
	}
}

// TestDistillPrecedence walks the chain flag > env > profile > built-in on each
// field independently — the ordering bugs in a layered resolver are always in one
// field, never in all of them.
func TestDistillPrecedence(t *testing.T) {
	path := writeProfile(t, distillProfile)

	t.Run("env beats profile", func(t *testing.T) {
		cfg, err := Resolve(Flags{}, envFunc(map[string]string{
			EnvDistillModel:    "env-model",
			EnvDistillMaxWords: "500",
		}), path)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cfg.DistillModel != "env-model" || cfg.DistillMaxWords != 500 {
			t.Errorf("env did not win: model=%q words=%d", cfg.DistillModel, cfg.DistillMaxWords)
		}
	})

	t.Run("flag beats env", func(t *testing.T) {
		cfg, err := Resolve(Flags{DistillModel: strp("flag-model"), DistillMaxWords: ip(600)}, envFunc(map[string]string{
			EnvDistillModel:    "env-model",
			EnvDistillMaxWords: "500",
		}), path)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if cfg.DistillModel != "flag-model" || cfg.DistillMaxWords != 600 {
			t.Errorf("flag did not win: model=%q words=%d", cfg.DistillModel, cfg.DistillMaxWords)
		}
	})

	t.Run("enabled: profile off, env on, flag off", func(t *testing.T) {
		off := writeProfile(t, `{"distill": {"enabled": false}}`)
		cfg, err := Resolve(Flags{}, envFunc(nil), off)
		if err != nil || cfg.DistillEnabled {
			t.Fatalf("profile enabled=false ignored (err=%v, enabled=%t)", err, cfg.DistillEnabled)
		}
		cfg, err = Resolve(Flags{}, envFunc(map[string]string{EnvDistill: "1"}), off)
		if err != nil || !cfg.DistillEnabled {
			t.Fatalf("KLOO_DISTILL=1 must re-enable above the profile (err=%v, enabled=%t)", err, cfg.DistillEnabled)
		}
		cfg, err = Resolve(Flags{Distill: bp(false)}, envFunc(map[string]string{EnvDistill: "1"}), off)
		if err != nil || cfg.DistillEnabled {
			t.Fatalf("--no-distill must beat the env (err=%v, enabled=%t)", err, cfg.DistillEnabled)
		}
	})

	t.Run("KLOO_DISTILL keeps its opt-out spelling", func(t *testing.T) {
		for _, v := range []string{"0", "false", "no", "off", "OFF"} {
			cfg, err := Resolve(Flags{}, envFunc(map[string]string{EnvDistill: v}), "")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if cfg.DistillEnabled {
				t.Errorf("KLOO_DISTILL=%q did not disable the pass", v)
			}
		}
	})
}

// TestDistillMisconfigurationIsAStartupError. Each of these used to resolve
// quietly to "the run's own model", which is indistinguishable from a working
// route until you read a brief and wonder which model wrote it.
func TestDistillMisconfigurationIsAStartupError(t *testing.T) {
	path := writeProfile(t, distillProfile)

	cases := []struct {
		name  string
		flags Flags
		env   map[string]string
		want  string
	}{
		{
			name:  "unknown provider",
			flags: Flags{DistillProvider: strp("nope")},
			want:  "unknown --distill-provider",
		},
		{
			name:  "negative word cap",
			flags: Flags{DistillMaxWords: ip(-1)},
			want:  "maxWords must be positive",
		},

		{
			name: "unparseable word cap in the env",
			env:  map[string]string{EnvDistillMaxWords: "lots"},
			want: "is not a number",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(tc.flags, envFunc(tc.env), path)
			if err == nil {
				t.Fatalf("want an error mentioning %q, got none", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestDistillEndpointWithoutAModelIsRefused: sending the RUN's model id to a
// different server is the one combination that is almost certainly a mistake —
// the id a hosted provider rejects is exactly the one your local server serves.
func TestDistillEndpointWithoutAModelIsRefused(t *testing.T) {
	_, err := Resolve(Flags{DistillEndpoint: strp("https://other.invalid/v1")}, envFunc(nil), "")
	if err == nil || !strings.Contains(err.Error(), "needs a distill model") {
		t.Fatalf("error = %v, want it to ask for a distill model", err)
	}
}

// TestDistillEndpointWithModelNeedsNoProvider: the common local case — a second
// model on the same server, or a bare endpoint, with no profile at all.
func TestDistillEndpointWithModelNeedsNoProvider(t *testing.T) {
	cfg, err := Resolve(Flags{
		DistillModel:    strp("qwen3:32b"),
		DistillEndpoint: strp("http://127.0.0.1:11434/v1"),
	}, envFunc(nil), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.DistillModel != "qwen3:32b" || cfg.DistillEndpoint != "http://127.0.0.1:11434/v1" {
		t.Errorf("flags did not resolve without a profile: %q @ %q", cfg.DistillModel, cfg.DistillEndpoint)
	}
}
