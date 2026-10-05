package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/session"
)

// testProfile writes a two-provider profile: the same alias resolves to a
// DIFFERENT id and host under each, which is what makes restoring a model without
// its provider unsafe.
func testProfile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kloo.json")
	cfg := map[string]any{
		"defaultProvider": "alpha",
		"providers": map[string]any{
			"alpha": map[string]any{
				"endpoint": "https://alpha.example/v1",
				"models":   map[string]any{"fast": "alpha/fast-1"},
			},
			"beta": map[string]any{
				"endpoint": "https://beta.example/v1",
				"models":   map[string]any{"fast": "beta/fast-9"},
			},
		},
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func noEnv(string) string { return "" }

// REQUIREMENT 1: a resumed session runs on the model it was saved with.
func TestResumeRestoresTheSessionsModel(t *testing.T) {
	profile := testProfile(t)
	beta, fast := "beta", "fast"
	saved, err := config.Resolve(config.Flags{Provider: &beta, Model: &fast}, noEnv, profile)
	if err != nil {
		t.Fatalf("resolve beta: %v", err)
	}

	// A bare resume: the profile default is alpha.
	cfg, err := config.Resolve(config.Flags{}, noEnv, profile)
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}
	if cfg.Model == saved.Model {
		t.Fatal("test is not exercising anything: default already equals the saved model")
	}

	sess := &session.Session{Model: saved.Model, Provider: saved.Provider, Endpoint: saved.Endpoint}
	msg := restoreSessionRuntime(&cfg, config.Flags{}, noEnv, profile, sess, true)
	if cfg.Model != saved.Model {
		t.Errorf("model = %q, want the session's %q", cfg.Model, saved.Model)
	}
	if cfg.Endpoint != saved.Endpoint {
		t.Errorf("endpoint = %q, want the session's %q", cfg.Endpoint, saved.Endpoint)
	}
	if !strings.Contains(msg, saved.Model) {
		t.Errorf("restore was silent about what it did: %q", msg)
	}
}

// REQUIREMENT 2: an explicit --model (or --provider) outranks the transcript.
// Flags.Model/Provider are pointers set only under fs.Changed, so a nil pointer is
// the only honest signal that the user did not type one — a profile default is
// already folded into cfg and must not win here.
func TestExplicitFlagsOutrankTheSession(t *testing.T) {
	profile := testProfile(t)
	alpha, fast := "alpha", "fast"
	sess := &session.Session{Model: "beta/fast-9", Provider: "beta", Endpoint: "https://beta.example/v1"}

	for name, flags := range map[string]config.Flags{
		"--model":    {Model: &fast},
		"--provider": {Provider: &alpha},
	} {
		cfg, err := config.Resolve(flags, noEnv, profile)
		if err != nil {
			t.Fatalf("%s: resolve: %v", name, err)
		}
		before := cfg
		if msg := restoreSessionRuntime(&cfg, flags, noEnv, profile, sess, true); msg != "" {
			t.Errorf("%s: restore spoke up when it should stand aside: %q", name, msg)
		}
		if cfg.Model != before.Model || cfg.Endpoint != before.Endpoint {
			t.Errorf("%s: session overrode an explicit flag: model %q→%q endpoint %q→%q",
				name, before.Model, cfg.Model, before.Endpoint, cfg.Endpoint)
		}
	}
}

// REQUIREMENT 3, THE TRAP. Restoring the model WITHOUT its provider sends the
// saved (already-resolved) id to whatever host the current default resolves to.
// Measured against the real profile before this fix:
//
//	saved on openrouter:  deepseek/deepseek-v4-flash @ openrouter.ai/api/v1
//	model-only restore:   deepseek/deepseek-v4-flash @ lokalai.silverjrom.app/v1
//
// config.Resolve does not error on an id the endpoint never served, so nothing
// downstream catches it. The model and endpoint must move together or not at all.
func TestResumeNeverSendsASavedModelToADifferentEndpoint(t *testing.T) {
	profile := testProfile(t)
	beta, fast := "beta", "fast"
	saved, err := config.Resolve(config.Flags{Provider: &beta, Model: &fast}, noEnv, profile)
	if err != nil {
		t.Fatalf("resolve beta: %v", err)
	}
	cfg, err := config.Resolve(config.Flags{}, noEnv, profile) // default: alpha
	if err != nil {
		t.Fatalf("resolve default: %v", err)
	}

	sess := &session.Session{Model: saved.Model, Provider: saved.Provider, Endpoint: saved.Endpoint}
	restoreSessionRuntime(&cfg, config.Flags{}, noEnv, profile, sess, true)

	if cfg.Model == saved.Model && cfg.Endpoint != saved.Endpoint {
		t.Fatalf("TRAP: model %q restored but pointed at %q instead of %q",
			cfg.Model, cfg.Endpoint, saved.Endpoint)
	}
}

// A session file written before provider/endpoint were persisted has no way to say
// which host served its model, so restoring it would be a guess. It declines — and
// says so, because a silently ignored saved model is the original bug again.
func TestLegacySessionWithoutAProviderDeclinesLoudly(t *testing.T) {
	profile := testProfile(t)
	cfg, err := config.Resolve(config.Flags{}, noEnv, profile)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	before := cfg

	sess := &session.Session{Model: "beta/fast-9"} // no Provider, no Endpoint
	msg := restoreSessionRuntime(&cfg, config.Flags{}, noEnv, profile, sess, true)
	if cfg.Model != before.Model || cfg.Endpoint != before.Endpoint {
		t.Errorf("restored from a session that never recorded its endpoint: %q @ %q", cfg.Model, cfg.Endpoint)
	}
	if !strings.Contains(msg, "beta/fast-9") {
		t.Errorf("declined silently, which is the original bug: %q", msg)
	}
}

// A FRESH launch must be untouched: this only applies to --resume.
func TestFreshLaunchIsNotTouched(t *testing.T) {
	profile := testProfile(t)
	cfg, err := config.Resolve(config.Flags{}, noEnv, profile)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	before := cfg
	sess := &session.Session{Model: "beta/fast-9", Provider: "beta", Endpoint: "https://beta.example/v1"}
	if msg := restoreSessionRuntime(&cfg, config.Flags{}, noEnv, profile, sess, false); msg != "" {
		t.Errorf("spoke up on a fresh launch: %q", msg)
	}
	if cfg.Model != before.Model || cfg.Endpoint != before.Endpoint || cfg.Provider != before.Provider {
		t.Errorf("a fresh launch was altered by a stored session: %q/%q/%q → %q/%q/%q",
			before.Provider, before.Model, before.Endpoint, cfg.Provider, cfg.Model, cfg.Endpoint)
	}
}

// An old session JSON with no provider/endpoint keys must load unchanged.
func TestLegacySessionJSONLoadsUnchanged(t *testing.T) {
	var s session.Session
	if err := json.Unmarshal([]byte(`{"id":"s1","model":"m","verify":"go test ./...","runs":2}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if s.Model != "m" || s.Runs != 2 || s.Provider != "" || s.Endpoint != "" {
		t.Fatalf("legacy load changed: %+v", s)
	}
}
