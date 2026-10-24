package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
)

// TestOffScopeWritesNamedInSummary: when the scope policy refused a write, the
// end-of-run summary NAMES the paths and offers a --allow that extends the run's
// existing allow list.
//
// off_scope_edits=1 is a number, not a path. The two mis-scoped runs that motivated
// this were both one-flag fixes, and in both the flag had to be reconstructed from
// mid-log. The suggestion must be paste-able as printed: it carries the existing
// globs, and it is single-quoted because an unquoted `**` is expanded by the shell
// before kloo ever sees it.
func TestOffScopeWritesNamedInSummary(t *testing.T) {
	rep := &agent.Report{
		Reason:       agent.ReasonBudgetExceeded,
		ToolCounters: agent.ToolCounters{OffScopeEdits: 3},
		OffScope: &agent.OffScopeWrites{
			Distinct: 2,
			Allow:    []string{"ui/src/app/**", "web/src/app/**"},
			Denials: []agent.ScopeDenial{
				{Class: "outside_allow", Tool: "edit_file", Path: "ui/src/components/lifecycle-bar/lifecycle-bar.component.html"},
				{Class: "outside_allow", Tool: "write_file", Path: "web/src/app/pages/terminal/initiative-events-logic.ts"},
			},
		},
	}
	var buf bytes.Buffer
	printHeadlessReport(&buf, rep, time.Second)
	got := buf.String()

	for _, want := range []string{
		"  off-scope writes denied (2 paths):\n",
		"    ui/src/components/lifecycle-bar/lifecycle-bar.component.html\n",
		"    web/src/app/pages/terminal/initiative-events-logic.ts\n",
		"  to allow them:  --allow 'ui/src/app/**,web/src/app/**," +
			"ui/src/components/lifecycle-bar/lifecycle-bar.component.html," +
			"web/src/app/pages/terminal/initiative-events-logic.ts'\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
	// The counter stays: it is the attempt count, and the path list is the distinct count.
	if !strings.Contains(got, "off_scope_edits=3") {
		t.Fatalf("the existing counter was dropped:\n%s", got)
	}
}

// TestOffScopeSummarySilentWithoutDenials: no denial, no output. The common case
// must not gain a line.
func TestOffScopeSummarySilentWithoutDenials(t *testing.T) {
	var buf bytes.Buffer
	printHeadlessReport(&buf, &agent.Report{Reason: agent.ReasonSuccess}, time.Second)
	if got := buf.String(); strings.Contains(got, "off-scope") || strings.Contains(got, "to allow them") {
		t.Fatalf("a clean run's summary mentions off-scope writes:\n%s", got)
	}
}

// TestOffScopeSummaryCapsTheList: a badly mis-scoped run can be denied dozens of
// paths. Cap the list, but SAY what was capped — a silent truncation produces a
// --allow that looks complete and is not.
func TestOffScopeSummaryCapsTheList(t *testing.T) {
	denials := make([]agent.ScopeDenial, 0, 14)
	for i := range 14 {
		denials = append(denials, agent.ScopeDenial{
			Class: "outside_allow", Tool: "edit_file",
			Path: string(rune('a'+i)) + "/x.ts",
		})
	}
	rep := &agent.Report{
		Reason:       agent.ReasonBudgetExceeded,
		ToolCounters: agent.ToolCounters{OffScopeEdits: 14},
		OffScope:     &agent.OffScopeWrites{Distinct: 14, Allow: []string{"keep/**"}, Denials: denials},
	}
	var buf bytes.Buffer
	printHeadlessReport(&buf, rep, time.Second)
	got := buf.String()

	if !strings.Contains(got, "  off-scope writes denied (14 paths):\n") {
		t.Fatalf("header must report the true count:\n%s", got)
	}
	if n := strings.Count(got, "/x.ts\n"); n != offScopePathsShown {
		t.Fatalf("listed %d paths on their own line, want %d (the cap)\n%s", n, offScopePathsShown, got)
	}
	if !strings.Contains(got, "    … and 4 more\n") {
		t.Fatalf("the truncation is silent:\n%s", got)
	}
	// The suggestion covers only what was listed, and says so rather than implying
	// it is the whole fix. "10 of the 14" and not "first 10": with a mixed denial set
	// the paths a --allow can reach are not necessarily the first ones.
	if !strings.Contains(got, "  to allow 10 of the 14:  --allow 'keep/**,a/x.ts,") {
		t.Fatalf("the capped suggestion does not admit it is partial:\n%s", got)
	}
}

// TestOffScopeSuggestionOmittedForDenyAndReadOnly: deny and read_only beat allow,
// so adding the path to --allow would NOT unblock it. Name the path (with why), and
// offer no flag that cannot work.
func TestOffScopeSuggestionOmittedForDenyAndReadOnly(t *testing.T) {
	rep := &agent.Report{
		Reason:       agent.ReasonBudgetExceeded,
		ToolCounters: agent.ToolCounters{OffScopeEdits: 2},
		OffScope: &agent.OffScopeWrites{
			Distinct: 2,
			Allow:    []string{"src/**"},
			Denials: []agent.ScopeDenial{
				{Class: "deny", Tool: "edit_file", Path: "src/generated/api.ts", Rule: "src/generated/**"},
				{Class: "read_only", Tool: "write_file", Path: "src/schema.sql", Rule: "src/*.sql"},
			},
		},
	}
	var buf bytes.Buffer
	printHeadlessReport(&buf, rep, time.Second)
	got := buf.String()

	for _, want := range []string{
		"    src/generated/api.ts (deny: src/generated/**)\n",
		"    src/schema.sql (read_only: src/*.sql)\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "to allow them") {
		t.Fatalf("suggested --allow for paths that deny/read_only would still refuse:\n%s", got)
	}
}

// TestOffScopePathsInJSON: the denied paths surface in KLOO_RESULT_JSON beside the
// existing off_scope_edits counter, so a harness can read them without parsing the
// human summary.
func TestOffScopePathsInJSON(t *testing.T) {
	cfg := config.Config{Model: "m", Endpoint: "http://x/v1"}
	rep := &agent.Report{
		Reason:       agent.ReasonBudgetExceeded,
		ToolCounters: agent.ToolCounters{OffScopeEdits: 3},
		OffScope: &agent.OffScopeWrites{
			Distinct: 2,
			Allow:    []string{"ui/src/app/**"},
			Denials: []agent.ScopeDenial{
				{Class: "outside_allow", Tool: "edit_file", Path: "ui/a.html"},
				{Class: "read_only", Tool: "write_file", Path: "ui/b.ts", Rule: "**/*.ts"},
			},
		},
	}
	var buf bytes.Buffer
	printHeadlessJSON(&buf, cfg, "", rep, time.Second, nil)
	line := strings.TrimPrefix(strings.TrimSpace(buf.String()), "KLOO_RESULT_JSON ")
	var s map[string]any
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if s["off_scope_edits"].(float64) != 3 {
		t.Fatalf("the existing counter changed: %v", s["off_scope_edits"])
	}
	block, _ := s["off_scope_writes"].(map[string]any)
	if block == nil {
		t.Fatalf("off_scope_writes missing from the JSON: %v", s)
	}
	if block["distinct"].(float64) != 2 {
		t.Fatalf("distinct = %v, want 2", block["distinct"])
	}
	allow, _ := block["allow"].([]any)
	if len(allow) != 1 || allow[0] != "ui/src/app/**" {
		t.Fatalf("allow = %v, want the run's allow globs", block["allow"])
	}
	paths, _ := block["paths"].([]any)
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want 2 entries", block["paths"])
	}
	first, _ := paths[0].(map[string]any)
	if first["path"] != "ui/a.html" || first["class"] != "outside_allow" {
		t.Fatalf("paths[0] = %v", paths[0])
	}
	second, _ := paths[1].(map[string]any)
	if second["path"] != "ui/b.ts" || second["class"] != "read_only" || second["rule"] != "**/*.ts" {
		t.Fatalf("paths[1] = %v", paths[1])
	}
}

// TestOffScopeJSONAbsentWithoutDenials: a run with no denial emits no
// off_scope_writes key, so an un-denied run's JSON is byte-identical to before.
func TestOffScopeJSONAbsentWithoutDenials(t *testing.T) {
	cfg := config.Config{Model: "m", Endpoint: "http://x/v1"}
	var buf bytes.Buffer
	printHeadlessJSON(&buf, cfg, "", &agent.Report{Reason: agent.ReasonSuccess}, time.Second, nil)
	if strings.Contains(buf.String(), "off_scope_writes") {
		t.Fatalf("off_scope_writes present on a run with no denial:\n%s", buf.String())
	}
}
