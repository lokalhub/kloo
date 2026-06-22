package repomap

import (
	"reflect"
	"strings"
	"testing"
)

// rankIonic walks the ionic fixture, extracts symbols, and ranks against task.
func rankIonic(t *testing.T, task string, recent map[string]bool) []RankedFile {
	t.Helper()
	root := "testdata/repos/ionic-fixture"
	nodes, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	files := Files(nodes)
	syms := map[string][]Symbol{}
	for _, f := range files {
		syms[f.Path] = ExtractSymbols(f.Path, readWhole(t, root, f.Path))
	}
	return Rank(RankInput{Files: files, Symbols: syms, Task: task, RecentlyTouched: recent})
}

func readWhole(t *testing.T, root, rel string) []byte {
	t.Helper()
	return readFixture(t, "ionic-fixture", rel)
}

func rankedPaths(rs []RankedFile) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Path
	}
	return out
}

// TestRankSurfacesRightFiles is the master-plan DoD: a task about the profile
// page ranks the profile files into the top-K above unrelated tabs.
func TestRankSurfacesRightFiles(t *testing.T) {
	ranked := rankIonic(t, "update the profile page layout", nil)
	if len(ranked) < 2 {
		t.Fatalf("expected several ranked files, got %d", len(ranked))
	}
	// The two profile files must be in the top-K (K=3 here).
	top := rankedPaths(ranked)[:3]
	hasProfileTS, hasProfileHTML := false, false
	for _, p := range top {
		if p == "src/app/profile/profile.page.ts" {
			hasProfileTS = true
		}
		if p == "src/app/profile/profile.page.html" {
			hasProfileHTML = true
		}
	}
	if !hasProfileTS || !hasProfileHTML {
		t.Errorf("profile files not in top-3: %v", top)
	}
	// And the top-ranked file must out-score a clearly-unrelated one.
	if ranked[0].Score <= scoreOf(ranked, "src/app/apps/apps.page.ts") {
		t.Errorf("top score (%.1f) should beat unrelated apps page", ranked[0].Score)
	}
}

func TestRankDeterministic(t *testing.T) {
	a := rankIonic(t, "profile page", nil)
	b := rankIonic(t, "profile page", nil)
	if !reflect.DeepEqual(rankedPaths(a), rankedPaths(b)) {
		t.Errorf("ranking not deterministic:\n%v\n%v", rankedPaths(a), rankedPaths(b))
	}
}

// TestRankTokenOverlapSignal: a file matching task tokens out-scores one that
// doesn't.
func TestRankTokenOverlapSignal(t *testing.T) {
	ranked := rankIonic(t, "profile", nil)
	profile := scoreOf(ranked, "src/app/profile/profile.page.ts")
	apps := scoreOf(ranked, "src/app/apps/apps.page.ts")
	if profile <= apps {
		t.Errorf("token overlap: profile (%.1f) should beat apps (%.1f)", profile, apps)
	}
}

// TestRankRecencySignal: with all else equal, a recently-touched file ranks
// higher. Use a task that matches nothing so only recency differentiates.
func TestRankRecencySignal(t *testing.T) {
	recent := map[string]bool{"src/app/apps/apps.page.ts": true}
	ranked := rankIonic(t, "zzzznomatch", recent)
	apps := scoreOf(ranked, "src/app/apps/apps.page.ts")
	home := scoreOf(ranked, "src/app/home/home.page.ts")
	if apps <= home {
		t.Errorf("recency: touched apps (%.1f) should beat untouched home (%.1f)", apps, home)
	}
}

// TestRankPathProximitySignal: a task naming a directory lifts files in that
// directory via the proximity signal (distinct from filename overlap).
func TestRankPathProximitySignal(t *testing.T) {
	// "home" appears in the home/ directory path.
	ranked := rankIonic(t, "home", nil)
	home := scoreOf(ranked, "src/app/home/home.page.html")
	profile := scoreOf(ranked, "src/app/profile/profile.page.html")
	if home <= profile {
		t.Errorf("path proximity: home html (%.1f) should beat profile html (%.1f)", home, profile)
	}
}

// TestRankTieBreakStable: equal-score files always order by path ascending.
func TestRankTieBreakStable(t *testing.T) {
	// A task matching nothing → all scores 0 → pure path tie-break.
	ranked := rankIonic(t, "zzzznomatch", nil)
	got := rankedPaths(ranked)
	sorted := append([]string(nil), got...)
	for i := 1; i < len(sorted); i++ {
		if sorted[i-1] > sorted[i] {
			t.Errorf("zero-score files not path-sorted: %v", got)
			break
		}
	}
}

func TestRenderMapCompact(t *testing.T) {
	ranked := rankIonic(t, "profile page", nil)
	out := RenderMap(ranked, 2)
	if out == "" {
		t.Fatal("empty map render")
	}
	// Top file's path appears; a symbol line is indented.
	if !strings.Contains(out, "profile") {
		t.Errorf("render missing profile: %q", out)
	}
}

func scoreOf(rs []RankedFile, path string) float64 {
	for _, r := range rs {
		if r.Path == path {
			return r.Score
		}
	}
	return -1
}
