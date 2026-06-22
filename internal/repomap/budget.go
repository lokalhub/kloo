package repomap

import (
	"fmt"
	"strings"
)

// TokenCounter estimates the token count of a string. It is pluggable so the
// approximation can be tightened later without touching the curator.
type TokenCounter func(string) int

// ApproxTokens is the default v1 token estimate: ~4 characters per token
// (a documented heuristic, conventions/budget). It deliberately over- rather
// than under-counts on average so the "stays under budget" contract holds with
// margin.
func ApproxTokens(s string) int {
	return (len(s) + 3) / 4
}

// Stat reports how the assembled context relates to the budget (observable, so
// the loop and tests can see how full the window is).
type Stat struct {
	Budget     int
	TokensUsed int
	Included   []string // file paths included (in rank order)
	Dropped    []string // file paths dropped to fit (lowest-rank-first)
	Truncated  bool     // a high-rank file was windowed to fit
}

// Assemble builds the per-step context string from the ranked files, staying
// STRICTLY under budget tokens. It includes highest-ranked entries first; when
// the next whole entry would overflow, an over-large HIGH-rank file is included
// as a bounded window (rather than dropped), after which the remaining
// lower-ranked entries are dropped. The result never exceeds the budget.
//
// count may be nil (defaults to ApproxTokens). budget ≤ 0 yields an empty
// context with everything dropped.
func Assemble(ranked []RankedFile, budget int, count TokenCounter) (string, Stat) {
	if count == nil {
		count = ApproxTokens
	}
	stat := Stat{Budget: budget}
	if budget <= 0 {
		for _, rf := range ranked {
			stat.Dropped = append(stat.Dropped, rf.Path)
		}
		return "", stat
	}

	var b strings.Builder
	for i, rf := range ranked {
		block := RenderFileBlock(rf)
		remaining := budget - stat.TokensUsed

		if count(block) <= remaining {
			b.WriteString(block)
			stat.TokensUsed += count(block)
			stat.Included = append(stat.Included, rf.Path)
			continue
		}

		// The whole block doesn't fit. If this is a high-rank entry and there is
		// meaningful room left, include a bounded WINDOW of it rather than
		// dropping it; then the budget is full and the rest are dropped.
		if window := windowToFit(block, remaining, count); window != "" {
			b.WriteString(window)
			stat.TokensUsed += count(window)
			stat.Included = append(stat.Included, rf.Path)
			stat.Truncated = true
		} else {
			stat.Dropped = append(stat.Dropped, rf.Path)
		}
		// Budget is now full (or this entry couldn't fit at all) — drop the rest.
		for _, rest := range ranked[i+1:] {
			stat.Dropped = append(stat.Dropped, rest.Path)
		}
		break
	}

	return b.String(), stat
}

// windowToFit truncates block so its token count is ≤ budget, appending a
// truncation marker. Returns "" if even a minimal window can't fit. Because
// ApproxTokens is len/4, truncating to budget*4 bytes guarantees the fit; we
// reserve room for the marker and verify with count.
func windowToFit(block string, budget int, count TokenCounter) string {
	const marker = "\n…[truncated to fit context budget]\n"
	if budget <= count(marker) {
		return ""
	}
	// Binary-free, simple shrink: start from a char budget and back off until
	// the counted total (content + marker) fits.
	maxChars := budget * 4
	if maxChars > len(block) {
		maxChars = len(block)
	}
	for maxChars > 0 {
		candidate := block[:maxChars] + marker
		if count(candidate) <= budget {
			return candidate
		}
		maxChars -= 16
	}
	return ""
}

// FormatStat renders a one-line summary of the assembled window's fullness.
func (s Stat) FormatStat() string {
	return fmt.Sprintf("context: %d/%d tokens, %d files included, %d dropped (truncated=%v)",
		s.TokensUsed, s.Budget, len(s.Included), len(s.Dropped), s.Truncated)
}
