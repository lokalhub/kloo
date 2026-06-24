package repomap

import (
	"math"
	"sort"
	"strings"
)

// PageRank parameters (greppable package consts, mirroring the weightOverlap
// style in rank.go). damping is the standard 0.85; iteration stops at the L1
// epsilon OR the hard cap, whichever first — the cap guarantees deterministic
// termination, epsilon exits early on the common (converging) case.
const (
	pageRankDamping  = 0.85
	pageRankMaxIters = 100
	pageRankEpsilon  = 1e-6
)

// graph is a directed def→ref graph over the repo-map files, used to compute
// structural centrality (PageRank, see PageRank below). It is deliberately
// SLICE-INDEXED, not map-keyed: node i is paths[i], with paths in ascending
// (lexicographic) order. Adjacency is stored as sorted int slices. This keeps
// every downstream computation order-stable — no Go map iteration order ever
// escapes into the graph structure or the PageRank math, so results are
// bit-identical across runs and platforms (overview §1 determinism rule).
//
// An edge i→j means file paths[i] references an identifier that file paths[j]
// defines (def→ref). Edges are set-deduped (a given i→j appears once regardless
// of how many times the identifier occurs) and never self-referential (i≠j).
type graph struct {
	paths []string // node id → workspace-relative path, ascending order
	out   [][]int  // out[i] = sorted, deduped target node ids (edges i→j)
}

// BuildRefGraph builds the def→ref graph over files. Definitions come from the
// existing symbol extraction (symbols, path → its symbols); references come from
// a single word-boundary scan of each file's already-read content (contents,
// path → ≤maxMappedFileBytes bytes). It never opens a file: a path missing from
// contents simply contributes no outgoing references (it is still a node if it
// is in files).
//
// Complexity is O(Σ tokens(file) + #symbols) — linear in total scanned bytes
// plus the symbol count. There is no file×file or file×symbol nested scan.
func BuildRefGraph(files []Node, symbols map[string][]Symbol, contents map[string][]byte) *graph {
	// Node set = the file nodes, in ascending path order (dirs ignored, exactly
	// as Rank ignores them). Sorting here makes node ids order-stable regardless
	// of the caller's slice order.
	paths := make([]string, 0, len(files))
	for _, f := range files {
		if f.IsDir {
			continue
		}
		paths = append(paths, f.Path)
	}
	sort.Strings(paths)

	idx := make(map[string]int, len(paths))
	for i, p := range paths {
		idx[p] = i
	}

	// Definer index: identifier name → file node ids that define it, each id
	// stored once and in ascending order. Built by iterating the path-sorted
	// node list (never by ranging the symbols map), so the per-name id lists are
	// naturally ascending and order-stable.
	def := map[string][]int{}
	for i, p := range paths {
		for _, s := range symbols[p] {
			if s.Name == "" {
				continue
			}
			ids := def[s.Name]
			// paths are ascending and i is non-decreasing, so a duplicate can
			// only be the last appended id (same file defining the name twice).
			if n := len(ids); n > 0 && ids[n-1] == i {
				continue
			}
			def[s.Name] = append(ids, i)
		}
	}

	out := make([][]int, len(paths))
	for i, p := range paths {
		content, ok := contents[p]
		if !ok || len(content) == 0 {
			continue // missing/empty content → no out-edges (safe, never panics)
		}
		targets := map[int]bool{}
		for tok := range refTokens(content) {
			for _, b := range def[tok] {
				if b != i { // no self-edges
					targets[b] = true
				}
			}
		}
		if len(targets) == 0 {
			continue
		}
		adj := make([]int, 0, len(targets))
		for b := range targets {
			adj = append(adj, b)
		}
		sort.Ints(adj) // sorted slice → order-stable adjacency
		out[i] = adj
	}

	return &graph{paths: paths, out: out}
}

// refTokens extracts the set of reference tokens from already-read file content.
// It is case-PRESERVING (identifiers are case-sensitive: a reference to Widget
// must match a definition of Widget, not "widget"), so it deliberately does NOT
// reuse tokenSet (which lowercases and drops stopwords for the task-relevance
// scorer).
//
// Two linear scans over the same content, both O(len(content)):
//   - identifier runs of [A-Za-z0-9_]  — Go/TS identifiers (Widget, NewWidget).
//   - selector/route runs that also keep '-' and '/' — Angular selectors
//     (app-home) and route paths (tabs/home) are matched as whole tokens too,
//     while the first scan still yields their alnum sub-tokens (home).
func refTokens(content []byte) map[string]bool {
	s := string(content)
	out := map[string]bool{}
	addRuns(out, s, isIdentRune)
	addRuns(out, s, isSelectorRune)
	return out
}

func addRuns(out map[string]bool, s string, isTok func(rune) bool) {
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool { return !isTok(r) }) {
		out[tok] = true
	}
}

func isIdentRune(r rune) bool {
	return r == '_' ||
		(r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

func isSelectorRune(r rune) bool {
	return r == '-' || r == '/' || isIdentRune(r)
}

// PageRank computes per-file structural centrality over the def→ref graph and
// returns it keyed by path, min-max normalized to [0,1] (divided by the max
// rank, so the most-central file is 1.0 and centrality is repo-size-independent).
//
// It is fully deterministic: every sum runs over the path-sorted index slices
// (never over a Go map), iteration stops at pageRankEpsilon OR pageRankMaxIters,
// and dangling-node mass (out-degree 0) is collected as a single sum and
// redistributed uniformly. N∈{0,1} are handled without panic or divide-by-zero.
//
// The formulation, for damping d and N nodes:
//
//	newRank[i] = (1-d)/N + d * ( danglingMass/N + Σ_{j→i} rank[j]/outDeg[j] )
func PageRank(g *graph) map[string]float64 {
	n := len(g.paths)
	cent := make(map[string]float64, n)
	if n == 0 {
		return cent
	}

	// Reverse adjacency (incoming edges) + out-degrees, both index-ordered.
	// Iterating i ascending and appending i to in[j] keeps each in[j] ascending,
	// so the Σ_{j→i} sum below runs in a fixed order → bit-stable floats.
	in := make([][]int, n)
	outDeg := make([]int, n)
	for i := 0; i < n; i++ {
		outDeg[i] = len(g.out[i])
		for _, j := range g.out[i] {
			in[j] = append(in[j], i)
		}
	}

	d := pageRankDamping
	rank := make([]float64, n)
	for i := range rank {
		rank[i] = 1.0 / float64(n)
	}
	next := make([]float64, n)

	for iter := 0; iter < pageRankMaxIters; iter++ {
		dangling := 0.0
		for i := 0; i < n; i++ {
			if outDeg[i] == 0 {
				dangling += rank[i]
			}
		}
		base := (1.0-d)/float64(n) + d*dangling/float64(n)
		for i := 0; i < n; i++ {
			sum := 0.0
			for _, j := range in[i] {
				sum += rank[j] / float64(outDeg[j])
			}
			next[i] = base + d*sum
		}
		delta := 0.0
		for i := 0; i < n; i++ {
			delta += math.Abs(next[i] - rank[i])
		}
		rank, next = next, rank
		if delta < pageRankEpsilon {
			break
		}
	}

	max := 0.0
	for i := 0; i < n; i++ {
		if rank[i] > max {
			max = rank[i]
		}
	}
	if max == 0 {
		for i := 0; i < n; i++ {
			cent[g.paths[i]] = 0
		}
		return cent
	}
	for i := 0; i < n; i++ {
		cent[g.paths[i]] = rank[i] / max
	}
	return cent
}
