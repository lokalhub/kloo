package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/tools"
)

// ── The off-scope denial block in the end-of-run summary ─────────────────────
//
// `tool counters: … off_scope_edits=1` tells the operator a number. The path the
// model was refused is only in a mid-log tool observation, so a mis-scoped run —
// measured twice in one evening, each a one-flag fix — had to be diagnosed by
// reading back the whole log of a run that had already ended. Naming the paths and
// printing the flag that would have allowed them turns that into a paste.
//
// Reporting only: what is denied, and when, is unchanged.

// offScopePathsShown caps how many denied paths the summary lists. A badly
// mis-scoped run can be refused dozens; the block has to stay readable, and the
// true count is printed in the header so the cap is never a silent truncation.
const offScopePathsShown = 10

// printOffScopeWrites writes the denial block. Called only when a write was denied,
// so a clean run's summary gains nothing.
func printOffScopeWrites(out io.Writer, os *agent.OffScopeWrites) {
	fmt.Fprintf(out, "  off-scope writes denied (%d path%s):\n", os.Distinct, plural(os.Distinct))

	shown := os.Denials
	if len(shown) > offScopePathsShown {
		shown = shown[:offScopePathsShown]
	}
	// Paths a --allow can actually reach. deny and read_only BEAT allow (see
	// tools.ScopePolicy.CanWrite), so adding one of those paths to --allow would not
	// unblock it — the fix there is to drop a --deny/--read-only entry instead, and a
	// suggestion that pretended otherwise would send the operator round again.
	allowable := make([]string, 0, len(shown))
	for _, d := range shown {
		if d.Class == tools.ScopeClassOutsideAllow {
			allowable = append(allowable, d.Path)
			fmt.Fprintf(out, "    %s\n", d.Path)
			continue
		}
		// Say WHY, because "add it to --allow" is the wrong reflex for these two.
		if d.Rule != "" {
			fmt.Fprintf(out, "    %s (%s: %s)\n", d.Path, d.Class, d.Rule)
		} else {
			fmt.Fprintf(out, "    %s (%s)\n", d.Path, d.Class)
		}
	}
	if more := os.Distinct - len(shown); more > 0 {
		fmt.Fprintf(out, "    … and %d more\n", more)
	}
	if len(allowable) == 0 {
		return
	}

	// The flag EXTENDS the run's allow list. Replacing it would silently narrow the
	// next run to the files the model happened to be refused. Single-quoted as one
	// value because an unquoted `**` is expanded by the shell before kloo sees it.
	scope := append(append([]string{}, os.Allow...), allowable...)
	label := "to allow them"
	if len(allowable) < os.Distinct {
		label = fmt.Sprintf("to allow %d of the %d", len(allowable), os.Distinct)
	}
	fmt.Fprintf(out, "  %s:  --allow '%s'\n", label, strings.Join(scope, ","))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// offScopeWritesSummary mirrors agent.OffScopeWrites in KLOO_RESULT_JSON, beside
// (never instead of) the off_scope_edits counter: the counter is attempts, this is
// the distinct paths. Omitted entirely when nothing was denied, so an un-denied
// run's JSON is unchanged.
type offScopeWritesSummary struct {
	Distinct int                   `json:"distinct"`
	Allow    []string              `json:"allow,omitempty"`
	Paths    []offScopePathSummary `json:"paths"`
}

type offScopePathSummary struct {
	Path  string `json:"path"`
	Class string `json:"class"`
	Tool  string `json:"tool,omitempty"`
	Rule  string `json:"rule,omitempty"`
}

// offScopeSummary converts the report field, or nil when there is nothing to
// report. Unlike the printed block this is NOT capped at offScopePathsShown — the
// cap is a readability concern, and a harness reading the JSON wants the whole set.
func offScopeSummary(os *agent.OffScopeWrites) *offScopeWritesSummary {
	if os == nil || len(os.Denials) == 0 {
		return nil
	}
	paths := make([]offScopePathSummary, 0, len(os.Denials))
	for _, d := range os.Denials {
		paths = append(paths, offScopePathSummary{Path: d.Path, Class: d.Class, Tool: d.Tool, Rule: d.Rule})
	}
	return &offScopeWritesSummary{Distinct: os.Distinct, Allow: os.Allow, Paths: paths}
}
