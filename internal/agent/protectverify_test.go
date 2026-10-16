package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// touch creates each path inside a fresh temp root and returns the root. The guard
// reads the filesystem now, so a test about WHICH files are protected has to put
// them somewhere.
func touch(t *testing.T, paths ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range paths {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestVerifyNamedFilesAreProtected: the files the verify command names are the
// specification the run is graded against. Measured on kloo-bench C30: the model
// edited undertime-overbreak-grace.test.ts three times, every one a no-op, never
// touched a source file, and churned to a repetition halt with the case red.
func TestVerifyNamedFilesAreProtected(t *testing.T) {
	t.Setenv("KLOO_PROTECT_VERIFY_PATHS", "1")
	// The guard only protects a spec that EXISTS (see
	// TestNonexistentVerifyNamedTestIsNotProtected), so these have to be on disk.
	root := touch(t,
		"src/modules-v2/payroll/__tests__/undertime-overbreak-grace.test.ts",
		"worker/src/modules-v2/payroll/__tests__/undertime-overbreak-grace.test.ts",
		"src/modules-v2/payroll/payroll-engine.ts",
		"vitest.config.ts",
	)
	l := &Loop{Root: root, VerifyCmd: "npx vitest run --config vitest.config.ts src/modules-v2/payroll/__tests__/undertime-overbreak-grace.test.ts"}

	// The edit path and the verify path differ by a leading component here (the
	// verify command is relative to the gate's cwd), so matching must not be a
	// plain string compare on the full path.
	if !l.protectedByVerify("src/modules-v2/payroll/__tests__/undertime-overbreak-grace.test.ts") {
		t.Fatal("the verify-named test file was not protected")
	}
	if !l.protectedByVerify("worker/src/modules-v2/payroll/__tests__/undertime-overbreak-grace.test.ts") {
		t.Fatal("protection failed across the cwd prefix difference")
	}
	if l.protectedByVerify("src/modules-v2/payroll/payroll-engine.ts") {
		t.Fatal("a source file was protected — the run could never make progress")
	}
	// A config file is NOT a test, and protecting it was over-reach: the guard must
	// only shield the spec being graded against.
	if l.protectedByVerify("vitest.config.ts") {
		t.Fatal("a config file was protected — it is not a spec")
	}
}

// TestProtectionIsOptIn: a task may legitimately ask for a change to a file its
// own verify command names, so the released behaviour must be unchanged.
func TestProtectionIsOptOut(t *testing.T) {
	t.Setenv("KLOO_PROTECT_VERIFY_PATHS", "")
	l := &Loop{Root: touch(t, "foo_test.go"), VerifyCmd: "go test ./foo_test.go"}
	if !l.protectedByVerify("foo_test.go") {
		t.Fatal("protection should be ON by default (v0.22.0)")
	}
	t.Setenv("KLOO_PROTECT_VERIFY_PATHS", "0")
	if l.protectedByVerify("foo_test.go") {
		t.Fatal("KLOO_PROTECT_VERIFY_PATHS=0 must restore the previous behaviour")
	}
}

// TestFlagsAndSwitchesAreNotPaths: the verify command is full of them, and
// treating one as a file name would protect something arbitrary.
func TestFlagsAndSwitchesAreNotPaths(t *testing.T) {
	t.Setenv("KLOO_PROTECT_VERIFY_PATHS", "1")
	l := &Loop{Root: touch(t, "tests/a.test.ts"), VerifyCmd: "npx vitest run --config vitest.config.ts tests/a.test.ts"}
	for _, notAPath := range []string{"run", "--config", "npx", "vitest"} {
		if l.protectedByVerify(notAPath) {
			t.Fatalf("%q was treated as a protected path", notAPath)
		}
	}
}

// TestDeliverableNamedByVerifyIsNotProtected: the bug this guard shipped with.
// kloo's own suite (TestHeadlessWiresMCPNonFatal and four others) churned to a
// halt with EVERY write_file refused, because the verify command was
// `grep -qx right answer.txt` and the guard matched answer.txt — the very file the
// task had to create. A verify command naming a file does not make that file a
// spec.
func TestDeliverableNamedByVerifyIsNotProtected(t *testing.T) {
	t.Setenv("KLOO_PROTECT_VERIFY_PATHS", "1")
	root := touch(t, "answer.txt", "README.md", "src/main.go", "dist/out.js", "vitest.config.ts")
	l := &Loop{Root: root, VerifyCmd: "grep -qx right answer.txt"}
	if l.protectedByVerify("answer.txt") {
		t.Fatal("the deliverable was protected — the run can never succeed")
	}
	for _, notATest := range []string{"README.md", "src/main.go", "dist/out.js", "vitest.config.ts"} {
		if l.protectedByVerify(notATest) {
			t.Fatalf("%q was protected", notATest)
		}
	}
}

// TestTestFileShapesRecognised: the shapes that ARE specs, across ecosystems.
func TestTestFileShapesRecognised(t *testing.T) {
	for _, p := range []string{
		"tests/a.test.ts", "src/x.spec.ts", "pkg/thing_test.go",
		"app/__tests__/y.ts", "spec/z.rb", "test/legacy.js", "tests/deep/nested.test.tsx",
	} {
		if !looksLikeTestFile(p) {
			t.Fatalf("%q not recognised as a test file", p)
		}
	}
	for _, p := range []string{"answer.txt", "src/service.ts", "vitest.config.ts", "latest.json"} {
		if looksLikeTestFile(p) {
			t.Fatalf("%q wrongly recognised as a test file", p)
		}
	}
}

// TestNonexistentVerifyNamedTestIsNotProtected: the live failure this check was
// added for. Asked to CREATE worker/tests/login-with-google.test.ts — a resolver
// that had no test at all, which is how "GOOGLE_CLIENT_SECRET not configured"
// reached a deployed host — kloo refused its own first write_file with
// "file is part of the verify command", because the verify command naturally
// names the test the task exists to write.
//
// A file that does not exist cannot be the specification the run is graded
// against; it is the deliverable. The guard protects the spec from being
// weakened, so it has nothing to protect until the file is there.
func TestNonexistentVerifyNamedTestIsNotProtected(t *testing.T) {
	t.Setenv("KLOO_PROTECT_VERIFY_PATHS", "1")
	root := t.TempDir()
	l := &Loop{Root: root, VerifyCmd: "cd worker && npx vitest run tests/login-with-google.test.ts"}

	const rel = "worker/tests/login-with-google.test.ts"
	if l.protectedByVerify(rel) {
		t.Fatal("a test file that does not exist yet was protected — the task can never be done")
	}

	// Once it EXISTS it is the spec again, and weakening it is the thing the guard
	// is for. This is the half that must not regress.
	if err := os.MkdirAll(filepath.Join(root, "worker", "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte("it('x', () => {})\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !l.protectedByVerify(rel) {
		t.Fatal("an EXISTING verify-named test was not protected — the guard is gone")
	}
}

// TestTestFileThisRunCreatedStaysEditable: creating the spec is not enough — the
// run has to be able to ITERATE on it. The live sequence: kloo authored
// worker/tests/login-with-google.test.ts, the first vitest run failed on an
// incomplete D1 stub, and the fix belonged in the very file it had just written.
// If the guard re-arms the moment the file lands, the task is unfinishable.
//
// A file this run created is its deliverable for the whole run. A file that was
// already there when the run started is the spec, and stays protected.
func TestTestFileThisRunCreatedStaysEditable(t *testing.T) {
	t.Setenv("KLOO_PROTECT_VERIFY_PATHS", "1")
	root := t.TempDir()
	l := &Loop{Root: root, VerifyCmd: "npx vitest run worker/tests/login-with-google.test.ts"}

	const rel = "worker/tests/login-with-google.test.ts"
	if err := os.MkdirAll(filepath.Join(root, "worker", "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, rel), []byte("it('x', () => {})\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// It exists, and the run did not create it → protected.
	if !l.protectedByVerify(rel) {
		t.Fatal("a pre-existing verify-named test must stay protected")
	}
	// Same file, but this run authored it → its own deliverable, still editable.
	l.noteCreated(rel)
	if l.protectedByVerify(rel) {
		t.Fatal("the run cannot edit the test it just wrote — the task is unfinishable")
	}
}
