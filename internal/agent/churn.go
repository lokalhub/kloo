package agent

import (
	"regexp"
	"strings"
)

// churnDetector halts a no-progress loop. It tracks two signals: the same failing
// verify output repeated, and the same edit attempted repeated. Either, repeated
// N rounds in a row, is churn. Distinct progress (a different failure, a
// different edit, or a passing verify) resets the relevant counter.
//
// Comparison is NORMALISED so "identical" means semantically the same, not
// byte-identical: durations, absolute/temp paths, and hex blobs are stripped
// before comparing (decisions.md). N is configurable (config.ChurnRounds).
type churnDetector struct {
	n int

	lastFail  string
	failCount int

	lastEdit  string
	editCount int

	everActed bool // has the agent taken ANY side-effecting action this run (edit OR run_command)?

	artifact string // the repeated artifact, for the report
}

// NewChurnDetector builds a detector that halts after n consecutive repeats.
func NewChurnDetector(n int) *churnDetector {
	if n <= 0 {
		n = 1
	}
	return &churnDetector{n: n}
}

// Observe records a turn's (failing) verify output and attempted edit.
func (c *churnDetector) Observe(t Turn) {
	// Edit signal first: a DISTINCT edit is forward progress (the agent changed
	// the code), so it both resets the repeated-edit run and — below — clears the
	// repeated-failure run. The same edit repeated still climbs editCount.
	// A side-effecting run_command (no Edit string, but Acted) also lifts the agent
	// off the read-only baseline so the failure rail can engage — without resetting
	// the repeated-edit run, since it changed no edit_file/write_file.
	if t.Acted {
		c.everActed = true
	}

	distinctEdit := false
	if t.Edit != "" {
		c.everActed = true
		ne := normalizeChurn(t.Edit)
		if ne == c.lastEdit {
			c.editCount++
		} else {
			c.lastEdit = ne
			c.editCount = 1
			distinctEdit = true
		}
	}

	// Failure signal: a passing verify (empty output) is progress → reset. So is
	// a distinct edit — mid-refactor the build error often stays identical for
	// several steps WHILE the agent makes real progress, so an unchanged failure
	// is only "no progress" when the agent also made no new edit. This keeps the
	// rail (repeated edit, or repeated failure with no edits) without strangling
	// honest multi-file refactors.
	//
	// Crucially, a repeated failure only counts once the agent has taken at least
	// one side-effecting action (an edit OR a run_command). Before any action, a
	// failing verify is the project's BASELINE (e.g. the default `go test` failing
	// on a non-Go app while the agent just reads/explores with read_file/list_dir,
	// or a conversational "hello" that triggers no real work) — not the agent stuck
	// redoing a broken fix. Counting it churned read-only runs. A run_command counts
	// as acting because it can mutate the tree (rm/mv/sed -i), so work done through
	// the shell — invisible to the edit signal — no longer hides a stuck loop.
	switch {
	case t.VerifyOutput == "":
		c.lastFail = ""
		c.failCount = 0
	case !c.everActed:
		c.lastFail = normalizeChurn(t.VerifyOutput) // remember it, but don't count it as no-progress yet
		c.failCount = 0
	case distinctEdit:
		c.lastFail = normalizeChurn(t.VerifyOutput)
		c.failCount = 0
	default:
		nf := normalizeChurn(t.VerifyOutput)
		if nf == c.lastFail {
			c.failCount++
		} else {
			c.lastFail = nf
			c.failCount = 1
		}
	}
}

// Check reports churn once a signal has repeated N times in a row.
func (c *churnDetector) Check() (bool, ChurnKind) {
	if c.failCount >= c.n {
		c.artifact = c.lastFail
		return true, ChurnRepeatedFailure
	}
	if c.editCount >= c.n {
		c.artifact = c.lastEdit
		return true, ChurnRepeatedEdit
	}
	return false, ""
}

// Artifact returns the repeated failure output / edit that caused the churn.
func (c *churnDetector) Artifact() string { return c.artifact }

// Reset clears all accumulated state so a reused detector starts each run fresh
// (n is configuration, so it's kept). Without this, a second task on the same
// Loop inherits the prior run's failure/edit streak and churns prematurely.
func (c *churnDetector) Reset() {
	c.lastFail, c.failCount = "", 0
	c.lastEdit, c.editCount = "", 0
	c.everActed = false
	c.artifact = ""
}

// Normalisation regexes: strip volatile bits so two semantically-identical
// failures/edits compare equal.
var (
	reDuration = regexp.MustCompile(`\b\d+(\.\d+)?\s*(ns|µs|us|ms|s|m|h)\b`)
	reTempPath = regexp.MustCompile(`(/tmp|/var/folders|/private/var)[\w./-]*`)
	reHexBlob  = regexp.MustCompile(`\b[0-9a-f]{12,}\b`)
	reWS       = regexp.MustCompile(`[ \t]+`)
)

// normalizeChurn strips timestamps/durations, absolute temp paths, and long hex
// blobs (sha/addresses), then collapses whitespace, so comparison is semantic.
func normalizeChurn(s string) string {
	s = reDuration.ReplaceAllString(s, "DUR")
	s = reTempPath.ReplaceAllString(s, "PATH")
	s = reHexBlob.ReplaceAllString(s, "HEX")
	s = reWS.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
