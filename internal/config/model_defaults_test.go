package config

import "testing"

// TestLookupModelDefaults proves substring matching is case-insensitive, handles
// size/quant/-instruct suffixes, and resolves the deepseek-coder vs deepseek
// overlap via declared order (first match wins). Unknown / empty ids fall through
// to genericModelDefault.
func TestLookupModelDefaults(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  modelDefault
	}{
		{
			name:  "qwen2.5-coder mixed-case with size+instruct suffix",
			model: "Qwen2.5-Coder-7B-Instruct",
			want:  modelDefault{match: "qwen2.5-coder", toolFormat: "native", temperature: 0.1, maxContextTokens: 24576},
		},
		{
			name:  "qwen2.5-coder different size, same family defaults",
			model: "qwen2.5-coder-32b",
			want:  modelDefault{match: "qwen2.5-coder", toolFormat: "native", temperature: 0.1, maxContextTokens: 24576},
		},
		{
			name:  "qwen3-coder",
			model: "Qwen3-Coder-30B-A3B",
			want:  modelDefault{match: "qwen3-coder", toolFormat: "native", temperature: 0.1, maxContextTokens: 32768},
		},
		{
			name:  "devstral",
			model: "Devstral-Small-2-24B",
			want:  modelDefault{match: "devstral", toolFormat: "native", temperature: 0.15, maxContextTokens: 32768},
		},
		{
			name:  "deepseek-coder routes to coder row, NOT the deepseek row",
			model: "deepseek-coder-33b-instruct",
			want:  modelDefault{match: "deepseek-coder", toolFormat: "native", temperature: 0.1, maxContextTokens: 16384},
		},
		{
			name:  "deepseek v3 routes to the deepseek family row",
			model: "deepseek-v3",
			want:  modelDefault{match: "deepseek", toolFormat: "native", temperature: 0.1, maxContextTokens: 32768},
		},
		{
			name:  "org-prefixed id still matches via substring",
			model: "unsloth/Qwen2.5-Coder-14B-Instruct-GGUF",
			want:  modelDefault{match: "qwen2.5-coder", toolFormat: "native", temperature: 0.1, maxContextTokens: 24576},
		},
		{
			name:  "unknown model falls through to generic",
			model: "totally-unknown-model",
			want:  genericModelDefault,
		},
		{
			name:  "empty model falls through to generic",
			model: "",
			want:  genericModelDefault,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lookupModelDefaults(tc.model)
			if got != tc.want {
				t.Errorf("lookupModelDefaults(%q) = %+v, want %+v", tc.model, got, tc.want)
			}
		})
	}
}

// TestGenericModelDefaultEqualsBuiltins is the guard that keeps "unknown model
// unchanged" true: the generic fallback must equal the built-in default
// constants, so applying it is observationally a no-op.
func TestGenericModelDefaultEqualsBuiltins(t *testing.T) {
	if genericModelDefault.toolFormat != DefaultToolFormat {
		t.Errorf("generic toolFormat = %q, want DefaultToolFormat %q", genericModelDefault.toolFormat, DefaultToolFormat)
	}
	if genericModelDefault.temperature != DefaultTemperature {
		t.Errorf("generic temperature = %v, want DefaultTemperature %v", genericModelDefault.temperature, DefaultTemperature)
	}
	if genericModelDefault.maxContextTokens != DefaultMaxContextTokens {
		t.Errorf("generic maxContextTokens = %d, want DefaultMaxContextTokens %d", genericModelDefault.maxContextTokens, DefaultMaxContextTokens)
	}
}

// TestApplyBundledDefaults proves the helper overwrites exactly the three
// bundled-owned fields and leaves every other Config field untouched.
func TestApplyBundledDefaults(t *testing.T) {
	cases := []struct {
		name                 string
		model                string
		wantToolFormat       string
		wantTemperature      float64
		wantMaxContextTokens int
	}{
		{name: "known model gets bundled values", model: "qwen2.5-coder-7b", wantToolFormat: "native", wantTemperature: 0.1, wantMaxContextTokens: 24576},
		{name: "devstral", model: "Devstral-Small-2-24B", wantToolFormat: "native", wantTemperature: 0.15, wantMaxContextTokens: 32768},
		{name: "unknown model keeps built-in defaults", model: "mystery-model", wantToolFormat: DefaultToolFormat, wantTemperature: DefaultTemperature, wantMaxContextTokens: DefaultMaxContextTokens},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Seed a Config with built-in defaults plus an unrelated field set, to
			// prove applyBundledDefaults touches only the three bundled-owned fields.
			cfg := Config{
				Endpoint:            DefaultEndpoint,
				Model:               tc.model,
				Temperature:         DefaultTemperature,
				MaxSteps:            DefaultMaxSteps,
				Mode:                DefaultMode,
				ToolFormat:          DefaultToolFormat,
				MaxContextTokens:    DefaultMaxContextTokens,
				MaxTokens:           DefaultMaxTokens,
				MaxWallClockSeconds: DefaultMaxWallClockSeconds,
				ChurnRounds:         DefaultChurnRounds,
			}

			applyBundledDefaults(&cfg, tc.model)

			if cfg.ToolFormat != tc.wantToolFormat {
				t.Errorf("ToolFormat = %q, want %q", cfg.ToolFormat, tc.wantToolFormat)
			}
			if cfg.Temperature != tc.wantTemperature {
				t.Errorf("Temperature = %v, want %v", cfg.Temperature, tc.wantTemperature)
			}
			if cfg.MaxContextTokens != tc.wantMaxContextTokens {
				t.Errorf("MaxContextTokens = %d, want %d", cfg.MaxContextTokens, tc.wantMaxContextTokens)
			}
			// Unrelated fields must be untouched.
			if cfg.Endpoint != DefaultEndpoint {
				t.Errorf("Endpoint mutated: %q", cfg.Endpoint)
			}
			if cfg.MaxSteps != DefaultMaxSteps {
				t.Errorf("MaxSteps mutated: %d", cfg.MaxSteps)
			}
			if cfg.MaxWallClockSeconds != DefaultMaxWallClockSeconds {
				t.Errorf("MaxWallClockSeconds mutated: %d", cfg.MaxWallClockSeconds)
			}
			if cfg.ChurnRounds != DefaultChurnRounds {
				t.Errorf("ChurnRounds mutated: %d", cfg.ChurnRounds)
			}
		})
	}
}
