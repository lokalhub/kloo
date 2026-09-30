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

// IsolateProfileSearchForPackage is the TestMain-scoped form of
// IsolateProfileSearch: it points HOME, XDG_CONFIG_HOME and the working directory
// at an empty tree for an ENTIRE test binary, and returns a cleanup function.
//
// Per-test isolation only protects the tests someone remembered to annotate, and
// this failure mode is invisible until a developer's own profile happens to
// contradict an assertion — it surfaced three times in two days, each time as a
// different test, each time only because THIS machine had a profile. A package
// that resolves kloo config should isolate once, at the top.
func IsolateProfileSearchForPackage() func() {
	root, err := os.MkdirTemp("", "kloo-testhome-")
	if err != nil {
		panic("isolate profile search: " + err.Error())
	}
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	for _, d := range []string{home, workspace} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			panic("isolate profile search: " + err.Error())
		}
	}
	oldHome, hadHome := os.LookupEnv("HOME")
	oldXDG, hadXDG := os.LookupEnv("XDG_CONFIG_HOME")
	os.Setenv("HOME", home)
	os.Unsetenv("XDG_CONFIG_HOME")
	// Deliberately NOT chdir: at package scope that breaks every test which reads a
	// golden file or parses a source file by relative path, and it buys nothing —
	// the workspace-local rung looks for <cwd>/.kloo, and a package source
	// directory has none. Per-test isolation still chdirs, where it is scoped.
	_ = workspace
	return func() {
		if hadHome {
			os.Setenv("HOME", oldHome)
		} else {
			os.Unsetenv("HOME")
		}
		if hadXDG {
			os.Setenv("XDG_CONFIG_HOME", oldXDG)
		} else {
			os.Unsetenv("XDG_CONFIG_HOME")
		}
		os.RemoveAll(root)
	}
}
