package agent

import (
	"strings"
	"testing"
)

// The escalation must be OFF unless asked for: it lets a run continue past a rail
// that currently stops it, so shipping it on would change every user's runs before
// it has been measured.
func TestChurnEscalateDefaultsOff(t *testing.T) {
	if churnEscalate() {
		t.Fatal("KLOO_CHURN_ESCALATE must default OFF")
	}
	t.Setenv("KLOO_CHURN_ESCALATE", "1")
	if !churnEscalate() {
		t.Fatal("KLOO_CHURN_ESCALATE=1 must enable escalation")
	}
}

func TestChurnEditTarget(t *testing.T) {
	cases := []struct{ in, want string }{
		{"edit_file src/modules/export/service.ts\n<<<<<<< SEARCH\nfoo\n", "src/modules/export/service.ts"},
		{"write_file src/a.ts\nbody", "src/a.ts"},
		// A repeated FAILURE artifact is verify output, not an edit — it must not be
		// mistaken for a path, or the escalation would close an arbitrary file.
		{"FAIL tests/export-branch-code.test.ts > exports branch code", ""},
		{"", ""},
		{"edit_file", ""},
	}
	for _, c := range cases {
		if got := churnEditTarget(c.in); got != c.want {
			t.Errorf("churnEditTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The corrective must NAME the closed file and the files already changed: the
// failure it exists for is a model that edited two of three files and cannot see
// which one it is missing.
func TestChurnEscalationCorrectiveNamesFiles(t *testing.T) {
	m := churnEscalationCorrective("src/export/service.ts", map[string]bool{
		"src/export/service.ts":       true,
		"src/export/field-catalog.ts": true,
	})
	for _, want := range []string{"src/export/service.ts", "src/export/field-catalog.ts", "CLOSED"} {
		if !strings.Contains(m.Content, want) {
			t.Errorf("corrective missing %q:\n%s", want, m.Content)
		}
	}
	if strings.Count(m.Content, "src/export/service.ts") != 1 {
		t.Error("the closed file must not also be listed as an already-changed file")
	}
}
