package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
)

func writeProfile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBuildReloadProfileResolvesNewProfile: the /profile reload closure re-resolves
// provider/endpoint/key/model-tuning from a different profiles.json for the next run.
func TestBuildReloadProfileResolvesNewProfile(t *testing.T) {
	path := writeProfile(t, `{
		"providers": {"openrouter": {"endpoint": "https://openrouter.ai/api/v1", "apiKey": "${OR_KEY}"}},
		"qwen/qwen-next-coder": {"maxContextTokens": 128000, "temperature": 0.2, "toolFormat": "native"}
	}`)
	// The provider apiKey "${OR_KEY}" is expanded via os.ExpandEnv, so set the real
	// env; provider/model selection reads the getenv closure (here os.Getenv too).
	t.Setenv("OR_KEY", "sk-or-live")
	t.Setenv(config.EnvProvider, "openrouter")
	t.Setenv(config.EnvModel, "qwen/qwen-next-coder")
	factory := func(endpoint, model, apiKey string) llm.LLMClient { return llm.New(endpoint, model) }

	reload := buildReloadProfile(config.Flags{}, os.Getenv, factory)
	rc, summary, err := reload(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if rc.Provider != "openrouter" || rc.Endpoint != "https://openrouter.ai/api/v1" {
		t.Errorf("provider/endpoint not resolved: %+v", rc)
	}
	if rc.APIKey != "sk-or-live" {
		t.Errorf("apiKey should expand from ${OR_KEY}, got %q", rc.APIKey)
	}
	if rc.Model != "qwen/qwen-next-coder" || rc.ContextTokens != 128000 {
		t.Errorf("model tuning not applied: %+v", rc)
	}
	if !rc.UseNewClient || rc.NewClient == nil {
		t.Error("reload must carry the client factory for the next run")
	}
	// The redacted summary must NOT contain the API key.
	if strings.Contains(summary, "sk-or-live") {
		t.Fatalf("summary leaked the API key: %q", summary)
	}
	for _, want := range []string{"provider=openrouter", "model=qwen/qwen-next-coder", "ctx=128000"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary missing %q: %q", want, summary)
		}
	}
}

// TestBuildReloadProfilePreservesLaunchFlags: an explicit launch flag (e.g. --model)
// still wins over the reloaded profile (flags > env > profile precedence).
func TestBuildReloadProfilePreservesLaunchFlags(t *testing.T) {
	path := writeProfile(t, `{"providers": {"local": {"endpoint": "http://127.0.0.1:8080/v1"}}}`)
	pinned := "pinned-by-flag"
	reload := buildReloadProfile(config.Flags{Model: &pinned}, func(string) string { return "" }, nil)
	rc, _, err := reload(path)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Model != "pinned-by-flag" {
		t.Errorf("explicit --model should survive a /profile reload, got %q", rc.Model)
	}
}

// TestBuildReloadProfileErrorLeavesNoRuntime: a malformed profile returns an error
// (so the TUI keeps its current runtime) rather than a partial config.
func TestBuildReloadProfileErrorLeavesNoRuntime(t *testing.T) {
	bad := writeProfile(t, `{ this is not valid json `)
	reload := buildReloadProfile(config.Flags{}, func(string) string { return "" }, nil)
	if _, _, err := reload(bad); err == nil {
		t.Fatal("expected an error for a malformed profile")
	}
}
