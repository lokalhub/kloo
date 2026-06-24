package repomap

import (
	"strings"
	"testing"
)

// rankGraphFixture walks the graph-fixture, extracts symbols, reads each file's
// content, and ranks with the given task — optionally passing Contents so the
// PageRank centrality tier is exercised (withContent=false reproduces the
// pre-graph behavior).
func rankGraphFixture(t *testing.T, task string, withContent bool) []RankedFile {
	t.Helper()
	root := "testdata/repos/graph-fixture"
	nodes, err := Walk(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	files := Files(nodes)
	syms := map[string][]Symbol{}
	contents := map[string][]byte{}
	for _, f := range files {
		b := readFixture(t, "graph-fixture", f.Path)
		syms[f.Path] = ExtractSymbols(f.Path, b)
		if withContent {
			contents[f.Path] = b
		}
	}
	return Rank(RankInput{Files: files, Symbols: syms, Task: task, Contents: contents})
}

func indexOfRanked(rs []RankedFile, path string) int {
	for i, r := range rs {
		if r.Path == path {
			return i
		}
	}
	return -1
}

// TestRankCentralitySurfacesReferenced is the new acceptance: with a task-neutral
// string (all task scores tie at 0), the centrality tier orders the referenced
// b.go ahead of the unreferenced d.go — and that same order is what Assemble
// receives.
func TestRankCentralitySurfacesReferenced(t *testing.T) {
	ranked := rankGraphFixture(t, "zzzznomatch", true)

	// All task scores must be 0 (neutral task) so centrality is what decides.
	for _, r := range ranked {
		if r.Score != 0 {
			t.Fatalf("expected neutral task → score 0 for %s, got %g", r.Path, r.Score)
		}
	}

	bi, di := indexOfRanked(ranked, "b.go"), indexOfRanked(ranked, "d.go")
	if bi < 0 || di < 0 {
		t.Fatalf("b.go and d.go must be ranked; got %v", rankedPaths(ranked))
	}
	if bi >= di {
		t.Errorf("centrality ordering: index(b.go)=%d should be < index(d.go)=%d (%v)", bi, di, rankedPaths(ranked))
	}

	// The produced order is what Assemble consumes: b.go appears before d.go in
	// the assembled context too.
	ctx, _ := Assemble(ranked, 100000, ApproxTokens)
	if bPos, dPos := strings.Index(ctx, "b.go"), strings.Index(ctx, "d.go"); bPos < 0 || dPos < 0 || bPos >= dPos {
		t.Errorf("Assemble order: b.go (%d) should appear before d.go (%d)", bPos, dPos)
	}

	// Deterministic: a second identical Rank yields the same order.
	ranked2 := rankGraphFixture(t, "zzzznomatch", true)
	for i := range ranked {
		if ranked[i].Path != ranked2[i].Path {
			t.Fatalf("rank not deterministic at %d: %q vs %q", i, ranked[i].Path, ranked2[i].Path)
		}
	}
}

// TestRankEmptyContentsIdentical pins the backward-compatibility contract: with
// no Contents the centrality tier is inert and the neutral-task order is pure
// ascending path — exactly the pre-graph behavior. (Contrast with the test above,
// where the same fixture + Contents reorders b.go ahead.)
func TestRankEmptyContentsIdentical(t *testing.T) {
	ranked := rankGraphFixture(t, "zzzznomatch", false)
	got := rankedPaths(ranked)
	want := []string{"a.go", "b.go", "c.go", "d.go"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("no-content neutral order should be pure path-sorted: want %v, got %v", want, got)
			break
		}
	}
}
