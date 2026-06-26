package tools

import (
	"context"
	"strings"
	"testing"
)

// TestRunCommandAllowEnv: a secret in kloo's env is HIDDEN from executed commands by
// default (least-privilege), and forwarded ONLY when its name is granted via
// WithAllowedEnv (--allow-env) — the trusted-deploy passthrough.
func TestRunCommandAllowEnv(t *testing.T) {
	t.Setenv("KLOO_TEST_SECRET", "sekret-value")
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	call := Call{Name: NameRunCommand, Args: map[string]any{"command": "printenv KLOO_TEST_SECRET || echo MISSING"}}

	// Default: the secret is NOT exposed.
	res, err := NewRunCommandTool(ws).Invoke(ctx, call)
	if err != nil {
		t.Fatalf("base run: %v", err)
	}
	if strings.Contains(res.Output, "sekret-value") {
		t.Errorf("secret leaked into command env without --allow-env: %q", res.Output)
	}
	if !strings.Contains(res.Output, "MISSING") {
		t.Errorf("expected the var to be absent by default; got %q", res.Output)
	}

	// Granted: the secret IS forwarded.
	res2, err := NewRunCommandTool(ws, WithAllowedEnv([]string{"KLOO_TEST_SECRET"})).Invoke(ctx, call)
	if err != nil {
		t.Fatalf("allow-env run: %v", err)
	}
	if !strings.Contains(res2.Output, "sekret-value") {
		t.Errorf("--allow-env should forward KLOO_TEST_SECRET; got %q", res2.Output)
	}

	// An unset name in the allow list is a harmless no-op (not an empty leak).
	res3, err := NewRunCommandTool(ws, WithAllowedEnv([]string{"KLOO_NOT_SET_XYZ"})).Invoke(ctx,
		Call{Name: NameRunCommand, Args: map[string]any{"command": "echo ok"}})
	if err != nil || !strings.Contains(res3.Output, "ok") {
		t.Errorf("unset allowed name should be a no-op; got %q err %v", res3.Output, err)
	}
}
