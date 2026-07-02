package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProfileFile writes a profiles.json with the given body to a temp dir.
func writeProfileFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	return path
}

// fakeReload is a stand-in ReloadProfile that resolves a small set of test profiles
// by path — it exercises the /profile handler without the CLI's config.Resolve. A
// path containing "bad" fails (no partial update); otherwise it returns a runtime
// keyed off the filename so tests can assert the swap.
func fakeReload(t *testing.T) func(string) (RuntimeConfig, string, error) {
	t.Helper()
	return func(path string) (RuntimeConfig, string, error) {
		if strings.Contains(path, "bad") {
			return RuntimeConfig{}, "", os.ErrNotExist
		}
		rc := RuntimeConfig{
			Provider:      "openrouter",
			Endpoint:      "https://openrouter.ai/api/v1",
			APIKey:        "sk-secret-should-not-print",
			Model:         "qwen/qwen-next-coder",
			ContextTokens: 128000,
			Temperature:   0.1,
			ToolFormat:    "native",
			UseNewClient:  true,
		}
		return rc, "provider=openrouter model=qwen/qwen-next-coder endpoint=https://openrouter.ai/api/v1 ctx=128000", nil
	}
}

func modelWithReload(t *testing.T) Model {
	t.Helper()
	return sized(New(Config{
		Model:         "local",
		MaxSteps:      40,
		ProfilePath:   "/tmp/original.json",
		Getenv:        func(string) string { return "" },
		ReloadProfile: fakeReload(t),
	}), tw, th)
}

// TestSlashProfileSwitchUpdatesRuntime: /profile <path> reloads the runtime for
// subsequent runs — provider/model/endpoint/ctx are swapped and the profile path
// updated. The API key is applied to the runtime but never printed.
func TestSlashProfileSwitchUpdatesRuntime(t *testing.T) {
	m := modelWithReload(t)
	m2 := typeAndEnter(m, "/profile /tmp/new.json")

	if m2.runtime.Model != "qwen/qwen-next-coder" {
		t.Errorf("runtime.Model = %q, want qwen/qwen-next-coder", m2.runtime.Model)
	}
	if m2.runtime.Endpoint != "https://openrouter.ai/api/v1" || m2.runtime.Provider != "openrouter" {
		t.Errorf("runtime provider/endpoint not swapped: %+v", m2.runtime)
	}
	if m2.runtime.ContextTokens != 128000 {
		t.Errorf("runtime.ContextTokens = %d, want 128000 (model tuning applied)", m2.runtime.ContextTokens)
	}
	if m2.profilePath != "/tmp/new.json" {
		t.Errorf("profilePath = %q, want /tmp/new.json", m2.profilePath)
	}
	if m2.status.model != "qwen/qwen-next-coder" || m2.status.provider != "openrouter" {
		t.Errorf("status not updated: model=%q provider=%q", m2.status.model, m2.status.provider)
	}
	v := m2.View()
	if !contains(v, "profile: loaded /tmp/new.json") {
		t.Errorf("expected a load confirmation, got:\n%s", v)
	}
	if strings.Contains(v, "sk-secret-should-not-print") {
		t.Fatalf("API key must never be printed in the TUI:\n%s", v)
	}
}

// TestSlashProfileFailureKeepsRuntime: a failed load does not partially update the
// runtime/profile and shows a clear error.
func TestSlashProfileFailureKeepsRuntime(t *testing.T) {
	m := modelWithReload(t)
	before := m.runtime
	beforePath := m.profilePath
	m2 := typeAndEnter(m, "/profile /tmp/bad.json")

	// RuntimeConfig has a func field (not comparable with ==); check the value fields.
	if m2.runtime.Model != before.Model || m2.runtime.Endpoint != before.Endpoint ||
		m2.runtime.Provider != before.Provider || m2.runtime.APIKey != before.APIKey ||
		m2.runtime.ContextTokens != before.ContextTokens {
		t.Errorf("failed load must not change runtime: before=%+v after=%+v", before, m2.runtime)
	}
	if m2.profilePath != beforePath {
		t.Errorf("failed load must not change profilePath: %q", m2.profilePath)
	}
	if !contains(m2.View(), "could not load /tmp/bad.json") {
		t.Errorf("expected a clear load-failure message, got:\n%s", m2.View())
	}
}

// TestSlashProfileNeedsPath: /profile with no argument explains it needs a path.
func TestSlashProfileNeedsPath(t *testing.T) {
	m := modelWithReload(t)
	m2 := typeAndEnter(m, "/profile")
	if !contains(m2.View(), "/profile needs a path") {
		t.Errorf("expected a needs-path message, got:\n%s", m2.View())
	}
}

// TestSlashProfileUnavailable: with no ReloadProfile wired, /profile reports it is
// unavailable rather than panicking.
func TestSlashProfileUnavailable(t *testing.T) {
	m := sized(New(Config{Model: "local", MaxSteps: 40}), tw, th)
	m2 := typeAndEnter(m, "/profile /tmp/x.json")
	if !contains(m2.View(), "/profile is not available") {
		t.Errorf("expected an unavailable message, got:\n%s", m2.View())
	}
}

// TestSlashProfileMenuEntry: /profile appears in the slash menu (filtered by "/pro")
// alongside /provider, and both remain distinct.
func TestSlashProfileMenuEntry(t *testing.T) {
	m := typeRunes(newSized(), "/pro")
	names := menuNames(m)
	var hasProfile, hasProvider bool
	for _, n := range names {
		switch n {
		case "/profile":
			hasProfile = true
		case "/provider":
			hasProvider = true
		}
	}
	if !hasProfile || !hasProvider {
		t.Errorf("both /profile and /provider should appear for '/pro', got %v", names)
	}
}

// TestSlashProviderListsReloadedProfileProviders: after /profile switches the loaded
// profile path, /provider lists the NEW profile's providers (not the old file's).
// Uses the real reload seam is unnecessary here — /provider reads m.profilePath via
// config.ListProviders, so we point the switch at a real file with a distinct provider.
func TestSlashProviderListsReloadedProfileProviders(t *testing.T) {
	// Start with a profile that has only "local"; switch to one that adds "together".
	newPath := writeProfileFile(t, `{
		"providers": {
			"local":    {"endpoint": "http://127.0.0.1:8080/v1"},
			"together": {"endpoint": "https://api.together.xyz/v1", "apiKey": "sk-tg"}
		}
	}`)
	m := sized(New(Config{
		Model:       "local",
		MaxSteps:    40,
		ProfilePath: writeProfileFile(t, `{"providers":{"local":{"endpoint":"http://127.0.0.1:8080/v1"}}}`),
		Getenv:      func(string) string { return "" },
		// A reload seam that just adopts the given path (so m.profilePath updates and
		// /provider reads the new file). Runtime fields are irrelevant to this test.
		ReloadProfile: func(path string) (RuntimeConfig, string, error) {
			return RuntimeConfig{Endpoint: "http://127.0.0.1:8080/v1", Model: "local"}, "provider=none model=local endpoint=http://127.0.0.1:8080/v1 ctx=0", nil
		},
	}), tw, th)

	// Before: /provider lists only local.
	if before := typeAndEnter(m, "/provider").View(); contains(before, "together") {
		t.Fatalf("precondition: together should not exist in the original profile:\n%s", before)
	}
	// Switch the loaded profile file, then /provider should now see "together".
	m = typeAndEnter(m, "/profile "+newPath)
	after := typeAndEnter(m, "/provider").View()
	if !contains(after, "together") {
		t.Errorf("/provider should list the reloaded profile's providers, got:\n%s", after)
	}
}
