package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// envFunc builds a getenv closure over a map for deterministic, isolated tests.
func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func strp(s string) *string { return &s }
func fp(f float64) *float64 { return &f }
func ip(i int) *int         { return &i }

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
			name:  "all-flags-set",
			flags: Flags{Endpoint: strp("http://e/v1"), Model: strp("m"), Temperature: fp(0.9), MaxSteps: ip(7), Mode: strp("manual")},
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
			if got != tc.want {
				t.Errorf("Resolve mismatch\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
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
	if got.Effort != "fast" || got.Model != DefaultModel || got.MaxSteps != 20 || got.ChurnRounds != 2 || got.MaxTokens != 80000 {
		t.Errorf("fast tier = %+v", got)
	}

	// heavy tier seeds patient budgets; the model is NOT changed by the tier.
	got, err = Resolve(Flags{Effort: strp("heavy")}, noEnv, missing)
	if err != nil {
		t.Fatalf("heavy: %v", err)
	}
	if got.Effort != "heavy" || got.Model != DefaultModel || got.MaxSteps != 80 || got.ChurnRounds != 10 {
		t.Errorf("heavy tier = %+v", got)
	}

	// --model sets the model independently of the tier; the tier's budgets apply.
	got, _ = Resolve(Flags{Effort: strp("heavy"), Model: strp("my-model")}, noEnv, missing)
	if got.Model != "my-model" || got.MaxSteps != 80 {
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
