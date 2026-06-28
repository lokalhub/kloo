package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// envFunc builds a getenv closure over a map for deterministic, isolated tests.
func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func strp(s string) *string               { return &s }
func fp(f float64) *float64               { return &f }
func ip(i int) *int                       { return &i }
func bp(b bool) *bool                     { return &b }
func durp(d time.Duration) *time.Duration { return &d }

// writeProfile writes a profile JSON to a temp file and returns its path.
func writeProfile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return path
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name        string
		flags       Flags
		env         map[string]string
		profileBody string // "" => no profile file written (pass a missing path)
		want        Config
	}{
		{
			name: "default-only",
			want: Config{
				Endpoint:            DefaultEndpoint,
				Model:               DefaultModel,
				Temperature:         DefaultTemperature,
				MaxSteps:            DefaultMaxSteps,
				Mode:                DefaultMode,
				ToolFormat:          DefaultToolFormat,
				Effort:              DefaultEffort,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
			},
		},
		{
			name: "env-overrides-default",
			env:  map[string]string{EnvEndpoint: "http://10.0.0.5:9000/v1", EnvModel: "my-model"},
			want: Config{
				Endpoint:            "http://10.0.0.5:9000/v1",
				Model:               "my-model",
				Temperature:         DefaultTemperature,
				MaxSteps:            DefaultMaxSteps,
				Mode:                DefaultMode,
				ToolFormat:          DefaultToolFormat,
				Effort:              DefaultEffort,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
			},
		},
		{
			name:  "flag-overrides-env",
			flags: Flags{Endpoint: strp("http://flag:1/v1"), Model: strp("flagmodel")},
			env:   map[string]string{EnvEndpoint: "http://env:2/v1", EnvModel: "envmodel"},
			want: Config{
				Endpoint:            "http://flag:1/v1",
				Model:               "flagmodel",
				Temperature:         DefaultTemperature,
				MaxSteps:            DefaultMaxSteps,
				Mode:                DefaultMode,
				ToolFormat:          DefaultToolFormat,
				Effort:              DefaultEffort,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
			},
		},
		{
			name:        "profile-value-used-when-no-flag-or-env",
			profileBody: `{"local":{"toolFormat":"xml","temperature":0.7,"fewShotPath":"/fs/model.txt"}}`,
			want: Config{
				Endpoint:            DefaultEndpoint,
				Model:               DefaultModel, // "local"
				Temperature:         0.7,          // from profile
				MaxSteps:            DefaultMaxSteps,
				Mode:                DefaultMode,
				ToolFormat:          "xml",           // from profile
				FewShotPath:         "/fs/model.txt", // from profile
				Effort:              DefaultEffort,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
			},
		},
		{
			name:        "flag-temperature-overrides-profile",
			flags:       Flags{Temperature: fp(0.05)},
			profileBody: `{"local":{"temperature":0.7}}`,
			want: Config{
				Endpoint:            DefaultEndpoint,
				Model:               DefaultModel,
				Temperature:         0.05, // flag wins over profile
				MaxSteps:            DefaultMaxSteps,
				Mode:                DefaultMode,
				ToolFormat:          DefaultToolFormat,
				Effort:              DefaultEffort,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
			},
		},
		{
			name:        "per-model-override-applied-for-selected-model",
			env:         map[string]string{EnvModel: "model-b"},
			profileBody: `{"model-a":{"toolFormat":"xml"},"model-b":{"toolFormat":"native","temperature":0.3}}`,
			want: Config{
				Endpoint:            DefaultEndpoint,
				Model:               "model-b",
				Temperature:         0.3, // model-b's profile entry, not model-a's
				MaxSteps:            DefaultMaxSteps,
				Mode:                DefaultMode,
				ToolFormat:          "native", // model-b's, not model-a's "xml"
				Effort:              DefaultEffort,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
			},
		},
		{
			name:        "profile-no-think-applied",
			profileBody: `{"local":{"noThink":true}}`,
			want: Config{
				Endpoint:            DefaultEndpoint,
				Model:               DefaultModel,
				Temperature:         DefaultTemperature,
				MaxSteps:            DefaultMaxSteps,
				Mode:                DefaultMode,
				ToolFormat:          DefaultToolFormat,
				Effort:              DefaultEffort,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
				NoThink:             true,
			},
		},
		{
			name:        "flag-no-think-overrides-profile",
			flags:       Flags{NoThink: bp(false)},
			profileBody: `{"local":{"noThink":true}}`,
			want: Config{
				Endpoint:            DefaultEndpoint,
				Model:               DefaultModel,
				Temperature:         DefaultTemperature,
				MaxSteps:            DefaultMaxSteps,
				Mode:                DefaultMode,
				ToolFormat:          DefaultToolFormat,
				Effort:              DefaultEffort,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
				NoThinkExplicit:     true,
			},
		},
		{
			name:  "all-flags-set",
			flags: Flags{Endpoint: strp("http://e/v1"), Model: strp("m"), Temperature: fp(0.9), MaxSteps: ip(7), Mode: strp("manual"), JSONOnly: bp(true), StatusFile: strp("/tmp/kloo-status.json")},
			want: Config{
				Endpoint:            "http://e/v1",
				Model:               "m",
				Temperature:         0.9,
				MaxSteps:            7,
				Mode:                "manual",
				ToolFormat:          DefaultToolFormat,
				Effort:              DefaultEffort,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
				JSONOnly:            true,
				StatusFile:          "/tmp/kloo-status.json",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profilePath := filepath.Join(t.TempDir(), "does-not-exist.json")
			if tc.profileBody != "" {
				profilePath = writeProfile(t, tc.profileBody)
			}
			got, err := Resolve(tc.flags, envFunc(tc.env), profilePath)
			if err != nil {
				t.Fatalf("Resolve returned error: %v", err)
			}
			// These cases exercise the core precedence chain, not MCP; neutralise
			// the MCP fields (covered by the dedicated MCP tests below) so the
			// struct can be compared without listing them in every want literal.
			// (Config now holds a map, so it is no longer != comparable.)
			got.MCPServers, got.MCPMaxExposedTools, got.MCPDisabled = nil, 0, false
			got.LLMMaxRetries = tc.want.LLMMaxRetries
			got.LLMRetryableStatusCodes = tc.want.LLMRetryableStatusCodes
			got.LLMRetryBaseDelay = tc.want.LLMRetryBaseDelay
			got.LLMRetryMaxDelay = tc.want.LLMRetryMaxDelay
			got.LLMColdLoadTimeout = tc.want.LLMColdLoadTimeout
			got.LLMStreamIdleTimeout = tc.want.LLMStreamIdleTimeout
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Resolve mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestResolveRetryPolicyPrecedence(t *testing.T) {
	profile := writeProfile(t, `{
		"local": {
			"llmMaxRetries": 1,
			"llmRetryCodes": [503],
			"llmRetryBaseDelay": "3s",
			"llmRetryMaxDelay": "9s",
			"llmColdLoadTimeout": "2m",
			"llmStreamIdleTimeout": "4m"
		}
	}`)
	flags := Flags{
		LLMMaxRetries:           ip(4),
		LLMRetryableStatusCodes: []int{429, 408, 429},
		LLMRetryBaseDelay:       durp(500 * time.Millisecond),
		LLMRetryMaxDelay:        durp(5 * time.Second),
		LLMColdLoadTimeout:      durp(7 * time.Minute),
		LLMStreamIdleTimeout:    durp(3 * time.Minute),
	}
	env := map[string]string{
		EnvLLMMaxRetries:        "2",
		EnvLLMRetryCodes:        "500,502",
		EnvLLMRetryBaseDelay:    "1s",
		EnvLLMRetryMaxDelay:     "6s",
		EnvLLMColdLoadTimeout:   "90",
		EnvLLMStreamIdleTimeout: "120",
	}
	got, err := Resolve(flags, envFunc(env), profile)
	if err != nil {
		t.Fatal(err)
	}
	if got.LLMMaxRetries != 4 {
		t.Fatalf("LLMMaxRetries = %d", got.LLMMaxRetries)
	}
	if !reflect.DeepEqual(got.LLMRetryableStatusCodes, []int{408, 429}) {
		t.Fatalf("retry codes = %v", got.LLMRetryableStatusCodes)
	}
	if got.LLMRetryBaseDelay != 500*time.Millisecond || got.LLMRetryMaxDelay != 5*time.Second ||
		got.LLMColdLoadTimeout != 7*time.Minute || got.LLMStreamIdleTimeout != 3*time.Minute {
		t.Fatalf("durations wrong: base=%s max=%s cold=%s idle=%s",
			got.LLMRetryBaseDelay, got.LLMRetryMaxDelay, got.LLMColdLoadTimeout, got.LLMStreamIdleTimeout)
	}
}

func TestResolveRetryPolicyEnvAndProfile(t *testing.T) {
	profile := writeProfile(t, `{"local":{"llmMaxRetries":1,"llmRetryCodes":[503],"llmRetryBaseDelay":"3s","llmRetryMaxDelay":"9s","llmColdLoadTimeout":"2m","llmStreamIdleTimeout":"4m"}}`)
	got, err := Resolve(Flags{}, envFunc(map[string]string{
		EnvLLMMaxRetries:      "2",
		EnvLLMRetryCodes:      "500,502",
		EnvLLMRetryBaseDelay:  "1s",
		EnvLLMRetryMaxDelay:   "6s",
		EnvLLMColdLoadTimeout: "90",
	}), profile)
	if err != nil {
		t.Fatal(err)
	}
	if got.LLMMaxRetries != 2 || !reflect.DeepEqual(got.LLMRetryableStatusCodes, []int{500, 502}) ||
		got.LLMRetryBaseDelay != time.Second || got.LLMRetryMaxDelay != 6*time.Second ||
		got.LLMColdLoadTimeout != 90*time.Second || got.LLMStreamIdleTimeout != 4*time.Minute {
		t.Fatalf("env/profile retry precedence wrong: %+v", got)
	}
}

func TestResolveMemoryProfileBlock(t *testing.T) {
	profile := writeProfile(t, `{
		"local": {"toolFormat": "xml"},
		"memory": {
			"enabled": true,
			"server": "memory",
			"recallTool": "recall",
			"storeTool": "store",
			"maxRecallBytes": 4096,
			"storeOnFailure": true
		}
	}`)
	got, err := Resolve(Flags{}, envFunc(nil), profile)
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolFormat != "xml" {
		t.Fatalf("per-model entry should still apply, got toolFormat=%q", got.ToolFormat)
	}
	if got.Memory != (MemoryConfig{Enabled: true, Server: "memory", RecallTool: "recall", StoreTool: "store", MaxRecallBytes: 4096, StoreOnFailure: true}) {
		t.Fatalf("memory config wrong: %+v", got.Memory)
	}
}

func TestResolveRetryPolicyExplicitZeroDisablesRetries(t *testing.T) {
	t.Run("profile zero", func(t *testing.T) {
		profile := writeProfile(t, `{"local":{"llmMaxRetries":0}}`)
		got, err := Resolve(Flags{}, envFunc(nil), profile)
		if err != nil {
			t.Fatal(err)
		}
		if got.LLMMaxRetries != 0 {
			t.Fatalf("LLMMaxRetries = %d, want 0", got.LLMMaxRetries)
		}
	})
	t.Run("env zero", func(t *testing.T) {
		profile := writeProfile(t, `{"local":{"llmMaxRetries":2}}`)
		got, err := Resolve(Flags{}, envFunc(map[string]string{EnvLLMMaxRetries: "0"}), profile)
		if err != nil {
			t.Fatal(err)
		}
		if got.LLMMaxRetries != 0 {
			t.Fatalf("LLMMaxRetries = %d, want 0", got.LLMMaxRetries)
		}
	})
	t.Run("flag zero", func(t *testing.T) {
		got, err := Resolve(Flags{LLMMaxRetries: ip(0)}, envFunc(map[string]string{EnvLLMMaxRetries: "2"}), "")
		if err != nil {
			t.Fatal(err)
		}
		if got.LLMMaxRetries != 0 {
			t.Fatalf("LLMMaxRetries = %d, want 0", got.LLMMaxRetries)
		}
	})
}

// TestResolveMissingProfileIsNotError: a non-existent profile path resolves to
// defaults without error.
func TestResolveMissingProfileIsNotError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	got, err := Resolve(Flags{}, envFunc(nil), missing)
	if err != nil {
		t.Fatalf("missing profile should not error, got: %v", err)
	}
	if got.Model != DefaultModel || got.ToolFormat != DefaultToolFormat {
		t.Errorf("expected defaults, got %+v", got)
	}
}

// TestResolveMaxContextTokens: defaults to DefaultMaxContextTokens and is
// overridable per-model via the profile (the Phase-03 context budget key).
func TestResolveMaxContextTokens(t *testing.T) {
	def, err := Resolve(Flags{}, envFunc(nil), filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if def.MaxContextTokens != DefaultMaxContextTokens {
		t.Errorf("default maxContextTokens = %d, want %d", def.MaxContextTokens, DefaultMaxContextTokens)
	}

	prof := writeProfile(t, `{"local":{"maxContextTokens":2048}}`)
	got, err := Resolve(Flags{}, envFunc(nil), prof)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxContextTokens != 2048 {
		t.Errorf("profile override maxContextTokens = %d, want 2048", got.MaxContextTokens)
	}
}

// TestResolveMalformedProfileErrors: an existing-but-invalid profile JSON is a
// clear wrapped error (ErrProfileParse), not a silent fallback.
func TestResolveMalformedProfileErrors(t *testing.T) {
	bad := writeProfile(t, `{"local": {bad json}`)
	_, err := Resolve(Flags{}, envFunc(nil), bad)
	if err == nil {
		t.Fatal("expected error for malformed profile JSON, got nil")
	}
	if !errors.Is(err, ErrProfileParse) {
		t.Errorf("error should wrap ErrProfileParse, got: %v", err)
	}
}

func TestResolveEffort(t *testing.T) {
	noEnv := func(string) string { return "" }
	missing := filepath.Join(t.TempDir(), "none.json")

	// fast tier seeds low budgets; the model is independent (stays the default).
	got, err := Resolve(Flags{Effort: strp("fast")}, noEnv, missing)
	if err != nil {
		t.Fatalf("fast: %v", err)
	}
	if got.Effort != "fast" || got.Model != DefaultModel || got.MaxSteps != 50 || got.ChurnRounds != 2 || got.MaxTokens != 0 {
		t.Errorf("fast tier = %+v", got)
	}

	// heavy tier seeds patient budgets; the model is NOT changed by the tier.
	got, err = Resolve(Flags{Effort: strp("heavy")}, noEnv, missing)
	if err != nil {
		t.Fatalf("heavy: %v", err)
	}
	if got.Effort != "heavy" || got.Model != DefaultModel || got.MaxSteps != 1000 || got.ChurnRounds != 10 {
		t.Errorf("heavy tier = %+v", got)
	}

	// --model sets the model independently of the tier; the tier's budgets apply.
	got, _ = Resolve(Flags{Effort: strp("heavy"), Model: strp("my-model")}, noEnv, missing)
	if got.Model != "my-model" || got.MaxSteps != 1000 {
		t.Errorf("model flag with tier = %+v", got)
	}

	// config "efforts" section overrides a tier's budgets (no model field).
	prof := writeProfile(t, `{"efforts":{"heavy":{"churnRounds":25,"maxTokens":900000}}}`)
	got, err = Resolve(Flags{Effort: strp("heavy")}, noEnv, prof)
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if got.ChurnRounds != 25 || got.MaxTokens != 900000 || got.Model != DefaultModel {
		t.Errorf("efforts override = %+v", got)
	}
}

// TestResolveBundledDefaults exercises the bundled per-model defaults layer:
// (a) a known model with a silent profile gets the bundled values; (b) a user
// profile value wins over bundled (unset fields keep bundled); (c) an unknown
// model is byte-for-byte the built-in defaults; flag/env still beat bundled; and
// a provider alias that resolves to a table-matched id receives bundled defaults.
func TestResolveBundledDefaults(t *testing.T) {
	missing := func(t *testing.T) string { return filepath.Join(t.TempDir(), "none.json") }

	// (a) known model + no profile file ⇒ bundled qwen2.5-coder row.
	t.Run("a-bundled-applied-no-profile", func(t *testing.T) {
		got, err := Resolve(Flags{Model: strp("qwen2.5-coder-7b")}, envFunc(nil), missing(t))
		if err != nil {
			t.Fatal(err)
		}
		if got.ToolFormat != "native" || got.Temperature != 0.1 || got.MaxContextTokens != 24576 {
			t.Errorf("bundled qwen2.5-coder not applied: %+v", got)
		}
	})

	// (a') profile file exists but has an entry with all three fields unset ⇒
	// still the bundled values (the user set nothing to override them).
	t.Run("a-bundled-applied-silent-entry", func(t *testing.T) {
		prof := writeProfile(t, `{"qwen2.5-coder-7b":{"fewShotPath":"/fs/x.txt"}}`)
		got, err := Resolve(Flags{Model: strp("qwen2.5-coder-7b")}, envFunc(nil), prof)
		if err != nil {
			t.Fatal(err)
		}
		if got.ToolFormat != "native" || got.Temperature != 0.1 || got.MaxContextTokens != 24576 {
			t.Errorf("bundled values not applied alongside a silent entry: %+v", got)
		}
		if got.FewShotPath != "/fs/x.txt" {
			t.Errorf("unrelated profile field lost: %+v", got)
		}
	})

	// (b) a profile value for two of the three fields ⇒ user wins; the unset
	// field (toolFormat) keeps the bundled "native".
	t.Run("b-user-overrides-bundled", func(t *testing.T) {
		prof := writeProfile(t, `{"qwen2.5-coder-7b":{"temperature":0.5,"maxContextTokens":4096}}`)
		got, err := Resolve(Flags{Model: strp("qwen2.5-coder-7b")}, envFunc(nil), prof)
		if err != nil {
			t.Fatal(err)
		}
		if got.Temperature != 0.5 || got.MaxContextTokens != 4096 {
			t.Errorf("user values should win over bundled: %+v", got)
		}
		if got.ToolFormat != "native" {
			t.Errorf("unset toolFormat should keep bundled native: %+v", got)
		}
	})

	// (c) unknown model ⇒ Config equals the built-in defaults (no change).
	t.Run("c-unknown-model-unchanged", func(t *testing.T) {
		got, err := Resolve(Flags{Model: strp("totally-unknown-model")}, envFunc(nil), missing(t))
		if err != nil {
			t.Fatal(err)
		}
		if got.ToolFormat != DefaultToolFormat || got.Temperature != DefaultTemperature || got.MaxContextTokens != DefaultMaxContextTokens {
			t.Errorf("unknown model should equal built-in defaults: %+v", got)
		}
	})

	// flag still wins over bundled.
	t.Run("flag-beats-bundled", func(t *testing.T) {
		got, err := Resolve(Flags{Model: strp("qwen2.5-coder-7b"), Temperature: fp(0.9)}, envFunc(nil), missing(t))
		if err != nil {
			t.Fatal(err)
		}
		if got.Temperature != 0.9 {
			t.Errorf("flag temperature should win over bundled: %+v", got)
		}
	})

	// a provider + a raw deepseek model id ⇒ the deepseek bundled defaults (proves
	// the lookup keys off the raw model id under a provider; provider unregressed).
	t.Run("provider-raw-id-matches-table", func(t *testing.T) {
		prof := writeProfile(t, `{
			"providers": {
				"or": {"endpoint": "https://openrouter.ai/api/v1"}
			}
		}`)
		got, err := Resolve(Flags{Provider: strp("or"), Model: strp("deepseek-chat")}, envFunc(nil), prof)
		if err != nil {
			t.Fatal(err)
		}
		if got.Model != "deepseek-chat" {
			t.Fatalf("raw model id should be used verbatim: %+v", got)
		}
		if got.ToolFormat != "native" || got.Temperature != 0.1 || got.MaxContextTokens != 32768 {
			t.Errorf("deepseek bundled defaults not applied under a provider: %+v", got)
		}
	})
}

// TestResolveProvider: a --provider selects an endpoint+key from the "providers"
// block; the --model raw id is then used verbatim (provider and model are
// independent axes — no per-provider alias map any more).
func TestResolveProvider(t *testing.T) {
	t.Setenv("KLOO_TEST_OR_KEY", "or-secret")
	t.Setenv("KLOO_TEST_TG_KEY", "tg-secret")
	prof := writeProfile(t, `{
		"providers": {
			"or": {
				"endpoint": "https://openrouter.ai/api/v1",
				"apiKey": "${KLOO_TEST_OR_KEY}"
			},
			"together": {
				"endpoint": "https://api.together.xyz/v1",
				"apiKey": "${KLOO_TEST_TG_KEY}"
			}
		}
	}`)

	// --provider or --model <raw-id> → OpenRouter endpoint/key + the verbatim id.
	got, err := Resolve(Flags{Provider: strp("or"), Model: strp("deepseek/deepseek-v4-flash")}, envFunc(nil), prof)
	if err != nil {
		t.Fatalf("Resolve(or): %v", err)
	}
	if got.Provider != "or" || got.Endpoint != "https://openrouter.ai/api/v1" || got.APIKey != "or-secret" {
		t.Errorf("provider or: endpoint/key wrong: %+v", got)
	}
	if got.Model != "deepseek/deepseek-v4-flash" {
		t.Errorf("raw model id should be used verbatim: %+v", got)
	}

	// A different provider supplies its own endpoint/key for the same raw id.
	got, err = Resolve(Flags{Provider: strp("together"), Model: strp("deepseek-ai/DeepSeek-V4-Flash")}, envFunc(nil), prof)
	if err != nil {
		t.Fatalf("Resolve(together): %v", err)
	}
	if got.Endpoint != "https://api.together.xyz/v1" || got.APIKey != "tg-secret" || got.Model != "deepseek-ai/DeepSeek-V4-Flash" {
		t.Errorf("together provider + raw id wrong: %+v", got)
	}

	// An unknown provider is a clear error.
	if _, err := Resolve(Flags{Provider: strp("nope")}, envFunc(nil), prof); err == nil {
		t.Error("unknown provider should error")
	}

	// KLOO_PROVIDER selects it from env; --endpoint flag still overrides the
	// provider's endpoint (flags > env > profile).
	got, err = Resolve(Flags{Endpoint: strp("http://local/v1")}, envFunc(map[string]string{EnvProvider: "or", EnvModel: "deepseek/deepseek-v4-flash"}), prof)
	if err != nil {
		t.Fatalf("Resolve(env provider): %v", err)
	}
	if got.Endpoint != "http://local/v1" || got.Model != "deepseek/deepseek-v4-flash" || got.APIKey != "or-secret" {
		t.Errorf("env provider + endpoint flag override: %+v", got)
	}
}

// TestResolveMCPServers: the mcpServers block decodes into typed entries (stdio +
// HTTP), the cap nests under "mcp", and the presence of these reserved keys does
// not disturb per-model entry resolution.
func TestResolveMCPServers(t *testing.T) {
	t.Setenv("KLOO_TEST_MCP_TOKEN", "test-token")
	prof := writeProfile(t, `{
		"qwen3-coder": {"toolFormat": "native", "temperature": 0.2},
		"efforts": {"heavy": {"churnRounds": 9}},
		"mcpServers": {
			"mempalace": {
				"command": "mempalace-mcp",
				"args": ["--db", "/var/db"],
				"env": {"MEMPALACE_LOG": "warn"},
				"exposeMode": "curated",
				"expose": ["recall", "remember"],
				"timeoutSeconds": 45,
				"disabled": false
			},
			"docs": {
				"url": "http://127.0.0.1:9000/mcp",
				"headers": {"Authorization": "Bearer ${KLOO_TEST_MCP_TOKEN}"},
				"exposeMode": "lazy"
			}
		},
		"mcp": {"maxExposedTools": 8}
	}`)

	got, err := Resolve(Flags{Model: strp("qwen3-coder")}, envFunc(nil), prof)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Per-model entry still resolves despite the reserved mcp keys.
	if got.Model != "qwen3-coder" || got.ToolFormat != "native" || got.Temperature != 0.2 {
		t.Errorf("model entry not resolved alongside mcp keys: %+v", got)
	}

	if len(got.MCPServers) != 2 {
		t.Fatalf("want 2 mcp servers, got %d: %+v", len(got.MCPServers), got.MCPServers)
	}
	mp := got.MCPServers["mempalace"]
	wantMP := MCPServerEntry{
		Command:        "mempalace-mcp",
		Args:           []string{"--db", "/var/db"},
		Env:            map[string]string{"MEMPALACE_LOG": "warn"},
		ExposeMode:     "curated",
		Expose:         []string{"recall", "remember"},
		TimeoutSeconds: 45,
		Disabled:       false,
	}
	if !reflect.DeepEqual(mp, wantMP) {
		t.Errorf("mempalace entry\n got: %+v\nwant: %+v", mp, wantMP)
	}
	docs := got.MCPServers["docs"]
	if docs.URL != "http://127.0.0.1:9000/mcp" || docs.ExposeMode != "lazy" || docs.Command != "" {
		t.Errorf("docs entry = %+v", docs)
	}
	if docs.Headers["Authorization"] != "Bearer test-token" {
		t.Errorf("docs headers = %+v", docs.Headers)
	}
	if got.MCPMaxExposedTools != 8 {
		t.Errorf("cap = %d, want 8", got.MCPMaxExposedTools)
	}
	if got.MCPDisabled {
		t.Error("MCPDisabled should default to false")
	}
}

// TestResolveMCPDefaults: no mcp config ⇒ empty servers map, default cap, enabled.
func TestResolveMCPDefaults(t *testing.T) {
	got, err := Resolve(Flags{}, envFunc(nil), filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.MCPServers == nil || len(got.MCPServers) != 0 {
		t.Errorf("want empty (non-nil) servers map, got %#v", got.MCPServers)
	}
	if got.MCPMaxExposedTools != DefaultMCPMaxExposedTools {
		t.Errorf("cap = %d, want default %d", got.MCPMaxExposedTools, DefaultMCPMaxExposedTools)
	}
	if got.MCPDisabled {
		t.Error("MCP should be enabled by default")
	}
}

// TestLoadMCPServersCap: the loader returns the raw cap (0 when absent); Resolve
// applies the default. A profile with mcpServers but no "mcp" key ⇒ cap 0 here.
func TestLoadMCPServersCap(t *testing.T) {
	prof := writeProfile(t, `{"mcpServers": {"x": {"command": "y"}}}`)
	servers, maxExposed, err := loadMCPServers(prof)
	if err != nil {
		t.Fatalf("loadMCPServers: %v", err)
	}
	if maxExposed != 0 {
		t.Errorf("absent cap = %d, want 0", maxExposed)
	}
	if _, ok := servers["x"]; !ok {
		t.Errorf("server x not decoded: %+v", servers)
	}

	// Absent file ⇒ empty map, 0, no error.
	servers, maxExposed, err = loadMCPServers(filepath.Join(t.TempDir(), "none.json"))
	if err != nil || maxExposed != 0 || servers == nil || len(servers) != 0 {
		t.Errorf("absent file: servers=%#v cap=%d err=%v", servers, maxExposed, err)
	}
}

// TestResolveMCPGlobalDisable: precedence --no-mcp (flag) > KLOO_MCP (env) > default.
func TestResolveMCPGlobalDisable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.json")
	bp := func(b bool) *bool { return &b }

	cases := []struct {
		name string
		flag *bool
		env  map[string]string
		want bool
	}{
		{name: "default-enabled", want: false},
		{name: "env-0-disables", env: map[string]string{EnvMCP: "0"}, want: true},
		{name: "env-false-disables", env: map[string]string{EnvMCP: "false"}, want: true},
		{name: "env-FALSE-disables", env: map[string]string{EnvMCP: "FALSE"}, want: true},
		{name: "env-1-enabled", env: map[string]string{EnvMCP: "1"}, want: false},
		{name: "flag-true-overrides-env-enable", flag: bp(true), env: map[string]string{EnvMCP: "1"}, want: true},
		{name: "flag-false-overrides-env-disable", flag: bp(false), env: map[string]string{EnvMCP: "0"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(Flags{NoMCP: tc.flag}, envFunc(tc.env), missing)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.MCPDisabled != tc.want {
				t.Errorf("MCPDisabled = %v, want %v", got.MCPDisabled, tc.want)
			}
		})
	}
}

// TestExpandValue: leading ~ / ~/ → home; $VAR/${VAR} → env; non-leading ~ and
// plain values are left literal.
func TestExpandValue(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	t.Setenv("KLOO_TEST_X", "VAL")

	cases := []struct {
		in, want string
	}{
		{"~/.mempalace", home + "/.mempalace"},
		{"~", home},
		{"$KLOO_TEST_X", "VAL"},
		{"${KLOO_TEST_X}", "VAL"},
		{"~/db/$KLOO_TEST_X", home + "/db/VAL"},
		{"/abs/path", "/abs/path"},
		{"mid~tilde", "mid~tilde"}, // non-leading ~ stays literal
		{"~user/x", "~user/x"},     // only ~ and ~/ expand; ~user is left literal
		{"plain-arg", "plain-arg"},
	}
	for _, tc := range cases {
		if got := expandValue(tc.in); got != tc.want {
			t.Errorf("expandValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestResolveMCPExpansion: end-to-end, the loader expands ~ and $VAR in
// command/args/env values surfaced on Config.
func TestResolveMCPExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	t.Setenv("KLOO_TEST_LOG", "warn")
	t.Setenv("KLOO_TEST_TOKEN", "secret")
	prof := writeProfile(t, `{"mcpServers": {"s": {
		"command": "~/bin/srv",
		"args": ["--db", "~/.data", "--mode", "$KLOO_TEST_LOG"],
		"env": {"LOG": "${KLOO_TEST_LOG}", "PLAIN": "x"},
		"headers": {
			"Authorization": "Bearer ${KLOO_TEST_TOKEN}",
			"X-Config": "~/headers/$KLOO_TEST_LOG",
			"$KLOO_TEST_LOG": "literal-name"
		}
	}}}`)
	got, err := Resolve(Flags{}, envFunc(nil), prof)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	s := got.MCPServers["s"]
	if s.Command != home+"/bin/srv" {
		t.Errorf("command = %q", s.Command)
	}
	wantArgs := []string{"--db", home + "/.data", "--mode", "warn"}
	if !reflect.DeepEqual(s.Args, wantArgs) {
		t.Errorf("args = %v, want %v", s.Args, wantArgs)
	}
	if s.Env["LOG"] != "warn" || s.Env["PLAIN"] != "x" {
		t.Errorf("env = %v", s.Env)
	}
	if s.Headers["Authorization"] != "Bearer secret" {
		t.Errorf("Authorization header = %q", s.Headers["Authorization"])
	}
	if s.Headers["X-Config"] != home+"/headers/warn" {
		t.Errorf("X-Config header = %q", s.Headers["X-Config"])
	}
	if _, ok := s.Headers["$KLOO_TEST_LOG"]; !ok {
		t.Errorf("header names should remain literal, got %v", s.Headers)
	}
}

// TestResolveContextTokensOverride: --ctx / KLOO_CONTEXT_TOKENS override the
// window above profile/bundled/built-in; flag beats env; unset ⇒ default.
func TestResolveContextTokensOverride(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.json")
	if got, _ := Resolve(Flags{MaxContextTokens: ip(32768)}, envFunc(nil), missing); got.MaxContextTokens != 32768 {
		t.Errorf("flag --ctx = %d, want 32768", got.MaxContextTokens)
	}
	if got, _ := Resolve(Flags{}, envFunc(map[string]string{EnvContextTokens: "16384"}), missing); got.MaxContextTokens != 16384 {
		t.Errorf("env KLOO_CONTEXT_TOKENS = %d, want 16384", got.MaxContextTokens)
	}
	if got, _ := Resolve(Flags{MaxContextTokens: ip(4096)}, envFunc(map[string]string{EnvContextTokens: "16384"}), missing); got.MaxContextTokens != 4096 {
		t.Errorf("flag should beat env, got %d", got.MaxContextTokens)
	}
	if got, _ := Resolve(Flags{}, envFunc(nil), missing); got.MaxContextTokens != DefaultMaxContextTokens {
		t.Errorf("unset = %d, want default %d", got.MaxContextTokens, DefaultMaxContextTokens)
	}
}
