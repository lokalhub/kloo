package repomap

import (
	"fmt"
	"sort"
	"strings"
)

// Scoring weights (decisions.md). Token overlap dominates; path proximity is a
// secondary directory-level signal; recency is a small tie-shifter.
const (
	weightOverlap   = 3.0
	weightProximity = 2.0
	weightRecency   = 1.0
)

// stopwords are dropped from token sets so trivial words and generic action
// verbs don't create noise overlap (a task "update the home page" keys on
// home/page, not "the"/"update"). Domain words like "page" are intentionally
// NOT stopwords. (decisions.md)
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "to": true, "of": true, "in": true,
	"on": true, "for": true, "and": true, "or": true, "is": true, "it": true,
	"this": true, "that": true, "with": true, "add": true, "update": true,
	"fix": true, "change": true, "make": true, "edit": true,
}

// RankInput carries everything the ranker needs (passed in for determinism: the
// recency signal is an explicit set, not a live mtime read, so tests can pin it).
type RankInput struct {
	Files   []Node              // files only (dirs are ignored for ranking)
	Symbols map[string][]Symbol // path -> its symbols
	Task    string              // the current task string
	// RecentlyTouched is the set of recently-edited file paths (the recency
	// signal). Passed in so ranking stays deterministic and testable.
	RecentlyTouched map[string]bool
}

// RankedFile is one file scored for relevance to the task, with its symbols.
type RankedFile struct {
	Path    string
	Score   float64
	Symbols []Symbol
}

// Rank scores and orders the input files by descending relevance to the task.
// Ordering is fully deterministic: ties are broken by ascending path, and no
// output depends on Go map iteration order.
func Rank(in RankInput) []RankedFile {
	taskTokens := tokenSet(in.Task)

	ranked := make([]RankedFile, 0, len(in.Files))
	for _, f := range in.Files {
		syms := in.Symbols[f.Path]
		score := scoreFile(f.Path, syms, taskTokens, in.RecentlyTouched[f.Path])
		ranked = append(ranked, RankedFile{Path: f.Path, Score: score, Symbols: syms})
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].Path < ranked[j].Path
	})
	return ranked
}

// scoreFile combines the three signals into one score.
func scoreFile(path string, syms []Symbol, taskTokens map[string]bool, recent bool) float64 {
	// Overlap: task tokens found in the file's name + symbol names.
	nameTokens := tokenSet(baseNoExt(path))
	for _, s := range syms {
		for tok := range tokenSet(s.Name) {
			nameTokens[tok] = true
		}
	}
	overlap := intersectCount(taskTokens, nameTokens)

	// Proximity: task tokens found in the file's directory components.
	dirTokens := map[string]bool{}
	if i := strings.LastIndex(path, "/"); i >= 0 {
		for tok := range tokenSet(strings.ReplaceAll(path[:i], "/", " ")) {
			dirTokens[tok] = true
		}
	}
	proximity := intersectCount(taskTokens, dirTokens)

	recency := 0.0
	if recent {
		recency = 1.0
	}

	return weightOverlap*float64(overlap) + weightProximity*float64(proximity) + weightRecency*recency
}

// RenderMap renders a compact repo-map string (file → its top symbols) for the
// top n ranked files, suitable for embedding in the system prompt. This is the
// artifact the budget curator (budget.go) trims to fit.
func RenderMap(ranked []RankedFile, topN int) string {
	var b strings.Builder
	for i, rf := range ranked {
		if topN > 0 && i >= topN {
			break
		}
		b.WriteString(rf.Path)
		b.WriteByte('\n')
		for _, s := range rf.Symbols {
			b.WriteString(fmt.Sprintf("  %s %s:%d\n", s.Kind, s.Name, s.Line))
		}
	}
	return b.String()
}

// RenderFileBlock renders a single ranked file's map block (path + symbols),
// used by the budget curator so it can measure/trim per file.
func RenderFileBlock(rf RankedFile) string {
	var b strings.Builder
	b.WriteString(rf.Path)
	b.WriteByte('\n')
	for _, s := range rf.Symbols {
		b.WriteString(fmt.Sprintf("  %s %s:%d\n", s.Kind, s.Name, s.Line))
	}
	return b.String()
}

// tokenSet lowercases s, splits on non-alphanumerics, and drops stopwords and
// 1-char tokens.
func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(tok) < 2 || stopwords[tok] {
			continue
		}
		out[tok] = true
	}
	return out
}

func intersectCount(a, b map[string]bool) int {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	return n
}

// baseNoExt returns the basename of a slash path without its extension.
func baseNoExt(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	return base
}
