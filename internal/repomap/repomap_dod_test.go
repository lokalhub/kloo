package repomap

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildMap runs the full pipeline (walk → extract → rank → curate) over a
// fixture repo for a task string and budget — the integration the Phase-04 loop
// will call.
func buildMap(t *testing.T, repo, task string, budget int) ([]RankedFile, string, Stat) {
	t.Helper()
	root := filepath.Join("testdata", "repos", repo)

	nodes, err := Walk(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	files := Files(nodes)

	var rels []string
	for _, f := range files {
		rels = append(rels, f.Path)
	}
	allSyms := Extract(root, rels) // ctags-if-present, else regex (transparent)

	byFile := map[string][]Symbol{}
	for _, s := range allSyms {
		byFile[s.File] = append(byFile[s.File], s)
	}

	ranked := Rank(RankInput{Files: files, Symbols: byFile, Task: task})
	ctx, stat := Assemble(ranked, budget, ApproxTokens)
	return ranked, ctx, stat
}

// TestDoDRightFilesSurfaced is Phase-03 DoD #1: on the sample repo + task, the
// expected files are in the top-K of the assembled map, deterministically.
func TestDoDRightFilesSurfaced(t *testing.T) {
	ranked, _, _ := buildMap(t, "ionic-fixture", "update the profile page", 8000)

	top3 := map[string]bool{}
	for i, rf := range ranked {
		if i >= 3 {
			break
		}
		top3[rf.Path] = true
	}
	if !top3["src/app/profile/profile.page.ts"] || !top3["src/app/profile/profile.page.html"] {
		var got []string
		for i := 0; i < 3 && i < len(ranked); i++ {
			got = append(got, ranked[i].Path)
		}
		t.Errorf("profile files not surfaced in top-3: %v", got)
	}

	// Deterministic: a second full run yields the identical ordering.
	ranked2, _, _ := buildMap(t, "ionic-fixture", "update the profile page", 8000)
	for i := range ranked {
		if ranked[i].Path != ranked2[i].Path {
			t.Fatalf("pipeline not deterministic at %d: %q vs %q", i, ranked[i].Path, ranked2[i].Path)
		}
	}
}

// TestDoDContextUnderBudget is Phase-03 DoD #2: the assembled context is ≤ the
// configured budget, and a tightened budget evicts lowest-rank-first while the
// ceiling still holds.
func TestDoDContextUnderBudget(t *testing.T) {
	_, ctxFull, statFull := buildMap(t, "ionic-fixture", "update the profile page", 8000)
	if ApproxTokens(ctxFull) > 8000 || statFull.TokensUsed > 8000 {
		t.Errorf("full context over budget: %d tokens", statFull.TokensUsed)
	}

	// Tighten the budget so not everything fits.
	ranked, ctxTight, statTight := buildMap(t, "ionic-fixture", "update the profile page", 12)
	if ApproxTokens(ctxTight) > 12 {
		t.Errorf("tight context busted budget: %d > 12", ApproxTokens(ctxTight))
	}
	if len(statTight.Dropped) == 0 {
		t.Errorf("tight budget should drop something, dropped none")
	}
	// Lowest-rank-first: every dropped file ranks at/after every included file.
	rankOf := map[string]int{}
	for i, rf := range ranked {
		rankOf[rf.Path] = i
	}
	worstIncluded := -1
	for _, p := range statTight.Included {
		if rankOf[p] > worstIncluded {
			worstIncluded = rankOf[p]
		}
	}
	for _, p := range statTight.Dropped {
		if rankOf[p] < worstIncluded {
			t.Errorf("eviction not lowest-rank-first: dropped %q (rank %d) ranks above an included file (rank %d)", p, rankOf[p], worstIncluded)
		}
	}
}

// TestDoDNoCGOImport is the central phase gate: no Go file in the repomap
// package actually imports "C" (tree-sitter / CGO is out of v1). It inspects
// real import specs via go/parser — not a string scan — so a comment or check
// string that merely mentions the cgo import never false-positives.
func TestDoDNoCGOImport(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"C"` {
				t.Errorf("%s imports \"C\" — the no-CGO rule is violated", e.Name())
			}
		}
	}
}
