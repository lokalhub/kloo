package agent

import (
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/tools"
)

func cmdCall(c string) tools.Call {
	return tools.Call{Name: tools.NameRunCommand, Args: map[string]any{"command": c}}
}

// TestSubsetTestRunIsCalledOut: measured on kloo-bench C62. The verify command
// names two test files; the model ran one of them four times, saw exit 0 each
// time, never ran the other, never edited, and the run ended "answered" with the
// case red. A green from a narrower command must never be allowed to look like
// the green that ends the task.
func TestSubsetTestRunIsCalledOut(t *testing.T) {
	t.Setenv("KLOO_VERIFY_AUTHORITY", "1")
	l := &Loop{VerifyCmd: "npx vitest run --config vitest.config.ts tests/owner-prepaid-diagnostics.test.ts tests/trial-prepaid/fences/reserve-settle-recover.spec.ts"}
	w := l.subsetTestWarning(cmdCall("npx vitest run --config vitest.config.ts tests/owner-prepaid-diagnostics.test.ts"), tools.Result{ExitCode: 0}, nil)
	if w == "" {
		t.Fatal("a subset test run was not called out")
	}
	if !strings.Contains(w, "reserve-settle-recover.spec.ts") {
		t.Fatalf("warning does not name the file that was skipped: %q", w)
	}
	if !strings.Contains(w, l.VerifyCmd) {
		t.Fatal("warning does not give the command to run instead")
	}
}

// TestFullRunIsNotCalledOut: when the model ran everything the gate covers, its
// pass is a real pass. Warning anyway would train it to ignore the warning.
func TestFullRunIsNotCalledOut(t *testing.T) {
	t.Setenv("KLOO_VERIFY_AUTHORITY", "1")
	l := &Loop{VerifyCmd: "npx vitest run --config vitest.config.ts tests/a.test.ts tests/b.test.ts"}
	if w := l.subsetTestWarning(cmdCall("npx vitest run --config vitest.config.ts tests/a.test.ts tests/b.test.ts"), tools.Result{ExitCode: 0}, nil); w != "" {
		t.Fatalf("a complete run was called out: %q", w)
	}
}

// TestUnrelatedCommandIsNotCalledOut: `ls`, `git status` and the like share no
// file with the gate and must pass through silently.
func TestUnrelatedCommandIsNotCalledOut(t *testing.T) {
	t.Setenv("KLOO_VERIFY_AUTHORITY", "1")
	l := &Loop{VerifyCmd: "npx vitest run --config vitest.config.ts tests/a.test.ts tests/b.test.ts"}
	for _, c := range []string{"ls -la", "git status", "npm run build"} {
		if w := l.subsetTestWarning(cmdCall(c), tools.Result{ExitCode: 0}, nil); w != "" {
			t.Fatalf("%q was called out: %q", c, w)
		}
	}
}

// TestFailingCommandIsNotCalledOut: the warning is about a misleading GREEN. A
// command that failed already tells the model what it needs to know.
func TestFailingCommandIsNotCalledOut(t *testing.T) {
	t.Setenv("KLOO_VERIFY_AUTHORITY", "1")
	l := &Loop{VerifyCmd: "npx vitest run --config vitest.config.ts tests/a.test.ts tests/b.test.ts"}
	if w := l.subsetTestWarning(cmdCall("npx vitest run tests/a.test.ts"), tools.Result{ExitCode: 1}, nil); w != "" {
		t.Fatalf("a failing command was called out: %q", w)
	}
}

// TestVerifyAuthorityIsOptIn.
func TestVerifyAuthorityIsOptOut(t *testing.T) {
	t.Setenv("KLOO_VERIFY_AUTHORITY", "")
	l := &Loop{VerifyCmd: "npx vitest run tests/a.test.ts tests/b.test.ts"}
	if w := l.subsetTestWarning(cmdCall("npx vitest run tests/a.test.ts"), tools.Result{ExitCode: 0}, nil); w == "" {
		t.Fatal("should be ON by default (v0.22.0)")
	}
	t.Setenv("KLOO_VERIFY_AUTHORITY", "0")
	if w := l.subsetTestWarning(cmdCall("npx vitest run tests/a.test.ts"), tools.Result{ExitCode: 0}, nil); w != "" {
		t.Fatal("KLOO_VERIFY_AUTHORITY=0 must restore the previous behaviour")
	}
}
