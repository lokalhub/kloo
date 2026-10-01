package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func writePkg(t *testing.T, dir, scripts string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"scripts":`+scripts+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The exact names must behave EXACTLY as before — this widening is additive, and a
// repo that already detected something must not start detecting something else.
func TestNodeVerifyExactNamesUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		scripts string
		want    string
	}{
		{"build wins", `{"build":"x","test":"y","typecheck":"z"}`, "npm run build"},
		{"test when no build", `{"test":"y","typecheck":"z"}`, "npm test"},
		{"build beats a namespaced build", `{"build":"x","build:web":"y"}`, "npm run build"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePkg(t, dir, tc.scripts)
			if got := detectVerifyHere(dir); got != tc.want {
				t.Errorf("detectVerifyHere = %q, want %q", got, tc.want)
			}
		})
	}
}

// THE REGRESSION THIS FIXES. HRIS — one of the two kloo-bench repos — has neither
// an exact "build" nor an exact "test", so it detected NOTHING and ran UNVERIFIED
// on every run. Its two build:* scripts are ambiguous, so the typecheck is the gate.
func TestNodeVerifyDetectsHRISShape(t *testing.T) {
	dir := t.TempDir()
	writePkg(t, dir, `{"build:frontend":"a","build:worker":"b","dev:frontend":"c",`+
		`"test:e2e":"d","test:l1-smoke:dev":"e","typecheck":"tsc --noEmit"}`)
	got := detectVerifyHere(dir)
	if got != "npm run typecheck" {
		t.Fatalf("detectVerifyHere = %q, want npm run typecheck (HRIS detected nothing before)", got)
	}
}

// A LONE namespaced build is unambiguous and is used; SEVERAL are not guessed,
// mirroring detectVerify's existing "exactly one subdir project" rule.
func TestNodeVerifyNamespacedBuildAmbiguity(t *testing.T) {
	dir := t.TempDir()
	writePkg(t, dir, `{"build:worker":"a","typecheck":"b"}`)
	if got := detectVerifyHere(dir); got != "npm run build:worker" {
		t.Errorf("single build:* = %q, want npm run build:worker", got)
	}

	dir2 := t.TempDir()
	writePkg(t, dir2, `{"build:a":"x","build:b":"y","typecheck":"z"}`)
	if got := detectVerifyHere(dir2); got != "npm run typecheck" {
		t.Errorf("ambiguous build:* = %q, want the typecheck fallback", got)
	}
}

// test:* suites want a dev server, a database or a browser. Picking one would turn
// "unverified" into "verify fails for environmental reasons" — strictly worse,
// because the model would churn against a gate it cannot turn green.
func TestNodeVerifyNeverPicksNamespacedTest(t *testing.T) {
	dir := t.TempDir()
	writePkg(t, dir, `{"test:e2e":"playwright","test:uat":"x","dev":"y"}`)
	if got := detectVerifyHere(dir); got != "" {
		t.Fatalf("detectVerifyHere = %q, want \"\" — a test:* suite must not become the gate", got)
	}
}

func TestNodeVerifyTypecheckAliases(t *testing.T) {
	for _, name := range typecheckScriptNames {
		dir := t.TempDir()
		writePkg(t, dir, `{"`+name+`":"x","dev":"y"}`)
		if got := detectVerifyHere(dir); got != "npm run "+name {
			t.Errorf("%s: detectVerifyHere = %q, want npm run %s", name, got, name)
		}
	}
}

// Nothing usable stays unverified rather than inventing a gate.
func TestNodeVerifyNoUsableScript(t *testing.T) {
	dir := t.TempDir()
	writePkg(t, dir, `{"dev":"vite","start":"node ."}`)
	if got := detectVerifyHere(dir); got != "" {
		t.Fatalf("detectVerifyHere = %q, want \"\"", got)
	}
}
