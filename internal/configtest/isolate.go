package configtest

import (
	"os"
	"path/filepath"
	"testing"
)

// IsolateProfileSearch points the profile search chain at an empty temporary tree
// for the duration of a test, so no test can read the DEVELOPER'S real profile.
//
// It exists because the search chain made config resolution depend on the machine
// it runs on. Tests that meant "no profile is configured" started picking up
// ~/.kloo or ~/etc/kloo.json and failing — or worse, passing for the wrong reason
// on a machine that happened to have no profile, while CI and a laptop disagreed.
//
// Call it from any test that resolves a Config without an explicit profile path.
// It restores HOME, XDG_CONFIG_HOME and the working directory on cleanup.
func IsolateProfileSearch(t *testing.T) (home, workspace string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	workspace = filepath.Join(root, "workspace")
	for _, d := range []string{home, workspace} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("isolate profile search: %v", err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("isolate profile search: %v", err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatalf("isolate profile search: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return home, workspace
}
