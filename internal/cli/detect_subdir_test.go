package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectVerifySubdir(t *testing.T) {
	// root has a project → used directly
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"scripts":{"test":"jest"}}`), 0o644)
	if got := detectVerify(root); got != "npm test" {
		t.Errorf("root project: got %q, want npm test", got)
	}

	// root has NO project, ONE subdir does → cd <subdir> && cmd
	parent := t.TempDir()
	os.MkdirAll(filepath.Join(parent, "myApp"), 0o755)
	os.WriteFile(filepath.Join(parent, "myApp", "package.json"), []byte(`{"scripts":{"build":"ng build"}}`), 0o644)
	os.MkdirAll(filepath.Join(parent, "node_modules", "x"), 0o755) // must be skipped
	os.WriteFile(filepath.Join(parent, "node_modules", "x", "package.json"), []byte(`{"scripts":{"build":"x"}}`), 0o644)
	if got := detectVerify(parent); got != "cd myApp && npm run build" {
		t.Errorf("subdir project: got %q, want 'cd myApp && npm run build'", got)
	}

	// TWO subdir projects → ambiguous → "" (don't guess)
	mono := t.TempDir()
	for _, s := range []string{"frontend", "worker"} {
		os.MkdirAll(filepath.Join(mono, s), 0o755)
		os.WriteFile(filepath.Join(mono, s, "package.json"), []byte(`{"scripts":{"test":"t"}}`), 0o644)
	}
	if got := detectVerify(mono); got != "" {
		t.Errorf("ambiguous monorepo: got %q, want \"\"", got)
	}
}
