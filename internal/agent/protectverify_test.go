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
	// The config file is named in the command too and is not a place to fix code,
	// but it IS named, so protecting it is correct and harmless.
	if !l.protectedByVerify("vitest.config.ts") {
		t.Fatal("a named config file was not protected")
	}
}

// TestProtectionIsOptIn: a task may legitimately ask for a change to a file its
// own verify command names, so the released behaviour must be unchanged.
func TestProtectionIsOptIn(t *testing.T) {
	t.Setenv("KLOO_PROTECT_VERIFY_PATHS", "")
	l := &Loop{VerifyCmd: "go test ./foo_test.go"}
	if l.protectedByVerify("foo_test.go") {
		t.Fatal("protection active with the flag off")
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
