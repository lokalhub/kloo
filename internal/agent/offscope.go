package agent

import "github.com/lokalhub/kloo/internal/tools"

// ── Naming the writes the scope refused ──────────────────────────────────────
//
// A scoped run that is mis-scoped reports `off_scope_edits=1` and nothing else.
// That is a number, not a path: the file the model was reaching for is only in the
// mid-log tool observation, so recovering it means reading the whole log of a run
// that has already ended. Measured twice in one evening on the same codebase — once
// with an Angular component's .ts allowed and its .html left out, once with a
// sibling logic module left out. Both were one-flag fixes; one run was killed and
// relaunched, the other burned steps.
//
// This is the reporting half of the failure class baseline.go describes: there, a
// corrective could order an edit the policy would reject; here, the operator who
// could have widened the scope in one flag could not see which flag. Nothing about
// WHAT is denied changes — only whether the run says so on its way out.

// maxOffScopeDenials bounds what the report retains. Paths are bounded in length
// but a long mis-scoped run can reach for many of them, and the report is held for
// the whole run; OffScopeWrites.Distinct carries the true count so a capped report
// still states the size of the problem rather than understating it.
const maxOffScopeDenials = 64

// OffScopeWrites is the end-of-run account of writes the scope policy refused: the
// distinct paths, in first-denial order, plus the allow list that was in force. The
// allow list is part of it because the actionable output is a flag that EXTENDS the
// run's scope — a suggestion that replaced it would silently narrow the next run.
//
// Nil on the Report when no write was denied, so an un-denied run's summary and JSON
// are unchanged.
type OffScopeWrites struct {
	// Denials holds one entry per distinct path (repeat attempts on the same path
	// collapse), capped at maxOffScopeDenials.
	Denials []ScopeDenial
	// Distinct is the true number of distinct paths denied, which exceeds
	// len(Denials) once the cap is hit.
	Distinct int
	// Allow is the run's allow globs as configured. Nil when the allow set is empty
	// ("everything in the jail except what deny/read_only remove"), which is not
	// expressible as a list of allow patterns — see tools.ScopePolicy.AllowPatterns.
	Allow []string
}

// offScopeTracker accumulates distinct denials over a run. Zero value is ready.
type offScopeTracker struct {
	seen     map[string]bool
	denials  []ScopeDenial
	distinct int
}

// note records one scope denial. Path-less denials are ignored: a scoped
// run_command rejection (ScopeClassRunCommandDisabled) has no path, and there is no
// file to name or to add to an allow list.
func (t *offScopeTracker) note(e *tools.ScopeError) {
	if e == nil || e.Path == "" {
		return
	}
	if t.seen[e.Path] {
		return
	}
	if t.seen == nil {
		t.seen = make(map[string]bool)
	}
	t.seen[e.Path] = true
	t.distinct++
	if len(t.denials) < maxOffScopeDenials {
		t.denials = append(t.denials, ScopeDenial{
			Class:   e.Class,
			Tool:    e.Tool,
			Path:    e.Path,
			Rule:    e.Rule,
			Message: e.Message,
		})
	}
}

// report builds the Report field, or nil when nothing nameable was denied.
func (t *offScopeTracker) report(allow []string) *OffScopeWrites {
	if len(t.denials) == 0 {
		return nil
	}
	return &OffScopeWrites{Denials: t.denials, Distinct: t.distinct, Allow: allow}
}
