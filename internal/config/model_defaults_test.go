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

// TestGLMRowMatchesLokalaiID pins the row added for the lokalai endpoint's
// glm-5.3-flash. The match key is only three characters, so this also guards the
// two properties that make a short key safe: it hits the real id, and it does not
// steal any OTHER id already in the table (first-match-wins means a stray hit
// would silently re-route that model's window and temperature).
func TestGLMRowMatchesLokalaiID(t *testing.T) {
	got := lookupModelDefaults("glm-5.3-flash")
	if got.match != "glm" {
		t.Fatalf("glm-5.3-flash matched %q, want the glm row", got.match)
	}
	if got.toolFormat != "native" {
		t.Errorf("toolFormat = %q, want native (verified against the endpoint)", got.toolFormat)
	}
	if got.maxContextTokens != 32768 {
		t.Errorf("maxContextTokens = %d, want 32768", got.maxContextTokens)
	}
	for _, other := range []string{
		"qwen2.5-coder-32b", "qwen3-coder-30b-a3b", "devstral-small-2-24b",
		"deepseek-coder-6.7b", "deepseek/deepseek-v4-flash", "muse-glimmer-30b",
	} {
		if lookupModelDefaults(other).match == "glm" {
			t.Errorf("%s was captured by the glm row", other)
		}
	}
}

// TestQwen3NextRowsMatchLokalaiIDs pins the regression that motivated these rows:
// lokalai serves this family as both "flash-next" and "qwen3.8-flash-next", and
// NEITHER id contains a key from any earlier row, so both fell through to the
// 8000-token default while the endpoint happily accepts a 300k-token prompt.
func TestQwen3NextRowsMatchLokalaiIDs(t *testing.T) {
	for _, id := range []string{
		"flash-next", "qwen3.8-flash-next",
		"Qwen3-Next-80B-A3B", "qwen3-next-80b-a3b-instruct",
	} {
		got := lookupModelDefaults(id)
		if got.match != "qwen3-next" && got.match != "flash-next" {
			t.Errorf("%s matched %q, want a qwen3-next row", id, got.match)
		}
		if got.maxContextTokens != 131072 {
			t.Errorf("%s maxContextTokens = %d, want 131072 (measured 2026-10-01)", id, got.maxContextTokens)
		}
		if got.toolFormat != "native" {
			t.Errorf("%s toolFormat = %q, want native", id, got.toolFormat)
		}
	}
	// The window is what the 8000 default was wrong about; assert the gap is real
	// so a future edit cannot quietly reinstate it.
	if lookupModelDefaults("flash-next").maxContextTokens <= DefaultMaxContextTokens {
		t.Fatal("flash-next window is not above the built-in default — the row is inert")
	}
	// qwen3-coder must keep its own row: "qwen3" is a shared prefix and a sloppier
	// match key would capture it.
	if m := lookupModelDefaults("qwen3-coder-30b-a3b").match; m != "qwen3-coder" {
		t.Errorf("qwen3-coder-30b-a3b matched %q, want qwen3-coder", m)
	}
	for _, other := range []string{
		"qwen2.5-coder-32b", "devstral-small-2-24b", "glm-5.3-flash",
		"deepseek-coder-6.7b", "muse-glimmer-30b",
	} {
		if m := lookupModelDefaults(other).match; m == "qwen3-next" || m == "flash-next" {
			t.Errorf("%s was captured by a qwen3-next row", other)
		}
	}
}
