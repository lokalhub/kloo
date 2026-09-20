package agent

import "testing"

// TestVerifyNamedFilesAreProtected: the files the verify command names are the
// specification the run is graded against. Measured on kloo-bench C30: the model
// edited undertime-overbreak-grace.test.ts three times, every one a no-op, never
// touched a source file, and churned to a repetition halt with the case red.
func TestVerifyNamedFilesAreProtected(t *testing.T) {
	t.Setenv("KLOO_PROTECT_VERIFY_PATHS", "1")
	l := &Loop{VerifyCmd: "npx vitest run --config vitest.config.ts src/modules-v2/payroll/__tests__/undertime-overbreak-grace.test.ts"}

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
	l := &Loop{VerifyCmd: "go test ./foo_test.go"}
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
	l := &Loop{VerifyCmd: "npx vitest run --config vitest.config.ts tests/a.test.ts"}
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
	l := &Loop{VerifyCmd: "grep -qx right answer.txt"}
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
