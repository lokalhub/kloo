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

// TestDetectLint: each recognised project signal maps to its fast lint command and
// per-file flag; an empty dir yields the zero lintCommand. Mirrors TestDetectVerify.
// The ruff-on-PATH branch is environment-dependent, so it is covered via the
// deterministic config-file signal ([tool.ruff]); flake8 is the no-ruff-signal case.
func TestDetectLint(t *testing.T) {
	cases := []struct {
		name  string
		setup func(dir string)
		want  lintCommand
	}{
		{"go", func(d string) { writeFile(t, d, "go.mod", "module x\n") }, lintCommand{"gofmt -l", true}},
		{"ts-tsconfig", func(d string) { writeFile(t, d, "tsconfig.json", "{}\n") }, lintCommand{"tsc --noEmit", false}},
		{"js-eslint", func(d string) {
			writeFile(t, d, "package.json", `{"name":"x"}`)
			writeFile(t, d, ".eslintrc.json", "{}\n")
		}, lintCommand{"eslint", true}},
		{"python-ruff", func(d string) { writeFile(t, d, "pyproject.toml", "[tool.ruff]\nline-length = 100\n") }, lintCommand{"ruff check", true}},
		{"python-flake8", func(d string) { writeFile(t, d, "setup.py", "from setuptools import setup\n") }, lintCommand{"flake8", true}},
		{"empty", func(string) {}, lintCommand{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(dir)
			if got := detectLint(dir); got != tc.want {
				t.Errorf("detectLint = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestResolveLintCommand: a disable (--no-lint) forces "" regardless of detection;
// an explicit --lint wins next and runs verbatim (per-file=false); otherwise the
// detected linter (per-file as detected); an unrecognised project is silent ("").
func TestResolveLintCommand(t *testing.T) {
	quiet := func(string, ...any) {}

	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module x\n")

	if cmd, _ := resolveLintCommand("", true, dir, quiet); cmd != "" {
		t.Errorf("disabled should yield no lint command, got %q", cmd)
	}
	if cmd, perFile := resolveLintCommand("golangci-lint run", false, dir, quiet); cmd != "golangci-lint run" || perFile {
		t.Errorf("explicit override = (%q, %v), want (%q, false)", cmd, perFile, "golangci-lint run")
	}
	if cmd, perFile := resolveLintCommand("", false, dir, quiet); cmd != "gofmt -l" || !perFile {
		t.Errorf("auto-detect = (%q, %v), want (%q, true)", cmd, perFile, "gofmt -l")
	}
	if cmd, _ := resolveLintCommand("", false, t.TempDir(), quiet); cmd != "" {
		t.Errorf("unrecognised project should be silent, got %q", cmd)
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
