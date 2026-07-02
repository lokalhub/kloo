package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestGlobMatch covers the documented glob semantics: "**" across segments (with a
// trailing "/**" matching the bare dir), single-segment "*"/"?", and literals.
func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"src/**", "src/a.go", true},
		{"src/**", "src/x/y/z.go", true},
		{"src/**", "src", true},            // trailing /** matches the bare dir
		{"src/**", "srcextra/a.go", false}, // must be under src/, not a prefix
		{"src/**", "lib/a.go", false},
		{".env", ".env", true},
		{".env", "config/.env", false}, // exact, not recursive
		{"**/.env", "config/.env", true},
		{"**/.env", ".env", true},
		{"dist/**", "dist/bundle.js", true},
		{"dist/**", "dist", true},
		{"*.go", "main.go", true},
		{"*.go", "sub/main.go", false}, // * does not cross a separator
		{"tests/**", "tests/login_test.go", true},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/pkg/main.go", false},
		{"src/**/x.go", "src/x.go", true},
		{"src/**/x.go", "src/a/b/x.go", true},
		{"a?c.txt", "abc.txt", true},
		{"a?c.txt", "ac.txt", false},
		{"**", "anything/at/all.txt", true},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"__"+tc.path, func(t *testing.T) {
			re, err := compileGlob(NormalizeScopePath(tc.pattern))
			if err != nil {
				t.Fatalf("compileGlob(%q): %v", tc.pattern, err)
			}
			if got := re.MatchString(NormalizeScopePath(tc.path)); got != tc.want {
				t.Fatalf("match(%q,%q) = %v, want %v (re=%s)", tc.pattern, tc.path, got, tc.want, re.String())
			}
		})
	}
}

// TestScopePolicyCanWrite exercises the full decision matrix + precedence
// (deny > read_only > outside_allow), the empty-allow "all in-jail" rule, and path
// normalization.
func TestScopePolicyCanWrite(t *testing.T) {
	cases := []struct {
		name      string
		allow     []string
		deny      []string
		readOnly  []string
		path      string
		wantAllow bool
		wantClass string
	}{
		{name: "allow-only in scope", allow: []string{"src/**"}, path: "src/a.go", wantAllow: true},
		{name: "allow-only outside", allow: []string{"src/**"}, path: "README.md", wantAllow: false, wantClass: ScopeClassOutsideAllow},
		{name: "deny-only hit", deny: []string{".env"}, path: ".env", wantAllow: false, wantClass: ScopeClassDeny},
		{name: "deny-only miss", deny: []string{".env"}, path: "src/a.go", wantAllow: true},
		{name: "empty allow = all in jail", deny: []string{"dist/**"}, path: "anything.go", wantAllow: true},
		{name: "allow+deny: deny wins", allow: []string{"src/**"}, deny: []string{"src/secret.go"}, path: "src/secret.go", wantAllow: false, wantClass: ScopeClassDeny},
		{name: "read_only wins over allow", allow: []string{"tests/**"}, readOnly: []string{"tests/**"}, path: "tests/login_test.go", wantAllow: false, wantClass: ScopeClassReadOnly},
		{name: "deny wins over read_only", deny: []string{"secret/**"}, readOnly: []string{"secret/**"}, path: "secret/x", wantAllow: false, wantClass: ScopeClassDeny},
		{name: "normalization ./ prefix", allow: []string{"src/**"}, path: "./src/a.go", wantAllow: true},
		{name: "read_only outside allow reports read_only first", allow: []string{"src/**"}, readOnly: []string{"go.mod"}, path: "go.mod", wantAllow: false, wantClass: ScopeClassReadOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewScopePolicy(tc.allow, tc.deny, tc.readOnly)
			if err != nil {
				t.Fatalf("NewScopePolicy: %v", err)
			}
			d := p.CanWrite(tc.path)
			if d.Allowed != tc.wantAllow {
				t.Fatalf("CanWrite(%q).Allowed = %v, want %v (class=%q)", tc.path, d.Allowed, tc.wantAllow, d.Class)
			}
			if !tc.wantAllow && d.Class != tc.wantClass {
				t.Fatalf("CanWrite(%q).Class = %q, want %q", tc.path, d.Class, tc.wantClass)
			}
		})
	}
}

// TestNewScopePolicyEmptyIsNil: an all-empty policy is "no policy" (nil), so the
// workspace jail remains the only boundary and Active() is false.
func TestNewScopePolicyEmptyIsNil(t *testing.T) {
	p, err := NewScopePolicy(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatalf("expected nil policy for empty lists, got %+v", p)
	}
	if p.Active() {
		t.Fatal("nil policy must not be Active")
	}
	// Blank/whitespace patterns collapse to no policy too.
	p2, err := NewScopePolicy([]string{"  ", ""}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p2 != nil {
		t.Fatalf("expected nil policy for blank patterns, got %+v", p2)
	}
}

// TestWriteFileScopeDenied: a denied write_file fails BEFORE touching disk — the
// file (if any) is byte-identical, no new file is created — and the error is a
// *ScopeError wrapping ErrOffScope with the expected class.
func TestWriteFileScopeDenied(t *testing.T) {
	ws, root := wsAt(t)
	policy, _ := NewScopePolicy([]string{"src/**"}, nil, nil)
	ws = ws.WithScope(policy)

	// Outside allow: refused, no file created.
	err := WriteFile(ws, "README.md", "hacked")
	if !errors.Is(err, ErrOffScope) {
		t.Fatalf("WriteFile off-scope: err = %v, want ErrOffScope", err)
	}
	var se *ScopeError
	if !errors.As(err, &se) || se.Class != ScopeClassOutsideAllow {
		t.Fatalf("want *ScopeError class=outside_allow, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "README.md")); !os.IsNotExist(statErr) {
		t.Fatal("README.md must not have been created by a denied write")
	}

	// In-scope: allowed.
	if err := WriteFile(ws, "src/a.go", "package a\n"); err != nil {
		t.Fatalf("in-scope write should succeed: %v", err)
	}
}

// TestEditFileReadOnlyDenied: a read-only file can be read but edit_file fails and
// leaves the bytes unchanged, with class read_only.
func TestEditFileReadOnlyDenied(t *testing.T) {
	ws, root := wsAt(t)
	const orig = "package a\nfunc A() {}\n"
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, _ := NewScopePolicy(nil, nil, []string{"a.go"})
	ws = ws.WithScope(policy)

	// Read still works.
	if got, err := ReadFile(ws, "a.go"); err != nil || got != orig {
		t.Fatalf("ReadFile on read-only file: got %q err %v", got, err)
	}
	// Edit refused, bytes unchanged.
	diff := "a.go\n<<<<<<< SEARCH\nfunc A() {}\n=======\nfunc A() { panic(0) }\n>>>>>>> REPLACE\n"
	err := EditFile(ws, "a.go", diff)
	var se *ScopeError
	if !errors.As(err, &se) || se.Class != ScopeClassReadOnly {
		t.Fatalf("EditFile read-only: want *ScopeError class=read_only, got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if string(after) != orig {
		t.Fatalf("read-only file mutated: %q", string(after))
	}
}

// TestScopeDoesNotWeakenJail: a path that escapes the workspace still returns
// ErrPathEscape even with a scope policy attached (scope is a second layer inside
// the jail, never a replacement).
func TestScopeDoesNotWeakenJail(t *testing.T) {
	ws, _ := wsAt(t)
	policy, _ := NewScopePolicy([]string{"**"}, nil, nil) // allow everything in-jail
	ws = ws.WithScope(policy)
	if err := WriteFile(ws, "../escape.txt", "x"); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("WriteFile escape: err = %v, want ErrPathEscape", err)
	}
}

// TestScopedRegistryWithholdsRunCommand: when the workspace withholds the shell,
// run_command is NOT advertised in Tools(), yet a fallback-adapter run_command call
// is still rejected pre-exec with a *ScopeError, and the file it tried to mutate is
// byte-identical (no process ran).
func TestScopedRegistryWithholdsRunCommand(t *testing.T) {
	ws, root := wsAt(t)
	const orig = "keep me\n"
	target := filepath.Join(root, "README.md")
	if err := os.WriteFile(target, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, _ := NewScopePolicy([]string{"src/**"}, nil, nil)
	reg := DefaultRegistry(ws.WithScope(policy))

	for _, tl := range reg.Tools() {
		if tl.Name() == NameRunCommand {
			t.Fatal("run_command must not be advertised while scope is active")
		}
	}

	// A fallback adapter emitting run_command anyway is rejected before execution.
	_, err := reg.Dispatch(context.Background(), Call{Name: NameRunCommand, Args: map[string]any{"command": "sed -i s/keep/gone/ README.md"}})
	var se *ScopeError
	if !errors.As(err, &se) || se.Class != ScopeClassRunCommandDisabled {
		t.Fatalf("scoped run_command dispatch: want *ScopeError class=run_command_disabled_for_scope, got %v", err)
	}
	if !errors.Is(err, ErrOffScope) {
		t.Fatalf("scoped run_command error must wrap ErrOffScope, got %v", err)
	}
	if after, _ := os.ReadFile(target); string(after) != orig {
		t.Fatalf("README.md mutated by a rejected run_command: %q", string(after))
	}
}

// TestPatchOnlyRegistryWithholdsRunCommand: patch-only mode (no scope) also
// withholds run_command; a fallback call is rejected with ErrPatchOnlyForbidden
// (NOT a scope error).
func TestPatchOnlyRegistryWithholdsRunCommand(t *testing.T) {
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws.WithPatchOnly(true))
	for _, tl := range reg.Tools() {
		if tl.Name() == NameRunCommand {
			t.Fatal("run_command must not be advertised in patch-only mode")
		}
	}
	_, err := reg.Dispatch(context.Background(), Call{Name: NameRunCommand, Args: map[string]any{"command": "echo hi"}})
	if !errors.Is(err, ErrPatchOnlyForbidden) {
		t.Fatalf("patch-only run_command: err = %v, want ErrPatchOnlyForbidden", err)
	}
	var se *ScopeError
	if errors.As(err, &se) {
		t.Fatalf("patch-only rejection must NOT be a scope error, got %v", err)
	}
}

// TestUnscopedRegistryExposesRunCommand: with no scope and no patch-only, the
// registry advertises run_command exactly as before (byte-identical behaviour).
func TestUnscopedRegistryExposesRunCommand(t *testing.T) {
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws)
	found := false
	for _, tl := range reg.Tools() {
		if tl.Name() == NameRunCommand {
			found = true
		}
	}
	if !found {
		t.Fatal("unscoped registry must expose run_command")
	}
}
