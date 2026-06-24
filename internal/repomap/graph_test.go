package repomap

import (
	"math"
	"reflect"
	"testing"
)

// buildGraphFixture walks the graph-fixture repo, extracts symbols, reads each
// file's content, and builds the reference graph — the same shape the production
// path (loop.go → Rank) feeds BuildRefGraph.
func buildGraphFixture(t *testing.T) *graph {
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
		contents[f.Path] = b
	}
	return BuildRefGraph(files, syms, contents)
}

// hasEdge reports whether the graph has a directed edge from→to (by path).
func hasEdge(g *graph, from, to string) bool {
	fi, ti := -1, -1
	for i, p := range g.paths {
		if p == from {
			fi = i
		}
		if p == to {
			ti = i
		}
	}
	if fi < 0 || ti < 0 {
		return false
	}
	for _, j := range g.out[fi] {
		if j == ti {
			return true
		}
	}
	return false
}

func TestBuildRefGraphEdges(t *testing.T) {
	g := buildGraphFixture(t)

	// def→ref: a and c reference Widget/NewWidget defined in b → edges to b.
	if !hasEdge(g, "a.go", "b.go") {
		t.Errorf("expected edge a.go→b.go")
	}
	if !hasEdge(g, "c.go", "b.go") {
		t.Errorf("expected edge c.go→b.go")
	}

	// b defines but references nothing the others define → no out-edges.
	if hasEdge(g, "b.go", "a.go") || hasEdge(g, "b.go", "c.go") || hasEdge(g, "b.go", "d.go") {
		t.Errorf("b.go should have no out-edges, got %v", g.out[indexOfPath(g, "b.go")])
	}

	// d is isolated: references nothing a/b/c define, and nothing references it.
	if hasEdge(g, "d.go", "a.go") || hasEdge(g, "d.go", "b.go") || hasEdge(g, "d.go", "c.go") {
		t.Errorf("d.go should have no out-edges into a/b/c")
	}
	if hasEdge(g, "a.go", "d.go") || hasEdge(g, "b.go", "d.go") || hasEdge(g, "c.go", "d.go") {
		t.Errorf("nothing should reference d.go")
	}

	// No self-edges anywhere.
	for i := range g.paths {
		for _, j := range g.out[i] {
			if i == j {
				t.Errorf("self-edge at %s", g.paths[i])
			}
		}
	}
}

func indexOfPath(g *graph, p string) int {
	for i, pp := range g.paths {
		if pp == p {
			return i
		}
	}
	return -1
}

// TestBuildRefGraphDedup: an identifier referenced many times in one file yields
// a single A→B edge.
func TestBuildRefGraphDedup(t *testing.T) {
	files := []Node{{Path: "a.go"}, {Path: "b.go"}}
	symbols := map[string][]Symbol{
		"b.go": {{Name: "Widget", Kind: KindType, File: "b.go", Line: 1}},
	}
	contents := map[string][]byte{
		"a.go": []byte("Widget Widget Widget\nWidget Widget\n"),
		"b.go": []byte("type Widget struct{}\n"),
	}
	g := BuildRefGraph(files, symbols, contents)
	ai := indexOfPath(g, "a.go")
	if got := len(g.out[ai]); got != 1 {
		t.Fatalf("expected exactly one deduped edge from a.go, got %d (%v)", got, g.out[ai])
	}
	if !hasEdge(g, "a.go", "b.go") {
		t.Errorf("expected the single edge to be a.go→b.go")
	}
}

// TestBuildRefGraphMultiDefiner: an identifier defined in two files yields an
// edge to BOTH definers.
func TestBuildRefGraphMultiDefiner(t *testing.T) {
	files := []Node{{Path: "a.go"}, {Path: "b.go"}, {Path: "c.go"}}
	symbols := map[string][]Symbol{
		"b.go": {{Name: "Shared", Kind: KindType, File: "b.go", Line: 1}},
		"c.go": {{Name: "Shared", Kind: KindFunc, File: "c.go", Line: 1}},
	}
	contents := map[string][]byte{
		"a.go": []byte("use Shared here\n"),
		"b.go": []byte("type Shared struct{}\n"),
		"c.go": []byte("func Shared() {}\n"),
	}
	g := BuildRefGraph(files, symbols, contents)
	if !hasEdge(g, "a.go", "b.go") {
		t.Errorf("expected edge a.go→b.go (definer 1)")
	}
	if !hasEdge(g, "a.go", "c.go") {
		t.Errorf("expected edge a.go→c.go (definer 2)")
	}
}

// TestBuildRefGraphDeterministic: two builds yield identical node order and
// adjacency.
func TestBuildRefGraphDeterministic(t *testing.T) {
	g1 := buildGraphFixture(t)
	g2 := buildGraphFixture(t)
	if !reflect.DeepEqual(g1.paths, g2.paths) {
		t.Errorf("node order not deterministic:\n%v\n%v", g1.paths, g2.paths)
	}
	if !reflect.DeepEqual(g1.out, g2.out) {
		t.Errorf("adjacency not deterministic:\n%v\n%v", g1.out, g2.out)
	}
}

// TestBuildRefGraphMissingContent: a node absent from contents contributes no
// out-edges and does not panic; it remains a node.
func TestBuildRefGraphMissingContent(t *testing.T) {
	files := []Node{{Path: "a.go"}, {Path: "b.go"}}
	symbols := map[string][]Symbol{
		"b.go": {{Name: "Widget", Kind: KindType, File: "b.go", Line: 1}},
	}
	contents := map[string][]byte{
		// a.go intentionally missing → no out-edges from a.
		"b.go": []byte("type Widget struct{}\n"),
	}
	g := BuildRefGraph(files, symbols, contents)
	if len(g.paths) != 2 {
		t.Fatalf("expected 2 nodes even with missing content, got %d", len(g.paths))
	}
	ai := indexOfPath(g, "a.go")
	if len(g.out[ai]) != 0 {
		t.Errorf("missing-content node a.go should have no out-edges, got %v", g.out[ai])
	}
}

// TestPageRankCentralityOrdering is the core signal the feature rests on:
// b.go (referenced by a and c) out-ranks d.go (unreferenced).
func TestPageRankCentralityOrdering(t *testing.T) {
	cent := PageRank(buildGraphFixture(t))
	if cent["b.go"] <= cent["d.go"] {
		t.Errorf("centrality(b.go)=%g should exceed centrality(d.go)=%g", cent["b.go"], cent["d.go"])
	}
	// Normalized: max is 1.0, all within [0,1].
	maxV := 0.0
	for _, v := range cent {
		if v < 0 || v > 1 {
			t.Errorf("centrality out of [0,1]: %g", v)
		}
		if v > maxV {
			maxV = v
		}
	}
	if maxV != 1.0 {
		t.Errorf("expected normalized max centrality 1.0, got %g", maxV)
	}
}

// TestPageRankDeterministic: two runs over the same graph return identical maps.
func TestPageRankDeterministic(t *testing.T) {
	g := buildGraphFixture(t)
	a := PageRank(g)
	b := PageRank(g)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("PageRank not deterministic:\n%v\n%v", a, b)
	}
}

// TestPageRankTerminatesOnCycle: a non-converging-shaped graph (a small cycle)
// still terminates and produces finite, normalized ranks.
func TestPageRankTerminatesOnCycle(t *testing.T) {
	// 3-cycle: a→b→c→a. Symmetric, so all centralities equal 1.0 normalized.
	files := []Node{{Path: "a.go"}, {Path: "b.go"}, {Path: "c.go"}}
	symbols := map[string][]Symbol{
		"a.go": {{Name: "A", Kind: KindType, File: "a.go", Line: 1}},
		"b.go": {{Name: "B", Kind: KindType, File: "b.go", Line: 1}},
		"c.go": {{Name: "C", Kind: KindType, File: "c.go", Line: 1}},
	}
	contents := map[string][]byte{
		"a.go": []byte("type A struct{}\nB\n"), // a→b
		"b.go": []byte("type B struct{}\nC\n"), // b→c
		"c.go": []byte("type C struct{}\nA\n"), // c→a
	}
	cent := PageRank(BuildRefGraph(files, symbols, contents))
	for p, v := range cent {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Errorf("non-finite centrality for %s: %g", p, v)
		}
	}
	// Perfect symmetry → all equal (within float noise) → all normalized to 1.0.
	for _, p := range []string{"a.go", "b.go", "c.go"} {
		if d := math.Abs(cent[p] - 1.0); d > 1e-9 {
			t.Errorf("symmetric cycle centrality(%s)=%g, expected ~1.0", p, cent[p])
		}
	}
}

// TestPageRankDanglingMassConserved: a dangling node (no out-edges) does not
// cause ranks to explode or vanish; mass is redistributed.
func TestPageRankDanglingMassConserved(t *testing.T) {
	// a→b, b is dangling (defines nothing referenced, references nothing).
	files := []Node{{Path: "a.go"}, {Path: "b.go"}}
	symbols := map[string][]Symbol{
		"b.go": {{Name: "Widget", Kind: KindType, File: "b.go", Line: 1}},
	}
	contents := map[string][]byte{
		"a.go": []byte("Widget\n"),
		"b.go": []byte("type Widget struct{}\n"),
	}
	cent := PageRank(BuildRefGraph(files, symbols, contents))
	for p, v := range cent {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			t.Errorf("dangling graph produced bad centrality %s=%g", p, v)
		}
	}
	// b is referenced by a → b more central than a.
	if cent["b.go"] <= cent["a.go"] {
		t.Errorf("centrality(b)=%g should exceed centrality(a)=%g", cent["b.go"], cent["a.go"])
	}
}

// TestPageRankEdgeCases: N==0 → empty; N==1 → single node normalized to 1.0.
func TestPageRankEdgeCases(t *testing.T) {
	empty := PageRank(BuildRefGraph(nil, nil, nil))
	if len(empty) != 0 {
		t.Errorf("N==0 should yield empty map, got %v", empty)
	}

	single := PageRank(BuildRefGraph([]Node{{Path: "only.go"}}, nil, nil))
	if len(single) != 1 {
		t.Fatalf("N==1 should yield one entry, got %v", single)
	}
	if single["only.go"] != 1.0 {
		t.Errorf("single node should normalize to 1.0, got %g", single["only.go"])
	}
}

// TestRefTokensCasePreserving: the reference tokenizer keeps identifier case and
// also yields whole selector/route tokens plus their alnum sub-tokens.
func TestRefTokensCasePreserving(t *testing.T) {
	toks := refTokens([]byte(`x := NewWidget(); <app-home routerLink="tabs/home">`))
	for _, want := range []string{"NewWidget", "app-home", "tabs/home", "home"} {
		if !toks[want] {
			t.Errorf("expected token %q in %v", want, toks)
		}
	}
	if toks["newwidget"] {
		t.Errorf("tokenizer must be case-preserving, must not lowercase to newwidget")
	}
}
