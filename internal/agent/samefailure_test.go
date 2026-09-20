package agent

import (
	"strings"
	"testing"
)

// TestRepeatedFailureNamesOtherFiles: measured on kloo-bench A16 and A33 — both
// lost the same way. kloo kept editing the file it was already in (A16:
// service.ts five times, two identical, three no-ops) while the change the test
// needed lived elsewhere: a SQL join in A16, a resolver in A33. grok passed both
// with a three-file change. kloo ALREADY detected the repeat and its advice was
// "try a DIFFERENT fix" — the wrong axis. What is needed is a different FILE.
func TestRepeatedFailureNamesOtherFiles(t *testing.T) {
	v := VerifyResult{Command: "vitest", Passed: false, Stdout: "× reassigned receipt exports the new branch code"}
	msg, ok := verifyPin(v, true, []string{"src/modules/export/queries.ts", "src/modules/export/types.ts"})
	if !ok {
		t.Fatal("no verify pin produced")
	}
	if !strings.Contains(msg.Content, "src/modules/export/queries.ts") {
		t.Fatalf("pin does not name the other exercised files: %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "probably NOT in that file") {
		t.Fatalf("pin does not redirect off the stuck file: %q", msg.Content)
	}
}

// TestFirstFailureDoesNotRedirect: the redirect is for a REPEATED failure. Firing
// it on the first red verify would push the model off a file before it has even
// tried, which is the opposite of the desired behaviour.
func TestFirstFailureDoesNotRedirect(t *testing.T) {
	v := VerifyResult{Command: "vitest", Passed: false, Stdout: "× something"}
	msg, _ := verifyPin(v, false, []string{"src/a.ts"})
	if strings.Contains(msg.Content, "probably NOT in that file") {
		t.Fatalf("redirected on a first failure: %q", msg.Content)
	}
}

// TestPassingVerifyNeverRedirects.
func TestPassingVerifyNeverRedirects(t *testing.T) {
	v := VerifyResult{Command: "vitest", Passed: true}
	msg, _ := verifyPin(v, true, []string{"src/a.ts"})
	if strings.Contains(msg.Content, "probably NOT in that file") {
		t.Fatalf("redirected on a green verify: %q", msg.Content)
	}
}
