package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestDetectVerify: each recognised manifest maps to its canonical build/test
// command; an empty dir is unverified (""). Node prefers build over test.
func TestDetectVerify(t *testing.T) {
	cases := []struct {
		name  string
		setup func(dir string)
		want  string
	}{
		{"empty", func(string) {}, ""},
		{"node-build", func(d string) { writeFile(t, d, "package.json", `{"scripts":{"build":"ng build","test":"ng test"}}`) }, "npm run build"},
		{"node-test-only", func(d string) { writeFile(t, d, "package.json", `{"scripts":{"test":"jest"}}`) }, "npm test"},
		{"node-no-scripts", func(d string) { writeFile(t, d, "package.json", `{"name":"x"}`) }, ""},
		{"go", func(d string) { writeFile(t, d, "go.mod", "module x\n") }, "go test ./..."},
		{"rust", func(d string) { writeFile(t, d, "Cargo.toml", "[package]\n") }, "cargo build"},
		{"python-pyproject", func(d string) { writeFile(t, d, "pyproject.toml", "[project]\n") }, "python -m pytest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(dir)
			if got := detectVerify(dir); got != tc.want {
				t.Errorf("detectVerify = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveVerifyCommand: an explicit --verify wins over detection; otherwise the
// detected command is used; an unrecognised project resolves to "" (unverified).
func TestResolveVerifyCommand(t *testing.T) {
	quiet := func(string, ...any) {}

	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module x\n")

	if got := resolveVerifyCommand("make check", dir, quiet); got != "make check" {
		t.Errorf("explicit override = %q, want %q", got, "make check")
	}
	if got := resolveVerifyCommand("", dir, quiet); got != "go test ./..." {
		t.Errorf("auto-detect = %q, want %q", got, "go test ./...")
	}
	if got := resolveVerifyCommand("", t.TempDir(), quiet); got != "" {
		t.Errorf("unrecognised project should be unverified, got %q", got)
	}
}
