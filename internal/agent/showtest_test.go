package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestFailingTestSourceIsShown: measured on kloo-bench C17 — kloo failed 3 of 4
// runs with ZERO edits, searching 17-30 times without writing a line, because the
// real fix is a VAT accounting rule that cannot be derived from the broken source.
// The test states the rule; the brief never shows it.
func TestFailingTestSourceIsShown(t *testing.T) {
	t.Setenv("KLOO_SHOW_FAILING_TEST", "1")
	root := testRoot(t, map[string]string{
		"tests/recon.test.ts": "it('exempt discount is subtracted', () => { expect(x).toBe(1) })",
		"src/recon.ts":        "export const x = 0",
	})
	l := &Loop{Root: root, VerifyCmd: "npx vitest run --config vitest.config.ts tests/recon.test.ts"}
	got := l.failingTestSource()
	if !strings.Contains(got, "exempt discount is subtracted") {
		t.Fatalf("test body not shown: %q", got)
	}
	if !strings.Contains(got, "Do NOT edit it") {
		t.Fatal("injection does not warn against editing the spec")
	}
}

// TestOnlyTestFilesAreShown: the verify command names a config file and a runner;
// neither is a spec, and dumping them wastes the window.
func TestOnlyTestFilesAreShown(t *testing.T) {
	t.Setenv("KLOO_SHOW_FAILING_TEST", "1")
	root := testRoot(t, map[string]string{
		"vitest.config.ts": "export default {}",
		"tests/a.test.ts":  "it('a', () => {})",
	})
	l := &Loop{Root: root, VerifyCmd: "npx vitest run --config vitest.config.ts tests/a.test.ts"}
	got := l.failingTestSource()
	if strings.Contains(got, "export default {}") {
		t.Fatal("config file was injected")
	}
	if !strings.Contains(got, "it('a'") {
		t.Fatal("the actual test was not injected")
	}
}

// TestShowFailingTestIsOptIn.
func TestShowFailingTestIsOptIn(t *testing.T) {
	t.Setenv("KLOO_SHOW_FAILING_TEST", "")
	root := testRoot(t, map[string]string{"tests/a.test.ts": "it('a', () => {})"})
	l := &Loop{Root: root, VerifyCmd: "npx vitest run tests/a.test.ts"}
	if l.failingTestSource() != "" {
		t.Fatal("active with the flag off")
	}
}

// TestLargeTestIsBounded: a 93-assertion suite must not swallow the window.
func TestLargeTestIsBounded(t *testing.T) {
	t.Setenv("KLOO_SHOW_FAILING_TEST", "1")
	big := strings.Repeat("it('x', () => {})\n", showTestMaxLines*3)
	root := testRoot(t, map[string]string{"tests/big.test.ts": big})
	l := &Loop{Root: root, VerifyCmd: "npx vitest run tests/big.test.ts"}
	got := l.failingTestSource()
	if strings.Count(got, "\n") > showTestMaxLines+10 {
		t.Fatalf("injection not bounded: %d lines", strings.Count(got, "\n"))
	}
	if !strings.Contains(got, "more lines") {
		t.Fatal("truncation is silent")
	}
}
