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
	distinctEdit := false
	if t.Edit != "" {
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
	switch {
	case t.VerifyOutput == "":
		c.lastFail = ""
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
